package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	homa "github.com/phenomenon0/MacHoma"
)

func tcpFixture(t *testing.T, handler func(*net.TCPConn) error) (*net.TCPConn, <-chan error) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.AcceptTCP()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		conn.SetNoDelay(true)
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		done <- handler(conn)
	}()
	client, err := net.DialTCP("tcp4", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	if err := client.SetNoDelay(true); err != nil {
		t.Fatal(err)
	}
	return client, done
}

func TestTCPBaselineReusesConnectionAcrossFrameSizes(t *testing.T) {
	sizes := []int{64, 65536, 1000000}
	conn, done := tcpFixture(t, func(server *net.TCPConn) error {
		for _, size := range sizes {
			var header [4]byte
			if _, err := io.ReadFull(server, header[:]); err != nil {
				return err
			}
			if got := int(binary.BigEndian.Uint32(header[:])); got != size {
				return fmt.Errorf("frame length %d; want %d", got, size)
			}
			data := make([]byte, size)
			if _, err := io.ReadFull(server, data); err != nil {
				return err
			}
			if err := writeFrame(server, data); err != nil {
				return err
			}
		}
		return nil
	})
	for _, size := range sizes {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i * 31)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		reply, err := tcpCall(ctx, conn, payload)
		cancel()
		if err != nil || !bytes.Equal(reply, payload) {
			t.Fatalf("%d-byte persistent echo: %v", size, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTCPBaselineRejectsOversizedReply(t *testing.T) {
	conn, done := tcpFixture(t, func(server *net.TCPConn) error {
		request := make([]byte, 5)
		if _, err := io.ReadFull(server, request); err != nil {
			return err
		}
		var header [4]byte
		binary.BigEndian.PutUint32(header[:], homa.MaxMessageSize+1)
		_, err := server.Write(header[:])
		return err
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := tcpCall(ctx, conn, []byte{1}); err == nil || !strings.Contains(err.Error(), "invalid TCP reply length") {
		t.Fatalf("oversized peer reply accepted: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestLatencyPercentilesPreserveRunOrder(t *testing.T) {
	samples := make([]int64, 100)
	for i := range samples {
		samples[i] = int64(100-i) * 1000
	}
	original := slices.Clone(samples)
	p50, p95, p99, maximum := latencySummary(samples)
	if p50 != 50 || p95 != 95 || p99 != 99 || maximum != 100 {
		t.Fatalf("known 1..100 us distribution: got %v %v %v %v", p50, p95, p99, maximum)
	}
	if !slices.Equal(samples, original) {
		t.Fatal("percentile calculation reordered raw run evidence")
	}
	p50, p95, p99, maximum = latencySummary([]int64{3000, 1000, 2000})
	if p50 != 2 || p95 != 3 || p99 != 3 || maximum != 3 {
		t.Fatalf("three samples should retain nearest-rank tails: got %v %v %v %v", p50, p95, p99, maximum)
	}
}
