# MacHoma

[![CI](https://github.com/phenomenon0/MacHoma/actions/workflows/ci.yml/badge.svg)](https://github.com/phenomenon0/MacHoma/actions/workflows/ci.yml)

An experimental Go implementation of **native Homa for macOS and Linux**.
MacHoma sends IPv4 protocol **146**, preserves application bytes, and implements
the Homa wire protocol in userspace without a custom kernel extension.

Native macOS loopback and bidirectional interoperability with the unmodified
Linux HomaModule have passed. See [validation evidence](VALIDATION.md) for the
exact test paths and limitations. This project does not claim a historical first
or the Linux implementation's performance.

Wire target: [HomaModule d8914b8a57aa48c19a2c8130484961585bcbe53c](https://github.com/PlatformLab/HomaModule/tree/d8914b8a57aa48c19a2c8130484961585bcbe53c).
The target must be pinned: Homa's development wire/API has changed over time.

## Implemented

- Current packed DATA, GRANT, RESEND, RPC_UNKNOWN, BUSY, CUTOFFS, NEED_ACK and ACK
  packets; even client IDs and odd server IDs, full 80-byte ACKs.
- Native Darwin/Linux raw IPv4 sockets with protocol 146. The OS supplies the IP
  header, routing, ARP and IPv4 checksum. No custom kernel extension is required.
- Receiver grants ordered by shortest remaining message, unscheduled prefixes,
  eight priority markings, cutoff exchange, original-boundary retransmission,
  arbitrary incoming segment boundaries and duplicate suppression.
- Request/reply calls, concurrent bounded handlers, response retention until ACK,
  cancellation/deadlines, bounded live RPCs and receive storage.
- Native echo CLI and an independent C peer using the **actual Linux kernel
  socket ABI**, not another copy of this protocol engine.

## Build and exercise

Go 1.24+; no third-party Go dependencies. Clone and enter the repository:

```sh
git clone https://github.com/phenomenon0/MacHoma.git
cd MacHoma
```

Build and test:

```sh
GOWORK=off GOCACHE=/tmp/homa-go-gocache go test -race -p 2 ./...
GOWORK=off CGO_ENABLED=0 go build -o /tmp/homa-echo ./cmd/homa-echo
```

Cross-compile on Linux for Apple Silicon:

```sh
GOWORK=off CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 \
  go build -o /tmp/homa-echo-darwin-arm64 ./cmd/homa-echo
```

On a Mac, native raw sockets require administrator privileges. The single-command
loopback check tests all eight message sizes:

```sh
sudo /tmp/homa-echo-darwin-arm64 selftest
```

For separate client/server processes, use separate terminals:

```sh
sudo /tmp/homa-echo-darwin-arm64 serve -listen 127.0.0.1:4000
sudo /tmp/homa-echo-darwin-arm64 call -listen 127.0.0.1:4001 \
  -peer 127.0.0.1:4000 -bytes 1000000 -count 10
```

For cross-machine tests, replace the literal addresses with the intended LAN
addresses. Native IP protocol 146 must pass in both directions. A UDP firewall
rule does not permit Homa. A tunnel's TCP/UDP support alone is insufficient proof
that its route carries this protocol. Linux raw sockets require CAP_NET_RAW/root;
opening the real Linux Homa kernel socket has separate module prerequisites.

For a Mac ↔ actual Linux HomaModule test, use [interop/README.md](interop/README.md).
Linux must use `hijack_tcp=0` before creating sockets; use the conservative default
`max_gso_size=1000` for the first test. IPv6 and Homa's TCP-hijacking mode are not
implemented here. Raw sockets do not reserve Homa ports: use one endpoint per
local IP/port, and do not overlap a userspace endpoint with Linux kernel Homa.

## Compare with TCP

The [benchmark guide](docs/BENCHMARK.md) measures native Homa against persistent
TCP_NODELAY using the same payloads, warmups, alternating rounds and raw latency
samples. A manually triggered GitHub workflow runs it on Linux and macOS
loopback. These checks do not reproduce the paper's loaded datacenter workload.

## API

```go
local, _ := homa.ParseAddr("192.0.2.10:4000")
ep, err := homa.Listen(local, homa.Config{}, func(ctx context.Context,
    peer homa.Addr, request []byte) []byte {
    return request // application echo
})
// Check err; defer ep.Close(). Clients call ep.Call(ctx, peer, payload).
```

Messages contain 1..1,000,000 bytes. Application errors belong in the application
payload; there is no custom transport error envelope. Failed handlers retain
state until expiry so a response probe does not immediately rerun the handler.
`CallError.Sent` means execution may have occurred. Homa's native RPC_UNKNOWN
recovery may repeat execution after peer state loss; side effects still require
application-level idempotency. Cancellation is local: Homa has no cancel frame.

`MaxBufferedBytes` counts protocol-owned payloads and receive bitmaps, including
requests still owned by running handlers. It is not a process RSS limit: bounded
packet queues, temporary encoded packets, caller buffers, and allocations inside
application handlers are additional. Each native socket requests a bounded
1 MiB kernel receive queue (the OS may cap it), also outside that budget. `Close` unblocks calls and packet loops;
a handler that ignores its context may remain running and retains its worker slot.

Native Homa provides neither encryption nor authenticated caller identity.
Source addresses and RPC IDs are correlation fields. Applications must preserve
their authorization, idempotency and durable execution semantics.

## Proof and current limits

See [VALIDATION.md](VALIDATION.md) for measured checks and remaining gates.
The C-generated fixtures compile the unchanged upstream wire header and compare
all sizes, offsets, opcodes and packet bytes. Fault tests cover losses, duplicate
and reordered data, concurrent RPCs, deadlines, bounded state and acknowledgement
recovery. A QEMU test can exercise the unmodified Linux module without loading a
kernel module into the host.

This is a compatibility prototype, not a production or performance-equivalent
port. There is no NIC offload, zero-copy receive pool, Linux qdisc, optimized
pacer, adaptive congestion control, or production observability. macOS may
rewrite IP_TOS on QoS-managed interfaces; capture packets on the actual interface
before claiming that switch priorities work. Do not infer the paper's tail
latency gains from correctness tests, loopback or a software-emulated VM.

Integration into an existing RPC stack should follow workload measurements.
The current API is experimental. See [ROADMAP.md](ROADMAP.md).

## Primary references

- [2021 Homa implementation paper](https://www.usenix.org/system/files/atc21-ousterhout.pdf).
- [Current pinned wire header](https://github.com/PlatformLab/HomaModule/blob/d8914b8a57aa48c19a2c8130484961585bcbe53c/homa_wire.h).
- [Apple XNU raw IPv4 implementation](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet/raw_ip.c).
- [Go raw IP implementation](https://github.com/golang/go/blob/master/src/net/iprawsock_posix.go).
- [Apple traffic-class handling](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/netinet/in_tclass.c).

MacHoma is licensed under [BSD-2-Clause](LICENSE). Vendored upstream headers
retain their original notices; see [NOTICE](NOTICE) for attribution.
