#!/usr/bin/env python3
"""Summarize one experiment run directory (load.jsonl, metrics.jsonl, audit.json).

    python3 ops/experiment/report.py RUN_DIR [RUN_DIR ...]

Writes RUN_DIR/summary.json and prints a Markdown summary. With several run
directories it also prints one comparison table, one row per run: that is the
scaling table (Q1-Q5, Q9 of docs/benchmarks/p2p-experiment.md).
"""
import json, math, sys
from pathlib import Path


def lines(path):
    if not path.exists():
        return []
    out = []
    for raw in path.read_text().splitlines():
        try:
            out.append(json.loads(raw))
        except json.JSONDecodeError:
            pass
    return out


def quantile(hist, q):
    """hist[i] counts [2^(i-1), 2^i) µs; returns the bucket's upper bound in ms."""
    total = sum(hist)
    if total == 0:
        return None
    seen = 0
    for i, n in enumerate(hist):
        seen += n
        if seen >= q * total:
            return (2 ** i) / 1000
    return (2 ** (len(hist) - 1)) / 1000


def add(a, b):
    out = list(a) + [0] * max(0, len(b) - len(a))
    for i, v in enumerate(b):
        out[i] += v
    return out


def summarize(run):
    run = Path(run)
    load = lines(run / "load.jsonl")
    samples = lines(run / "metrics.jsonl")
    audit = json.loads((run / "audit.json").read_text()) if (run / "audit.json").exists() else {}
    final = json.loads((run / "load-summary.json").read_text()) if (run / "load-summary.json").exists() else {}
    s = {"run": run.name}
    if final:
        s["load_final"] = {k: final.get(k) for k in ("offered", "acked", "shed", "exhausted", "queued_unfinished", "drain_completed")}

    # Client side.
    if load or final:
        hist, intended = [], []
        for l in load:
            hist = add(hist, l.get("latency_us_log2", []))
            intended = add(intended, l.get("intended_latency_us_log2", []))
        hist = add(hist, (final.get("latency_us_log2_remaining") or []))
        intended = add(intended, (final.get("intended_latency_us_log2_remaining") or []))
        active = [l for l in load if l["offered"] > 0]
        secs = max(1, len(active))
        acked = sum(l["acked"] for l in load)
        s["client"] = {
            "seconds": secs,
            "offered": sum(l["offered"] for l in load),
            "acked": acked,
            "shed": sum(l["shed"] for l in load),
            "acked_per_s": round(acked / secs, 1),
            "peak_acked_per_s": max((l["acked"] for l in load), default=0),
            "failed": {k: sum(l.get("failed", {}).get(k, 0) for l in load) for k in {k for l in load for k in l.get("failed", {})}},
            "p50_ms": quantile(hist, 0.5), "p95_ms": quantile(hist, 0.95), "p99_ms": quantile(hist, 0.99),
            "latency_samples": sum(hist), "intended_samples": sum(intended),
            "latency_covers_final_acks": (sum(hist) == final.get("acked") and sum(intended) == final.get("acked")) if final else None,
            "intended_p50_ms": quantile(intended, 0.5),
            "intended_p95_ms": quantile(intended, 0.95),
            "intended_p99_ms": quantile(intended, 0.99),
        }
        reads = sum(l.get("reads", 0) for l in load)
        totals = sum(l.get("totals", 0) for l in load)
        if reads or totals or final.get("read_latency_us_log2_remaining") or final.get("total_latency_us_log2_remaining"):
            rh, th = [], []
            for l in load:
                rh = add(rh, l.get("read_latency_us_log2", []))
                th = add(th, l.get("total_latency_us_log2", []))
            rh = add(rh, (final.get("read_latency_us_log2_remaining") or []))
            th = add(th, (final.get("total_latency_us_log2_remaining") or []))
            exact = sum(l.get("totals_exact", 0) for l in load)
            deficit = sum(l.get("totals_deficit", 0) for l in load)
            s["readers"] = {
                "reads": reads, "reads_per_s": round(reads / secs, 1),
                "miss": sum(l.get("read_miss", 0) for l in load),
                "failed": sum(l.get("read_failed", 0) for l in load),
                "oversized": sum(l.get("read_oversized", 0) for l in load),
                "read_p50_ms": quantile(rh, 0.5), "read_p95_ms": quantile(rh, 0.95), "read_p99_ms": quantile(rh, 0.99),
                "totals": totals, "totals_exact_share": round(exact / totals, 3) if totals else None,
                "mean_deficit_when_behind": round(deficit / max(1, totals - exact), 1),
                "total_p50_ms": quantile(th, 0.5), "total_p95_ms": quantile(th, 0.95), "total_p99_ms": quantile(th, 0.99),
            }

    # Node side: last good sample per node, and rates between first and last.
    by_node = {}
    for x in samples:
        if "metrics" in x and x["metrics"]:
            by_node.setdefault(x["monitor"], []).append(x)
    nodes = len(by_node)
    s["nodes"] = nodes
    if nodes:
        first = {n: v[0] for n, v in by_node.items()}
        last = {n: v[-1] for n, v in by_node.items()}
        span = max(1e-9, max(last[n]["t"] - first[n]["t"] for n in by_node))

        def wire_total(side, kinds=None, field="request_bytes"):
            total = 0
            for n in by_node:
                for k, w in last[n]["metrics"].get(side, {}).items():
                    if kinds is None or k in kinds:
                        previous = first[n]["metrics"].get(side, {}).get(k, {})
                        before = previous.get(field, previous.get("knowledge_bytes", 0) if field == "membership_bytes" else 0)
                        total += w.get(field, w.get("knowledge_bytes", 0) if field == "membership_bytes" else 0) - before
            return total

        sent_bytes = wire_total("mesh_sent") + wire_total("mesh_sent", field="response_bytes")
        control_kinds = {"membership", "exchange"}
        gossip_bytes = wire_total("mesh_sent", control_kinds) + wire_total("mesh_sent", control_kinds, "response_bytes")
        membership_bytes = wire_total("mesh_sent", field="membership_bytes")
        gossip_calls = wire_total("mesh_sent", control_kinds, "calls")
        kinds = {}
        for n in by_node:
            for k, w in last[n]["metrics"].get("mesh_sent", {}).items():
                b = first[n]["metrics"].get("mesh_sent", {}).get(k, {})
                d = kinds.setdefault(k, {"calls": 0, "bytes": 0, "membership_bytes": 0, "errors": 0})
                d["calls"] += w["calls"] - b.get("calls", 0)
                d["bytes"] += w["request_bytes"] + w["response_bytes"] - b.get("request_bytes", 0) - b.get("response_bytes", 0)
                d["membership_bytes"] += w.get("membership_bytes", w.get("knowledge_bytes", 0)) - b.get("membership_bytes", b.get("knowledge_bytes", 0))
                d["errors"] += w["errors"] - b.get("errors", 0)
        items = [last[n]["metrics"].get("storage", {}).get("items", 0) for n in by_node]
        owned = [last[n]["metrics"].get("storage", {}).get("owned_bytes", 0) for n in by_node]
        received = [last[n]["metrics"]["writes"]["received"] - first[n]["metrics"]["writes"]["received"] for n in by_node]
        relayed = sum(last[n]["metrics"]["writes"]["relayed"] - first[n]["metrics"]["writes"]["relayed"] for n in by_node)
        hops = [0] * 8
        for n in by_node:
            for i, v in enumerate(last[n]["metrics"]["writes"]["hops"]):
                hops[i] += v - first[n]["metrics"]["writes"]["hops"][i]
        applied = sum(hops)
        cpu = [(last[n]["metrics"]["process"]["cpu_seconds"] - first[n]["metrics"]["process"]["cpu_seconds"]) / span for n in by_node]
        partners = sorted(last[n]["metrics"]["gossip"]["partners"] for n in by_node)
        kb = sorted(last[n]["metrics"]["gossip"].get("membership_bytes", last[n]["metrics"]["gossip"].get("knowledge_bytes", 0)) for n in by_node)
        control_names = {"membership" if "membership_bytes" in last[n]["metrics"]["gossip"] else "historical_knowledge" for n in by_node}
        # Convergence: when did every node last agree on one membership version?
        by_t = {}
        for x in samples:
            if x.get("metrics") and x["metrics"]["gossip"].get("version"):
                by_t.setdefault(round(x["t"]), set()).add(x["metrics"]["gossip"]["version"])
        disagree = [t for t, v in by_t.items() if len(v) > 1]
        mean = lambda v: sum(v) / len(v) if v else 0
        intervals = {}
        for key in ("cutover_call_us_log2", "cutover_failed_us_log2", "freeze_return_to_install_ack_us_log2"):
            pooled = []
            for node in by_node:
                end = last[node]["metrics"].get("handoffs", {}).get(key, [])
                start = first[node]["metrics"].get("handoffs", {}).get(key, [])
                delta = [max(0, value - (start[i] if i < len(start) else 0)) for i, value in enumerate(end)]
                pooled = add(pooled, delta)
            intervals[key.removesuffix("_us_log2")] = dict(samples=sum(pooled),
                p50_ms=quantile(pooled, .5), p95_ms=quantile(pooled, .95), p99_ms=quantile(pooled, .99))
        s["handoff_intervals"] = intervals
        s["network"] = {
            "observed_seconds": round(span, 1),
            "control_metadata_kind": sorted(control_names),
            "control_bytes_scope": "whole RPCs tagged as carrying control metadata; historical piggyback RPCs may also contain data",
            "open_transfers_at_end": sum(last[n]["metrics"].get("handoffs", {}).get("open", {}).get("count", 0) for n in by_node),
            "oldest_transfer_seconds_at_end": max(last[n]["metrics"].get("handoffs", {}).get("open", {}).get("oldest_seconds", 0) for n in by_node),
            "mesh_bytes_per_node_per_s": round(sent_bytes / nodes / span),
            "gossip_calls_per_node_per_s": round(gossip_calls / nodes / span, 2),
            "gossip_bytes_per_node_per_s": round(gossip_bytes / nodes / span),
            "membership_share_of_mesh_bytes": round(membership_bytes / sent_bytes, 3) if sent_bytes else 0,
            "gossip_partners_median": partners[len(partners) // 2],
            "membership_bytes_median": kb[len(kb) // 2],
            "versions_at_end": len({last[n]["metrics"]["gossip"].get("version") for n in by_node if last[n]["metrics"]["gossip"].get("version")}) or None,
            "seconds_with_disagreement": len(disagree) if by_t else None,
            "relayed_share": round(sum(hops[1:]) / applied, 3) if applied else 0,
            "forward_attempts_per_success": round(relayed / applied, 3) if applied else 0,
            "hops": hops[: max(i for i, v in enumerate(hops) if v) + 1] if applied else [],
            "items_max_over_mean": round(max(items) / mean(items), 2) if mean(items) else 0,
            "owned_bytes_max_over_mean": round(max(owned) / mean(owned), 2) if mean(owned) else 0,
            "writes_received_max_over_mean": round(max(received) / mean(received), 2) if mean(received) else 0,
            "cpu_cores_per_node_mean": round(mean(cpu), 3),
            "cpu_cores_per_node_max": round(max(cpu), 3),
            "by_kind": dict(sorted(kinds.items(), key=lambda kv: -kv[1]["bytes"])),
        }
    # Timeline: the network's dynamics, 20 s per row (scale up, down, stalls).
    if samples:
        rows = {}
        for x in samples:
            b = int(x["t"] // 20) * 20
            r = rows.setdefault(b, {"alive": set(), "known": [], "folds": {}, "taken": {}, "items": {}})
            m = x.get("metrics")
            if m:
                r["alive"].add(x["monitor"])
                r["known"].append(m["gossip"]["known_vnodes"])
                r["folds"][x["monitor"]] = m["handoffs"]["folds"]
                r["taken"][x["monitor"]] = m["handoffs"]["taken"]
                r["items"][x["monitor"]] = m["storage"]["items"]
        loads = {}
        for l in load:
            b = int(l["t"] // 20) * 20
            d = loads.setdefault(b, {"offered": 0, "acked": 0, "shed": 0, "hist": []})
            d["offered"] += l["offered"]; d["acked"] += l["acked"]; d["shed"] += l["shed"]
            d["hist"] = add(d["hist"], l.get("latency_us_log2", []))
        timeline = []
        for b in sorted(rows):
            r, d = rows[b], loads.get(b, {"offered": 0, "acked": 0, "shed": 0, "hist": []})
            known = sorted(r["known"])
            items = list(r["items"].values())
            timeline.append({"t": b, "alive": len(r["alive"]), "known_vnodes": known[len(known) // 2] if known else 0,
                             "folds": sum(r["folds"].values()), "installs": sum(r["taken"].values()),
                             "offered_per_s": round(d["offered"] / 20), "acked_per_s": round(d["acked"] / 20),
                             "shed_per_s": round(d["shed"] / 20), "p99_ms": quantile(d["hist"], 0.99),
                             "items_max_over_mean": round(max(items) / (sum(items) / len(items)), 2) if items and sum(items) else 0})
        s["timeline"] = timeline
    if audit:
        s["audit"] = {k: audit.get(k) for k in ("pass", "acked_keys", "checked_keys", "missing", "double_owned_zones", "read_errors", "items_held")}
    (run / "summary.json").write_text(json.dumps(s, indent=2))
    return s


def show(s):
    print(f"## {s['run']}  ({s.get('nodes', 0)} nodes)\n")
    c, n, a = s.get("client", {}), s.get("network", {}), s.get("audit", {})
    if c:
        print(f"- client: {c['acked']}/{c['offered']} acked, {c['acked_per_s']}/s (peak {c['peak_acked_per_s']}), shed {c['shed']}, "
              f"p50 {c['p50_ms']} ms, p95 {c['p95_ms']} ms, p99 {c['p99_ms']} ms, failed {c['failed']}")
        print(f"- intended arrival: p50 {c['intended_p50_ms']} ms, p95 {c['intended_p95_ms']} ms, p99 {c['intended_p99_ms']} ms; "
              f"{c['intended_samples']} samples (histogram upper bounds)")
    if s.get("load_final"):
        f = s["load_final"]
        print(f"- final after drain: {f['acked']}/{f['offered']} ACK, exhausted {f['exhausted']}, queued {f['queued_unfinished']}, drained {f['drain_completed']}; full latency coverage {c.get('latency_covers_final_acks')}")
    r = s.get("readers")
    if r:
        print(f"- readers: {r['reads']} point reads ({r['reads_per_s']}/s), miss {r['miss']}, failed {r['failed']}, oversized {r['oversized']}, "
              f"p50 {r['read_p50_ms']} ms, p95 {r['read_p95_ms']} ms, p99 {r['read_p99_ms']} ms; totals {r['totals']}, exact {r['totals_exact_share']}, "
              f"mean deficit when behind {r['mean_deficit_when_behind']}, p50 {r['total_p50_ms']} ms, p95 {r['total_p95_ms']} ms, p99 {r['total_p99_ms']} ms")
    if n:
        print(f"- mesh: {n['mesh_bytes_per_node_per_s']} B/s per node, membership {n['membership_share_of_mesh_bytes']*100:.1f}% of it; "
              f"gossip {n['gossip_calls_per_node_per_s']} calls/s, {n['gossip_bytes_per_node_per_s']} B/s per node, partners {n['gossip_partners_median']}")
        print(f"- membership {n['membership_bytes_median']} B, versions at end {n['versions_at_end']}, seconds in disagreement {n['seconds_with_disagreement']}")
        print(f"- routing: successful writes with relay {n['relayed_share']*100:.1f}%, forward attempts/success {n['forward_attempts_per_success']}, hops {n['hops']}")
        print(f"- balance max/mean: items {n['items_max_over_mean']}, owned bytes {n['owned_bytes_max_over_mean']}, writes received {n['writes_received_max_over_mean']}")
        print(f"- cpu per node: mean {n['cpu_cores_per_node_mean']} cores, max {n['cpu_cores_per_node_max']}")
        top = list(n["by_kind"].items())[:6]
        print("- bytes by kind: " + ", ".join(f"{k} {v['bytes']//1024} KiB/{v['calls']} calls" for k, v in top))
    tl = s.get("timeline")
    if tl and len(tl) > 2:
        print("\n| t (s) | alive | vnodes | installs | folds | offered/s | acked/s | shed/s | p99 ms | items max/mean |")
        print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
        for r in tl:
            print(f"| {r['t']} | {r['alive']} | {r['known_vnodes']} | {r['installs']} | {r['folds']} | {r['offered_per_s']} | {r['acked_per_s']} | {r['shed_per_s']} | {r['p99_ms']} | {r['items_max_over_mean']} |")
        print()
    if a:
        print(f"- audit: {'PASS' if a['pass'] else 'FAIL'} — {a['checked_keys']}/{a['acked_keys']} read back, missing {a['missing']}, "
              f"double owners {a['double_owned_zones']}, held {a['items_held']}")
    print()


def main():
    runs = [summarize(r) for r in sys.argv[1:]]
    for s in runs:
        show(s)
    if len(runs) > 1:
        print("| run | nodes | acked/s | p99 ms | mesh B/s/node | gossip B/s/node | partners | relayed | items max/mean | audit |")
        print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
        for s in runs:
            c, n, a = s.get("client", {}), s.get("network", {}), s.get("audit", {})
            print(f"| {s['run']} | {s.get('nodes')} | {c.get('acked_per_s')} | {c.get('p99_ms')} | {n.get('mesh_bytes_per_node_per_s')} | "
                  f"{n.get('gossip_bytes_per_node_per_s')} | {n.get('gossip_partners_median')} | {n.get('relayed_share')} | "
                  f"{n.get('items_max_over_mean')} | {'PASS' if a.get('pass') else ('FAIL' if a else '-')} |")


if __name__ == "__main__":
    main()
