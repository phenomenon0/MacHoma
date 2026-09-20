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

## Physical Mac / Linux validation — PASSED

A physical Apple M3 Ultra Mac on macOS 27.0 exchanged native IPv4 protocol-146
packets over Wi-Fi with a Linux host. The compatibility fixture used an explicit
address-rewriting relay from that Linux host into the unmodified HomaModule in
QEMU. Homa headers and application bytes were unchanged. This is relay-assisted
physical-Mac/kernel interoperability; the Linux kernel peer was not bare metal.

All eight sizes, **1, 100, 1424, 65535, 65536, 65537, 100000, 1000000 bytes**,
passed full byte comparison in both directions. The final relay evidence records
eight kernel server requests, eight distinct Mac client ACKs and a completed
kernel client sweep. The successful run recorded no relay packet drops.

- [Mac exact-payload checks](validation/2026-09-20-physical/mac-kernel-calls.json)
- [Independent kernel guest checks](validation/2026-09-20-physical/guest-console.txt)
- [Relay counters and observed priorities](validation/2026-09-20-physical/relay-report.json)
- [Physical packet-capture summary](validation/2026-09-20-physical/capture-summary.json)
- [Host and test-path metadata](validation/2026-09-20-physical/metadata.json)

The retained capture contains 8,333 native-Homa packets, with maximum IPv4 lengths
of 1,476 bytes from the Mac and 1,500 from Linux, and no observed fragmentation.
Several nonzero IP priority markings were observed in both directions. This
establishes markings in these captured packets, not Wi-Fi/switch queue treatment.

An earlier relay attempt aborted on a local ENOBUFS transmit error. The fixture
now counts ENOBUFS/EAGAIN as a datagram drop for native Homa recovery, with other
errors remaining fatal. The successful rerun did not encounter this error.
A separate direct MacHoma-versus-TCP Wi-Fi benchmark then completed with the VM
and relay stopped; see [measured results](docs/BENCHMARK_RESULTS.md). The temporary
Mac desktop job, test processes and peer-specific firewall rule were removed.
No host Homa module or persistent privileged helper was installed.

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

`cloc 2.11` measured **1,201 code lines** in the Go protocol library (`api.go`,
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

## Measured TCP comparison

[Full results and raw samples](docs/BENCHMARK_RESULTS.md) cover three paired
rounds on hosted macOS/arm64 and Linux/amd64. TCP was faster at every tested
payload size. Increasing the requested native receive queue from the OS default
to 1 MiB removed a large Darwin multi-packet delay and measured client
retransmissions. These are sequential loopback results, not a physical-network
or original-paper performance claim.
