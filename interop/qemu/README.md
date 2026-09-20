# Isolated QEMU kernel interoperability fixture

This initramfs loads the real pinned Homa kernel module **inside a disposable
VM**. Building it requires no host root access. It does not load a host module,
create a host device node, change host networking, or alter host sysctls.

Build after compiling HomaModule for a readable installed kernel:

```sh
python3 interop/qemu/build_initramfs.py \
  --module /tmp/homa-port-review/HomaModule/PlatformLab-HomaModule-d8914b8/homa.ko \
  --kernel-release 6.17.12-300.fc43.x86_64 \
  --out-dir /tmp/homa-qemu
```

The builder compiles the PID 1 init and independent C kernel peer, copies their
dynamic libraries, copies the matching `e1000` module, verifies module kernel
versions/dependencies, and writes a gzip/newc initramfs plus SHA-256 manifest.
It emits `dev/console` in the archive without calling `mknod` on the host.
Only execute `guest_init.c` as `/init` inside the VM; it rejects ordinary host
process execution and requires an explicit kernel command-line marker.

Example QEMU command (the host Ethernet test harness must connect to the socket
backend and handle Ethernet/ARP plus native protocol 146):

```sh
qemu-system-x86_64 -machine accel=tcg -m 1024 -smp 2 \
  -kernel /boot/vmlinuz-6.17.12-300.fc43.x86_64 \
  -initrd /tmp/homa-qemu/homa-initramfs.cpio.gz \
  -append 'console=ttyS0 rdinit=/init panic=1 homa.test_guest=1 homa.mode=serve homa.count=4' \
  -display none -serial stdio -monitor none -no-reboot \
  -netdev socket,id=homanet,listen=127.0.0.1:18080 \
  -device e1000,netdev=homanet,mac=52:54:00:12:34:56
```

The guest configures `eth0=10.0.0.2/24`, MTU 1500, and enables loopback. Only
inside the guest, it sets `hijack_tcp=0`, `max_gso_size=1000` (one segment),
`unsched_bytes=14000`, and `timeout_ticks=2000` (additional TCG tolerance).

Kernel command-line modes:

- `homa.mode=serve`: native Linux echo server on Homa port 4000;
  `homa.count=N` requests, default 1. Use count 0 for a continuous server
  (120-second idle deadline).
- `homa.mode=client`: native Linux client targets the host harness at
  `10.0.0.1:4001`, using local Homa port 4002. `homa.bytes=N` selects message
  length, default 100000; `homa.count=N`, default 1.
- `homa.mode=both`: starts a continuous server and the client. The server
  remains until its idle deadline or VM termination; use separate serve/client
  boots for finite automated runs.
- `homa.mode=interop`: starts the continuous server and an independent kernel
  client sweep across sizes `1,100,1424,65535,65536,65537,100000,1000000` against
  `10.0.0.1:4001`. It prints `HOMA_GUEST_CLIENT_PASS` only after all eight clients
  validate their complete payloads and exit successfully. This mode is intended
  for the host `interop/vmcheck` harness, which also calls the guest server and
  terminates the VM after validating both directions.

The guest prints `HOMA_GUEST_READY` after its server has bound and registered
receive buffers, the peer's per-RPC payload checks,
`HOMA_GUEST_CHILD`, and finally `HOMA_GUEST_DONE status=PASS|FAIL`, then powers
off. QEMU process exit status alone is not a successful test: verify the
guest's final marker and expected per-call records. A boot failure or timeout
without the PASS marker fails the run. Guest readiness does not establish
interoperability until the native kernel calls complete with correct bytes.
