# Native Homa versus persistent TCP

`cmd/homa-bench` measures verified echo RPCs between the same two hosts. Both
transports carry identical plaintext application bytes. TCP uses one persistent
connection with `TCP_NODELAY` at both ends and a four-byte length prefix. Its
prefix and payload are submitted together with a vectored write. Homa uses one
persistent native IPv4 protocol-146 endpoint. There is no connection per request,
application retry, dropped failed sample, or fallback between transports.

Build the two binaries on a machine with Go:

```sh
GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/machoma-bench-linux ./cmd/homa-bench
GOWORK=off CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -o /tmp/machoma-bench-darwin ./cmd/homa-bench
```

Copy the Darwin binary to `/tmp/machoma-bench-darwin` on the Mac through your
normal trusted transfer channel. Stop other Homa services using ports 4000 or 4003
before the run. Raw Homa sockets do not reserve those ports exclusively.

On the Mac, replace `MAC_LAN_IPV4` with its directly reachable LAN address:

```sh
sudo /tmp/machoma-bench-darwin serve -bind MAC_LAN_IPV4 -duration 10m
```

The server listens on native Homa port 4000 and TCP port 14000 on that same IP.
It allows at most 16 TCP connections and exits after 10 minutes, or on a signal.
TCP connections may idle while Homa is being measured; individual frame bodies
and writes still have five-second deadlines. Native Homa is unauthenticated and
unencrypted, as is this TCP comparison. Use the intended test network.

On Linux, replace both host addresses with the addresses on that same route:

```sh
sudo /tmp/machoma-bench-linux client \
  -local LINUX_LAN_IPV4:4003 \
  -peer MAC_LAN_IPV4:4000 \
  -tcp-peer MAC_LAN_IPV4:14000 \
  -scope 'WiFi LAN; Linux client, Mac server' \
  -out /tmp/machoma-wifi-echo.json
```

The default workload is:

| Setting | Default |
|---|---|
| Payload sizes | 64, 1024, 16384, 65536, 1000000 bytes |
| Warmups | 10 verified, unmeasured calls before each measurement |
| Samples per size/transport/round | 1000 below 65536 bytes; 100 otherwise |
| Rounds | 3; Homa/TCP order reverses each round |
| Concurrency | One sequential outstanding call |
| Call timeout | 5 seconds |
| Overall duration limit | 10 minutes |

Use `-sizes`, `-warmup`, `-small-samples`, `-large-samples`, `-rounds`, and
`-timeout` to change these settings. For a quick connectivity check:

```sh
sudo /tmp/machoma-bench-linux client \
  -local LINUX_LAN_IPV4:4003 -peer MAC_LAN_IPV4:4000 \
  -tcp-peer MAC_LAN_IPV4:14000 \
  -sizes 64,65536,1000000 -warmup 2 -small-samples 10 -large-samples 3 -rounds 1
```

Progress goes to stderr. JSON includes every successful measured RTT in
nanoseconds, nearest-rank p50/p95/p99/max RTT, counts, payload bytes, elapsed
time, goodput, round order, client platform, configuration, and Homa counters.
Payload construction and full byte verification are outside the RTT interval.
Reported **bidirectional application-payload goodput** is
`2 × verified payload bytes ÷ measurement wall time`; that wall time includes
validation and per-call setup. It is neither physical link capacity nor an
estimate of wire throughput. Payload sizes exclude both protocols' headers.

Any timeout, network error, or byte mismatch aborts the entire run with a nonzero
exit code. Failed runs do not produce a successful JSON report. `-out` creates
its file exclusively before connecting; a failed run can leave an empty file,
and a new run needs a new filename. No payload contents are logged.

These are measurements of this userspace port, the selected hosts, and the
observed network. A WiFi result includes radio contention and host scheduling;
it does not reproduce the paper's datacenter Ethernet configuration, incast
workload, kernel implementation, or loaded tail-latency result. Three alternating
rounds reduce ordering bias but do not establish statistical confidence. Keep
the raw JSON, repeat runs, and inspect per-round variation before making a speed
claim. In particular, 100 large-message samples give a coarse p99 estimate.

Homa requests IP precedence priorities; TCP uses the operating system's default
QoS settings. A physical network may honor, change, or ignore those markings.
Any observed difference can therefore include QoS treatment and cannot be
attributed solely to transport logic.

The manually triggered `Hosted loopback benchmark` GitHub Actions workflow runs
the full default workload on separate Linux and macOS hosted runners. Each
runner acts as its own client and server. Its artifact includes raw JSON, server
and client logs, the source commit, kernel, CPU model/count, memory, and loopback
MTU. These results measure hosted loopback, not a Mac-to-Linux LAN path or the
physical Mac. Trigger it with:

```sh
gh workflow run benchmark.yml --repo phenomenon0/MacHoma
```
