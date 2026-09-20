# Independent Linux kernel peer

`linux_peer.c` talks to the actual Homa kernel module through
`socket(AF_INET, SOCK_DGRAM, 146)`. It does not implement a second userspace
transport. Use it to test the Mac implementation in both directions against
an independent implementation. This is a correctness probe, not a latency
benchmark or evidence of successful kernel interoperability by itself.

The vendored `vendor/homa.h` is an **unmodified copy** from PlatformLab/HomaModule
commit `d8914b8a57aa48c19a2c8130484961585bcbe53c`:

<https://github.com/PlatformLab/HomaModule/blob/d8914b8a57aa48c19a2c8130484961585bcbe53c/homa.h>

Its original SPDX notice is retained: `BSD-2-Clause OR GPL-2.0+ WITH
Linux-syscall-note`. The new peer source is BSD-2-Clause. The header is a
Linux userspace API header, not a macOS build dependency.

## Build and local checks

Run from the repository root on Linux; ordinary user permissions suffice:

```sh
cc -std=c11 -O2 -Wall -Wextra -Werror -pedantic \
  -o /tmp/homa-linux-peer interop/linux_peer.c
/tmp/homa-linux-peer --self-test
/tmp/homa-linux-peer --help
```

The self-test verifies receive buffer reconstruction, including reordered
pages and an unaligned final fragment, plus bounds rejection. It opens no
socket and does **not** demonstrate kernel interoperability. For memory
instrumentation, compile with `-O1 -g -fsanitize=address,undefined` and run
the same self-test.

## Requirements for the real test

- A compatible HomaModule must already be loaded on Linux. This program
  never installs, loads, unloads, or changes a kernel module or sysctl.
- **`net.homa.hijack_tcp` must be `0` before either Linux peer socket is
  created.** Otherwise Homa replies travel in specially marked TCP frames
  (IP protocol 6), which the native protocol-146 Mac implementation does not
  receive. The peer refuses to start when the readable setting is nonzero.
- For the initial test, use **`net.homa.max_gso_size=1000`**, the pinned
  implementation's default. This limits each output skb to one segment and
  avoids reliance on NIC Homa segmentation or software GSO. The pinned
  upstream INSTALL.md says software GSO is temporarily broken. Do not enable
  TCP hijacking to compensate: that changes the wire path under test.
- The native IPv4 route between machines must carry IP protocol 146 in
  **both directions**. A firewall rule allowing UDP port 4000 is insufficient:
  these are Homa ports, not UDP ports. Prefer a directly reachable LAN for the
  first test; confirm tunnel and firewall protocol support separately.
- Use a common working path MTU. The Mac sender must limit individual IP
  packets to that MTU; Linux uses route MTU. Offloads need not be disabled
  globally when using the conservative one-segment configuration above.
- Bind a Linux service to port `1..32767`. Port zero means keep the kernel's
  automatically allocated client port, normally `32768..65535`. Linux Homa
  `bind` uses **only the port**; the provided IP does not restrict the listener
  or select source routing. The tool prints this limitation for nonzero bind
  addresses.

Read the current settings without changing the host:

```sh
sysctl net.homa.hijack_tcp net.homa.max_gso_size
```

If those prerequisites are unmet, configure the test host separately before
using the peer. The executable does not attempt a fallback to UDP or TCP.

## Mac client to Linux server

On Linux:

```sh
/tmp/homa-linux-peer serve --bind 0.0.0.0:4000 \
  --count 10 --timeout-ms 30000
```

Then direct the Mac native Homa echo client to `LINUX_LAN_IPV4:4000`. This
server accepts arbitrary nonempty request bytes and replies with the exact
same bytes. It prints the peer, server RPC ID, length, and FNV-1a digest for
each request. Match the Mac client's full byte comparison and the request
count against these records.

To constrain the caller to a known Mac Homa endpoint, add
`--remote MAC_LAN_IPV4:MAC_HOMA_PORT`. This checks address and port; it is
**not** cryptographic caller authentication. The tool fails on an unexpected
caller. Add `--bytes 1000000` to require a specific request size.

## Linux client to Mac server

Start the native Mac echo server at `MAC_LAN_IPV4:4000`, then on Linux:

```sh
/tmp/homa-linux-peer client --remote MAC_LAN_IPV4:4000 \
  --bytes 1000000 --count 10 --timeout-ms 5000
```

The client checks the returned peer address and port, RPC ID, completion
cookie, message length, and **every payload byte**. Payload byte `i` is
`((i * 31) ^ (i >> 8) ^ 0xa5) & 0xff`. It does not require the Mac server to
understand this format: echoing the bytes suffices. To choose a fixed client
Homa port for the Mac allowlist, add `--bind 0.0.0.0:4001`.

Repeat both directions with sizes `1`, `100`, `1424`, `65535`, `65536`,
`65537`, `100000`, and `1000000`. The first two fit a single packet; large
sizes exercise scheduling and multiple receive pages. Packet-boundary cases
depend on the route MTU (`1424` is IPv4 payload capacity at MTU 1500).

For a kernel-to-kernel baseline on two Linux hosts, the server can add
`--verify-pattern` to validate the client's deterministic bytes before
echoing them.

## API and evidence notes

- This header uses **direct structs in `msg_control`**, not `cmsghdr`
  ancillary records. As upstream `man/sendmsg.2` requires, `sendmsg` sets
  **`msg_controllen=0` while retaining the non-NULL `msg_control` pointer**;
  otherwise Linux copies it into kernel memory and Homa refuses it.
  `recvmsg` instead requires `msg_controllen=sizeof(homa_recvmsg_args)`.
  `sendmsg` returns **zero on success**, and writes the
  allocated RPC ID into the struct. `recvmsg` returns the received length;
  payload lives in the registered pool, not `msg_iov`.
- The receive pool is 256 MiB of virtual memory by default. `--pool-mib`
  changes it. The kernel requires at least one 64 KiB page per possible CPU
  plus space for two maximum messages. Pool pages are recycled through the
  next `recvmsg`; socket close occurs before unmapping the pool.
- `poll` and nonblocking `recvmsg` enforce application deadlines without
  relying on unsupported Homa socket timeout options. The kernel's own
  shorter liveness timeout can still finish a call sooner.
- A 100 ms end-of-run pause lets the kernel exchange ACK/NEED_ACK before
  socket close. This is not a claim that every response was acknowledged.
- RPC transport is unauthenticated and unencrypted, matching the upstream
  native wire format. Keep the probe on the intended test network.
- Success output means payload correctness for that completed call. Module
  absence, malformed replies, peer mismatches, and deadlines are failures;
  the peer does not quietly skip them or report a passing network test.
