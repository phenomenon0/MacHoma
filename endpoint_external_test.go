package homa_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	homa "github.com/phenomenon0/MacHoma"
	"github.com/phenomenon0/MacHoma/internal/wire"
)

type nativeFrame struct {
	data []byte
	from netip.Addr
}

// nativePipe preserves native Homa transport bytes and the IP source address.
// It models packet loss/reordering at PacketIO, not a second transport protocol.
type nativePipe struct {
	ip      netip.Addr
	peer    *nativePipe
	input   chan nativeFrame
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	counts  map[wire.Type]int
	fault   func(wire.Packet, int) string
	held    []nativeFrame
	actions map[string]int
}

func nativePair(t *testing.T) (*nativePipe, *nativePipe) {
	t.Helper()
	newPipe := func(ip string) *nativePipe {
		return &nativePipe{ip: netip.MustParseAddr(ip), input: make(chan nativeFrame, 512), done: make(chan struct{}), counts: make(map[wire.Type]int), actions: make(map[string]int)}
	}
	a, b := newPipe("192.0.2.1"), newPipe("192.0.2.2")
	a.peer, b.peer = b, a
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func (p *nativePipe) ReadPacket(buf []byte) (int, netip.Addr, error) {
	select {
	case <-p.done:
		return 0, netip.Addr{}, io.ErrClosedPipe
	case f := <-p.input:
		return copy(buf, f.data), f.from, nil
	}
}

func (p *nativePipe) deliver(f nativeFrame) error {
	select {
	case <-p.done:
		return io.ErrClosedPipe
	case <-p.peer.done:
		return io.ErrClosedPipe
	case p.peer.input <- f:
		return nil
	default:
		return nil // A full receive queue is packet loss, as on a real NIC.
	}
}

func (p *nativePipe) WritePacket(data []byte, dest netip.Addr, _ uint8) error {
	if dest != p.peer.ip {
		return fmt.Errorf("test network: unknown destination %v", dest)
	}
	decoded, err := wire.Parse(data)
	if err != nil {
		return fmt.Errorf("endpoint emitted invalid native packet: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	typ := decoded.Header().Type
	p.counts[typ]++
	action := ""
	if p.fault != nil {
		action = p.fault(decoded, p.counts[typ])
	}
	if action != "" {
		p.actions[action]++
	}
	f := nativeFrame{data: append([]byte(nil), data...), from: p.ip}
	switch action {
	case "drop":
		return nil
	case "hold":
		p.held = append(p.held, f)
		return nil
	case "duplicate":
		if err := p.deliver(f); err != nil {
			return err
		}
	}
	if err := p.deliver(f); err != nil {
		return err
	}
	if action == "release" {
		for _, held := range p.held {
			if err := p.deliver(held); err != nil {
				return err
			}
		}
		p.held = nil
	}
	return nil
}

func (p *nativePipe) Close() error { p.once.Do(func() { close(p.done) }); return nil }

func nativeConfig() homa.Config {
	return homa.Config{Timeout: time.Second, RetryInterval: 2 * time.Millisecond, SegmentBytes: 509, UnscheduledBytes: 1700, GrantWindow: 1700}
}

func nativeEndpoint(t *testing.T, p *nativePipe, port uint16, cfg homa.Config, handler homa.Handler) *homa.Endpoint {
	t.Helper()
	e, err := homa.New(p, homa.Addr{IP: p.ip, Port: port}, cfg, handler)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func nativeEventually(t *testing.T, timeout time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func nativePayload(n int, salt byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*37+i/251) ^ salt
	}
	return b
}

func injectNative(t *testing.T, p *nativePipe, from netip.Addr, packet wire.Packet) {
	t.Helper()
	b, err := wire.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case p.input <- nativeFrame{b, from}:
	case <-time.After(time.Second):
		t.Fatal("manual native packet queue blocked")
	}
}

func TestNativeRecoversDataGrantAndAckLossWithDifferentSegmentation(t *testing.T) {
	clientIO, serverIO := nativePair(t)
	clientIO.fault = func(p wire.Packet, n int) string {
		switch p.Header().Type {
		case wire.DataType:
			switch n {
			case 1:
				return "drop"
			case 3:
				return "hold"
			case 4:
				return "release"
			}
		case wire.AckType:
			if n == 1 {
				return "drop"
			}
		}
		return ""
	}
	serverIO.fault = func(p wire.Packet, n int) string {
		switch p.Header().Type {
		case wire.GrantType:
			if n == 1 {
				return "drop"
			}
		case wire.ResendType:
			if n == 1 {
				return "drop"
			}
		case wire.DataType:
			if n == 1 {
				return "drop"
			}
			if n == 3 {
				return "duplicate"
			}
		}
		return ""
	}
	request, response := nativePayload(27*1024+17, 0x56), nativePayload(19*1024+81, 0xa7)
	var executions atomic.Int32
	serverCfg := nativeConfig()
	serverCfg.SegmentBytes = 613
	server := nativeEndpoint(t, serverIO, 123, serverCfg, func(_ context.Context, _ homa.Addr, got []byte) []byte {
		executions.Add(1)
		if !bytes.Equal(got, request) {
			return []byte("corrupt request")
		}
		return append([]byte(nil), response...)
	})
	client := nativeEndpoint(t, clientIO, 40001, nativeConfig(), nil)
	got, err := client.Call(context.Background(), server.LocalAddr(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, response) {
		t.Fatalf("response bytes differ: got=%d want=%d", len(got), len(response))
	}
	if executions.Load() != 1 {
		t.Fatalf("handler ran %d times", executions.Load())
	}
	nativeEventually(t, time.Second, "NEED_ACK recovery of dropped ACK", func() bool { return server.Stats().ActiveRPCs == 0 })
	for name, p := range map[string]*nativePipe{"client": clientIO, "server": serverIO} {
		p.mu.Lock()
		if p.actions["drop"] == 0 {
			t.Errorf("%s loss fault was never activated", name)
		}
		if name == "client" && (p.actions["hold"] != 1 || p.actions["release"] != 1) {
			t.Error("reordering fault never activated")
		}
		if name == "client" && p.counts[wire.AckType] < 2 {
			t.Error("lost ACK was not recovered by a second ACK")
		}
		if name == "server" && p.actions["duplicate"] != 1 {
			t.Error("duplication fault never activated")
		}
		if name == "server" && p.counts[wire.NeedAckType] == 0 {
			t.Error("server never requested acknowledgment after ACK loss")
		}
		p.mu.Unlock()
	}
	if client.Stats().Retransmissions+server.Stats().Retransmissions == 0 {
		t.Fatal("loss did not exercise retransmission")
	}
}

func TestNativeConcurrentShortAndLongRPCsKeepPayloadsIsolated(t *testing.T) {
	clientIO, serverIO := nativePair(t)
	server := nativeEndpoint(t, serverIO, 122, nativeConfig(), func(_ context.Context, _ homa.Addr, b []byte) []byte {
		return append([]byte("reply:"), b...)
	})
	client := nativeEndpoint(t, clientIO, 40007, nativeConfig(), nil)
	const count = 18
	results := make(chan error, count)
	start := make(chan struct{})
	for i := 0; i < count; i++ {
		go func(i int) {
			<-start
			size := 19 + i
			if i%3 == 0 {
				size = 20*1024 + i
			}
			request := nativePayload(size, byte(i))
			got, err := client.Call(context.Background(), server.LocalAddr(), request)
			if err == nil && !bytes.Equal(got, append([]byte("reply:"), request...)) {
				err = fmt.Errorf("RPC %d received another call's payload", i)
			}
			results <- err
		}(i)
	}
	close(start)
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if got := server.Stats().HandledRequests; got != count {
		t.Fatalf("handled %d requests, want %d", got, count)
	}
}

func TestNativeLostEntireFirstRequestRecoversThroughRPCUnknown(t *testing.T) {
	clientIO, serverIO := nativePair(t)
	clientIO.fault = func(p wire.Packet, n int) string {
		if p.Header().Type == wire.DataType && n == 1 {
			return "drop"
		}
		return ""
	}
	server := nativeEndpoint(t, serverIO, 124, nativeConfig(), func(_ context.Context, _ homa.Addr, b []byte) []byte { return append([]byte("reply:"), b...) })
	client := nativeEndpoint(t, clientIO, 40001, nativeConfig(), nil)
	got, err := client.Call(context.Background(), server.LocalAddr(), []byte("tiny request"))
	if err != nil || string(got) != "reply:tiny request" {
		t.Fatalf("reply=%q error=%v", got, err)
	}
	serverIO.mu.Lock()
	count := serverIO.counts[wire.RPCUnknownType]
	serverIO.mu.Unlock()
	if count == 0 {
		t.Fatal("server never sent RPC_UNKNOWN after missing the entire request")
	}
}

// Hand-constructed native datagrams establish behavior independently of the
// endpoint's sender: arbitrary boundaries, reverse order and overlapping bytes.
func TestNativeManualPeerArbitrarySegmentsAndConflictingOverlap(t *testing.T) {
	peerIO, serverIO := nativePair(t)
	request := nativePayload(4099, 0x93)
	digest := sha256.Sum256(request)
	received := make(chan []byte, 1)
	server := nativeEndpoint(t, serverIO, 125, nativeConfig(), func(_ context.Context, _ homa.Addr, b []byte) []byte {
		received <- append([]byte(nil), b...)
		return digest[:]
	})
	const id uint64 = 0x10203040
	packet := func(offset int, payload []byte) wire.Data {
		return wire.Data{Common: wire.NewCommon(wire.DataType, 40002, 125, id), MessageLength: uint32(len(request)), Incoming: uint32(len(request)), SegmentOffset: uint32(offset), Payload: payload}
	}
	injectNative(t, serverIO, peerIO.ip, packet(0, request[:7]))
	bad := append([]byte(nil), request[3:7]...)
	bad[0] ^= 1
	injectNative(t, serverIO, peerIO.ip, packet(3, bad))
	type span struct{ start, end int }
	var spans []span
	for start := 7; start < len(request); {
		end := min(len(request), start+17+(start*13)%257)
		spans = append(spans, span{start, end})
		start = end
	}
	for i := len(spans) - 1; i >= 1; i-- {
		s := spans[i]
		injectNative(t, serverIO, peerIO.ip, packet(s.start, request[s.start:s.end]))
		if i%5 == 0 {
			injectNative(t, serverIO, peerIO.ip, packet(s.start, request[s.start:s.end]))
		}
	}
	nativeEventually(t, time.Second, "conflicting overlap rejection", func() bool { return server.Stats().InvalidPackets >= 1 })
	select {
	case <-received:
		t.Fatal("handler ran with a missing segment")
	default:
	}
	s := spans[0]
	injectNative(t, serverIO, peerIO.ip, packet(s.start, request[s.start:s.end]))
	select {
	case got := <-received:
		if !bytes.Equal(got, request) {
			t.Fatal("reassembly silently changed bytes")
		}
	case <-time.After(time.Second):
		t.Fatal("arbitrarily segmented request did not reassemble")
	}
	deadline := time.After(time.Second)
	for {
		select {
		case f := <-peerIO.input:
			p, err := wire.Parse(f.data)
			if err != nil {
				t.Fatal(err)
			}
			data, ok := p.(wire.Data)
			if !ok {
				continue
			}
			if data.SenderID != id^1 || data.SourcePort != 125 || data.DestinationPort != 40002 || !bytes.Equal(data.Payload, digest[:]) {
				t.Fatalf("manual peer response invalid: %#v", data)
			}
			injectNative(t, serverIO, peerIO.ip, wire.Ack{Common: wire.NewCommon(wire.AckType, 40002, 125, id)})
			nativeEventually(t, time.Second, "manual ACK releasing server state", func() bool { s := server.Stats(); return s.ActiveRPCs == 0 && s.BufferedBytes == 0 })
			return
		case <-deadline:
			t.Fatal("server never produced native DATA response")
		}
	}
}

func TestNativeCallerDeadlineCancellationAndCloseUnblock(t *testing.T) {
	clientIO, peerIO := nativePair(t)
	client := nativeEndpoint(t, clientIO, 40003, nativeConfig(), nil)
	peer := homa.Addr{IP: peerIO.ip, Port: 126}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Call(canceled, peer, []byte("not sent"))
	var callErr *homa.CallError
	if !errors.Is(err, context.Canceled) || !errors.As(err, &callErr) || callErr.Sent {
		t.Fatalf("pre-canceled error=%#v", err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer stop()
	_, err = client.Call(ctx, peer, []byte("sent but lost"))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &callErr) || !callErr.Sent {
		t.Fatalf("deadline error=%#v", err)
	}
	if s := client.Stats(); s.ActiveRPCs != 0 || s.BufferedBytes != 0 {
		t.Fatalf("deadline leaked caller state: %#v", s)
	}
	done := make(chan error, 1)
	go func() { _, err := client.Call(context.Background(), peer, []byte("close me")); done <- err }()
	nativeEventually(t, time.Second, "pending call", func() bool { return client.Stats().ActiveRPCs == 1 })
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, homa.ErrClosed) || !errors.As(err, &callErr) || !callErr.Sent {
			t.Fatalf("close error=%#v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Call")
	}
	if s := client.Stats(); s.ActiveRPCs != 0 || s.BufferedBytes != 0 {
		t.Fatalf("Close leaked state: %#v", s)
	}
}

func TestNativeResponseMustMatchPeerAddressAndPort(t *testing.T) {
	clientIO, peerIO := nativePair(t)
	client := nativeEndpoint(t, clientIO, 40004, nativeConfig(), nil)
	peer := homa.Addr{IP: peerIO.ip, Port: 127}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() { b, err := client.Call(context.Background(), peer, []byte("request")); done <- result{b, err} }()
	var first nativeFrame
	select {
	case first = <-peerIO.input:
	case <-time.After(time.Second):
		t.Fatal("request never sent")
	}
	p, err := wire.Parse(first.data)
	if err != nil {
		t.Fatal(err)
	}
	reply := wire.Data{Common: wire.NewCommon(wire.DataType, 127, 40004, p.Header().SenderID^1), MessageLength: 4, Incoming: 4, Payload: []byte("good")}
	injectNative(t, clientIO, netip.MustParseAddr("192.0.2.99"), reply)
	wrongPort := reply
	wrongPort.SourcePort = 128
	injectNative(t, clientIO, peerIO.ip, wrongPort)
	select {
	case got := <-done:
		t.Fatalf("foreign peer completed RPC: %#v", got)
	case <-time.After(20 * time.Millisecond):
	}
	injectNative(t, clientIO, peerIO.ip, reply)
	select {
	case got := <-done:
		if got.err != nil || string(got.data) != "good" {
			t.Fatalf("correct peer reply=%q err=%v", got.data, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("correct peer did not finish RPC")
	}
}

func TestNativeIncompleteRequestFloodHasBoundedAndExpiringState(t *testing.T) {
	peerIO, serverIO := nativePair(t)
	cfg := nativeConfig()
	cfg.MaxRPCs = 3
	cfg.MaxBufferedBytes = 2304
	cfg.Timeout = 60 * time.Millisecond
	var calls atomic.Int32
	server := nativeEndpoint(t, serverIO, 129, cfg, func(_ context.Context, _ homa.Addr, _ []byte) []byte { calls.Add(1); return []byte("unexpected") })
	for i := 0; i < 40; i++ {
		injectNative(t, serverIO, peerIO.ip, wire.Data{Common: wire.NewCommon(wire.DataType, 40005, 129, uint64(2+i*2)), MessageLength: 1024, Incoming: 1, Payload: []byte{byte(i)}})
	}
	nativeEventually(t, time.Second, "all flood packets processed", func() bool { return server.Stats().ReceivedPackets >= 40 })
	s := server.Stats()
	if s.ActiveRPCs > cfg.MaxRPCs || s.BufferedBytes > cfg.MaxBufferedBytes || s.BufferedBytes == 0 {
		t.Fatalf("invalid admission accounting: %#v", s)
	}
	nativeEventually(t, time.Second, "incomplete message expiry", func() bool { s := server.Stats(); return s.ActiveRPCs == 0 && s.BufferedBytes == 0 })
	if calls.Load() != 0 {
		t.Fatalf("incomplete messages ran %d handlers", calls.Load())
	}
}

func TestNativeQueuedHandlerRunsAfterExpiredWorkerReturns(t *testing.T) {
	peerIO, serverIO := nativePair(t)
	cfg := nativeConfig()
	cfg.HandlerWorkers = 1
	cfg.Timeout = 400 * time.Millisecond
	firstStarted, firstCanceled, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	server := nativeEndpoint(t, serverIO, 130, cfg, func(ctx context.Context, _ homa.Addr, request []byte) []byte {
		if string(request) == "first" {
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			<-release
			return []byte("late")
		}
		close(secondStarted)
		return []byte("second reply")
	})
	request := func(id uint64, payload string) wire.Data {
		return wire.Data{Common: wire.NewCommon(wire.DataType, 40006, 130, id), MessageLength: uint32(len(payload)), Incoming: uint32(len(payload)), Payload: []byte(payload)}
	}
	injectNative(t, serverIO, peerIO.ip, request(2, "first"))
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first handler did not start")
	}
	time.Sleep(200 * time.Millisecond)
	injectNative(t, serverIO, peerIO.ip, request(4, "second"))
	nativeEventually(t, time.Second, "second request queued", func() bool { return server.Stats().ActiveRPCs == 2 })
	select {
	case <-secondStarted:
		t.Fatal("worker limit was exceeded")
	default:
	}
	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("first handler context did not expire")
	}
	nativeEventually(t, time.Second, "first RPC removed while handler still owns bytes", func() bool { return server.Stats().ActiveRPCs == 1 })
	if s := server.Stats(); s.ActiveHandlers != 1 || s.BufferedBytes < len("first")+len("second") {
		t.Fatalf("live handler payload escaped accounting: %#v", s)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-secondStarted:
	case <-time.After(150 * time.Millisecond):
		t.Fatal("queued handler stranded after expired worker returned")
	}
}
