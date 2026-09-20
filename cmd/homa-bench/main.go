// homa-bench compares sequential verified echo RPCs over native Homa and
// persistent TCP. Both transports are plaintext and carry identical payloads.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	homa "github.com/phenomenon0/MacHoma"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "homa-bench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "serve" && args[0] != "client" {
		return errors.New("usage: homa-bench serve|client [flags]; use -h for options")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Second, "timeout per echo or TCP frame body")
	duration := fs.Duration("duration", 10*time.Minute, "maximum process run duration, at most 10m")
	var bind, local, peer, tcpPeer, sizes, out, scope *string
	var warmup, small, large, rounds *int
	if args[0] == "serve" {
		bind = fs.String("bind", "127.0.0.1", "local IPv4; Homa port 4000 and TCP port 14000")
	} else {
		local = fs.String("local", "127.0.0.1:4003", "local IPv4:Homa-port")
		peer = fs.String("peer", "127.0.0.1:4000", "server IPv4:Homa-port")
		tcpPeer = fs.String("tcp-peer", "127.0.0.1:14000", "same server IPv4:TCP-port")
		sizes = fs.String("sizes", "64,1024,16384,65536,1000000", "comma-separated application payload bytes")
		warmup = fs.Int("warmup", 10, "unmeasured verified warmups before each transport/size/round")
		small = fs.Int("small-samples", 1000, "samples per round for payloads below 65536 bytes")
		large = fs.Int("large-samples", 100, "samples per round for payloads at least 65536 bytes")
		rounds = fs.Int("rounds", 3, "paired rounds; transport order reverses each round")
		out = fs.String("out", "-", "JSON output path (created exclusively), or - for stdout")
		scope = fs.String("scope", "unspecified network", "network description recorded with results, e.g. WiFi LAN")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *timeout < time.Millisecond || *timeout > time.Minute {
		return errors.New("timeout must be between 1ms and 1m")
	}
	if *duration <= 0 || *duration > 10*time.Minute {
		return errors.New("duration must be positive and at most 10m")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	if args[0] == "serve" {
		return serve(ctx, *bind, *timeout, *duration)
	}
	parsedSizes, err := parseSizes(*sizes)
	if err != nil {
		return err
	}
	if *warmup < 0 || *warmup > 1000 || *small < 1 || *small > 100000 || *large < 1 || *large > 10000 || *rounds < 1 || *rounds > 20 {
		return errors.New("require warmup 0..1000, small-samples 1..100000, large-samples 1..10000, rounds 1..20")
	}
	var samples int64
	for _, size := range parsedSizes {
		n := *small
		if size >= 65536 {
			n = *large
		}
		samples += int64(n) * int64(*rounds) * 2
	}
	if samples > 2000000 {
		return errors.New("requested workload exceeds 2,000,000 total measured calls")
	}
	return client(ctx, clientOptions{Local: *local, Peer: *peer, TCPPeer: *tcpPeer, Sizes: parsedSizes,
		Warmup: *warmup, SmallSamples: *small, LargeSamples: *large, Rounds: *rounds, Timeout: *timeout,
		Scope: *scope}, *out)
}

func parseSizes(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	if len(parts) > 32 {
		return nil, errors.New("at most 32 payload sizes are allowed")
	}
	result := make([]int, 0, len(parts))
	seen := make(map[int]bool)
	for _, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 1 || n > homa.MaxMessageSize || seen[n] {
			return nil, errors.New("sizes must be distinct integers between 1 and 1,000,000")
		}
		seen[n] = true
		result = append(result, n)
	}
	return result, nil
}

func serve(ctx context.Context, bind string, timeout, duration time.Duration) error {
	ip, err := netip.ParseAddr(bind)
	if err != nil || !ip.Is4() {
		return errors.New("bind must be a literal IPv4 address")
	}
	h, err := homa.Listen(homa.Addr{IP: ip, Port: 4000}, homa.Config{Timeout: timeout},
		func(_ context.Context, _ homa.Addr, request []byte) []byte { return request })
	if err != nil {
		return err
	}
	defer h.Close()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IP(ip.AsSlice()), Port: 14000})
	if err != nil {
		return err
	}
	defer listener.Close()
	var mu sync.Mutex
	connections := make(map[*net.TCPConn]struct{})
	var workers sync.WaitGroup
	shutdown := func() {
		listener.Close()
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		h.Close()
	}
	stopClose := context.AfterFunc(ctx, shutdown)
	defer func() { stopClose(); shutdown(); workers.Wait() }()
	fmt.Fprintf(os.Stderr, "Echo server: Homa %s:4000; persistent TCP_NODELAY %s:14000; plaintext; max 16 TCP connections; duration %s\n", ip, ip, duration)
	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := conn.SetNoDelay(true); err != nil {
			conn.Close()
			continue
		}
		mu.Lock()
		if ctx.Err() != nil || len(connections) >= 16 {
			mu.Unlock()
			conn.Close()
			continue
		}
		connections[conn] = struct{}{}
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { conn.Close(); mu.Lock(); delete(connections, conn); mu.Unlock() }()
			for {
				// Keep the persistent stream alive while the other transport is
				// being measured; limit frame bodies and writes independently.
				conn.SetReadDeadline(time.Now().Add(duration))
				var header [4]byte
				if _, err := io.ReadFull(conn, header[:]); err != nil {
					return
				}
				n := binary.BigEndian.Uint32(header[:])
				if n == 0 || n > homa.MaxMessageSize {
					return
				}
				conn.SetReadDeadline(time.Now().Add(timeout))
				payload := make([]byte, int(n))
				if _, err := io.ReadFull(conn, payload); err != nil {
					return
				}
				conn.SetWriteDeadline(time.Now().Add(timeout))
				if err := writeFrame(conn, payload); err != nil {
					return
				}
			}
		}()
	}
}

func writeFrame(conn *net.TCPConn, payload []byte) error {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	// writev keeps the prefix and body in one socket operation, avoiding an
	// artificial small-packet penalty from two TCP_NODELAY writes.
	parts := net.Buffers{header[:], payload}
	n, err := parts.WriteTo(conn)
	if err == nil && n != int64(len(payload)+4) {
		err = io.ErrShortWrite
	}
	return err
}

func tcpCall(ctx context.Context, conn *net.TCPConn, payload []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if err := writeFrame(conn, payload); err != nil {
		return nil, err
	}
	var header [4]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > homa.MaxMessageSize {
		return nil, errors.New("invalid TCP reply length")
	}
	reply := make([]byte, int(n))
	_, err := io.ReadFull(conn, reply)
	return reply, err
}

type clientOptions struct {
	Local        string        `json:"local"`
	Peer         string        `json:"homa_peer"`
	TCPPeer      string        `json:"tcp_peer"`
	Sizes        []int         `json:"payload_sizes"`
	Warmup       int           `json:"warmups_per_measurement"`
	SmallSamples int           `json:"small_samples"`
	LargeSamples int           `json:"large_samples"`
	Rounds       int           `json:"rounds"`
	Timeout      time.Duration `json:"call_timeout_ns"`
	Scope        string        `json:"network_scope"`
}

type measurement struct {
	Round        int         `json:"round"`
	Order        int         `json:"order_in_pair"`
	Transport    string      `json:"transport"`
	Bytes        int         `json:"payload_bytes"`
	Count        int         `json:"verified_calls"`
	Elapsed      float64     `json:"elapsed_seconds_including_validation"`
	PayloadBytes int64       `json:"verified_bidirectional_payload_bytes"`
	Goodput      float64     `json:"bidirectional_payload_goodput_mbps"`
	P50          float64     `json:"p50_rtt_us"`
	P95          float64     `json:"p95_rtt_us"`
	P99          float64     `json:"p99_rtt_us"`
	Max          float64     `json:"max_rtt_us"`
	Samples      []int64     `json:"rtt_samples_ns"`
	HomaBefore   *homa.Stats `json:"homa_stats_before,omitempty"`
	HomaAfter    *homa.Stats `json:"homa_stats_after,omitempty"`
}

func client(ctx context.Context, options clientOptions, outPath string) error {
	local, err := homa.ParseAddr(options.Local)
	if err != nil {
		return fmt.Errorf("local: %w", err)
	}
	peer, err := homa.ParseAddr(options.Peer)
	if err != nil {
		return fmt.Errorf("peer: %w", err)
	}
	tcpAddr, err := netip.ParseAddrPort(options.TCPPeer)
	if err != nil || !tcpAddr.Addr().Is4() || tcpAddr.Port() == 0 || tcpAddr.Addr() != peer.IP {
		return errors.New("tcp-peer must use the same literal IPv4 host as peer")
	}
	if local == peer {
		return errors.New("local and peer Homa endpoints must differ")
	}
	var output io.Writer = os.Stdout
	if outPath != "-" {
		file, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		output = file
	}
	h, err := homa.Listen(local, homa.Config{Timeout: options.Timeout}, nil)
	if err != nil {
		return err
	}
	defer h.Close()
	dialCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	connection, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.IP(local.IP.AsSlice())}}).DialContext(dialCtx, "tcp4", options.TCPPeer)
	cancel()
	if err != nil {
		return err
	}
	conn := connection.(*net.TCPConn)
	defer conn.Close()
	if err = conn.SetNoDelay(true); err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() { conn.Close(); h.Close() })
	defer stopClose()
	var results []measurement
	started := time.Now()
	for round := 1; round <= options.Rounds; round++ {
		order := []string{"homa", "tcp"}
		if round%2 == 0 {
			order = []string{"tcp", "homa"}
		}
		for _, size := range options.Sizes {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte((i * 31) ^ (i >> 8) ^ round)
			}
			n := options.SmallSamples
			if size >= 65536 {
				n = options.LargeSamples
			}
			for position, transport := range order {
				exchange := func(c context.Context, p []byte) ([]byte, error) { return h.Call(c, peer, p) }
				if transport == "tcp" {
					exchange = func(c context.Context, p []byte) ([]byte, error) { return tcpCall(c, conn, p) }
				}
				for warm := 0; warm < options.Warmup; warm++ {
					callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
					reply, err := exchange(callCtx, payload)
					cancel()
					if err != nil {
						return fmt.Errorf("round %d %s %d-byte warmup %d: %w", round, transport, size, warm+1, err)
					}
					if !bytes.Equal(reply, payload) {
						return fmt.Errorf("round %d %s %d-byte warmup echo mismatch", round, transport, size)
					}
				}
				m := measurement{Round: round, Order: position + 1, Transport: transport, Bytes: size, Samples: make([]int64, 0, n)}
				if transport == "homa" {
					s := h.Stats()
					m.HomaBefore = &s
				}
				beganRound := time.Now()
				for sample := 0; sample < n; sample++ {
					callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
					began := time.Now()
					reply, err := exchange(callCtx, payload)
					elapsed := time.Since(began)
					cancel()
					if err != nil {
						return fmt.Errorf("round %d %s %d-byte sample %d/%d failed; entire run invalid: %w", round, transport, size, sample+1, n, err)
					}
					// Full validation is deliberately outside the RTT interval.
					if !bytes.Equal(reply, payload) {
						return fmt.Errorf("round %d %s %d-byte sample %d echo mismatch; entire run invalid", round, transport, size, sample+1)
					}
					m.Samples = append(m.Samples, elapsed.Nanoseconds())
				}
				m.Elapsed = time.Since(beganRound).Seconds()
				m.Count = len(m.Samples)
				m.PayloadBytes = int64(2*size) * int64(m.Count)
				m.Goodput = float64(m.PayloadBytes) * 8 / m.Elapsed / 1e6
				m.P50, m.P95, m.P99, m.Max = latencySummary(m.Samples)
				if transport == "homa" {
					s := h.Stats()
					m.HomaAfter = &s
				}
				results = append(results, m)
				fmt.Fprintf(os.Stderr, "round %d %s bytes=%d n=%d p50=%.1f us p99=%.1f us payload-goodput=%.2f Mbps\n", round, transport, size, n, m.P50, m.P99, m.Goodput)
			}
		}
	}
	report := struct {
		Status       string        `json:"status"`
		Started      time.Time     `json:"started_utc"`
		Platform     string        `json:"client_platform"`
		GoVersion    string        `json:"go_version"`
		Evidence     string        `json:"evidence"`
		Options      clientOptions `json:"options"`
		Elapsed      float64       `json:"total_elapsed_seconds"`
		Measurements []measurement `json:"measurements"`
	}{Status: "passed", Started: started.UTC(), Platform: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version(),
		Evidence: "Sequential verified plaintext echo RTT and bidirectional application-payload goodput; one persistent endpoint/connection; not link capacity, loaded tail-latency, or the Homa paper's kernel implementation performance. Homa requests IP precedence priorities while TCP uses default QoS; physical-network differences can include QoS treatment, not only transport logic.",
		Options:  options, Elapsed: time.Since(started).Seconds(), Measurements: results}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

// latencySummary uses nearest-rank percentiles and leaves the original sample
// sequence intact so that the report retains run-order evidence. Callers supply
// at least one measured sample. Results are microseconds.
func latencySummary(samples []int64) (p50, p95, p99, maximum float64) {
	ordered := append([]int64(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	quantile := func(q float64) float64 {
		return float64(ordered[int(math.Ceil(q*float64(len(ordered))))-1]) / 1000
	}
	return quantile(.5), quantile(.95), quantile(.99), quantile(1)
}
