# Validation — 2026-09-19

## Demonstrated

**Bidirectional native Homa interoperability with the unmodified Linux module.**
The pinned HomaModule compiled against Fedora kernel 6.17.12-300.fc43.x86_64
and ran inside QEMU TCG. The Go endpoint used QEMU's Ethernet socket backend,
constructing ordinary IPv4 protocol-146 packets; the VM ran the real Linux
Homa socket implementation through the independent C peer.

Both directions passed complete byte-for-byte request/reply comparison at each
size: **1, 100, 1424, 65535, 65536, 65537, 100000, 1000000 bytes**. The final
snapshot showed eight completed outgoing calls, eight handled incoming requests,
and zero active RPCs, active handlers, or buffered payload bytes. This exercises
DATA, receiver grants, large-message reassembly, retransmission and ACK cleanup
against a different implementation. It does not prove every failure scenario
against Linux, IPv6, TCP hijacking, or arbitrary Homa revisions.

- [Host result](validation/2026-09-19-qemu-result.txt)
- [Guest console with independent client checks; terminal controls normalized](validation/2026-09-19-qemu-console.txt)
- [Kernel/module/initramfs manifest](validation/2026-09-19-qemu-manifest.json)

There were **704 bounded transmit-queue drops and 716 retransmitted packets** in
this software-emulated run; recovery succeeded. This demonstrates correctness
under those losses, not acceptable production throughput or latency. The initial
~3-second call includes guest link/ARP startup. None of these timings is a Mac,
physical LAN, or paper-performance benchmark.

**Local software checks:**

- Full `go test -race -p 2 -count=1 ./...` and `go vet ./...` passed.
- Canonical wire fixtures compile the exact upstream C header, independently
  checking every struct size, field offset, packet type and representative bytes.
- Parser fuzz run: 1,194,907 executions in ten seconds, no failure.
- Fault tests: lost initial request, data, grant and ACK; duplicate and reordered
  packets; arbitrary segment boundaries; conflicting overlap; peer isolation;
  18 concurrent mixed-size calls; deadlines, shutdown and resource limits.
- Regression checks: blocked socket writer does not block deadlines; piggyback
  ACK cannot orphan memory; response probes do not suppress loss recovery;
  destination-specific write errors do not kill unrelated calls; expired queued
  data is skipped; completion ACK survives RPC cleanup; priority queues drain
  highest first; limited broadcast rejected before transmission; untrusted RESEND
  offsets rejected before signed conversion (also tested on 386); expired
  requests cannot begin handler execution while awaiting timer cleanup.
- Linux C peer passed strict compilation and ASan/UBSan receive-pool self-tests.
- Linux amd64 and Darwin arm64/amd64 builds succeeded. Cross compilation is not
  Darwin packet-I/O execution evidence.

## Native Mac execution — PASSED

The prepared binary ran with administrator authorization on the actual Mac:
macOS 27.0 (26A428), Apple Silicon arm64. The desktop job exited with code 0.
[Captured JSON result](validation/2026-09-19-macos-selftest.json).

Both endpoints used the native Darwin raw IPv4 backend, IP protocol **146**, on
127.0.0.1 ports 4000 and 4001. All eight sizes passed byte-for-byte echo checks:
**1, 100, 1424, 65535, 65536, 65537, 100000, 1000000 bytes**.

The run took 0.364 seconds overall. Both endpoints reported zero invalid packets,
transmit-queue drops and write errors, and finished with zero active RPCs,
handlers or buffered payload bytes. The server recorded 27 retransmitted
packets and the client 15; this remains an unoptimized correctness prototype.
Single-run round-trip times ranged from 198 microseconds (100 bytes) to 151 ms
(1 MB); these are not benchmark distributions or physical-network measurements.

Executable SHA-256, verified locally and on the Mac:
`2d1d1f83101faba53d64f1af5c55159d8615ab11dc17c7d0fec445404ebb3456`.

The JSON field `linux_kernel_interoperability_verified: false` is scoped to this
Mac loopback test. Linux interoperability was independently demonstrated by the
QEMU run above. Together these establish the portable engine's Linux Homa
interoperability and the native Darwin packet backend's execution, separately.

**Remaining physical-network gate:** Mac ↔ Linux over the actual LAN, including
protocol146 passage, MTU and observed IP priority markings. Apple's QoS handling
can rewrite IP_TOS on some interfaces. Neither loopback nor the VM establishes
physical-network throughput, latency, reliability, or the paper's performance.

## Reproduce Linux interoperability without host kernel changes

Build the pinned module unprivileged against matching installed kernel headers.
Then, from this directory:

```sh
python3 interop/qemu/build_initramfs.py \
  --module /tmp/homa-port-review/HomaModule/PlatformLab-HomaModule-d8914b8/homa.ko \
  --kernel-release 6.17.12-300.fc43.x86_64 --out-dir /tmp/homa-qemu
GOWORK=off go run ./interop/vmcheck \
  -kernel /boot/vmlinuz-6.17.12-300.fc43.x86_64 \
  -initrd /tmp/homa-qemu/homa-initramfs.cpio.gz
```

The VM uses the unchanged module, native146 (hijack disabled), single-segment
Linux sends (`max_gso_size=1000`), MTU1500, and increased Linux timeout ticks for
TCG tolerance. Host networking/sysctls/modules are unchanged. The first test
attempt exposed an incorrect C peer `sendmsg` control length; it was corrected
to the upstream-required zero length while keeping a valid control pointer.
No kernel or wire modification was made to get the successful result.

## Size

`cloc 2.11` measured **1,197 code lines** in the Go protocol library (`api.go`,
`endpoint.go`, `rawip.go`, `rawip_unsupported.go`, `internal/wire/wire.go`), excluding
comments, blanks, CLI, tests, fixtures, VM tooling and vendored upstream headers.
The upstream Linux implementation previously measured 11,559 C/header code lines
at this pin; its larger kernel, offload, pacing and instrumentation scope is not
implemented by this prototype.

## Public CI

[Initial release CI](https://github.com/phenomenon0/MacHoma/actions/runs/35489905200)
passed tests, race detection, vet and native raw IPv4 loopback on Linux/amd64
(Ubuntu 24.04) and Darwin/arm64 (macOS 15), with Go 1.25.13. CI uses hosted
machines and loopback; it does not establish the physical LAN path. The final
release engine also repeated all 16 bidirectional QEMU kernel checks successfully.
