// vmcheck runs the unmodified Linux Homa kernel peer in a disposable QEMU VM.
// The host exchanges native protocol-146 Ethernet frames over QEMU's local NIC
// backend. It requires no host raw sockets, kernel module loading, or sudo.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	homa "github.com/phenomenon0/MacHoma"
)

var hostIP = netip.MustParseAddr("10.0.0.1")
var guestIP = netip.MustParseAddr("10.0.0.2")
var hostMAC = []byte{2, 0, 0, 0, 0, 1}
var guestMAC = []byte{0x52, 0x54, 0, 0x12, 0x34, 0x56}

type nic struct {
	conn net.Conn
	mu   sync.Mutex
}

func (n *nic) Close() error { return n.conn.Close() }
func (n *nic) frame(f []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	_ = n.conn.SetWriteDeadline(time.Now().Add(250 * time.Millisecond))
	b := make([]byte, 4+len(f))
	binary.BigEndian.PutUint32(b, uint32(len(f)))
	copy(b[4:], f)
	for len(b) > 0 {
		count, err := n.conn.Write(b)
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
		b = b[count:]
	}
	return nil
}
func checksum(b []byte) uint16 {
	sum := uint32(0)
	for i := 0; i < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	for sum > 65535 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}
func (n *nic) WritePacket(p []byte, peer netip.Addr, priority uint8) error {
	if peer != guestIP {
		return fmt.Errorf("unexpected VM peer %s", peer)
	}
	f := make([]byte, 14+20+len(p))
	copy(f, guestMAC)
	copy(f[6:], hostMAC)
	binary.BigEndian.PutUint16(f[12:], 0x0800)
	ip := f[14:]
	ip[0] = 0x45
	ip[1] = priority << 5
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	ip[8] = 64
	ip[9] = 146
	copy(ip[12:], hostIP.AsSlice())
	copy(ip[16:], guestIP.AsSlice())
	binary.BigEndian.PutUint16(ip[10:], checksum(ip[:20]))
	copy(ip[20:], p)
	return n.frame(f)
}
func (n *nic) ReadPacket(buf []byte) (int, netip.Addr, error) {
	for {
		var prefix [4]byte
		if _, err := io.ReadFull(n.conn, prefix[:]); err != nil {
			return 0, netip.Addr{}, err
		}
		size := binary.BigEndian.Uint32(prefix[:])
		if size < 14 || size > 65535+14 {
			return 0, netip.Addr{}, fmt.Errorf("invalid Ethernet frame length %d", size)
		}
		f := make([]byte, size)
		if _, err := io.ReadFull(n.conn, f); err != nil {
			return 0, netip.Addr{}, err
		}
		switch binary.BigEndian.Uint16(f[12:14]) {
		case 0x0806:
			if len(f) < 42 || binary.BigEndian.Uint16(f[20:22]) != 1 || !bytes.Equal(f[38:42], hostIP.AsSlice()) {
				continue
			}
			reply := make([]byte, 42)
			copy(reply, f[6:12])
			copy(reply[6:], hostMAC)
			copy(reply[12:22], f[12:22])
			binary.BigEndian.PutUint16(reply[20:22], 2)
			copy(reply[22:], hostMAC)
			copy(reply[28:], hostIP.AsSlice())
			copy(reply[32:], f[22:28])
			copy(reply[38:], f[28:32])
			if err := n.frame(reply); err != nil {
				return 0, netip.Addr{}, err
			}
		case 0x0800:
			if len(f) < 34 {
				continue
			}
			ip := f[14:]
			hlen := int(ip[0]&15) * 4
			total := int(binary.BigEndian.Uint16(ip[2:4]))
			if ip[0]>>4 != 4 || hlen < 20 || total < hlen || total > len(ip) || ip[9] != 146 || checksum(ip[:hlen]) != 0 || binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 {
				continue
			}
			src := netip.AddrFrom4([4]byte(ip[12:16]))
			if src != guestIP || !bytes.Equal(ip[16:20], hostIP.AsSlice()) {
				continue
			}
			if total-hlen > len(buf) {
				return 0, netip.Addr{}, io.ErrShortBuffer
			}
			return copy(buf, ip[hlen:total]), src, nil
		}
	}
}

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(b)
}
func (l *logBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	kernel := flag.String("kernel", "", "Linux kernel image matching Homa module")
	initrd := flag.String("initrd", "", "initramfs built by interop/qemu/build_initramfs.py")
	logPath := flag.String("log", "/tmp/homa-qemu.log", "guest console log path")
	flag.Parse()
	if *kernel == "" || *initrd == "" {
		return fmt.Errorf("require -kernel and -initrd")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", "-accel", "tcg,thread=multi", "-smp", "2", "-m", "512", "-nodefaults", "-nographic", "-serial", "stdio", "-monitor", "none", "-no-reboot", "-kernel", *kernel, "-initrd", *initrd, "-append", "console=ttyS0 rdinit=/init panic=1 nokaslr quiet homa.test_guest=1 homa.mode=interop", "-netdev", "socket,id=n0,connect="+listener.Addr().String(), "-device", "e1000,netdev=n0,mac=52:54:00:12:34:56")
	var logs logBuffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = os.WriteFile(*logPath, []byte(logs.String()), 0644)
	}()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second))
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	endpoint, err := homa.New(&nic{conn: conn}, homa.Addr{IP: hostIP, Port: 4001}, homa.Config{Timeout: 15 * time.Second, RetryInterval: 10 * time.Millisecond}, func(_ context.Context, _ homa.Addr, b []byte) []byte { return b })
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer endpoint.Close()
	deadline := time.Now().Add(70 * time.Second)
	for !strings.Contains(logs.String(), "HOMA_GUEST_READY") {
		if time.Now().After(deadline) {
			return fmt.Errorf("guest failed to become ready; see %s\n%s", *logPath, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, size := range []int{1, 100, 1424, 65535, 65536, 65537, 100000, 1000000} {
		b := make([]byte, size)
		for i := range b {
			b[i] = byte((i * 31) ^ (i >> 8) ^ 0xa5)
		}
		began := time.Now()
		response, err := endpoint.Call(ctx, homa.Addr{IP: guestIP, Port: 4000}, b)
		if err != nil {
			return fmt.Errorf("native userspace -> Linux HomaModule %d bytes: %w; see %s", size, err, *logPath)
		}
		if !bytes.Equal(response, b) {
			return fmt.Errorf("%d-byte echo mismatch", size)
		}
		fmt.Printf("PASS userspace -> unmodified Linux HomaModule: %d bytes (%s, TCG correctness only)\n", size, time.Since(began))
	}
	deadline = time.Now().Add(40 * time.Second)
	for !strings.Contains(logs.String(), "HOMA_GUEST_CLIENT_PASS") {
		if time.Now().After(deadline) {
			return fmt.Errorf("kernel -> userspace not verified; see %s\n%s", *logPath, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("PASS Linux HomaModule -> userspace (guest checks); stats=%+v\n", endpoint.Stats())
	return nil
}
