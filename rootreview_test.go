package homa

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/phenomenon0/MacHoma/internal/wire"
)

// reviewPacketIO models a slow but bounded socket independently of the RPC
// scheduler. Incoming packets are injected explicitly; outgoing packets remain
// observable so loss recovery can be checked without raw-socket privileges.
type reviewPacketIO struct {
	done   chan struct{}
	writes chan []byte
	once   sync.Once
	delay  time.Duration
}

type reviewDatagram struct {
	data []byte
	peer netip.Addr
}

// reviewQueueIO blocks the first socket write so tests can construct a real
// backlog before releasing the writer. All later writes complete immediately.
type reviewQueueIO struct {
	done     chan struct{}
	gate     chan struct{}
	started  chan struct{}
	incoming chan reviewDatagram
	writes   chan reviewDatagram
	once     sync.Once
	first    sync.Once
	badPeer  netip.Addr
	echo     bool
}

func newReviewQueueIO() *reviewQueueIO {
	return &reviewQueueIO{done: make(chan struct{}), gate: make(chan struct{}),
		started: make(chan struct{}), incoming: make(chan reviewDatagram, 128), writes: make(chan reviewDatagram, 256)}
}

func (p *reviewQueueIO) ReadPacket(buffer []byte) (int, netip.Addr, error) {
	select {
	case <-p.done:
		return 0, netip.Addr{}, errors.New("test socket closed")
	case packet := <-p.incoming:
		return copy(buffer, packet.data), packet.peer, nil
	}
}

func (p *reviewQueueIO) WritePacket(packet []byte, peer netip.Addr, _ uint8) error {
	var gateErr error
	p.first.Do(func() {
		close(p.started)
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-p.gate:
		case <-p.done:
			gateErr = errors.New("test socket closed")
		case <-timer.C:
			gateErr = errors.New("test write deadline expired")
		}
	})
	if gateErr != nil {
		return gateErr
	}
	if peer == p.badPeer {
		return syscall.ENETUNREACH
	}
	p.writes <- reviewDatagram{append([]byte(nil), packet...), peer}
	if p.echo {
		decoded, err := wire.Parse(packet)
		if err != nil {
			return err
		}
		if data, ok := decoded.(wire.Data); ok {
			response := wire.Data{Common: wire.NewCommon(wire.DataType, data.DestinationPort, data.SourcePort, data.SenderID^1),
				MessageLength: data.MessageLength, Incoming: data.MessageLength, Payload: data.Payload, CutoffVersion: 1}
			encoded, err := wire.Marshal(response)
			if err != nil {
				return err
			}
			p.incoming <- reviewDatagram{encoded, peer}
		}
	}
	return nil
}

func (p *reviewQueueIO) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func awaitReviewWrite(t *testing.T, p *reviewQueueIO) wire.Packet {
	t.Helper()
	select {
	case packet := <-p.writes:
		decoded, err := wire.Parse(packet.data)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for socket write")
		return nil
	}
}

func awaitReviewStart(t *testing.T, p *reviewQueueIO) {
	t.Helper()
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("socket write did not start")
	}
}

func awaitReviewRPCs(t *testing.T, e *Endpoint, count int) {
	t.Helper()
	deadline := time.Now().Add(150 * time.Millisecond)
	for e.Stats().ActiveRPCs != count {
		if time.Now().After(deadline) {
			t.Fatalf("expected %d active RPCs; got %+v", count, e.Stats())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPacketWriteErrorDoesNotFailOtherPeers(t *testing.T) {
	p := newReviewQueueIO()
	p.badPeer = netip.MustParseAddr("127.0.0.2")
	p.echo = true
	local := Addr{netip.MustParseAddr("127.0.0.1"), 4001}
	e, err := New(p, local, Config{RetryInterval: 100 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	badResult := make(chan error, 1)
	go func() {
		_, err := e.Call(ctx, Addr{p.badPeer, 4000}, []byte("bad route"))
		badResult <- err
	}()
	awaitReviewStart(t, p)
	goodPeer := Addr{netip.MustParseAddr("127.0.0.3"), 4000}
	goodResult := make(chan result, 1)
	go func() {
		data, err := e.Call(ctx, goodPeer, []byte("reachable"))
		goodResult <- result{data, err}
	}()
	awaitReviewRPCs(t, e, 2)
	close(p.gate)
	if err := <-badResult; !errors.Is(err, syscall.ENETUNREACH) {
		t.Fatalf("bad peer error = %v; want ENETUNREACH", err)
	}
	got := <-goodResult
	if got.err != nil || !bytes.Equal(got.data, []byte("reachable")) {
		t.Fatalf("unrelated peer failed after other route error: %q, %v", got.data, got.err)
	}
	if stats := e.Stats(); stats.WriteErrors != 1 || stats.CompletedCalls != 1 {
		t.Fatalf("unexpected independent call counters: %+v", stats)
	}
	if _, err := e.Call(ctx, goodPeer, []byte("still alive")); err != nil {
		t.Fatalf("endpoint did not survive route error: %v", err)
	}
}

func TestQueuedDataExpiresButCompletionAckSurvives(t *testing.T) {
	p := newReviewQueueIO()
	local := Addr{netip.MustParseAddr("127.0.0.1"), 4001}
	peer := Addr{netip.MustParseAddr("127.0.0.2"), 4000}
	e, err := New(p, local, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	e.mu.Lock()
	err = e.send(peer, wire.Busy{Common: wire.NewCommon(wire.BusyType, local.Port, peer.Port, 10)}, 0)
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	awaitReviewStart(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	callResult := make(chan error, 1)
	go func() { _, err := e.Call(ctx, peer, []byte("queued")); callResult <- err }()
	awaitReviewRPCs(t, e, 1)
	// This ACK acquires the same origin pointer as queued DATA. It must remain
	// sendable after that RPC disappears, as real completion ACKs do.
	e.mu.Lock()
	for _, r := range e.rpcs {
		err = e.send(peer, wire.Ack{Common: e.common(r, wire.AckType)}, 7)
	}
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-callResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued call error = %v; want deadline exceeded", err)
	}
	// The final low-priority marker proves all earlier higher-priority queue
	// entries were processed before assertions finish.
	e.mu.Lock()
	err = e.send(peer, wire.Busy{Common: wire.NewCommon(wire.BusyType, local.Port, peer.Port, 100)}, 0)
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	close(p.gate)
	acked := false
	for {
		packet := awaitReviewWrite(t, p)
		if _, ok := packet.(wire.Data); ok {
			t.Fatal("DATA reached socket after its queued RPC expired")
		}
		if _, ok := packet.(wire.Ack); ok {
			acked = true
		}
		if packet.Header().SenderID == 100 {
			break
		}
	}
	if !acked {
		t.Fatal("completion ACK was lost when its RPC expired")
	}
	if e.Stats().DroppedPackets == 0 {
		t.Fatal("expired queued DATA was not counted as dropped")
	}
}

func TestTransmitQueueServesHigherPriorityFirst(t *testing.T) {
	p := newReviewQueueIO()
	local := Addr{netip.MustParseAddr("127.0.0.1"), 4001}
	peer := Addr{netip.MustParseAddr("127.0.0.2"), 4000}
	e, err := New(p, local, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	send := func(id uint64, priority uint8) {
		t.Helper()
		e.mu.Lock()
		err := e.send(peer, wire.Busy{Common: wire.NewCommon(wire.BusyType, local.Port, peer.Port, id)}, priority)
		e.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	send(10, 0)
	awaitReviewStart(t, p)
	send(20, 0)
	send(22, 4)
	send(24, 7)
	close(p.gate)
	for _, want := range []uint64{10, 24, 22, 20} {
		if got := awaitReviewWrite(t, p).Header().SenderID; got != want {
			t.Fatalf("next transmitted RPC ID = %d; want %d", got, want)
		}
	}
}

func TestLimitedBroadcastRejectedBeforeDispatch(t *testing.T) {
	p := newReviewPacketIO(0)
	e, err := New(p, Addr{netip.MustParseAddr("127.0.0.1"), 4001}, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	_, err = e.Call(context.Background(), Addr{netip.MustParseAddr("255.255.255.255"), 4000}, []byte{1})
	var callErr *CallError
	if !errors.Is(err, ErrProtocol) || !errors.As(err, &callErr) || callErr.Sent {
		t.Fatalf("broadcast should fail before sending; got %v", err)
	}
	if e.Stats().SentPackets != 0 {
		t.Fatal("limited broadcast reached socket")
	}
}

func newReviewPacketIO(delay time.Duration) *reviewPacketIO {
	return &reviewPacketIO{done: make(chan struct{}), writes: make(chan []byte, 256), delay: delay}
}

func (p *reviewPacketIO) ReadPacket([]byte) (int, netip.Addr, error) {
	<-p.done
	return 0, netip.Addr{}, errors.New("test socket closed")
}

func (p *reviewPacketIO) WritePacket(packet []byte, _ netip.Addr, _ uint8) error {
	if p.delay > 0 {
		timer := time.NewTimer(p.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-p.done:
			return errors.New("test socket closed")
		}
	}
	select {
	case p.writes <- append([]byte(nil), packet...):
	default:
	}
	return nil
}

func (p *reviewPacketIO) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func TestCallDeadlineWhileSocketWriteIsBlocked(t *testing.T) {
	p := newReviewPacketIO(200 * time.Millisecond)
	local := Addr{netip.MustParseAddr("127.0.0.1"), 4001}
	endpoint, err := New(p, local, Config{UnscheduledBytes: 2800}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = endpoint.Call(ctx, Addr{local.IP, 4000}, make([]byte, 2800))
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call error = %v; want deadline exceeded", err)
	}
	if elapsed >= 150*time.Millisecond {
		t.Fatalf("50ms call deadline blocked behind 200ms socket writes: %s", elapsed)
	}
}

func TestPiggybackAckCannotOrphanReceiveMemory(t *testing.T) {
	p := newReviewPacketIO(0)
	local := Addr{netip.MustParseAddr("127.0.0.1"), 4000}
	peer := Addr{local.IP, 4001}
	endpoint, err := New(p, local, Config{Timeout: 20 * time.Millisecond, RetryInterval: 5 * time.Millisecond},
		func(context.Context, Addr, []byte) []byte { return []byte{1} })
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	first := wire.Data{Common: wire.NewCommon(wire.DataType, peer.Port, local.Port, 2),
		MessageLength: 100, Incoming: 10, Payload: []byte{1}}
	endpoint.mu.Lock()
	endpoint.receive(peer.IP, first)
	endpoint.mu.Unlock()
	second := first
	second.SegmentOffset = 1
	// A malformed piggyback ACK refers to the very RPC carrying it. Removing
	// that RPC must not leave its receive assembly outside the tracked map.
	second.Ack = wire.AckRecord{ClientID: 2, ServerPort: local.Port}
	endpoint.mu.Lock()
	endpoint.receive(peer.IP, second)
	endpoint.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		stats := endpoint.Stats()
		if stats.ActiveRPCs == 0 && stats.ActiveHandlers == 0 {
			if stats.BufferedBytes != 0 {
				t.Fatalf("all RPCs expired but %d receive bytes remain accounted", stats.BufferedBytes)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("incomplete RPC did not expire")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestResponseProbesDoNotSuppressRequestLossRecovery(t *testing.T) {
	p := newReviewPacketIO(0)
	local := Addr{netip.MustParseAddr("127.0.0.1"), 4000}
	peer := Addr{local.IP, 4001}
	endpoint, err := New(p, local, Config{Timeout: time.Second, RetryInterval: 20 * time.Millisecond},
		func(context.Context, Addr, []byte) []byte { return []byte{1} })
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	// The receiver has a hole at the start of the request. Client probes ask
	// about its future response, which does not fill that request-side hole.
	data := wire.Data{Common: wire.NewCommon(wire.DataType, peer.Port, local.Port, 2),
		MessageLength: 1000, Incoming: 1000, SegmentOffset: 500, Payload: make([]byte, 100)}
	probe := wire.Resend{Common: wire.NewCommon(wire.ResendType, peer.Port, local.Port, 2),
		Length: wire.ResendAll, Priority: 7}
	endpoint.mu.Lock()
	endpoint.receive(peer.IP, data)
	endpoint.mu.Unlock()
	deadline := time.Now().Add(120 * time.Millisecond)
	for time.Now().Before(deadline) {
		endpoint.mu.Lock()
		endpoint.receive(peer.IP, probe)
		endpoint.mu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	for {
		select {
		case packet := <-p.writes:
			decoded, err := wire.Parse(packet)
			if err != nil {
				t.Fatal(err)
			}
			if resend, ok := decoded.(wire.Resend); ok && resend.Offset == 0 && resend.Length == 500 {
				return
			}
		default:
			t.Fatal("response probes prevented retransmission of the missing request bytes")
		}
	}
}

func TestResendOutOfRangeOffsetsCannotWrapOrTransmitData(t *testing.T) {
	for _, offset := range []uint32{3, 0x7fffffff, 0x80000000, 0xffffffff} {
		t.Run(fmt.Sprintf("offset_%08x", offset), func(t *testing.T) {
			p := newReviewPacketIO(0)
			local := Addr{netip.MustParseAddr("127.0.0.1"), 4001}
			peer := Addr{netip.MustParseAddr("127.0.0.2"), 4000}
			e, err := New(p, local, Config{Timeout: time.Second, RetryInterval: time.Second}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			key := rpcKey{peer: peer, id: 2}
			r := &rpc{key: key, tx: []byte("abc"), sent: 3, granted: 3, deadline: time.Now().Add(time.Second)}
			e.mu.Lock()
			e.rpcs[key] = r
			e.stats.BufferedBytes = len(r.tx)
			// This valid native control packet supplies an out-of-message offset.
			// On 32-bit targets, the high-bit values must never become indices.
			e.receive(peer.IP, wire.Resend{Common: wire.NewCommon(wire.ResendType, peer.Port, local.Port, 3),
				Offset: offset, Length: wire.ResendAll, Priority: 7})
			e.mu.Unlock()
			select {
			case b := <-p.writes:
				packet, err := wire.Parse(b)
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := packet.(wire.Busy); !ok {
					t.Fatalf("out-of-range request emitted %T, want BUSY", packet)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("out-of-range RESEND did not receive BUSY")
			}
			if got := e.Stats().Retransmissions; got != 0 {
				t.Fatalf("out-of-range request retransmitted %d data packets", got)
			}
		})
	}
}

func TestExpiredRequestsNeverAcquireHandlerWorkers(t *testing.T) {
	for _, path := range []string{"last_fragment", "queued_worker"} {
		t.Run(path, func(t *testing.T) {
			p := newReviewPacketIO(0)
			local := Addr{netip.MustParseAddr("127.0.0.1"), 4000}
			peer := Addr{netip.MustParseAddr("127.0.0.2"), 4001}
			ran := make(chan struct{}, 1)
			e, err := New(p, local, Config{Timeout: time.Second, RetryInterval: time.Second, HandlerWorkers: 1},
				func(context.Context, Addr, []byte) []byte { ran <- struct{}{}; return []byte{1} })
			if err != nil {
				t.Fatal(err)
			}
			defer e.Close()
			request := wire.Data{Common: wire.NewCommon(wire.DataType, peer.Port, local.Port, 2),
				MessageLength: 2, Incoming: 2, Payload: []byte{1}, CutoffVersion: 1}
			e.mu.Lock()
			if path == "queued_worker" {
				// Occupy the sole worker while a complete request is admitted.
				e.workers <- struct{}{}
				request.Payload = []byte{1, 2}
			}
			e.receive(peer.IP, request)
			r := e.rpcs[rpcKey{peer: peer, id: 3}]
			if r == nil || r.handling {
				e.mu.Unlock()
				t.Fatal("request was not retained awaiting completion/worker")
			}
			// Advance the RPC past its lifetime while holding the protocol lock,
			// so timer-loop timing cannot make this regression nondeterministic.
			r.deadline = time.Now().Add(-time.Second)
			if path == "last_fragment" {
				request.SegmentOffset = 1
				request.Payload = []byte{2}
				e.receive(peer.IP, request)
			} else {
				<-e.workers
				e.startHandler(r)
			}
			e.mu.Unlock()
			stats := e.Stats()
			if stats.HandledRequests != 0 || stats.ActiveHandlers != 0 {
				t.Fatalf("expired request acquired an application worker: %+v", stats)
			}
			if stats.ActiveRPCs != 0 || stats.BufferedBytes != 0 {
				t.Fatalf("expired request retained protocol state: %+v", stats)
			}
			select {
			case <-ran:
				t.Fatal("expired request invoked application handler")
			default:
			}
		})
	}
}
