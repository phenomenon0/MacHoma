//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
)

func TestTransientSendDrop(t *testing.T) {
	for _, cause := range []error{syscall.ENOBUFS, syscall.EAGAIN, syscall.EWOULDBLOCK} {
		wrapped := &net.OpError{Op: "write", Net: "ip4", Err: &os.SyscallError{Syscall: "sendto", Err: cause}}
		if !transientSendDrop(cause) || !transientSendDrop(wrapped) {
			t.Errorf("transient send failure must drop only its datagram: %v", cause)
		}
	}
	for _, err := range []error{nil, syscall.EACCES, syscall.EMSGSIZE, syscall.ENETUNREACH, os.ErrDeadlineExceeded, errors.New("sendto: no buffer space available")} {
		if transientSendDrop(err) {
			t.Errorf("unrelated failure classified as recoverable packet loss: %v", err)
		}
	}
}

func testHoma(source, destination uint16, typ byte) []byte {
	p := make([]byte, 80)
	binary.BigEndian.PutUint16(p, source)
	binary.BigEndian.PutUint16(p[2:], destination)
	p[11] = typ
	for i := 28; i < len(p); i++ {
		p[i] = byte(i)
	}
	return p
}

func TestRelayPreservesHomaBytesAndObservedTOS(t *testing.T) {
	p := testHoma(4003, 4000, 0x10)
	for _, tos := range []byte{0, 32, 96, 224, 227} {
		frame := ethernetPacket(p, tos)
		got, traffic, ok := nativePacket(frame[14:], virtualHost, virtualGuest)
		if !ok || traffic != tos || !bytes.Equal(got, p) {
			t.Fatalf("ToS%d: ok=%v traffic=%d bytes unchanged=%v", tos, ok, traffic, bytes.Equal(got, p))
		}
		if !bytes.Equal(frame[:6], guestMAC) || !bytes.Equal(frame[6:12], hostMAC) {
			t.Fatal("incorrect Ethernet routing")
		}
	}
}

func TestRelayRejectsCrossPeerMalformedAndFragmentedIP(t *testing.T) {
	good := ethernetPacket(testHoma(4003, 4000, 0x10), 224)[14:]
	for _, offset := range []int{0, 2, 6, 9, 10, 12, 16} {
		bad := bytes.Clone(good)
		bad[offset] ^= 1
		if _, _, ok := nativePacket(bad, virtualHost, virtualGuest); ok {
			t.Errorf("accepted corruption at offset%d", offset)
		}
	}
	for size := 0; size < 48; size++ {
		if _, _, ok := nativePacket(good[:size], virtualHost, virtualGuest); ok {
			t.Errorf("accepted truncated length%d", size)
		}
	}
	if _, _, ok := nativePacket(good, netip.MustParseAddr("10.0.0.99"), virtualGuest); ok {
		t.Fatal("accepted different IP peer")
	}
	fragment := bytes.Clone(good)
	fragment[6] = 0x20
	fragment[10] = 0
	fragment[11] = 0
	binary.BigEndian.PutUint16(fragment[10:], ipChecksum(fragment[:20]))
	if _, _, ok := nativePacket(fragment, virtualHost, virtualGuest); ok {
		t.Fatal("accepted valid-checksum fragment")
	}
}

func TestRelayPortAllowlistAndAcknowledgmentEvidence(t *testing.T) {
	for _, ports := range [][2]uint16{{4003, 4000}, {4001, 4002}} {
		if !allowedHoma(testHoma(ports[0], ports[1], 0x18), true) {
			t.Fatalf("allowedLAN route refused %v", ports)
		}
		if !allowedHoma(testHoma(ports[1], ports[0], 0x18), false) {
			t.Fatalf("allowedVM route refused %v", ports)
		}
	}
	for _, ports := range [][2]uint16{{4001, 4000}, {4003, 4002}, {4000, 4003}, {9999, 4000}} {
		if allowedHoma(testHoma(ports[0], ports[1], 0x18), true) {
			t.Fatalf("cross-route allowed %v", ports)
		}
	}
	s := &state{acks: map[uint64]bool{}}
	p := testHoma(4003, 4000, 0x18)
	binary.BigEndian.PutUint64(p[20:], 24)
	binary.BigEndian.PutUint16(p[28:], 0)
	s.noteAcks(p)
	s.noteAcks(p)
	if len(s.acks) != 1 || !s.acks[24] {
		t.Fatal("ACK duplicates inflated completed RPC evidence")
	}
	binary.BigEndian.PutUint64(p[20:], 25)
	s.noteAcks(p)
	if len(s.acks) != 1 {
		t.Fatal("odd serverID counted as Mac clientACK")
	}
	if allowedHoma(p[:79], true) {
		t.Fatal("truncatedACK accepted")
	}
}
