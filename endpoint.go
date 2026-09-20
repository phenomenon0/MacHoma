package homa

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"sync"
	"time"

	"github.com/phenomenon0/MacHoma/internal/wire"
)

type rpcKey struct {
	peer Addr
	id   uint64
}
type result struct {
	data []byte
	err  error
}
type assembly struct {
	data, bits                 []byte
	count, contiguous, granted int
}
type rpc struct {
	key            rpcKey
	tx             []byte
	sent, granted  int
	priority       uint8
	rx             *assembly
	received       bool
	handling       bool
	deadline, last time.Time
	lastCutoff     time.Time
	result         chan result
	cancel         context.CancelFunc
}

var defaultCutoffs = [8]uint32{MaxMessageSize, 0, 0, 0, MaxMessageSize, 15000, 2800, 200}

type cutoff struct {
	values  [8]uint32
	version uint16
}

type outgoingPacket struct {
	data     []byte
	peer     netip.Addr
	priority uint8
	origin   *rpc
	typ      wire.Type
}

type Endpoint struct {
	mu       sync.Mutex
	io       PacketIO
	local    Addr
	cfg      Config
	handler  Handler
	rpcs     map[rpcKey]*rpc
	cutoffs  map[netip.Addr]cutoff
	nextID   uint64
	stats    Stats
	closed   bool
	done     chan struct{}
	workers  chan struct{}
	outbound [8]chan outgoingPacket
	wake     chan struct{}
	wg       sync.WaitGroup
}

func defaults(c Config) (Config, error) {
	if c.MaxMessageSize == 0 {
		c.MaxMessageSize = MaxMessageSize
	}
	if c.MaxRPCs == 0 {
		c.MaxRPCs = 128
	}
	if c.MaxBufferedBytes == 0 {
		c.MaxBufferedBytes = 64 << 20
	}
	if c.HandlerWorkers == 0 {
		c.HandlerWorkers = 8
	}
	if c.Timeout == 0 {
		c.Timeout = 5 * time.Second
	}
	if c.RetryInterval == 0 {
		c.RetryInterval = 5 * time.Millisecond
	}
	if c.SegmentBytes == 0 {
		c.SegmentBytes = 1400
	}
	if c.UnscheduledBytes == 0 {
		c.UnscheduledBytes = 14000
	}
	if c.GrantWindow == 0 {
		c.GrantWindow = 14000
	}
	if c.Overcommit == 0 {
		c.Overcommit = 2
	}
	if c.MaxMessageSize < 1 || c.MaxMessageSize > MaxMessageSize || c.MaxRPCs < 1 || c.MaxRPCs > 65536 || c.MaxBufferedBytes < 1 || c.HandlerWorkers < 1 || c.HandlerWorkers > 1024 || c.Timeout < time.Millisecond || c.Timeout > time.Minute || c.RetryInterval < time.Millisecond || c.RetryInterval > c.Timeout || c.SegmentBytes < 256 || c.SegmentBytes > 1424 || c.UnscheduledBytes < 1 || c.UnscheduledBytes > MaxMessageSize || c.GrantWindow < 1 || c.GrantWindow > MaxMessageSize || c.Overcommit < 1 || c.Overcommit > c.MaxRPCs {
		return c, fmt.Errorf("homa: invalid configuration")
	}
	c.UnscheduledBytes = ((c.UnscheduledBytes + c.SegmentBytes - 1) / c.SegmentBytes) * c.SegmentBytes
	return c, nil
}

func Listen(local Addr, cfg Config, handler Handler) (*Endpoint, error) {
	p, err := OpenRaw(local.IP)
	if err != nil {
		return nil, err
	}
	e, err := New(p, local, cfg, handler)
	if err != nil {
		_ = p.Close()
	}
	return e, err
}

// New takes ownership of packetIO on success. Raw sockets do not reserve ports;
// run only one endpoint per local IP/port, including across processes.
func New(packetIO PacketIO, local Addr, cfg Config, handler Handler) (*Endpoint, error) {
	c, err := defaults(cfg)
	if err != nil {
		return nil, err
	}
	if packetIO == nil || !local.IP.Is4() || local.Port == 0 {
		return nil, fmt.Errorf("homa: require packet I/O and IPv4/nonzero local port")
	}
	if handler != nil && local.Port >= 32768 {
		return nil, fmt.Errorf("homa: server port must be 1..32767")
	}
	var seed [8]byte
	if _, err = rand.Read(seed[:]); err != nil {
		return nil, err
	}
	e := &Endpoint{io: packetIO, local: local, cfg: c, handler: handler, rpcs: make(map[rpcKey]*rpc), cutoffs: make(map[netip.Addr]cutoff), nextID: binary.BigEndian.Uint64(seed[:]) &^ 1, done: make(chan struct{}), workers: make(chan struct{}, c.HandlerWorkers), wake: make(chan struct{}, 1)}
	for priority := range e.outbound {
		e.outbound[priority] = make(chan outgoingPacket, 32)
	}
	e.wg.Add(3)
	go e.readLoop()
	go e.timerLoop()
	go e.writeLoop()
	return e, nil
}
func (e *Endpoint) LocalAddr() Addr { return e.local }
func (e *Endpoint) Stats() Stats {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.stats
	s.ActiveRPCs = len(e.rpcs)
	s.ActiveHandlers = len(e.workers)
	return s
}

func (e *Endpoint) Call(ctx context.Context, peer Addr, request []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, &CallError{Err: err}
	}
	if !peer.IP.Is4() || peer.IP.IsUnspecified() || peer.IP.IsMulticast() || peer.IP == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || peer.Port == 0 {
		return nil, &CallError{Err: ErrProtocol}
	}
	if len(request) == 0 || len(request) > e.cfg.MaxMessageSize {
		return nil, &CallError{Err: ErrTooLarge}
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, &CallError{Err: ErrClosed}
	}
	if len(e.rpcs) >= e.cfg.MaxRPCs || !e.reserve(len(request)) {
		e.mu.Unlock()
		return nil, &CallError{Err: ErrBusy}
	}
	e.nextID += 2
	if e.nextID == 0 {
		e.nextID = 2
	}
	now := time.Now()
	deadline := now.Add(e.cfg.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	r := &rpc{key: rpcKey{peer, e.nextID}, tx: append([]byte(nil), request...), granted: min(len(request), e.cfg.UnscheduledBytes), deadline: deadline, last: now, result: make(chan result, 1)}
	r.priority = e.unsched(peer.IP, len(request))
	e.rpcs[r.key] = r
	err := e.sendNew(r)
	if err != nil {
		e.finish(r, nil, err)
	}
	e.mu.Unlock()
	select {
	case got := <-r.result:
		return got.data, got.err
	case <-ctx.Done():
		e.mu.Lock()
		if e.rpcs[r.key] == r {
			e.finish(r, nil, ctx.Err())
		}
		e.mu.Unlock()
		got := <-r.result
		return got.data, got.err
	}
}
func (e *Endpoint) reserve(n int) bool {
	if n < 0 || n > e.cfg.MaxBufferedBytes-e.stats.BufferedBytes {
		return false
	}
	e.stats.BufferedBytes += n
	return true
}
func (e *Endpoint) common(r *rpc, t wire.Type) wire.Common {
	return wire.NewCommon(t, e.local.Port, r.key.peer.Port, r.key.id)
}
func (e *Endpoint) send(peer Addr, p wire.Packet, priority uint8) error {
	if priority >= uint8(len(e.outbound)) {
		return ErrProtocol
	}
	b, err := wire.Marshal(p)
	if err != nil {
		return err
	}
	// Eight bounded priority queues keep socket waits out of the protocol lock.
	// Each reserves 32 slots, so lower priorities cannot fill higher queues.
	// Overflow is ordinary packet loss, recovered by native RESEND/UNKNOWN.
	h := p.Header()
	origin := e.rpcs[rpcKey{peer: peer, id: h.SenderID}]
	select {
	case e.outbound[priority] <- outgoingPacket{data: b, peer: peer.IP, priority: priority, origin: origin, typ: h.Type}:
		select {
		case e.wake <- struct{}{}:
		default:
		}
	default:
		e.stats.DroppedPackets++
	}
	return nil
}
func (e *Endpoint) control(r *rpc, t wire.Type) error {
	return e.send(r.key.peer, wire.CommonPacket{Common: e.common(r, t)}, 7)
}
func (e *Endpoint) unsched(ip netip.Addr, n int) uint8 {
	c, ok := e.cutoffs[ip]
	if !ok {
		c.values = defaultCutoffs
	}
	for i := 7; i >= 0; i-- {
		if uint32(n) <= c.values[i] {
			return uint8(i)
		}
	}
	return 0
}
func (e *Endpoint) sendSegment(r *rpc, start int, retransmit bool, priority uint8) error {
	end := min(start+e.cfg.SegmentBytes, len(r.tx))
	incoming := min(len(r.tx), ((r.granted+e.cfg.SegmentBytes-1)/e.cfg.SegmentBytes)*e.cfg.SegmentBytes)
	p := wire.Data{Common: e.common(r, wire.DataType), MessageLength: uint32(len(r.tx)), Incoming: uint32(incoming), CutoffVersion: e.cutoffs[r.key.peer.IP].version, SegmentOffset: uint32(start), Payload: r.tx[start:end]}
	if retransmit {
		p.Retransmit = 1
		e.stats.Retransmissions++
	}
	return e.send(r.key.peer, p, priority)
}
func (e *Endpoint) sendNew(r *rpc) error {
	for r.sent < min(r.granted, len(r.tx)) {
		start := r.sent
		// Conservatively mark attempted bytes even on ambiguous socket failure.
		r.sent = min(start+e.cfg.SegmentBytes, len(r.tx))
		if err := e.sendSegment(r, start, false, r.priority); err != nil {
			return err
		}
	}
	return nil
}
func (e *Endpoint) resend(r *rpc, start, end int, priority uint8) {
	end = min(end, r.sent)
	// Reuse original segment boundaries: Linux rejects partial overlaps.
	for at := (start / e.cfg.SegmentBytes) * e.cfg.SegmentBytes; at < end; at += e.cfg.SegmentBytes {
		if at < len(r.tx) {
			_ = e.sendSegment(r, at, true, priority)
		}
	}
}
func (e *Endpoint) remove(r *rpc) {
	if e.rpcs[r.key] != r {
		return
	}
	delete(e.rpcs, r.key)
	e.stats.BufferedBytes -= len(r.tx)
	r.tx = nil
	if r.rx != nil {
		e.stats.BufferedBytes -= len(r.rx.data) + len(r.rx.bits)
		r.rx = nil
	}
	if r.cancel != nil {
		r.cancel()
	}
}
func (e *Endpoint) finish(r *rpc, data []byte, err error) {
	if e.rpcs[r.key] != r {
		return
	}
	sent := r.sent > 0 || r.received
	e.remove(r)
	if r.result != nil {
		if err != nil {
			err = &CallError{Err: err, Sent: sent}
		}
		r.result <- result{data, err}
	}
}
func (e *Endpoint) shutdown(err error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	close(e.done)
	for _, r := range e.rpcs {
		e.finish(r, nil, err)
	}
	e.mu.Unlock()
	_ = e.io.Close()
}
func (e *Endpoint) Close() error { e.shutdown(ErrClosed); e.wg.Wait(); return nil }

// dequeue selects the highest currently queued priority. Caller holds e.mu.
func (e *Endpoint) dequeue() (outgoingPacket, bool) {
	for priority := len(e.outbound) - 1; priority >= 0; priority-- {
		select {
		case packet := <-e.outbound[priority]:
			return packet, true
		default:
		}
	}
	return outgoingPacket{}, false
}

func (e *Endpoint) writeLoop() {
	defer e.wg.Done()
	for {
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return
		}
		p, ok := e.dequeue()
		if !ok {
			e.mu.Unlock()
			select {
			case <-e.done:
				return
			case <-e.wake:
			}
			continue
		}
		// A completion ACK intentionally survives removal of its local RPC.
		// DATA does not: cancellation or expiry must prevent queued work from
		// first reaching a peer later. An already-started socket write remains
		// an unknown outcome and is covered by CallError.Sent.
		if p.typ == wire.DataType && (p.origin == nil || e.rpcs[p.origin.key] != p.origin || !time.Now().Before(p.origin.deadline)) {
			e.stats.DroppedPackets++
			e.mu.Unlock()
			continue
		}
		e.mu.Unlock()
		err := e.io.WritePacket(p.data, p.peer, p.priority)
		e.mu.Lock()
		if err != nil {
			e.stats.WriteErrors++
			// Route/MTU errors concern this packet's RPC, not all other peers.
			// Pointer identity prevents an old queued packet from failing a new
			// RPC if its identifier is ever reused.
			if p.origin != nil && e.rpcs[p.origin.key] == p.origin {
				e.finish(p.origin, nil, err)
			}
		} else {
			e.stats.SentPackets++
		}
		e.mu.Unlock()
	}
}
func (e *Endpoint) readLoop() {
	defer e.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, ip, err := e.io.ReadPacket(buf)
		if err != nil {
			e.shutdown(err)
			return
		}
		p, err := wire.Parse(buf[:n])
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			return
		}
		if err != nil {
			e.stats.InvalidPackets++
		} else {
			e.stats.ReceivedPackets++
			e.receive(ip, p)
		}
		e.mu.Unlock()
	}
}
func (e *Endpoint) timerLoop() {
	defer e.wg.Done()
	ticker := time.NewTicker(e.cfg.RetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.done:
			return
		case now := <-ticker.C:
			e.mu.Lock()
			if e.closed {
				e.mu.Unlock()
				return
			}
			for _, r := range e.rpcs {
				if !now.Before(r.deadline) {
					e.finish(r, nil, context.DeadlineExceeded)
					continue
				}
				if r.key.id&1 == 1 && r.received && !r.handling {
					e.startHandler(r)
				}
				if now.Sub(r.last) < e.cfg.RetryInterval {
					continue
				}
				r.last = now
				if r.key.id&1 == 1 && len(r.tx) > 0 && r.sent == len(r.tx) {
					_ = e.control(r, wire.NeedAckType)
				} else if r.rx != nil {
					e.requestMissing(r)
				} else if r.key.id&1 == 0 && !r.received {
					e.requestMissing(r)
				}
			}
			e.schedule()
			e.mu.Unlock()
		}
	}
}
func (e *Endpoint) requestMissing(r *rpc) {
	offset := 0
	length := uint32(wire.ResendAll)
	if r.rx != nil {
		a := r.rx
		offset = a.contiguous
		end := min(a.granted, len(a.data))
		for at := offset; at < end; at++ {
			if a.bits[at/8]&(1<<uint(at%8)) != 0 {
				end = at
				break
			}
		}
		if offset >= end {
			return
		}
		length = uint32(end - offset)
	}
	_ = e.send(r.key.peer, wire.Resend{Common: e.common(r, wire.ResendType), Offset: uint32(offset), Length: length, Priority: 7}, 7)
}
func (e *Endpoint) schedule() {
	var candidates []*rpc
	for _, r := range e.rpcs {
		if r.rx != nil && r.rx.count < len(r.rx.data) && r.rx.granted < len(r.rx.data) {
			candidates = append(candidates, r)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		left, right := len(a.rx.data)-a.rx.count, len(b.rx.data)-b.rx.count
		if left == right {
			return a.key.id < b.key.id
		}
		return left < right
	})
	for rank, r := range candidates {
		if rank >= e.cfg.Overcommit {
			break
		}
		a := r.rx
		end := min(len(a.data), a.count+e.cfg.GrantWindow)
		if end <= a.granted {
			continue
		}
		a.granted = end
		_ = e.send(r.key.peer, wire.Grant{Common: e.common(r, wire.GrantType), Offset: uint32(end), Priority: uint8(max(0, 3-rank))}, 7)
	}
}
func (e *Endpoint) receive(ip netip.Addr, p wire.Packet) {
	h := p.Header()
	if !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || h.DestinationPort != e.local.Port || h.SourcePort == 0 || h.SenderID == 0 {
		return
	}
	peer := Addr{ip, h.SourcePort}
	key := rpcKey{peer, h.LocalID()}
	r := e.rpcs[key]
	if r != nil && (h.Type == wire.DataType || h.Type == wire.GrantType || h.Type == wire.BusyType) {
		r.last = time.Now()
	}
	switch v := p.(type) {
	case wire.Data:
		e.data(peer, key, r, v)
	case wire.Grant:
		if r == nil || v.Priority > 7 || len(r.tx) == 0 {
			return
		}
		if int(v.Offset) > r.granted {
			r.granted = min(int(v.Offset), len(r.tx))
			r.priority = v.Priority
			if err := e.sendNew(r); err != nil {
				e.finish(r, nil, err)
			}
		}
	case wire.Resend:
		if v.Priority > 7 {
			return
		}
		if r == nil {
			tmp := &rpc{key: key}
			_ = e.control(tmp, wire.RPCUnknownType)
			return
		}
		if len(r.tx) == 0 {
			_ = e.control(r, wire.BusyType)
			return
		}
		// Validate in the wire's unsigned domain before converting an offset
		// to int: high-bit uint32 values become negative indices on 32-bit Go.
		// There is also nothing to retransmit at the exclusive message end.
		if uint64(v.Offset) >= uint64(len(r.tx)) {
			_ = e.control(r, wire.BusyType)
			return
		}
		end := len(r.tx)
		if v.Length != wire.ResendAll {
			end = int(min(uint64(len(r.tx)), uint64(v.Offset)+uint64(v.Length)))
		} else {
			end = r.sent
		}
		e.resend(r, int(v.Offset), end, v.Priority)
		if end > r.granted {
			r.granted = end
			r.priority = v.Priority
			_ = e.sendNew(r)
		}
		if int(v.Offset) >= r.sent {
			_ = e.control(r, wire.BusyType)
		}
	case wire.RPCUnknown:
		if r == nil {
			return
		}
		if r.key.id&1 == 1 {
			e.remove(r)
		} else if r.rx == nil && !r.received {
			e.resend(r, 0, r.sent, e.unsched(ip, len(r.tx)))
		}
	case wire.Busy:
	case wire.NeedAck:
		if key.id&1 != 0 {
			return
		}
		if r == nil || r.received {
			tmp := &rpc{key: key}
			_ = e.send(peer, wire.Ack{Common: e.common(tmp, wire.AckType)}, 7)
		} else {
			e.requestMissing(r)
		}
	case wire.Ack:
		if key.id&1 != 1 {
			return
		}
		if r != nil {
			e.remove(r)
		}
		for _, a := range v.Acks[:v.NumAcks] {
			e.ack(ip, a)
		}
	case wire.Cutoffs:
		if _, exists := e.cutoffs[ip]; exists || len(e.cutoffs) < e.cfg.MaxRPCs {
			e.cutoffs[ip] = cutoff{v.UnscheduledCutoffs, v.CutoffVersion}
		}
	case wire.Freeze: // Linux debugging hook; this port has no timetrace to freeze.
	}
}
func (e *Endpoint) ack(ip netip.Addr, a wire.AckRecord) {
	if a.ClientID == 0 || a.ClientID&1 != 0 || a.ServerPort != e.local.Port {
		return
	}
	for k, r := range e.rpcs {
		if k.peer.IP == ip && k.id == a.ClientID^1 {
			e.remove(r)
			return
		}
	}
}
func (e *Endpoint) data(peer Addr, key rpcKey, r *rpc, p wire.Data) {
	if p.ValidateBounds(uint32(e.cfg.MaxMessageSize)) != nil || len(p.Payload) == 0 || p.Incoming > p.MessageLength {
		e.stats.InvalidPackets++
		return
	}
	e.ack(peer.IP, p.Ack)
	r = e.rpcs[key]
	if r == nil {
		if key.id&1 == 0 || e.handler == nil || len(e.rpcs) >= e.cfg.MaxRPCs {
			return
		}
		r = &rpc{key: key, deadline: time.Now().Add(e.cfg.Timeout), last: time.Now()}
		e.rpcs[key] = r
	}
	if p.CutoffVersion != 1 && time.Since(r.lastCutoff) >= e.cfg.RetryInterval {
		r.lastCutoff = time.Now()
		_ = e.send(peer, wire.Cutoffs{Common: e.common(r, wire.CutoffsType), UnscheduledCutoffs: defaultCutoffs, CutoffVersion: 1}, 7)
	}
	if r.received {
		return
	}
	if r.rx == nil {
		n := int(p.MessageLength)
		cost := n + (n+7)/8
		if !e.reserve(cost) {
			if key.id&1 == 1 {
				e.remove(r)
			}
			return
		}
		r.rx = &assembly{data: make([]byte, n), bits: make([]byte, (n+7)/8), granted: int(p.Incoming)}
		if key.id&1 == 0 {
			e.stats.BufferedBytes -= len(r.tx)
			r.tx = nil
		}
	}
	a := r.rx
	if len(a.data) != int(p.MessageLength) {
		e.stats.InvalidPackets++
		return
	}
	start := int(p.EffectiveOffset())
	end := start + len(p.Payload)
	// Conflicting overlap is invalid. Never silently overwrite accepted bytes.
	for at := start; at < end; at++ {
		if a.bits[at/8]&(1<<uint(at%8)) != 0 && a.data[at] != p.Payload[at-start] {
			e.stats.InvalidPackets++
			return
		}
	}
	for at := start; at < end; at++ {
		mask := byte(1 << uint(at%8))
		if a.bits[at/8]&mask == 0 {
			a.bits[at/8] |= mask
			a.data[at] = p.Payload[at-start]
			a.count++
		}
	}
	a.granted = max(a.granted, int(p.Incoming))
	for a.contiguous < len(a.data) && a.bits[a.contiguous/8]&(1<<uint(a.contiguous%8)) != 0 {
		a.contiguous++
	}
	if a.count != len(a.data) {
		e.schedule()
		return
	}
	r.received = true
	if key.id&1 == 0 {
		_ = e.send(peer, wire.Ack{Common: e.common(r, wire.AckType)}, 7)
		data := a.data
		a.data = nil
		e.stats.BufferedBytes -= len(data)
		e.stats.CompletedCalls++
		e.finish(r, data, nil)
		return
	}
	e.startHandler(r)
	e.schedule()
}
func (e *Endpoint) startHandler(r *rpc) {
	if r.handling || r.rx == nil {
		return
	}
	// A last fragment or newly available worker can arrive before the timer
	// removes expired state. Do not begin application work for that state.
	if !time.Now().Before(r.deadline) {
		e.finish(r, nil, context.DeadlineExceeded)
		return
	}
	select {
	case e.workers <- struct{}{}:
	default:
		return
	}
	r.handling = true
	payload := r.rx.data
	e.stats.BufferedBytes -= len(r.rx.bits)
	r.rx = nil // payload stays accounted until handler returns.
	ctx, cancel := context.WithDeadline(context.Background(), r.deadline)
	r.cancel = cancel
	e.stats.HandledRequests++
	go func() {
		// A panicking application handler cannot bring down the transport process.
		var response []byte
		func() { defer func() { _ = recover() }(); response = e.handler(ctx, r.key.peer, payload) }()
		e.mu.Lock()
		defer e.mu.Unlock()
		defer cancel()
		<-e.workers
		e.stats.BufferedBytes -= len(payload)
		if e.closed || e.rpcs[r.key] != r {
			return
		}
		if len(response) == 0 || len(response) > e.cfg.MaxMessageSize || !e.reserve(len(response)) {
			// Keep failed handler state until timeout; a retry must not rerun it.
			return
		}
		r.tx = append([]byte(nil), response...)
		r.granted = min(len(response), e.cfg.UnscheduledBytes)
		r.priority = e.unsched(r.key.peer.IP, len(response))
		r.last = time.Now()
		if err := e.sendNew(r); err != nil {
			e.remove(r)
		}
		// Ready requests wait for a bounded worker slot without spawning goroutines.
		for _, waiting := range e.rpcs {
			if waiting.key.id&1 == 1 && waiting.received && !waiting.handling {
				e.startHandler(waiting)
			}
		}
	}()
}
