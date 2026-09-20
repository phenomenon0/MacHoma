// homa-echo exercises the native Homa IPv4 wire protocol. Run serve and call on
// separate hosts, or distinct Homa ports on loopback, with raw-socket privileges.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"syscall"
	"time"

	homa "github.com/phenomenon0/MacHoma"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "homa-echo:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 && args[0] == "selftest" {
		return selftest(args[1:])
	}
	if len(args) == 0 || args[0] != "serve" && args[0] != "call" {
		return errors.New("usage: homa-echo selftest [-timeout 5s] | serve|call [-listen IPv4:port] [-peer IPv4:port] [-bytes N] [-count N] [-timeout 5s]")
	}
	mode := args[0]
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	defaultListen := "127.0.0.1:4000"
	if mode == "call" {
		defaultListen = "127.0.0.1:4001"
	}
	listen := fs.String("listen", defaultListen, "local literal IPv4 address and nonzero Homa port")
	peer := fs.String("peer", "127.0.0.1:4000", "server literal IPv4 address and Homa port (call mode)")
	size := fs.Int("bytes", 128, "request/reply bytes, 1 to 1000000 (call mode)")
	count := fs.Int("count", 1, "sequential echo calls, 1 to 1000000 (call mode)")
	timeout := fs.Duration("timeout", 5*time.Second, "RPC timeout, positive and at most 1 minute")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *size < 1 || *size > homa.MaxMessageSize || *count < 1 || *count > 1000000 {
		return errors.New("bytes and count must each be between 1 and 1000000")
	}
	if *timeout <= 0 || *timeout > time.Minute {
		return errors.New("timeout must be positive and at most 1 minute")
	}
	local, err := homa.ParseAddr(*listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	var remote homa.Addr
	if mode == "call" {
		remote, err = homa.ParseAddr(*peer)
		if err != nil {
			return fmt.Errorf("peer: %w", err)
		}
		if local == remote {
			return errors.New("client and server must use distinct Homa addresses or ports")
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var handler homa.Handler
	if mode == "serve" {
		handler = func(_ context.Context, _ homa.Addr, request []byte) []byte { return request }
	}
	endpoint, err := homa.Listen(local, homa.Config{Timeout: *timeout}, handler)
	if err != nil {
		return err // OpenRaw includes the raw-socket privilege requirement.
	}
	defer endpoint.Close()
	stopClose := context.AfterFunc(ctx, func() { endpoint.Close() })
	defer stopClose()
	fmt.Fprintf(os.Stderr, "Native Homa IPv4 protocol %d on %s; unauthenticated plaintext; use a trusted test network.\n", homa.Protocol, local)
	if mode == "serve" {
		fmt.Fprintln(os.Stderr, "Echo server ready.")
		<-ctx.Done()
		return nil
	}
	latencies := make([]time.Duration, 0, *count)
	started := time.Now()
	for i := 0; i < *count; i++ {
		payload := make([]byte, *size)
		for j := range payload {
			payload[j] = byte(i*17 + j*31)
		}
		callCtx, cancel := context.WithTimeout(ctx, *timeout)
		began := time.Now()
		reply, callErr := endpoint.Call(callCtx, remote, payload)
		elapsed := time.Since(began)
		cancel()
		if callErr != nil {
			return fmt.Errorf("call %d/%d failed; completed %d verified echoes: %w", i+1, *count, len(latencies), callErr)
		}
		if !bytes.Equal(reply, payload) {
			return fmt.Errorf("call %d/%d echo mismatch: sent %d bytes, received %d", i+1, *count, len(payload), len(reply))
		}
		latencies = append(latencies, elapsed)
	}
	elapsed := time.Since(started)
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	report := struct {
		Protocol int        `json:"ip_protocol"`
		Local    string     `json:"local"`
		Peer     string     `json:"peer"`
		Calls    int        `json:"verified_echoes"`
		Bytes    int        `json:"bytes_per_request_and_reply"`
		Elapsed  float64    `json:"elapsed_seconds"`
		Median   float64    `json:"median_rtt_us"`
		Max      float64    `json:"max_rtt_us"`
		Stats    homa.Stats `json:"endpoint_stats"`
	}{homa.Protocol, local.String(), remote.String(), len(latencies), *size, elapsed.Seconds(),
		float64(latencies[(len(latencies)-1)/2]) / float64(time.Microsecond),
		float64(latencies[len(latencies)-1]) / float64(time.Microsecond), endpoint.Stats()}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// selftest validates two userspace endpoints through real native raw IPv4
// sockets. It does not establish interoperability with the Linux kernel module.
func selftest(args []string) error {
	fs := flag.NewFlagSet("selftest", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Second, "timeout per echo, positive and at most 1 minute")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *timeout <= 0 || *timeout > time.Minute {
		return errors.New("timeout must be positive and at most 1 minute")
	}
	serverAddr, _ := homa.ParseAddr("127.0.0.1:4000")
	clientAddr, _ := homa.ParseAddr("127.0.0.1:4001")
	fmt.Fprintln(os.Stderr, "Native Homa protocol 146 self-test: raw IPv4 loopback between userspace endpoints.")
	fmt.Fprintln(os.Stderr, "Using server 127.0.0.1:4000 and client 127.0.0.1:4001; these Homa ports must be unused.")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg := homa.Config{Timeout: *timeout}
	server, err := homa.Listen(serverAddr, cfg, func(_ context.Context, _ homa.Addr, request []byte) []byte { return request })
	if err != nil {
		return fmt.Errorf("selftest server: %w", err)
	}
	defer server.Close()
	client, err := homa.Listen(clientAddr, cfg, nil)
	if err != nil {
		return fmt.Errorf("selftest client: %w", err)
	}
	defer client.Close()
	stopClose := context.AfterFunc(ctx, func() { client.Close(); server.Close() })
	defer stopClose()
	type echoResult struct {
		Bytes int     `json:"bytes"`
		RTT   float64 `json:"rtt_us"`
	}
	sizes := []int{1, 100, 1424, 65535, 65536, 65537, 100000, 1000000}
	results := make([]echoResult, 0, len(sizes))
	started := time.Now()
	for i, size := range sizes {
		payload := make([]byte, size)
		for j := range payload {
			payload[j] = byte(i*17 + j*31)
		}
		callCtx, cancel := context.WithTimeout(ctx, *timeout)
		began := time.Now()
		reply, callErr := client.Call(callCtx, serverAddr, payload)
		elapsed := time.Since(began)
		cancel()
		if callErr != nil {
			return fmt.Errorf("selftest %d-byte echo failed after %d verified sizes: %w", size, len(results), callErr)
		}
		if !bytes.Equal(reply, payload) {
			return fmt.Errorf("selftest %d-byte echo mismatch: received %d bytes", size, len(reply))
		}
		results = append(results, echoResult{Bytes: size, RTT: float64(elapsed) / float64(time.Microsecond)})
		fmt.Fprintf(os.Stderr, "Verified exact %d-byte echo.\n", size)
	}
	report := struct {
		Status        string       `json:"status"`
		Evidence      string       `json:"evidence"`
		KernelInterop bool         `json:"linux_kernel_interoperability_verified"`
		Platform      string       `json:"platform"`
		Protocol      int          `json:"ip_protocol"`
		Server        string       `json:"server"`
		Client        string       `json:"client"`
		Elapsed       float64      `json:"elapsed_seconds"`
		Results       []echoResult `json:"verified_echoes"`
		ServerStats   homa.Stats   `json:"server_stats"`
		ClientStats   homa.Stats   `json:"client_stats"`
	}{Status: "passed", Evidence: "native raw IPv4 loopback between two userspace Homa endpoints",
		KernelInterop: false, Platform: runtime.GOOS + "/" + runtime.GOARCH, Protocol: homa.Protocol,
		Server: serverAddr.String(), Client: clientAddr.String(), Elapsed: time.Since(started).Seconds(),
		Results: results, ServerStats: server.Stats(), ClientStats: client.Stats()}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
