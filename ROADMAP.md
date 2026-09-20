# MacHoma roadmap

MacHoma currently demonstrates native Homa packet compatibility and bounded
request/reply operation. It is an experimental transport with a pinned wire ABI.

## Release gates

- Direct bare-metal Linux kernel peer on wired Ethernet; relay-assisted physical
  Wi-Fi interoperability, MTU and packet priority checks have passed.
- Extend the published sequential TCP baseline to loaded mixed-message tests,
  isolated listeners and QUIC comparisons
  on the same hardware; separate Wi-Fi, wired LAN and datacenter results.
- Pacing, receiver scheduling and congestion behavior under competing traffic.
- Broader Linux fault interoperability: peer restarts, stale state, loss bursts,
  independent implementations' timeout interactions and state cleanup.
- Resource-exhaustion and deployment review for the intended network boundary.

## Integration

- Add an opt-in transport adapter after workload measurements demonstrate value.
- Preserve application authorization and idempotency across retries and peer loss.
- Retain the existing transport as an explicit fallback; native protocol 146 may
  be blocked by routes, firewalls or tunnels.
- Treat IPv6, Linux TCP-hijacking mode, NIC offloads and optimized buffer handling
  as separate compatibility and performance projects.

Historical priority is unverified. A search that finds no earlier macOS port is
not proof that no private or unindexed implementation exists.
