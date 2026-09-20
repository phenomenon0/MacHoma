// Package homa implements the native Homa IPv4 wire protocol in userspace.
// Its wire target is PlatformLab/HomaModule d8914b8a57aa48c19a2c8130484961585bcbe53c.
// This experimental port is not the Linux kernel implementation and makes no
// equivalent performance claim. Native Homa supplies no encryption or identity
// authentication; deploy only on an appropriate trusted network.
package homa

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

const Protocol = 146
const MaxMessageSize = 1000000

var (
	ErrClosed   = errors.New("homa: closed")
	ErrBusy     = errors.New("homa: capacity exhausted")
	ErrTooLarge = errors.New("homa: message length must be 1..configured maximum")
	ErrProtocol = errors.New("homa: invalid protocol message")
)

type Addr struct {
	IP   netip.Addr
	Port uint16
}

func (a Addr) String() string { return netip.AddrPortFrom(a.IP, a.Port).String() }
func ParseAddr(s string) (Addr, error) {
	a, e := netip.ParseAddrPort(s)
	if e != nil {
		return Addr{}, e
	}
	if !a.Addr().Is4() || a.Port() == 0 {
		return Addr{}, fmt.Errorf("homa: require IPv4 address and nonzero port")
	}
	return Addr{a.Addr(), a.Port()}, nil
}

// PacketIO exchanges Homa transport headers plus payload (without IPv4 headers).
// WritePacket must have a bounded write deadline; Close must unblock ReadPacket.
// One endpoint owns one PacketIO. It must not deliver other IP protocols.
type PacketIO interface {
	ReadPacket([]byte) (int, netip.Addr, error)
	WritePacket([]byte, netip.Addr, uint8) error
	Close() error
}

// Handler receives unmodified application bytes. Return 1..MaxMessageSize bytes.
// Encode application errors in the application payload: Homa has no error frame.
// Respect ctx cancellation; Close does not wait for an uncooperative handler.
// Peer IP/port is NOT an authenticated identity. Do not retain request afterward.
type Handler func(context.Context, Addr, []byte) []byte

type Config struct {
	MaxMessageSize   int           // default 1,000,000 (upstream wire maximum)
	MaxRPCs          int           // default 128, total live client and server RPCs
	MaxBufferedBytes int           // default 64 MiB, protocol payloads and receive bitmaps
	HandlerWorkers   int           // default 8; canceled handlers still consume a slot
	Timeout          time.Duration // default 5 seconds, absolute local RPC lifetime
	RetryInterval    time.Duration // default 5 milliseconds
	SegmentBytes     int           // default 1400; 256..1424 for standard IPv4 Ethernet MTU1500
	UnscheduledBytes int           // default 14000, rounded up to a segment boundary
	GrantWindow      int           // default 14000
	Overcommit       int           // default 2 receiver-granted messages at once
}

// CallError.Sent means execution may have happened. Timeouts are not permission
// to repeat side effects. Homa RPC_UNKNOWN recovery can also repeat execution
// after peer state loss; use application idempotency for side effects.
type CallError struct {
	Err  error
	Sent bool
}

func (e *CallError) Error() string {
	return fmt.Sprintf("homa: call failed (sent=%t): %v", e.Sent, e.Err)
}
func (e *CallError) Unwrap() error { return e.Err }

type Stats struct {
	SentPackets, ReceivedPackets, Retransmissions, InvalidPackets uint64
	DroppedPackets                                                uint64 // transmit queue overflows or stale queued DATA
	WriteErrors                                                   uint64 // packet write failures; scoped to the originating RPC
	CompletedCalls, HandledRequests                               uint64
	ActiveRPCs, ActiveHandlers, BufferedBytes                     int
}
