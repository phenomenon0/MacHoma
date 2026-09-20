# Native Homa versus TCP: measured results

Run date: 2026-09-20 UTC. **TCP was faster than the current MacHoma prototype at every tested payload size.** Hosted-loopback and physical Wi-Fi results are reported separately below. These sequential echo tests do not reproduce the Homa paper.

## Method

- Same host and payloads for both protocols; one outstanding RPC; persistent TCP_NODELAY.
- Three alternating-order rounds; ten unmeasured warmups for every size/transport/round.
- 3,000 measured calls per transport at 64, 1,024 and 16,384 bytes; 300 at 65,536 and 1,000,000 bytes.
- Full byte verification for every call. RTT excludes verification; reported payload goodput includes it.
- Tables pool the raw samples across rounds and use nearest-rank percentiles. They do not average per-round percentiles.
- Homa requests IP precedence; TCP uses its OS defaults. This difference matters when testing networks that honor QoS.

[Initial run](https://github.com/phenomenon0/MacHoma/actions/runs/35490579270) used commit `ab51cf5757f6f162f5c57814b533c89dce96dee7`. [Receive-buffer rerun](https://github.com/phenomenon0/MacHoma/actions/runs/35490913545) used `bb3d05036f821eaa16dc9fb517b51b728ac2247d`. Both completed without failed measured calls.

## macOS 15 / Apple M1 virtual host / 3 CPUs / 7 GiB RAM

Current port, requesting a 1 MiB raw-socket receive queue; the OS may cap the request. Client and server run on the same host.

| Payload | Homa median µs | TCP median µs | Homa p99 µs | TCP p99 µs | Homa / TCP payload goodput, Mb/s |
|---:|---:|---:|---:|---:|---:|
| 64 B | 65.17 | 35.83 | 129.21 | 63.58 | 14.87 / 26.98 |
| 1,024 B | 80.79 | 38.33 | 153.00 | 72.96 | 193.97 / 388.33 |
| 16,384 B | 511.12 | 49.42 | 748.67 | 151.92 | 496.17 / 4,644.62 |
| 65,536 B | 2,262.33 | 69.00 | 2,863.96 | 150.33 | 455.28 / 13,557.08 |
| 1,000,000 B | 35,130.62 | 242.21 | 44,862.79 | 586.79 | 446.81 / 53,163.19 |

[Raw samples](../validation/benchmarks/receive-buffer-fix/macos/raw.json) · [machine metadata](../validation/benchmarks/receive-buffer-fix/macos/metadata.txt).

## Linux / AMD EPYC 7763 virtual host / 4 CPUs / approximately 16 GiB RAM

Current port, requesting a 1 MiB raw-socket receive queue; the OS may cap the request. Client and server run on the same host.

| Payload | Homa median µs | TCP median µs | Homa p99 µs | TCP p99 µs | Homa / TCP payload goodput, Mb/s |
|---:|---:|---:|---:|---:|---:|
| 64 B | 123.24 | 68.07 | 194.83 | 94.06 | 7.84 / 14.33 |
| 1,024 B | 145.09 | 67.53 | 216.60 | 100.81 | 107.73 / 228.88 |
| 16,384 B | 711.12 | 88.00 | 1,055.92 | 132.61 | 359.25 / 2,857.04 |
| 65,536 B | 2,783.59 | 181.47 | 3,345.85 | 408.01 | 372.77 / 5,296.85 |
| 1,000,000 B | 44,865.92 | 1,365.19 | 59,453.66 | 1,719.17 | 350.97 / 11,104.86 |

[Raw samples](../validation/benchmarks/receive-buffer-fix/linux/raw.json) · [machine metadata](../validation/benchmarks/receive-buffer-fix/linux/metadata.txt).

## What the benchmark caught

The initial macOS run showed a sharp multi-packet slowdown and many client retransmissions. Darwin defaults raw-IP receive queues to 8 KiB; this is smaller than the normal 14 KiB unscheduled Homa burst, before kernel packet-memory accounting. The same-IP loopback path also delivers protocol-146 traffic to both raw endpoints before userspace Homa-port filtering.

The only transport change for the rerun was requesting a bounded 1 MiB receive queue. The macOS 16,384-byte median dropped from **16,907.75 µs to 511.12 µs** (about **33×**). Client retransmissions for those measured calls fell from **10,053 to 0**. This supports receive-queue pressure as the source of the earlier cliff; independent hosted runs are not a tightly controlled same-VM experiment. Effective socket capacity was not recorded. The initial Linux runner used an Intel Xeon 8370C and the rerun an AMD EPYC 7763, so Linux differences between runs cannot be assigned to the queue change. Each within-run Homa/TCP comparison used the same host.

After that fix, the port still took about **10.3×** TCP's median RTT at 16,384 bytes and **145×** at 1,000,000 bytes on the measured Mac runner. The raw samples include all results; no failed calls or slow samples were discarded.

## Interpretation and remaining tests

These measurements do not support a claim that MacHoma is faster than TCP. The current userspace implementation lacks the Linux implementation's optimized packet path, batching, offloads and pacing. Homa sends at most 1,400 data bytes per packet by default; TCP benefits from the host's loopback MTU and optimized stack. Loopback also creates extra raw-socket traffic at both endpoints. The benchmark measures the implementations as configured, rather than equal packet counts.

The original paper evaluates its Linux kernel implementation under datacenter workloads. Reproducing its speed claims requires suitable physical Ethernet, loaded mixed-size RPC workloads, controlled competing traffic/incast, and a documented TCP baseline. This sequential hosted-loopback comparison does not establish saturated network capacity, loaded p99 latency or a physical-Mac result.

Physical Mac/Linux kernel interoperability and the direct Wi-Fi comparison subsequently completed; see the physical section below.

## Physical Wi-Fi: Linux client to Mac server

**TCP had lower median RTT and higher payload goodput at every tested size.** The Mac was an Apple M3 Ultra (28 logical CPUs, 96 GiB RAM, macOS 27.0); Linux used a Ryzen 7 7700X (16 logical CPUs, about 32 GB RAM) and Fedora kernel 6.17.12. Both used their Wi-Fi interfaces on the same LAN with MTU 1500. The speed test used direct native IP protocol 146 and direct TCP between hosts; the VM/relay had exited before measurements.

Three alternating rounds used 100 small-message and 10 large-message samples per round, plus ten warmups for every group. Thus each protocol has 300 samples per small size and 30 per large size. These counts are explicit: large-message p99 is the maximum of only 30 samples, not a stable tail estimate.

| Payload | Homa median ms | TCP median ms | Homa p99 ms | TCP p99 ms | Homa / TCP payload goodput, Mb/s |
|---:|---:|---:|---:|---:|---:|
| 64 B | 6.999 | 4.704 | 12.542 | 15.061 | 0.15 / 0.19 |
| 1,024 B | 7.049 | 4.708 | 14.149 | 16.611 | 2.31 / 2.86 |
| 16,384 B | 23.828 | 9.579 | 39.755 | 24.067 | 10.52 / 24.42 |
| 65,536 B | 101.821 | 17.647 | 192.208 | 27.212 | 9.90 / 56.99 |
| 1,000,000 B | 1,540.004 | 142.462 | 1,742.862 | 260.072 | 10.24 / 104.32 |

[Raw samples](../validation/benchmarks/physical-wifi/raw.json) · [recomputed summary](../validation/benchmarks/physical-wifi/summary.json) · [machine and path metadata](../validation/2026-09-20-physical/metadata.json). Private host IPs were replaced with documentation addresses; timings and counts are unchanged.

The initial full-size Wi-Fi workload was explicitly stopped during the first 1 MB group because it could not fit the ten-minute desktop test window. It produced no successful result JSON and is not mixed into this table. The shorter paired run completed every configured call without an error.

A separate diagnostic Homa echo listener remained idle on Mac port 4001 during the benchmark. Raw protocol sockets receive matching traffic before userspace port filtering, so this adds receive work to Homa; the physical comparison characterizes that diagnostic setup. It is not an isolated single-listener performance ceiling. The packet capture stopped before the speed test, and no kernel-relay workload ran concurrently. Homa and TCP also used their different default priority behavior.

Small-message sample p99 was lower for Homa in this physical run, while its medians were worse. The limited sample count and uncontrolled radio contention do not establish a repeatable tail-latency advantage. Client retransmission counters also increased for large Homa messages (6,090 retransmitted packets across the measured 1 MB calls). Retained raw samples and repeated trials are necessary before broader claims.

The current 14 KB receiver grant window and per-packet userspace path are plausible bottlenecks on millisecond-RTT Wi-Fi; this run does not isolate their individual costs. Further performance work should first isolate the benchmark listener, profile packet processing and vary pacing/window settings in controlled experiments.

## Reproduce the summary

See [BENCHMARK.md](BENCHMARK.md) for running the workload. The preserved raw samples and metadata are under `validation/benchmarks/`, including the initial run. From the repository root:

```sh
python3 scripts/summarize_benchmark.py validation/benchmarks/receive-buffer-fix/macos/raw.json
```
