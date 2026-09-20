# Native LAN to kernel-VM relay fixture

This fixture tests a physical Mac's native protocol-146 socket against the
**unmodified Linux Homa kernel module in QEMU**, through an explicit relay:

```text
Mac native Homa -> physical LAN protocol146 -> Linux-host raw socket
  -> IPv4 address rewrite -> QEMU Ethernet backend -> Linux Homa kernel
```

The relay leaves every Homa header and payload byte unchanged. It rewrites the
IP endpoints, rebuilds IPv4/Ethernet headers, and services the guest's ARP.
QEMU's local socket backend uses TCP on loopback. This is **relay-assisted
interoperability**, not a directly bridged VM, bare-metal Linux peer, or latency
benchmark. It does not load a host kernel module or configure TAP, bridges, forwarding,
routes or persistent privileged helpers. The physical network and host firewall
must already permit protocol 146; any temporary firewall rule is configured
separately from this tool.

The exact allowed routes are:

| Physical Mac endpoint | Relay LAN endpoint | Guest endpoint |
|---|---|---|
| client `MAC_IP:4003` | `LINUX_LAN_IP:4000` | server `10.0.0.2:4000` |
| server `MAC_IP:4001` | `LINUX_LAN_IP:4002` | client `10.0.0.2:4002` |

The guest sees the Mac as `10.0.0.1`; the Mac sees the guest as the Linux host's
LAN address. Other peers and port pairs are rejected. Native Homa is
unauthenticated, so this address filter is not cryptographic identity.

Build from the project root:

```sh
GOWORK=off go test -race ./interop/lanrelay
GOWORK=off CGO_ENABLED=0 go build -o /tmp/homa-lanrelay ./interop/lanrelay
```

First start the Mac echo server with its existing authorized raw-socket
privileges:

```sh
/tmp/homa-echo-darwin-arm64 serve -listen 192.0.2.20:4001 -timeout 15s
```

Then run the relay on Linux with root/CAP_NET_RAW. The command below is an
explicit privileged invocation; the program never elevates itself. The QEMU
child runs as the original unprivileged user identified by `PKEXEC_UID` or
`SUDO_UID`. If neither exists, provide `-qemu-uid UID` explicitly.

```sh
pkexec /tmp/homa-lanrelay \
  -local 192.0.2.10 -peer 192.0.2.20 \
  -kernel /boot/vmlinuz-6.17.12-300.fc43.x86_64 \
  -initrd /tmp/homa-qemu/homa-initramfs.cpio.gz \
  -ready-file /tmp/homa-lanrelay.ready \
  -log /tmp/homa-lanrelay-qemu.log \
  -report /tmp/homa-lanrelay-report.json
```

Wait for `RELAY_READY` or the ready file, which is created only after the guest
kernel server binds and registers its receive pool. Then run the Mac client
eight times, once for each size `1,100,1424,65535,65536,65537,100000,1000000`:

```sh
/tmp/homa-echo-darwin-arm64 call -listen 192.0.2.20:4003 \
  -peer 192.0.2.10:4000 -bytes 1000000 -count 1 -timeout 15s
```

The guest simultaneously calls the Mac server at all eight sizes and validates
every returned byte using the independent C kernel peer. The relay exits when
the kernel client sweep passes, the kernel server has received eight requests,
and eight distinct client RPC acknowledgments have returned from the Mac. The
whole QEMU/relay lifetime is limited to three minutes; signals also stop it.
Verify the Mac client's byte-check results separately before claiming success.

The final JSON reports:

- observed traffic-class bytes on incoming physical LAN IPv4 packets;
- observed traffic-class bytes on the guest's Ethernet IPv4 packets;
- requested traffic-class bytes for outgoing LAN writes, separately;
- forwarded/dropped packets and completion evidence.

Requested outgoing ToS is **not** a physical wire capture. The report does not
claim Wi-Fi preserves priority or that switch queues use the markings. No
application payloads or unrelated LAN packets are retained in the logs.
