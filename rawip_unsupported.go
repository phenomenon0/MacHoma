//go:build !darwin && !linux

package homa

import (
	"errors"
	"net/netip"
)

// OpenRaw is implemented only for Linux and macOS IPv4 raw sockets.
func OpenRaw(ip netip.Addr) (PacketIO, error) {
	return nil, errors.New("homa: native raw IPv4 transport is supported only on Linux and macOS")
}
