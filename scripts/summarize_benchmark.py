#!/usr/bin/env python3
"""Summarize successful homa-bench JSON; pool raw samples, never average p99s."""
import json
import math
import sys
from pathlib import Path


def summarize(path):
    report = json.loads(Path(path).read_text())
    if report["status"] != "passed":
        raise ValueError("benchmark did not pass")
    groups = {}
    for m in report["measurements"]:
        samples = m["rtt_samples_ns"]
        if not samples or len(samples) != m["verified_calls"] or min(samples) <= 0:
            raise ValueError("invalid sample counts or durations")
        groups.setdefault((m["payload_bytes"], m["transport"]), []).append(m)
    rows = []
    for (size, transport), measurements in sorted(groups.items()):
        samples = sorted(x for m in measurements for x in m["rtt_samples_ns"])
        elapsed = sum(m["elapsed_seconds_including_validation"] for m in measurements)
        payload = sum(m["verified_bidirectional_payload_bytes"] for m in measurements)
        row = {"payload_bytes": size, "transport": transport, "samples": len(samples)}
        for label, quantile in [("p50_us", .5), ("p95_us", .95), ("p99_us", .99)]:
            row[label] = samples[math.ceil(len(samples) * quantile) - 1] / 1000
        row["bidirectional_payload_goodput_mbps"] = payload * 8 / elapsed / 1e6
        row["per_round_p50_us"] = [m["p50_rtt_us"] for m in measurements]
        if transport == "homa":
            row["client_retransmissions"] = sum(
                m["homa_stats_after"]["Retransmissions"]
                - m["homa_stats_before"]["Retransmissions"] for m in measurements
            )
        rows.append(row)
    return {"source": str(path), "platform": report["client_platform"], "rows": rows}


if __name__ == "__main__":
    if len(sys.argv) < 2:
        raise SystemExit("usage: summarize_benchmark.py raw.json [raw.json ...]")
    print(json.dumps([summarize(path) for path in sys.argv[1:]], indent=2))
