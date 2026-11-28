#!/usr/bin/env python3
"""Follow a campaign live from counters only (no logs).

  live.py agg DIR OUT.jsonl [NAME]   on the load side, during a phase: every
      5 s append one compact line (offered, acked, shed, p99, vnodes, open
      transfers, busy answers, items) read from DIR/load.jsonl and the tail
      of DIR/metrics.jsonl. Stops when DIR/audit.json appears.
  live.py html LIVE.jsonl OUT.html   one self-contained page of curves that
      reloads itself every 5 s. Open it in a browser and leave it.
  live.py machines AGENTS_FILE DIR  observe machine facts/slots separately.
  live.py line LIVE.jsonl            the last point as one line of text.
  live.py replay DIR OUT.jsonl       the same points from a finished run.
  live.py compare OUT.html LABEL=LIVE.jsonl ...   up to four runs overlaid,
      to see what a change did to each curve.
"""
import json, os, sys, time, html
from pathlib import Path
from fleet import Fleet, collect


def percentile_ms(hist, q):
    total = sum(hist)
    if not total:
        return None
    seen = 0
    for i, n in enumerate(hist):
        seen += n
        if seen >= q * total:
            return (1 << i) / 1000.0  # bucket i ends at 2^i microseconds
    return None


def p99_ms(hist):
    return percentile_ms(hist, .99)


def latest_metrics(path, span=4 << 20, elapsed=None):
    """The last sample of every monitor, from the end of metrics.jsonl."""
    try:
        size = os.path.getsize(path)
        with open(path, "rb") as f:
            f.seek(max(0, size - span))
            lines = f.read().split(b"\n")
            if size > span:
                lines = lines[1:]
    except OSError:
        return {}
    last, newest = {}, 0.0
    for raw in lines:
        try:
            r = json.loads(raw)
        except ValueError:
            continue
        if "metrics" in r:
            m = dict(r["metrics"] or {}, took_ms=r.get("took_ms", 0))
            last[r["monitor"]] = (r["t"], m)
            newest = max(newest, r["t"])
    return {m: v for m, (t, v) in last.items() if t >= (newest if elapsed is None else elapsed) - 10}


def network(ms, prev, at):
    """Cumulative counters of every node, and their rates since prev."""
    now = {"at": at, "bytes": 0, "relayed": 0, "received": 0, "applied": 0, "relayed_successes": 0, "gossip": 0, "value_ms": 0, "push_ms": 0}
    for m in ms.values():
        for kind, c in (m.get("mesh_sent") or {}).items():
            now["bytes"] += c["request_bytes"] + c["response_bytes"]
            if kind in {"exchange", "membership"}:
                now["gossip"] += c["request_bytes"] + c["response_bytes"]
        now["relayed"] += m["writes"]["relayed"]
        now["received"] += m["writes"]["received"]
        hops = m["writes"].get("hops") or []
        now["applied"] += sum(hops)
        now["relayed_successes"] += sum(hops[1:])
        summary = m.get("summary") or {}
        now["value_ms"] += summary.get("value_ms", 0)
        now["push_ms"] += summary.get("push_ms", 0)
    out = {"pending_k": round(max((m.get("summary") or {}).get("pending_edges", 0) for m in ms.values()) / 1000, 1),
           "flush_s": round(max((m.get("summary") or {}).get("flush_last_ms", 0) for m in ms.values()) / 1000, 1),
           "versions": len({m["gossip"]["version"] for m in ms.values() if m["gossip"].get("version")}) or None,
           "membership_kb": round(max(m["gossip"]["membership_bytes"] for m in ms.values()) / 1024, 1)}
    dt = now["at"] - prev["at"] if prev else 0
    if dt > 0 and now["bytes"] >= prev["bytes"]:  # a restarted node resets its counters
        n = max(len(ms), 1)
        out["mesh_kb_s"] = round((now["bytes"] - prev["bytes"]) / dt / n / 1024, 1)
        out["gossip_kb_s"] = round((now["gossip"] - prev["gossip"]) / dt / n / 1024, 2)
        got = now["received"] - prev["received"]
        out["forward_attempts_per_entry"] = round((now["relayed"] - prev["relayed"]) / got, 3) if got > 0 else None
        successes = now["applied"] - prev.get("applied", now["applied"])
        forwarded = now["relayed_successes"] - prev.get("relayed_successes", now["relayed_successes"])
        out["relayed_pct"] = round(100 * forwarded / successes, 1) if successes > 0 and 0 <= forwarded <= successes else None
        # Seconds per second, summed over the nodes: how busy the summary
        # flushes are valuing edges and pushing them.
        if "value_ms" in prev:
            out["value_load"] = round((now["value_ms"] - prev["value_ms"]) / 1000 / dt, 2)
            out["push_load"] = round((now["push_ms"] - prev["push_ms"]) / 1000 / dt, 2)
    return out, now


def build(point, rows, ms, prev):
    """Fill one point from the load rows of its window and the nodes' last samples."""
    if rows:
        hist = [0] * 40
        intended = [0] * 40
        fresh = [0] * 40
        read_hist = [0] * 40
        for r in rows:
            for i, count in enumerate(r.get("read_latency_us_log2") or []):
                read_hist[i] += count
            for i, n in enumerate(r.get("latency_us_log2") or []):
                hist[i] += n
            for i, n in enumerate(r.get("intended_latency_us_log2") or []):
                intended[i] += n
            for i, n in enumerate(r.get("fresh_lag_us_log2") or []):
                fresh[i] += n
        n = len(rows)
        for key in ("reads", "read_failed", "read_miss"):
            point[key+"_s" if key != "reads" else "reads_s"] = sum(r.get(key, 0) for r in rows)/n if any(key in r or (key != "reads" and "reads" in r) for r in rows) else None
        point["read_p99_ms"] = p99_ms(read_hist)
        point.update(t=int(rows[-1]["t"]), offered=round(sum(r["offered"] for r in rows) / n),
                     acked=round(sum(r["acked"] for r in rows) / n), shed=round(sum(r["shed"] for r in rows) / n),
                     p50_ms=percentile_ms(hist, .5), p95_ms=percentile_ms(hist, .95), p99_ms=p99_ms(hist),
                     intended_p50_ms=percentile_ms(intended, .5), intended_p95_ms=percentile_ms(intended, .95), intended_p99_ms=p99_ms(intended),
                     in_flight=max(r.get("in_flight", 0) for r in rows),
                     queued=max(r.get("queued", 0) for r in rows), workers=max(r.get("workers", 0) for r in rows),
                     refused_503=sum((r.get("failed") or {}).get("503", 0) for r in rows) / n,
                     # bucket i holds [2^(i-1), 2^i) µs: from 23 on, 4 s or more
                     slow_s=round(sum(hist[23:]) / n),
                     # Freshness probe: acknowledged write -> counted by the root total.
                     fresh_p99_ms=p99_ms(fresh) if sum(fresh) else None,
                     loader_pct=round(100 * max(r.get("cpu_cores", 0) for r in rows) / max(rows[-1].get("machine_cores", 0), 1)))
    if ms:
        items = [m["storage"]["items"] for m in ms.values()]
        mean = sum(items) / len(items)
        point.update(ready_nodes=sum(m.get("client_ready", False) for m in ms.values()) if any("client_ready" in m for m in ms.values()) else None, nodes=len(ms), vnodes=max(m["gossip"]["known_vnodes"] for m in ms.values()),
                     open=sum(m["handoffs"]["open"]["count"] for m in ms.values()),
                     busy=sum(m["handoffs"].get("busy", 0) for m in ms.values()),
                     frozen=sum(len(m.get("frozen_transfers") or []) for m in ms.values()),
                     items=sum(items), skew=round(max(items) / mean, 2) if mean else None,
                     # A node that takes seconds to answer /metrics is stalled.
                     stall_ms=max(m.get("took_ms", 0) for m in ms.values()))
        net, prev = network(ms, prev, point["wall"])
        point.update(net)
    return prev


def agg(directory, out, name=""):
    load, metrics = os.path.join(directory, "load.jsonl"), os.path.join(directory, "metrics.jsonl")
    offset, start, prev = 0, time.time(), None
    fleet = Fleet()
    while True:
        rows = []
        try:
            with open(load) as f:
                f.seek(offset)
                for l in f:
                    if not l.endswith("\n"):
                        break  # a line still being written
                    offset += len(l.encode())
                    if l.startswith('{"t"'):
                        rows.append(json.loads(l))
        except OSError:
            pass
        point = {"phase": name, "wall": int(time.time()), "t": int(time.time() - start)}
        samples = latest_metrics(metrics, elapsed=time.time() - start)
        point["metrics_stale"], point["load_stale"] = not bool(samples), not bool(rows)
        prev = build(point, rows, samples, prev)
        point["fleet"] = fleet.frame(samples, latest_machines(os.path.join(directory, "machines.jsonl")), point["wall"])
        done = os.path.exists(os.path.join(directory, "audit.json"))
        if done:
            audit(point, directory)
        if len(point) > 3:
            with open(out, "a") as f:
                f.write(json.dumps(point, separators=(",", ":")) + "\n")
        if done:
            return
        time.sleep(5)


def audit(point, directory):
    try:
        a = json.load(open(os.path.join(directory, "audit.json")))
        point.update(audit="pass" if a.get("pass") else "fail", missing=a.get("missing"),
                     double=a.get("double_owned_zones"), read_errors=a.get("read_errors"))
    except (OSError, ValueError):
        pass


def replay(directory, out, name=""):
    """The points agg would have written, from a finished run's files."""
    rows, samples, origin = {}, {}, None
    for l in open(os.path.join(directory, "load.jsonl")):
        if l.startswith('{"t"'):
            r = json.loads(l)
            rows.setdefault(int(r["t"]) // 5 * 5, []).append(r)
    try:
        for l in open(os.path.join(directory, "metrics.jsonl")):
            try:
                r = json.loads(l)
            except ValueError:
                continue
            if "wall" in r:
                offset = r["wall"]-r.get("t", 0)
                origin = offset if origin is None else min(origin, offset)
            if "metrics" in r and r["metrics"]:
                samples.setdefault(int(r["t"]) // 5 * 5, {})[r["monitor"]] = r["metrics"]
    except OSError:
        pass
    prev, points = None, []
    fleet = Fleet()
    machine_rows = read(os.path.join(directory, "machines.jsonl"))
    origin = origin or 0
    for t in sorted(set(rows) | set(samples)):
        point = {"phase": name, "wall": origin+t, "t": t, "metrics_stale": not bool(samples.get(t)), "load_stale": not bool(rows.get(t))}
        prev = build(point, rows.get(t), samples.get(t), prev)
        point["fleet"] = fleet.frame(samples.get(t) or {}, [r for r in machine_rows if r.get("wall", 0) <= point["wall"]], point["wall"])
        points.append(point)
    if points:
        audit(points[-1], directory)
    with open(out, "w") as f:
        for point in points:
            f.write(json.dumps(point, separators=(",", ":")) + "\n")


def read(path):
    out = []
    try:
        for l in open(path):
            try:
                out.append(json.loads(l))
            except ValueError:
                pass
    except OSError:
        pass
    return out


W, H, L, R, T, B = 560, 190, 52, 92, 12, 24


def chart(points, title, unit, series, index):
    xs = [p["x"] for p in points]
    vals = [p[k] for k, _, _ in series for p in points if p.get(k) is not None]
    if not xs or not vals:
        return f'<figure><figcaption>{html.escape(title)}</figcaption><p class="empty">pas encore de mesure</p></figure>'
    x0, x1, top = min(xs), max(max(xs), min(xs) + 1), max(max(vals) * 1.1, 1e-9)
    sx = lambda x: L + (x - x0) / (x1 - x0) * (W - L - R)
    sy = lambda v: T + (1 - v / top) * (H - T - B)
    fmt = lambda v: f"{v:,.0f}".replace(",", " ") if v >= 100 else f"{v:.3g}"
    g = [f'<svg viewBox="0 0 {W} {H}" data-chart="{index}" role="img" aria-label="{html.escape(title)}">']
    for frac in (0, 0.5, 1):
        y = sy(top * frac)
        g.append(f'<line class="grid" x1="{L}" x2="{W - R}" y1="{y:.1f}" y2="{y:.1f}"/><text class="tick" x="{L - 6}" y="{y + 4:.1f}" text-anchor="end">{fmt(top * frac)}</text>')
    for x in (x0, x1):
        g.append(f'<text class="tick" x="{sx(x):.1f}" y="{H - 6}" text-anchor="middle">{int(x)} s</text>')
    for k, label, cls in series:
        pts = [(sx(p["x"]), sy(p[k])) for p in points if p.get(k) is not None]
        if not pts:
            continue
        g.append(f'<polyline class="{cls}" points="{" ".join(f"{x:.1f},{y:.1f}" for x, y in pts)}"/>')
        g.append(f'<circle class="{cls} dot" cx="{pts[-1][0]:.1f}" cy="{pts[-1][1]:.1f}" r="4"/>')
    ends = sorted(((sy(next(p[k] for p in reversed(points) if p.get(k) is not None)), label) for k, label, _ in series
                   if any(p.get(k) is not None for p in points)))
    if len(series) > 2:
        ends = []  # the legend names them; more labels would collide
    for i, (y, label) in enumerate(ends):  # direct labels, kept 13 px apart
        if i and y - ends[i - 1][0] < 13:
            y = ends[i - 1][0] + 13
            ends[i] = (y, label)
        g.append(f'<text class="label" x="{W - R + 8}" y="{y + 4:.1f}">{html.escape(label)}</text>')
    g.append(f'<line class="cross" y1="{T}" y2="{H - B}" x1="-9" x2="-9"/><rect class="hit" x="{L}" y="{T}" width="{W - L - R}" height="{H - T - B}"/></svg>')
    legend = "".join(f'<span><i class="{cls}"></i>{html.escape(label)}</span>' for _, label, cls in series) if len(series) > 1 else ""
    return f'<figure><figcaption>{html.escape(title)}</figcaption><div class="legend">{legend}</div>{"".join(g)}</figure>'


def compare(runs):
    """One page overlaying up to four runs, each a (label, points) pair, on the
    time of their phase: what a change did to each curve."""
    merged = {}
    for i, (_, pts) in enumerate(runs[:4]):
        for p in pts:
            q = merged.setdefault(p["t"] // 5 * 5, {"x": p["t"] // 5 * 5, "phase": ""})
            q.update({f"{k}#{i}": v for k, v in p.items() if isinstance(v, (int, float))})
    measures = [("acked", "Écritures acquittées par seconde", "/s"), ("offered", "Écritures demandées par seconde", "/s"),
                ("vnodes", "Vnodes actifs", ""), ("p99_ms", "Latence d'écriture p99", " ms"), ("shed", "Arrivées non envoyées : file du générateur pleine", "/s"),
                ("open", "Transferts ouverts", ""), ("skew", "Déséquilibre : max / moyenne (items)", "×"),
                ("mesh_kb_s", "Réseau : trafic mesh par nœud", " Ko/s"), ("relayed_pct", "Réseau : écritures réussies avec relais", " %"),
                ("versions", "Réseau : versions de la membership", "")]
    charts = [(title, unit, [(f"{k}#{i}", label, f"s{i + 1}") for i, (label, _) in enumerate(runs[:4])]) for k, title, unit in measures]
    return page([merged[x] for x in sorted(merged)], charts, "Indexus – comparaison de campagnes",
                "Chaque courbe est une campagne, alignée sur le temps de sa phase. " + " · ".join(l for l, _ in runs[:4]), live=False)


def page(points_raw, charts=None, title="Indexus – campagne en direct", sub=None, live=True):
    if charts is None:
        return fleet_page(points_raw, title, live)
    points, base = [], None
    for p in points_raw:  # one time axis across phases: wall-clock seconds since the first point
        base = base if base is not None else p.get("wall", 0)
        points.append(p if "x" in p else dict(p, x=p.get("wall", 0) - base))
    last = {}
    for p in points:  # the last value of each counter: a point may carry only some
        last.update({k: v for k, v in p.items() if v is not None})
    if points and points[-1].get("metrics_stale"):
        for k in ("vnodes", "open", "versions"):
            last[k] = "périmé"
    if points and points[-1].get("load_stale"):
        for k in ("offered", "acked"):
            last.pop(k, None)
    pct = f'{100 * last["acked"] / last["offered"]:.1f} %' if last.get("offered") else "–"
    audit = next((p for p in reversed(points) if p.get("audit")), None)
    tiles = [("Phase", last.get("phase") or "–"), ("Demandées", f'{last.get("offered", "–")}/s'), ("ACK/s ÷ arrivées/s", pct),
             ("Vnodes", last.get("vnodes", "–")), ("Transferts ouverts", last.get("open", "–")), ("Versions", last.get("versions", "–")),
             ("Dernier audit", "–" if not audit else f'{audit["phase"]} : {"vert" if audit["audit"] == "pass" else "rouge"}, {audit.get("missing")} manquante(s), {audit.get("double")} double(s)')]
    keys = ["x", "phase"] + [k for _, _, series in charts for k, _, _ in series]
    data = [{k: p.get(k) for k in keys} for p in points]
    sub = sub or f'Compteurs seulement, un point toutes les 5 s. La page se recharge seule. Dernier point : {time.strftime("%H:%M:%S", time.localtime(last.get("wall", 0))) if last else "aucun"}.' 
    table = "".join(f'<tr><td>{p["x"]}</td><td>{html.escape(str(p.get("phase", "")))}</td>' + "".join(f'<td>{"" if p.get(k) is None else p[k]}</td>' for k in ("offered", "acked", "shed", "p99_ms", "vnodes", "open", "skew")) + "</tr>" for p in points[-200:])
    # A copied loader may contain only live.py and fleet.py. Its comparison
    # pages keep native scheme colors when the operator UI assets are absent.
    palette = Path(__file__).parent/'dashboard'/'palette.css'
    return f'''<!doctype html><html lang="fr"><head><meta charset="utf-8">{'<meta http-equiv="refresh" content="5">' if live else ""}
<title>{html.escape(title)}</title><style>
{palette.read_text() if palette.is_file() else ''}
:root{{color-scheme:light dark;--surface:var(--ix-page,Canvas);--text:var(--ix-ink,CanvasText);--muted:var(--ix-muted,GrayText);--grid:var(--ix-rule,GrayText);--s1:var(--ix-blue,LinkText);--s2:var(--ix-orange,Chocolate);--s3:var(--ix-green,SeaGreen);--s4:var(--ix-accent,MediumPurple)}}
body{{margin:0;padding:20px;background:var(--surface);color:var(--text);font:14px/1.4 system-ui,sans-serif}}
h1{{font-size:18px;margin:0 0 4px}} .sub{{color:var(--muted);margin:0 0 16px}}
.tiles{{display:flex;flex-wrap:wrap;gap:12px;margin-bottom:16px}} .tile{{border:1px solid var(--grid);border-radius:8px;padding:8px 12px;min-width:120px}}
.tile b{{display:block;font-size:20px;font-weight:600}} .tile span{{color:var(--muted);font-size:12px}}
.grid2{{display:grid;grid-template-columns:repeat(auto-fill,minmax(420px,1fr));gap:16px}}
figure{{margin:0;border:1px solid var(--grid);border-radius:8px;padding:10px 12px;position:relative}} figcaption{{font-weight:600}}
.legend{{min-height:18px;color:var(--muted);font-size:12px}} .legend span{{margin-right:12px}} .legend i{{display:inline-block;width:10px;height:2px;margin-right:5px;vertical-align:middle}}
i.s1{{background:var(--s1)}} i.s2{{background:var(--s2)}} i.s3{{background:var(--s3)}} i.s4{{background:var(--s4)}} svg{{width:100%;height:auto;display:block}}
polyline{{fill:none;stroke-width:2;stroke-linejoin:round}} polyline.s1{{stroke:var(--s1)}} polyline.s2{{stroke:var(--s2)}} polyline.s3{{stroke:var(--s3)}} polyline.s4{{stroke:var(--s4)}}
circle.s1{{fill:var(--s1)}} circle.s2{{fill:var(--s2)}} circle.s3{{fill:var(--s3)}} circle.s4{{fill:var(--s4)}} .dot{{stroke:var(--surface);stroke-width:2}}
.grid{{stroke:var(--grid);stroke-width:1}} .tick{{fill:var(--muted);font-size:11px}} .label{{fill:var(--muted);font-size:12px}}
.cross{{stroke:var(--muted);stroke-width:1;stroke-dasharray:3 3}} .hit{{fill:transparent}} .empty{{color:var(--muted)}}
#tip{{position:fixed;display:none;background:var(--surface);color:var(--text);border:1px solid var(--grid);border-radius:6px;padding:6px 8px;font-size:12px;pointer-events:none;box-shadow:0 2px 8px #0003}}
details{{margin-top:16px}} table{{border-collapse:collapse;font-size:12px}} td,th{{padding:2px 8px;text-align:right;border-bottom:1px solid var(--grid)}}
</style></head><body>
<h1>{html.escape(title)}</h1><p class="sub">{html.escape(sub)}</p>
<div class="tiles">{"".join(f'<div class="tile"><span>{html.escape(k)}</span><b>{html.escape(str(v))}</b></div>' for k, v in tiles) if live else ""}</div>
<div class="grid2">{"".join(chart(points, t, u, s, i) for i, (t, u, s) in enumerate(charts))}</div>
<details{"" if live else " hidden"}><summary>Table des 200 derniers points</summary><table><tr><th>t (s)</th><th>phase</th><th>demandées</th><th>acquittées</th><th>refusées</th><th>p99 ms</th><th>vnodes</th><th>ouverts</th><th>max/moy</th></tr>{table}</table></details>
<div id="tip"></div><script>
const D={json.dumps(data, separators=(",", ":"))},C={json.dumps([[u, [(k, l) for k, l, _ in s]] for _, u, s in charts], ensure_ascii=False)},L={L},R={R},W={W};
const tip=document.getElementById("tip");
document.querySelectorAll("svg[data-chart]").forEach(svg=>{{const [unit,series]=C[+svg.dataset.chart],hit=svg.querySelector(".hit"),cross=svg.querySelector(".cross");
 if(!hit||!D.length)return;const x0=D[0].x,x1=Math.max(D[D.length-1].x,x0+1);
 hit.addEventListener("mousemove",e=>{{const b=svg.getBoundingClientRect(),vx=(e.clientX-b.left)/b.width*W,x=x0+(vx-L)/(W-L-R)*(x1-x0);
  let p=D[0];for(const q of D)if(Math.abs(q.x-x)<Math.abs(p.x-x))p=q;const cx=L+(p.x-x0)/(x1-x0)*(W-L-R);cross.setAttribute("x1",cx);cross.setAttribute("x2",cx);
  tip.innerHTML="<b>"+p.x+" s</b> "+(p.phase||"")+"<br>"+series.map(([k,l])=>l+" : "+(p[k]==null?"–":p[k]+unit)).join("<br>");
  tip.style.display="block";tip.style.left=Math.min(e.clientX+12,innerWidth-180)+"px";tip.style.top=(e.clientY+12)+"px";}});
 hit.addEventListener("mouseleave",()=>{{tip.style.display="none";cross.setAttribute("x1",-9);cross.setAttribute("x2",-9);}});}});
</script></body></html>'''



def latest_machines(path):
    try:
        with open(path, "rb") as f:
            size = f.seek(0, 2)
            f.seek(max(0, size-(2 << 20)))
            lines = f.read().splitlines()
            if size > 2 << 20:
                lines = lines[1:]
    except OSError:
        return []
    last = {}
    for raw in lines:
        try:
            row = json.loads(raw)
            last[row['agent']] = row
        except (ValueError, KeyError):
            continue
    return list(last.values())


def observe_machines(agents_path, directory):
    while not os.path.exists(os.path.join(directory, 'audit.json')):
        try:
            agents = Path(agents_path).read_text().replace(',', ' ').split()
            with open(os.path.join(directory, 'machines.jsonl'), 'a') as f:
                for row in collect(agents):
                    f.write(json.dumps(row, separators=(',', ':'))+'\n')
        except OSError:
            pass
        time.sleep(15)


def fleet_page(points, title, live):
    assets = Path(__file__).parent/'dashboard'
    data = []
    base = points[0].get('wall', 0) if points else 0
    for raw in points:
        p = dict(raw, x=raw.get('wall', 0)-base)
        if p.get('load_stale'):
            for key in ('offered', 'acked', 'p50_ms', 'p95_ms', 'p99_ms', 'reads_s', 'read_failed_s', 'read_p99_ms'):
                p[key] = None
        data.append(p)
    # JSON lives inside a script: escaped terminators protect against node names.
    payload = json.dumps(data, separators=(',', ':'), ensure_ascii=False).replace('<', '\\u003c')
    refresh = 'setInterval(()=>{if(!playing && +slider.value===points.length-1 && !document.hidden)location.reload()},10000);' if live else ''
    return ('<!doctype html><html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">'+
            '<title>'+html.escape(title)+'</title><style>'+assets.joinpath('palette.css').read_text()+
            'body{margin:0;padding:20px;background:var(--ix-page);color:var(--ix-ink)}'+assets.joinpath('view.css').read_text()+'</style></head><body><h1>Indexus · Suivi du réseau</h1>'+
            '<nav class="timeline" aria-label="Lecture du banc"><button id="playback" type="button">Lire à 5×</button> <label>Instant observé <input id="moment" type="range" min="0" max="'+str(max(0,len(data)-1))+'" value="'+str(max(0,len(data)-1))+'"></label> <output id="time"></output></nav><main id="dashboard"></main><script>'+assets.joinpath('view.js').read_text()+
            ';const points='+payload+';const slider=document.getElementById("moment");function draw(){const root=document.getElementById("dashboard");root.dataset.playing=String(playing);IndexusDashboard.render(root,points,+slider.value);document.getElementById("time").textContent=(points[+slider.value]?.x??0)+" s";}' + """const play=document.getElementById("playback");let playing=false,timer;
function pause(){playing=false;clearTimeout(timer);play.textContent="Lire à 5×";}
function tick(){draw();const i=+slider.value;if(i>=points.length-1){pause();draw();return;}
  timer=setTimeout(()=>{slider.value=i+1;tick()},Math.max(16,(points[i+1].x-points[i].x)*200));}
play.onclick=()=>{if(playing){pause();draw();return;}if(+slider.value>=points.length-1)slider.value=0;
  playing=true;play.textContent="Pause";tick();};
slider.oninput=()=>{pause();draw();};draw();"""+refresh+'</script></body></html>')


if __name__ == "__main__":
    mode = sys.argv[1] if len(sys.argv) > 1 else ""
    if mode == "machines":
        observe_machines(sys.argv[2], sys.argv[3])
    elif mode == "agg":
        agg(sys.argv[2], sys.argv[3], sys.argv[4] if len(sys.argv) > 4 else os.path.basename(sys.argv[2].rstrip("/")))
    elif mode == "html":
        tmp = sys.argv[3] + ".tmp"
        open(tmp, "w").write(page(read(sys.argv[2])))
        os.replace(tmp, sys.argv[3])
    elif mode == "replay":
        replay(sys.argv[2], sys.argv[3], sys.argv[4] if len(sys.argv) > 4 else os.path.basename(sys.argv[2].rstrip("/")))
    elif mode == "compare":  # compare OUT.html LABEL=LIVE.jsonl ...
        runs = [(a.split("=", 1)[0], read(a.split("=", 1)[1])) for a in sys.argv[3:]]
        open(sys.argv[2], "w").write(compare(runs))
    elif mode == "line":
        pts = read(sys.argv[2])
        p = pts[-1] if pts else {}
        print("live:", " ".join(f"{k}={p[k]}" for k in ("phase", "t", "offered", "acked", "shed", "p99_ms", "vnodes", "open", "skew", "audit") if p.get(k) is not None) or "no point yet")
    else:
        sys.exit(__doc__)
