//go:build darwin || linux

package homa

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// rawIP moves native Homa packets, not UDP datagrams. The kernel supplies the
// IPv4 header and its checksum; bytes passed to WritePacket start at the Homa
// common header. A raw socket does not reserve Homa ports: the endpoint must
// demultiplex the destination port in the Homa header itself.
type rawIP struct {
	conn    *net.IPConn
	readMu  sync.Mutex
	writeMu sync.Mutex
	// ReadFromIP first receives the whole IPv4 packet before stripping its
	// header. Reading directly into the caller's payload buffer could truncate
	// a valid payload by up to 60 bytes. The full-size buffer avoids that.
	readBuf [65535]byte
}

// OpenRaw opens an IPv4 socket for IP protocol 146 on the given local address.
// Pass 0.0.0.0 to listen on all local IPv4 interfaces. Opening requires root on
// macOS, or root/CAP_NET_RAW on Linux. No kernel extension or UDP encapsulation
// is used. IPv6 is not implemented.
//
// Do not run a userspace endpoint and the Linux Homa module for the same local
// Homa port: both can receive the same native IP packets.
func OpenRaw(ip netip.Addr) (PacketIO, error) {
	if !ip.Is4() {
		return nil, errors.New("homa: raw socket requires a local IPv4 address")
	}
	conn, err := net.ListenIP("ip4:146", &net.IPAddr{IP: net.IP(ip.AsSlice())})
	if err != nil {
		return nil, fmt.Errorf("homa: open raw IPv4 protocol 146 socket (requires raw-socket privilege): %w", err)
	}
	// Darwin's default raw-IP receive queue is only 8 KiB: smaller than one
	// normal 14 KiB unscheduled burst, before kernel packet accounting. Ask
	// for a bounded queue large enough to absorb bursts while Go is scheduled.
	// The OS may cap this request; it is separate from Config.MaxBufferedBytes.
	if err := conn.SetReadBuffer(1 << 20); err != nil {
		conn.Close()
		return nil, fmt.Errorf("homa: configure raw IPv4 receive queue: %w", err)
	}
	return &rawIP{conn: conn}, nil
}

// ReadPacket returns only the Homa header and data. On a small caller buffer,
// it discards that packet and returns io.ErrShortBuffer without partial data.
func (r *rawIP) ReadPacket(buf []byte) (int, netip.Addr, error) {
	r.readMu.Lock()
	defer r.readMu.Unlock()
	n, from, err := r.conn.ReadFromIP(r.readBuf[:])
	if err != nil {
		return 0, netip.Addr{}, err
	}
	ip, ok := netip.AddrFromSlice(from.IP)
	if !ok || !ip.Unmap().Is4() {
		return 0, netip.Addr{}, errors.New("homa: raw socket received invalid IPv4 source")
	}
	if n > len(buf) {
		return 0, ip.Unmap(), io.ErrShortBuffer
	}
	return copy(buf, r.readBuf[:n]), ip.Unmap(), nil
}

// WritePacket selects the packet's Homa priority using the IPv4 precedence
// bits. Serialize the IP_TOS change with the write so concurrent sends cannot
// accidentally inherit another packet's priority. The local OS or network
// may rewrite traffic class markings; queue behavior needs packet-capture and
// switch validation on the target network.
func (r *rawIP) WritePacket(packet []byte, peer netip.Addr, priority uint8) error {
	if !peer.Is4() || peer.IsUnspecified() || peer.IsMulticast() || peer == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return errors.New("homa: raw packet requires a unicast IPv4 peer")
	}
	if priority > 7 {
		return errors.New("homa: packet priority must be between 0 and 7")
	}
	if len(packet) == 0 || len(packet) > 65535-20 {
		return errors.New("homa: invalid raw IPv4 payload size")
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	if err := r.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		return err
	}
	control, err := r.conn.SyscallConn()
	if err != nil {
		return err
	}
	var optionErr error
	if err := control.Control(func(fd uintptr) {
		optionErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, int(priority)<<5)
	}); err != nil {
		return err
	}
	if optionErr != nil {
		return fmt.Errorf("homa: set raw packet priority: %w", optionErr)
	}
	n, err := r.conn.WriteToIP(packet, &net.IPAddr{IP: net.IP(peer.AsSlice())})
	if err != nil {
		return err
	}
	if n != len(packet) {
		return io.ErrShortWrite
	}
	return nil
}

func (r *rawIP) Close() error { return r.conn.Close() }
