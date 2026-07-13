#!/usr/bin/env python3
"""Plot a real mitigated stage-3 run: score channels, per-replica raw
latency, load distribution, and controller actions on one time axis.

Inputs (all wall-clock aligned):
  --scores    controller SCORE_TRACE csv (node-4 replica's channels + n/cap)
  --run-data  one or more per-replica instrumentation JSONs (captured by the
              support pipeline); each replica's curves start at its creation
  --label     arm label used in the title and output filename

The scores CSV may span several runs (the controller runs continuously);
the plotted window is clipped to the span covered by the run-data files.

Example:
  python plot_real_run.py --label mildA3 \
      --scores data/real-runs/mildA3.scores.csv \
      --run-data "data/real-runs/mildA3-run_data-*.json"
"""
import argparse
import csv
import glob
import json
import os
from datetime import datetime

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import numpy as np

SIM_DIR = os.path.dirname(os.path.abspath(__file__))
RESULTS_DIR = os.path.join(SIM_DIR, 'results')

# Fixed-order categorical palette (CVD-validated) — replicas keep one color
# across every panel.
PALETTE = ['#0077BB', '#EE7733', '#009988', '#EE3377',
           '#997700', '#33BBEE', '#CC3311', '#AA4499']


def rolling_median(x, w):
    if len(x) < w:
        return np.asarray(x, float)
    out = np.empty(len(x))
    for i in range(len(x)):
        lo = max(0, i - w // 2)
        out[i] = np.median(x[lo:i + w // 2 + 1])
    return out


def load_scores(path):
    rows = list(csv.DictReader(open(path)))
    out = {k: np.array([float(r[k]) for r in rows]) for k in
           ('yhat50', 'y50', 'y90', 'ext90', 'e_iso', 'n', 'applied_cap')}
    out['t'] = np.array([int(r['unix_ms']) for r in rows], float) / 1000.0
    out['pod'] = np.array([r['pod'] for r in rows])
    return out


def load_score_log(path):
    """Parse a victim score_events.log (logfmt, possibly ANSI-colored) into
    the same channel dict as the controller CSV. Unmitigated reference runs
    have no controller trace; this is their sensor record."""
    import re
    pat = {k: re.compile(k + r'=(?:\x1b\[0m)?([0-9.eE+-]+)') for k in
           ('p50_trend_pred', 'y50_current', 'tail_trend_label', 'ext_pct_90',
            'sample_id')}
    tpat = re.compile(r'timestamp=(?:\x1b\[0m)?([0-9T:.Z-]+)')
    t, sid, yhat, y50, y90, ext = [], [], [], [], [], []
    for line in open(path, encoding='utf-8', errors='replace'):
        if 'score_event' not in line:
            continue
        vals = {}
        for k, p in pat.items():
            m = p.search(line)
            if m:
                vals[k] = float(m.group(1))
        m = tpat.search(line)
        if len(vals) < 5 or not m:
            continue
        ts = datetime.fromisoformat(m.group(1).replace('Z', '+00:00')).timestamp()
        t.append(ts)
        sid.append(vals['sample_id'])
        yhat.append(vals['p50_trend_pred'])
        y50.append(vals['y50_current'])
        y90.append(vals['tail_trend_label'])
        ext.append(vals['ext_pct_90'] if vals['ext_pct_90'] > 0 else 1.0)
    # The log sink stamps whole seconds, collapsing ~10 events per second
    # onto one x. sample_id ticks every 100 ms, so rebuild sub-second time
    # from it, anchored so the reconstruction tracks the coarse stamps.
    if len(t) > 1:
        sid_a = np.array(sid)
        t_a = np.array(t)
        recon = t_a[0] + (sid_a - sid_a[0]) * 0.1
        recon += np.median(t_a - recon)  # re-anchor against clock drift
        t = list(recon)
    y90 = np.array(y90)
    ext = np.array(ext)
    n = np.zeros(len(t))
    return dict(t=np.array(t), yhat50=np.array(yhat), y50=np.array(y50),
                y90=y90, ext90=ext, e_iso=y90 * ext,
                n=n, applied_cap=np.full(len(t), np.nan),
                pod=np.array(['victim'] * len(t)))


def load_replica(path):
    d = json.load(open(path))
    t, p99, p50, rps = [], [], [], []
    for s in d['samples']:
        tw = s.get('timing_window', {})
        if not tw.get('request_count'):
            continue
        ts = datetime.fromisoformat(s['timestamp'].replace('Z', '+00:00')).timestamp()
        tt = tw.get('total_time', {})
        t.append(ts)
        p99.append(tt.get('p99_ns', 0) / 1e6)
        p50.append(tt.get('p50_ns', 0) / 1e6)
        rps.append(tw.get('arrival_rps_1s', 0))
    pod = os.path.basename(path).split('run_data-')[-1].replace('.json', '')
    return dict(pod=pod, t=np.array(t), p99=np.array(p99),
                p50=np.array(p50), rps=np.array(rps))


def main():
    ap = argparse.ArgumentParser()
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument('--scores', help='controller SCORE_TRACE csv (mitigated runs)')
    src.add_argument('--score-log', help='victim score_events.log (unmitigated refs)')
    ap.add_argument('--run-data', required=True,
                    help='glob for per-replica run_data JSONs')
    ap.add_argument('--label', default='run')
    ap.add_argument('--theta-on', type=float, default=0.55)
    ap.add_argument('--theta-off', type=float, default=0.15)
    ap.add_argument('--theta-ref', type=float, default=0.55)
    args = ap.parse_args()

    def _p(x):
        return x if os.path.isabs(x) else os.path.join(SIM_DIR, x)
    mitigated = args.scores is not None
    sc = load_scores(_p(args.scores)) if mitigated else load_score_log(_p(args.score_log))
    pattern = args.run_data if os.path.isabs(args.run_data) else os.path.join(SIM_DIR, args.run_data)
    reps = [load_replica(p) for p in sorted(glob.glob(pattern))]
    if not reps:
        raise SystemExit(f'no run-data files match {pattern}')

    # Window = union of replica activity, padded; clip scores to it.
    lo = min(r['t'].min() for r in reps) - 5
    hi = max(r['t'].max() for r in reps) + 5
    m = (sc['t'] >= lo) & (sc['t'] <= hi)
    t0 = lo

    fig, axes = plt.subplots(5, 1, figsize=(14, 13), sharex=True,
                             gridspec_kw=dict(height_ratios=[3, 3, 2, 1.2, 1.4]))
    kind = 'mitigated' if mitigated else 'unmitigated reference'
    fig.suptitle(f'{args.label}: live {kind} run — sensor channels, raw latency, '
                 f'load, and controller actions', fontsize=11)

    # ── panel 0: score channels (node-4 replica / controller view) ──
    ax = axes[0]
    chans = [('yhat50', 'p50_trend_pred (ŷ50, prediction)', PALETTE[3]),
             ('y50', 'y50_current (formula)', PALETTE[0]),
             ('y90', 'y90 (tail formula)', PALETTE[2]),
             ('ext90', 'ext_pct90', PALETTE[4]),
             ('e_iso', 'e_iso = y90·ext90', PALETTE[6])]
    for key, lbl, col in chans:
        ax.plot(sc['t'][m] - t0, sc[key][m], lw=1.0, c=col, label=lbl,
                alpha=0.9 if key != 'yhat50' else 0.6)
    ax.axhline(args.theta_on, ls='--', c='gray', lw=0.7, label=f'θ_on={args.theta_on}')
    ax.axhline(args.theta_ref, ls=':', c='gray', lw=0.7, label=f'θ_ref={args.theta_ref}')
    ax.set_ylabel('score')
    ax.set_ylim(-0.05, 1.1)
    ax.legend(fontsize=7, ncol=4, loc='lower right')
    ax.set_title('Sensor channels (node-4 replica — controller view)', fontsize=9)

    # ── panel 1: per-replica raw latency p99 ──
    ax = axes[1]
    for i, r in enumerate(reps):
        col = PALETTE[i % len(PALETTE)]
        ax.plot(r['t'] - t0, r['p99'], lw=0.3, alpha=0.25, c=col)
        ax.plot(r['t'] - t0, rolling_median(r['p99'], 21), lw=1.3, c=col,
                label=f"{r['pod'] if len(r['pod']) <= 8 else r['pod'][-5:]} p99")
    ax.set_ylabel('latency (ms)')
    ax.set_yscale('log')
    ax.legend(fontsize=7, ncol=len(reps))
    ax.set_title('Raw victim latency per replica (per-50ms window total_time p99, '
                 '1s median; series start = replica creation)', fontsize=9)

    # ── panel 2: per-replica arrival rate ──
    ax = axes[2]
    for i, r in enumerate(reps):
        col = PALETTE[i % len(PALETTE)]
        ax.plot(r['t'] - t0, rolling_median(r['rps'], 21), lw=1.1, c=col,
                label=r['pod'] if len(r['pod']) <= 8 else r['pod'][-5:])
    ax.set_ylabel('arrival rps')
    ax.legend(fontsize=7, ncol=len(reps))
    ax.set_title('Load distribution across replicas', fontsize=9)

    # ── panel 3: replica count n(t) ──
    ax = axes[3]
    ax.step(sc['t'][m] - t0, sc['n'][m] + 1, where='post', lw=1.4, c=PALETTE[0])
    ax.set_ylabel('replicas')
    ax.set_ylim(0.5, sc['n'][m].max() + 1.8)
    ax.set_title('Horizontal: total replicas (1 baseline + n(t))'
                 + ('' if mitigated else '  [unmitigated reference: fixed at 1]'),
                 fontsize=9)

    # ── panel 4: isolation budget ──
    ax = axes[4]
    if mitigated:
        ax.step(sc['t'][m] - t0, sc['applied_cap'][m], where='post', lw=1.4, c=PALETTE[6])
        ax.set_ylim(0, np.nanmax(sc['applied_cap'][m]) * 1.15 + 1)
        ax.set_title('Isolation: aggressor core budget c(t)', fontsize=9)
    else:
        ax.set_ylim(0, 30)
        ax.set_title('Isolation: n/a (unmitigated reference)', fontsize=9)
    ax.set_ylabel('cores')
    ax.set_xlabel('time (s)')

    plt.tight_layout()
    os.makedirs(RESULTS_DIR, exist_ok=True)
    out = os.path.join(RESULTS_DIR, f'real_run_{args.label}.png')
    fig.savefig(out, dpi=130)
    print('[saved]', out)


if __name__ == '__main__':
    main()
