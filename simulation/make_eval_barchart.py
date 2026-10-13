#!/usr/bin/env python3
"""eval.1 prototype: per-config avg victim latency across the three
conditions — p1 (no contention baseline), p2 (contention, unmitigated),
p3 (contention + mitigation) — count-weighted across all replicas.
Built from the existing sweep3 phased runs (severe level)."""
import json, glob, os
from datetime import datetime, timezone
import numpy as np
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt

BASE = 'data/sweep3'
CONFIGS = [('ext-steady-severe', 'extrinsic\nsteady'),
           ('ext-bursty-severe', 'extrinsic\nbursty'),
           ('int-steady-severe', 'intrinsic\nsteady'),
           ('int-bursty-severe', 'intrinsic\nbursty')]
PALETTE = {'p1': '#66aabb', 'p2': '#ee7733', 'p3': '#009988'}

def ts_parse(s):
    frac = s.split('.')[1].rstrip('Z') if '.' in s else '0'
    dt = datetime.strptime(s.split('.')[0], '%Y-%m-%dT%H:%M:%S').replace(tzinfo=timezone.utc)
    return dt.timestamp() + float('0.' + frac)

def phase_lat(d, t0, arm, lo, hi, pct):
    key = f'p{pct}_ns'
    vals, wts = [], []
    for f in glob.glob(f'{d}/run_data-*.json'):
        for s in json.load(open(f)).get('samples', []):
            tw = s.get('timing_window') or {}
            rc = tw.get('request_count') or 0
            if not rc: continue
            ts = ts_parse(s['timestamp'])
            if not (lo <= ts < hi): continue
            vals.append((tw.get('total_time') or {}).get(key, 0) / 1e6); wts.append(rc)
    if not vals: return float('nan')
    return float(np.average(vals, weights=wts))

rows = {}
for name, _ in CONFIGS:
    d = f'{BASE}/{name}'
    t0 = int(open(f'{d}/started_epoch_ns.txt').read())/1e9
    arm = int(open(f'{d}/armed_epoch_ns.txt').read())/1e9
    tend = t0 + 140
    rows[name] = {
        'p1': (phase_lat(d, t0, arm, t0+3, t0+18, 50), phase_lat(d, t0, arm, t0+3, t0+18, 90)),
        'p2': (phase_lat(d, t0, arm, t0+25, arm, 50), phase_lat(d, t0, arm, t0+25, arm, 90)),
        'p3': (phase_lat(d, t0, arm, arm+10, tend, 50), phase_lat(d, t0, arm, arm+10, tend, 90)),
    }

fig, axes = plt.subplots(1, 2, figsize=(13, 5), sharey=False)
labels = [lbl for _, lbl in CONFIGS]
x = np.arange(len(CONFIGS)); w = 0.26
for pi, (pct, ax) in enumerate(zip((50, 90), axes)):
    for j, (cond, cname) in enumerate([('p1', 'no contention (p1)'),
                                       ('p2', 'contention, unmitigated (p2)'),
                                       ('p3', 'contention + mitigation (p3)')]):
        vals = [rows[n][cond][pi] for n, _ in CONFIGS]
        b = ax.bar(x + (j-1)*w, vals, w, label=cname, color=PALETTE[cond])
        ax.bar_label(b, fmt='%.1f', fontsize=7, padding=2)
    ax.set_xticks(x); ax.set_xticklabels(labels, fontsize=9)
    ax.set_ylabel(f'victim total_time p{pct} (ms)')
    ax.set_title(f'p{pct}', fontsize=11)
    ax.grid(axis='y', alpha=0.3)
    if pi == 0:
        ax.legend(fontsize=8, loc='upper left')
fig.suptitle('eval.1 (prototype, sweep3 severe): avg victim latency by condition', fontsize=12)
plt.tight_layout()
os.makedirs('results', exist_ok=True)
fig.savefig('results/eval1_barchart.png', dpi=130)
print('[saved] results/eval1_barchart.png')
for n, _ in CONFIGS:
    r = rows[n]
    print(f"  {n:20s} p50 {r['p1'][0]:.1f}/{r['p2'][0]:.1f}/{r['p3'][0]:.1f}  "
          f"p90 {r['p1'][1]:.1f}/{r['p2'][1]:.1f}/{r['p3'][1]:.1f}  (p1/p2/p3)")
