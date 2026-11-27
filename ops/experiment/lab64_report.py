#!/usr/bin/env python3
"""Render recorded local-64 events, without treating them as EC2 capacity."""
import argparse
import html
import json
from pathlib import Path


def report(source, output):
    rows = []
    for line in source.read_text().splitlines():
        try:
            rows.append(json.loads(line))
        except json.JSONDecodeError:
            continue  # live publisher can be between two complete lines
    config = next((r for r in rows if r['event'] == 'configuration'), {})
    samples = [r for r in rows if r['event'] == 'sample']
    audits = [r for r in rows if r['event'] == 'audit']
    if not samples:
        raise ValueError('no samples recorded')
    audit = audits[-1] if audits else None
    summary = {'configuration': config, 'audit': audit,
               'status': 'audited' if audit else 'incomplete',
               'wire': next((r['counters'] for r in reversed(rows) if r['event'] == 'wire'), {}),
               'membership_transport': next((r['counters'] for r in reversed(rows) if r['event'] == 'membership-transport'), {}),
               'client_latency': next((r for r in reversed(rows) if r['event'] == 'client-latency'), {}),
               'handoff_timings': next((r['counters'] for r in reversed(rows) if r['event'] == 'handoff-timings'), {}),
               'milestones': [r for r in rows if r['event'] in (
                   'load-began', 'first-zone-installed', 'first-ack',
                   'first-direct-ack', 'join-complete')],
               'scope': 'one physical computer; real fsync; simulated device sharing; no EC2 throughput claim'}
    output.mkdir(parents=True, exist_ok=True)
    (output/'summary.json').write_text(json.dumps(summary, indent=2))
    end = samples[-1]['t']
    svg = ['<svg xmlns="http://www.w3.org/2000/svg" width="1280" height="1080" viewBox="0 0 1280 1080">',
           '<style>text{font-family:Arial,sans-serif;fill:#1f2937;font-size:13px}.title{font-size:25px;font-weight:bold}.small{font-size:11px}</style>',
           '<rect width="1280" height="1080" fill="#fafafa"/>']

    def text(x, y, value, css=''):
        svg.append(f'<text x="{x}" y="{y}" class="{css}">{html.escape(str(value))}</text>')

    text(50, 38, 'Indexus · laboratoire de 64 vnodes', 'title')
    text(50, 66, 'Une machine physique · 32 périphériques partagés simulés · fsync réels · HTTP/JSON')
    text(50, 89, 'État : ' + ('audit terminé' if audit else 'incomplet : aucun audit final'))
    text(50, 111, f"Zones initiales : {config.get('seed_zones', 0)} · compteurs incrémentaux : {config.get('incremental_accounting', 'non enregistré')}")
    if audit:
        text(50, 132, f"ACK : {audit.get('acked')}/{audit.get('offered')} · refus finaux : {audit.get('failed')} · ACK perdus : {audit.get('ack_missing')} · doublons inscriptibles : {audit.get('writable_ack_duplicates')} · gels restants : {audit.get('frozen_remaining')}")
    previous = {'t': 0, 'offered': 0, 'acked': 0}
    rates = []
    for s in samples:
        dt = s['t'] - previous['t']
        rates.append((s['t'], (s['offered']-previous['offered'])/dt,
                      (s['acked']-previous['acked'])/dt))
        previous = s

    def panel(y, title, series):
        x, w, h = 75, 1130, 145
        text(50, y, title)
        ceiling = max(1, *(v for _, points in series for _, v in points)) * 1.08
        top = y+25
        svg.append(f'<path d="M{x},{top} V{top+h} H{x+w}" fill="none" stroke="#9ca3af"/>')
        for j in range(5):
            t = end*j/4
            text(x+w*j/4, top+h+18, f'{t:.0f} s', 'small')
        text(15, top+5, f'{ceiling:.0f}', 'small')
        for (color, name), points in series:
            points_svg = ' '.join(f'{x+t/end*w:.2f},{top+h-value/ceiling*h:.2f}' for t, value in points)
            svg.append(f'<polyline points="{points_svg}" fill="none" stroke="{color}" stroke-width="2"/>')
        text(75, top+h+38, ' · '.join(name for (color, name), _ in series), 'small')

    panel(147, 'Écritures par seconde : arrivées (bleu), ACK (orange)', [
        (('#2563eb', 'arrivées'), [(t, o) for t, o, a in rates]),
        (('#ea580c', 'ACK'), [(t, a) for t, o, a in rates])])
    ready = [(s['t'], sum(bool(n.get('client_ready')) for n in s['nodes'])) for s in samples]
    panel(385, 'Disponibles pour les clients (vert), joins terminés (bleu), régions gelées (orange)', [
        (('#16a34a', 'disponibles (mesure de façade)'), ready),
        (('#2563eb', 'joins terminés'), [(s['t'], s['joined']) for s in samples]),
        (('#ea580c', 'régions gelées'), [(s['t'], sum(len(n.get('frozen') or []) for n in s['nodes'])) for s in samples])])
    text(50, 628, '64 vnodes : ACK pendant cet intervalle (vert foncé), disponible sans ACK (vert clair), autre (gris)')
    prior = {}
    last_t = 0
    for s in samples:
        for n in s['nodes']:
            i, count = n['node'], n.get('applied', 0)
            color = '#15803d' if count > prior.get(i, 0) else ('#bbf7d0' if n.get('client_ready') else '#e5e7eb')
            svg.append(f'<rect x="{75+last_t/end*1130:.2f}" y="{644+i*5.5:.2f}" width="{max(.2,(s["t"]-last_t)/end*1130):.2f}" height="5" fill="{color}"/>')
            prior[i] = count
        last_t = s['t']
    for i in (0, 15, 31, 47, 63):
        text(30, 649+i*5.5, i, 'small')
    text(50, 1024, 'Les phases de charge sont suivies d’un drain ; une zone verte claire ne prouve pas un débit.')
    text(50, 1047, 'Pas de prédiction AWS : les durées incluent les ressources partagées de cet ordinateur.')
    text(50, 1070, f"Charge locale : {config.get('base_rate')} → {config.get('base_rate', 0)*8}/s par paliers · trafic de fin : {config.get('tail_rate', 0)}/s")
    svg.append('</svg>')
    (output/'timeline.svg').write_text('\n'.join(svg))
    (output/'index.html').write_text('<!doctype html><meta charset="utf-8"><title>Indexus · 64 vnodes</title>'
        '<style>body{margin:0;background:#fafafa}svg{width:100%;height:auto;max-width:1500px}</style>'+'\n'.join(svg))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('events', type=Path)
    parser.add_argument('out', type=Path)
    args = parser.parse_args()
    report(args.events, args.out)
