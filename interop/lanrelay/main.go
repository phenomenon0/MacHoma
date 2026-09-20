//go:build linux

// lanrelay connects one allowlisted native-Homa LAN peer to an isolated Linux
// kernel VM. It rewrites IPv4 addresses; Homa headers and payload stay unchanged.
// This is explicitly a relay correctness fixture, not a direct VM LAN interface.
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
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var virtualHost = netip.MustParseAddr("10.0.0.1")
var virtualGuest = netip.MustParseAddr("10.0.0.2")
var hostMAC = []byte{2, 0, 0, 0, 0, 1}
var guestMAC = []byte{0x52, 0x54, 0, 0x12, 0x34, 0x56}

type report struct {
	Path                         string `json:"path"`
	Local, Peer                  string
	LANToVM, VMToLAN             uint64
	DroppedLAN, DroppedVM        uint64
	LANSendBufferDrops           uint64
	ObservedLANIngressTOS        map[uint8]uint64
	ObservedVMIngressTOS         map[uint8]uint64
	RequestedLANEgressTOS        map[uint8]uint64
	MacClientRPCsAcknowledged    int
	KernelServerRequestsReceived int
	KernelClientSweepPassed      bool
	Complete                     bool
	Error                        string `json:"error,omitempty"`
}

type state struct {
	mu   sync.Mutex
	r    report
	acks map[uint64]bool
}

func (s *state) snapshot() report {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.r
	r.MacClientRPCsAcknowledged = len(s.acks)
	r.ObservedLANIngressTOS = cloneMap(s.r.ObservedLANIngressTOS)
	r.ObservedVMIngressTOS = cloneMap(s.r.ObservedVMIngressTOS)
	r.RequestedLANEgressTOS = cloneMap(s.r.RequestedLANEgressTOS)
	return r
}
func cloneMap(m map[uint8]uint64) map[uint8]uint64 {
	out := make(map[uint8]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func ipChecksum(b []byte) uint16 {
	var sum uint32
	for len(b) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(b))
		b = b[2:]
	}
	if len(b) > 0 {
		sum += uint32(b[0]) << 8
	}
	for sum > 65535 {
		sum = sum&65535 + sum>>16
	}
	return ^uint16(sum)
}

// nativePacket validates a complete IPv4 datagram and returns its original Homa
// bytes and observed traffic class. IPv4 options are accepted but not relayed.
func nativePacket(ip []byte, source, destination netip.Addr) ([]byte, uint8, bool) {
	if len(ip) < 20 || ip[0]>>4 != 4 {
		return nil, 0, false
	}
	header := int(ip[0]&15) * 4
	total := int(binary.BigEndian.Uint16(ip[2:4]))
	if header < 20 || header > len(ip) || total < header+28 || total > len(ip) ||
		ip[9] != 146 || ipChecksum(ip[:header]) != 0 || binary.BigEndian.Uint16(ip[6:8])&0x3fff != 0 ||
		!bytes.Equal(ip[12:16], source.AsSlice()) || !bytes.Equal(ip[16:20], destination.AsSlice()) {
		return nil, 0, false
	}
	return ip[header:total], ip[1], true
}

func allowedHoma(p []byte, fromLAN bool) bool {
	if len(p) < 28 {
		return false
	}
	src, dst := binary.BigEndian.Uint16(p), binary.BigEndian.Uint16(p[2:])
	if fromLAN {
		if !((src == 4003 && dst == 4000) || (src == 4001 && dst == 4002)) {
			return false
		}
	} else if !((src == 4000 && dst == 4003) || (src == 4002 && dst == 4001)) {
		return false
	}
	lengths := map[byte]int{0x10: 56, 0x11: 33, 0x12: 37, 0x13: 28, 0x14: 28, 0x15: 62, 0x17: 28, 0x18: 80}
	minimum, ok := lengths[p[11]]
	return ok && len(p) >= minimum
}

func ethernetPacket(payload []byte, tos byte) []byte {
	frame := make([]byte, 14+20+len(payload))
	copy(frame, guestMAC)
	copy(frame[6:], hostMAC)
	binary.BigEndian.PutUint16(frame[12:], 0x0800)
	ip := frame[14:]
	ip[0], ip[1], ip[8], ip[9] = 0x45, tos, 64, 146
	binary.BigEndian.PutUint16(ip[2:], uint16(len(ip)))
	copy(ip[12:], virtualHost.AsSlice())
	copy(ip[16:], virtualGuest.AsSlice())
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip[:20]))
	copy(ip[20:], payload)
	return frame
}

type nic struct {
	conn net.Conn
	mu   sync.Mutex
}

func (n *nic) write(frame []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.conn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	b := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(b, uint32(len(frame)))
	copy(b[4:], frame)
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
func (n *nic) read() ([]byte, error) {
	var head [4]byte
	if _, err := io.ReadFull(n.conn, head[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(head[:])
	if size < 14 || size > 65535+14 {
		return nil, fmt.Errorf("invalid VM frame size %d", size)
	}
	frame := make([]byte, size)
	_, err := io.ReadFull(n.conn, frame)
	return frame, err
}
func (n *nic) arp(frame []byte) error {
	if len(frame) < 42 || binary.BigEndian.Uint16(frame[14:16]) != 1 ||
		binary.BigEndian.Uint16(frame[16:18]) != 0x0800 || frame[18] != 6 || frame[19] != 4 ||
		binary.BigEndian.Uint16(frame[20:22]) != 1 || !bytes.Equal(frame[28:32], virtualGuest.AsSlice()) ||
		!bytes.Equal(frame[38:42], virtualHost.AsSlice()) {
		return nil
	}
	reply := make([]byte, 42)
	copy(reply, frame[6:12])
	copy(reply[6:], hostMAC)
	copy(reply[12:22], frame[12:22])
	binary.BigEndian.PutUint16(reply[20:], 2)
	copy(reply[22:], hostMAC)
	copy(reply[28:], virtualHost.AsSlice())
	copy(reply[32:], frame[22:28])
	copy(reply[38:], frame[28:32])
	return n.write(reply)
}

func (s *state) noteAcks(p []byte) {
	if binary.BigEndian.Uint16(p) != 4003 || binary.BigEndian.Uint16(p[2:]) != 4000 {
		return
	}
	add := func(id uint64) {
		if id != 0 && id&1 == 0 && len(s.acks) < 1024 {
			s.acks[id] = true
		}
	}
	if p[11] == 0x18 {
		add(binary.BigEndian.Uint64(p[20:28]))
		count := int(binary.BigEndian.Uint16(p[28:30]))
		if count > 5 {
			return
		}
		for i := 0; i < count; i++ {
			at := 30 + i*10
			if binary.BigEndian.Uint16(p[at+8:]) == 4000 {
				add(binary.BigEndian.Uint64(p[at:]))
			}
		}
	} else if p[11] == 0x10 && binary.BigEndian.Uint16(p[44:46]) == 4000 {
		add(binary.BigEndian.Uint64(p[36:44]))
	}
}

func lanToVM(raw *net.IPConn, nic *nic, local, peer netip.Addr, s *state) error {
	buf := make([]byte, 65535)
	for {
		// ReadMsgIP intentionally retains the IPv4 header on Linux; unlike
		// ReadFromIP this preserves the observed ingress ToS for evidence.
		n, _, flags, _, err := raw.ReadMsgIP(buf, nil)
		if err != nil {
			return err
		}
		p, tos, ok := nativePacket(buf[:n], peer, local)
		if !ok || flags&syscall.MSG_TRUNC != 0 || !allowedHoma(p, true) {
			s.mu.Lock()
			s.r.DroppedLAN++
			s.mu.Unlock()
			continue
		}
		if err := nic.write(ethernetPacket(p, tos)); err != nil {
			return err
		}
		s.mu.Lock()
		s.r.LANToVM++
		s.r.ObservedLANIngressTOS[tos]++
		s.noteAcks(p)
		s.mu.Unlock()
	}
}

// The kernel transports recover packet loss themselves. A transient local raw
// socket queue failure drops this datagram; it must not kill their RPC state.
func transientSendDrop(err error) bool {
	return errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.EAGAIN)
}

func vmToLAN(raw *net.IPConn, nic *nic, peer netip.Addr, s *state) error {
	control, err := raw.SyscallConn()
	if err != nil {
		return err
	}
	for {
		frame, err := nic.read()
		if err != nil {
			return err
		}
		if binary.BigEndian.Uint16(frame[12:]) == 0x0806 {
			if err := nic.arp(frame); err != nil {
				return err
			}
			continue
		}
		if binary.BigEndian.Uint16(frame[12:]) != 0x0800 || !bytes.Equal(frame[6:12], guestMAC) {
			continue
		}
		p, tos, ok := nativePacket(frame[14:], virtualGuest, virtualHost)
		if !ok || !allowedHoma(p, false) {
			s.mu.Lock()
			s.r.DroppedVM++
			s.mu.Unlock()
			continue
		}
		var optionErr error
		if err := control.Control(func(fd uintptr) {
			optionErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_TOS, int(tos))
		}); err != nil {
			return err
		}
		if optionErr != nil {
			return optionErr
		}
		if err := raw.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		s.mu.Lock()
		s.r.ObservedVMIngressTOS[tos]++
		s.r.RequestedLANEgressTOS[tos]++
		s.mu.Unlock()
		n, err := raw.WriteToIP(p, &net.IPAddr{IP: net.IP(peer.AsSlice())})
		if err != nil {
			if transientSendDrop(err) {
				s.mu.Lock()
				s.r.DroppedVM++
				s.r.LANSendBufferDrops++
				s.mu.Unlock()
				continue
			}
			return err
		}
		if n != len(p) {
			return io.ErrShortWrite
		}
		s.mu.Lock()
		s.r.VMToLAN++
		s.mu.Unlock()
	}
}

type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.b.Len()+len(p) <= 4*1024*1024 {
		_, _ = l.b.Write(p)
	}
	return len(p), nil
}
func (l *logBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func writeFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".homa-lanrelay-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Chmod(0644); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func qemuCredentials(uidFlag int) (*syscall.Credential, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	uid := uidFlag
	if uid < 0 {
		for _, key := range []string{"PKEXEC_UID", "SUDO_UID"} {
			if os.Getenv(key) != "" {
				value, err := strconv.Atoi(os.Getenv(key))
				if err != nil {
					return nil, err
				}
				uid = value
				break
			}
		}
	}
	if uid <= 0 {
		return nil, errors.New("root launch requires -qemu-uid with an unprivileged UID, or PKEXEC_UID/SUDO_UID")
	}
	account, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return nil, err
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "homa-lanrelay:", err)
		os.Exit(1)
	}
}
func run() (result error) {
	localText := flag.String("local", "", "exact local LAN IPv4 address")
	peerText := flag.String("peer", "", "one allowed Mac LAN IPv4 address; ports4001/4003 only")
	kernel := flag.String("kernel", "", "kernel image matching guest Homa module")
	initrd := flag.String("initrd", "", "built native-Homa guest initramfs")
	logPath := flag.String("log", "/tmp/homa-lanrelay-qemu.log", "guest console log")
	reportPath := flag.String("report", "/tmp/homa-lanrelay-report.json", "relay evidence JSON")
	readyPath := flag.String("ready-file", "/tmp/homa-lanrelay.ready", "created after guest server readiness, removed at exit")
	duration := flag.Duration("duration", 3*time.Minute, "whole run deadline, maximum3minutes")
	qemuUID := flag.Int("qemu-uid", -1, "unprivileged QEMU uid (defaults to PKEXEC_UID or SUDO_UID)")
	flag.Parse()
	local, err := netip.ParseAddr(*localText)
	if err != nil || !local.Is4() || local.IsUnspecified() || local.IsMulticast() {
		return errors.New("-local requires unicast IPv4")
	}
	peer, err := netip.ParseAddr(*peerText)
	if err != nil || !peer.Is4() || peer.IsUnspecified() || peer.IsMulticast() || peer == local {
		return errors.New("-peer requires a different unicast IPv4")
	}
	if *kernel == "" || *initrd == "" || *duration <= 0 || *duration > 3*time.Minute || flag.NArg() != 0 {
		return errors.New("require -kernel/-initrd and duration in (0,3m]")
	}
	credential, err := qemuCredentials(*qemuUID)
	if err != nil {
		return err
	}
	if err := os.Remove(*readyPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	raw, err := net.ListenIP("ip4:146", &net.IPAddr{IP: net.IP(local.AsSlice())})
	if err != nil {
		return fmt.Errorf("open native146 raw socket (requires root/CAP_NET_RAW): %w", err)
	}
	defer raw.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	interrupt, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(interrupt, *duration)
	defer cancel()
	s := &state{r: report{Path: "Mac native IPv4 protocol146 LAN <-> explicit address-rewriting relay <-> QEMU Ethernet backend <-> unmodified Linux Homa kernel", Local: local.String(), Peer: peer.String(), ObservedLANIngressTOS: map[uint8]uint64{}, ObservedVMIngressTOS: map[uint8]uint64{}, RequestedLANEgressTOS: map[uint8]uint64{}}, acks: map[uint64]bool{}}
	var logs logBuffer
	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", "-accel", "tcg,thread=multi", "-smp", "2", "-m", "512", "-nodefaults", "-no-user-config", "-nographic", "-serial", "stdio", "-monitor", "none", "-no-reboot", "-kernel", *kernel, "-initrd", *initrd, "-append", "console=ttyS0 rdinit=/init panic=1 nokaslr quiet homa.test_guest=1 homa.mode=interop", "-netdev", "socket,id=n0,connect="+listener.Addr().String(), "-device", "e1000,netdev=n0,mac=52:54:00:12:34:56")
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential, Pdeathsig: syscall.SIGKILL}
	cmd.Stdout = io.MultiWriter(os.Stdout, &logs)
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	defer func() {
		cancel()
		_ = cmd.Process.Kill()
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
		}
		_ = os.Remove(*readyPath)
		r := s.snapshot()
		r.KernelClientSweepPassed = strings.Contains(logs.String(), "HOMA_GUEST_CLIENT_PASS")
		r.KernelServerRequestsReceived = strings.Count(logs.String(), "ok mode=serve call=")
		r.Complete = result == nil
		if result != nil {
			r.Error = result.Error()
		}
		data, _ := json.MarshalIndent(r, "", "  ")
		if err := writeFile(*logPath, []byte(logs.String())); err != nil {
			fmt.Fprintln(os.Stderr, "write log:", err)
		}
		if err := writeFile(*reportPath, append(data, '\n')); err != nil {
			fmt.Fprintln(os.Stderr, "write report:", err)
		}
		fmt.Printf("RELAY_REPORT %s\n%s\n", *reportPath, data)
	}()
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(20 * time.Second))
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	nic := &nic{conn: conn}
	errorsCh := make(chan error, 2)
	go func() { errorsCh <- lanToVM(raw, nic, local, peer, s) }()
	go func() { errorsCh <- vmToLAN(raw, nic, peer, s) }()
	stopIO := context.AfterFunc(ctx, func() { _ = raw.Close(); _ = conn.Close() })
	defer stopIO()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	ready := false
	bootDeadline := time.Now().Add(75 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errorsCh:
			return fmt.Errorf("relay packet path: %w", err)
		case err := <-wait:
			return fmt.Errorf("guest exited before completion: %v", err)
		case <-tick.C:
			text := logs.String()
			if strings.Contains(text, "HOMA_GUEST_ERROR") || strings.Contains(text, "HOMA_GUEST_CLIENT_FAIL") {
				return errors.New("guest reported failure")
			}
			if !ready && strings.Contains(text, "HOMA_GUEST_READY") {
				ready = true
				if err := writeFile(*readyPath, []byte(fmt.Sprintf("pid=%d local=%s peer=%s native_protocol=146\n", os.Getpid(), local, peer))); err != nil {
					return err
				}
				fmt.Printf("RELAY_READY local=%s peer=%s Mac-client4003->guest4000 Mac-server4001<-guest4002\n", local, peer)
			}
			if !ready && time.Now().After(bootDeadline) {
				return errors.New("guest readiness deadline exceeded")
			}
			if ready && strings.Contains(text, "HOMA_GUEST_CLIENT_PASS") && strings.Count(text, "ok mode=serve call=") >= 8 && s.snapshot().MacClientRPCsAcknowledged >= 8 {
				fmt.Println("RELAY_COMPLETE kernel client sweep passed; at least8 unique Mac client ACKs forwarded (confirm Mac payload-check logs separately)")
				return nil
			}
		}
	}
}
