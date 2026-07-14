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


BASE_COLS = ['unix_ms', 'target', 'pod', 'yhat50', 'y50', 'y90', 'ext90',
             'e_iso', 'n', 'applied_cap']
EXT_COLS = ['ext50', 'e_horz']


def load_scores(path):
    """SCORE_TRACE csv. The controller appends across runs of the same config
    name, so one file can mix 10-column (old) and 12-column (ext50/e_horz)
    rows under whichever header came first — parse positionally."""
    rows = []
    with open(path) as f:
        for i, line in enumerate(f):
            parts = line.rstrip('\n').split(',')
            if i == 0 and parts[0] == 'unix_ms':
                continue
            if len(parts) < len(BASE_COLS):
                continue
            r = dict(zip(BASE_COLS, parts))
            if len(parts) >= len(BASE_COLS) + 2:
                r.update(zip(EXT_COLS, parts[len(BASE_COLS):]))
            rows.append(r)
    keys = [k for k in BASE_COLS[3:]]
    if any('e_horz' in r for r in rows):
        keys += EXT_COLS
    out = {k: np.array([float(r.get(k, 'nan') or 'nan') for r in rows])
           for k in keys}
    out['t'] = np.array([int(r['unix_ms']) for r in rows], float) / 1000.0
    out['pod'] = np.array([r['pod'] for r in rows])
    return out


def bucket_median(t, v, width=0.5):
    """Collapse a multi-pod interleaved series to one per-bucket median.
    Without this the line zigzags between the per-pod values every tick and
    renders as a filled band once the run has >1 replica."""
    if len(t) == 0:
        return t, v
    b = np.floor(t / width) * width
    ub = np.unique(b)
    med = np.array([np.median(v[b == x]) for x in ub])
    return ub + width / 2, med


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
    win_ms = d.get('window_interval_ms', 100)
    t, p99, p90, p50, rps, rq = [], [], [], [], [], []
    for s in d['samples']:
        tw = s.get('timing_window', {})
        if not tw.get('request_count'):
            continue
        ts = datetime.fromisoformat(s['timestamp'].replace('Z', '+00:00')).timestamp()
        tt = tw.get('total_time', {})
        t.append(ts)
        p99.append(tt.get('p99_ns', 0) / 1e6)
        p90.append(tt.get('p90_ns', 0) / 1e6)
        p50.append(tt.get('p50_ns', 0) / 1e6)
        rps.append(tw.get('arrival_rps_1s', 0))
        rq.append(tw.get('request_count', 0))
    pod = os.path.basename(path).split('run_data-')[-1].replace('.json', '')
    return dict(pod=pod, t=np.array(t), p99=np.array(p99), p90=np.array(p90),
                p50=np.array(p50), rps=np.array(rps), rq=np.array(rq, float),
                win_ms=win_ms)


def agg_1s(t, v, w):
    """Per-1s count-weighted mean of per-window percentiles — the closest
    honest stand-in for the per-second tail metric papers report when only
    windowed percentiles (not raw samples) are exported."""
    if len(t) == 0:
        return t, v
    b = np.floor(t)
    ub = np.unique(b)
    out = np.array([np.average(v[b == x], weights=np.maximum(w[b == x], 1e-9))
                    for x in ub])
    return ub + 0.5, out


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
    ap.add_argument('--mark', action='append', default=[],
                    help='vertical phase marker "epoch_seconds:label" '
                         '(repeatable; e.g. contention start, ARM)')
    ap.add_argument('--t0-epoch', type=float, default=None,
                    help='x-axis origin as epoch seconds (default: first '
                         'replica activity minus 5s)')
    ap.add_argument('--duration', type=float, default=140,
                    help='run window length in s (with --t0-epoch, scores are '
                         'clipped to [t0-5, t0+duration+10] instead of the '
                         'replica-activity span)')
    ap.add_argument('--cap-base', type=float, default=8,
                    help='shared-pool size c_base; the isolation panel plots '
                         'the victim-side view c_base - c(t)')
    ap.add_argument('--horz-theta-on', type=float, default=0.40,
                    help='Eq.1 threshold for the e_horz channel (traces with '
                         'ext50/e_horz columns)')
    ap.add_argument('--horz-theta-off', type=float, default=0.15)
    ap.add_argument('--show-yhat', action='store_true',
                    help='plot the ŷ50 prediction channel (off by default: it '
                         'no longer drives horizontal scaling and is pinned online)')
    ap.add_argument('--tail-pct', type=int, default=99, choices=[90, 99],
                    help='tail percentile for the top latency panel (default 99)')
    ap.add_argument('--x-start', type=float, default=None,
                    help='clip everything before this many seconds after t0 '
                         '(drops the warmup/handoff region; e.g. 5)')
    ap.add_argument('--phase-avgs', action='store_true',
                    help='annotate the top panel with count-weighted p50/tail '
                         'means for phase 2 (contention, disarmed) and phase 3 '
                         '(mitigated)')
    args = ap.parse_args()

    def _p(x):
        return x if os.path.isabs(x) else os.path.join(SIM_DIR, x)
    mitigated = args.scores is not None
    sc = load_scores(_p(args.scores)) if mitigated else load_score_log(_p(args.score_log))
    pattern = args.run_data if os.path.isabs(args.run_data) else os.path.join(SIM_DIR, args.run_data)
    reps = [load_replica(p) for p in sorted(glob.glob(pattern))]
    if not reps:
        raise SystemExit(f'no run-data files match {pattern}')

    # Plot window: with --t0-epoch clip to the declared run window (the
    # captured run_data may cover only part of the run — e.g. a replica born
    # at the ARM point — and must not truncate the score axes). Otherwise
    # fall back to the union of replica activity.
    if args.t0_epoch is not None:
        t0 = args.t0_epoch
        lo, hi = t0 - 5, t0 + args.duration + 10
    else:
        lo = min(r['t'].min() for r in reps) - 5
        hi = max(r['t'].max() for r in reps) + 5
        t0 = lo
    # --x-start clips the left edge: drop the warmup/handoff region so it
    # neither shows nor drives the y-autoscale. Clipping the DATA (below,
    # via x_start on every series) is what fixes autoscale; the xlim just
    # frames it.
    x_start = (args.x_start if args.x_start is not None else lo - t0)
    lo = max(lo, t0 + x_start)
    m = (sc['t'] >= lo) & (sc['t'] <= hi)

    def clip(rel):
        return rel >= x_start

    marks = []
    for spec in args.mark:
        ep, _, lbl = spec.partition(':')
        marks.append((float(ep) - t0, lbl or None))

    fig, axes = plt.subplots(4, 1, figsize=(14, 12), sharex=True,
                             gridspec_kw=dict(height_ratios=[3, 2, 2.6, 2.6]))
    kind = 'mitigated' if mitigated else 'unmitigated reference'
    fig.suptitle(f'{args.label}: live {kind} run — victim latency, load, and '
                 f'each control law with the signal it reacts to', fontsize=11)
    win_ms = reps[0].get('win_ms', 100)

    # ── panel 0: per-replica victim latency (1s aggregate) ──
    tail_key = {90: 'p90', 99: 'p99'}[args.tail_pct]
    tail_lbl = f'p{args.tail_pct}'
    ax = axes[0]
    for i, r in enumerate(reps):
        col = PALETTE[i % len(PALETTE)]
        rel = r['t'] - t0
        k = clip(rel)
        at, av = agg_1s(rel[k], r[tail_key][k], r['rq'][k])
        ax.plot(at, av, lw=1.3, c=col,
                label=f"{r['pod'] if len(r['pod']) <= 8 else r['pod'][-5:]} {tail_lbl}")
        at, av = agg_1s(rel[k], r['p50'][k], r['rq'][k])
        ax.plot(at, av, lw=1.0, ls='--', c=col, alpha=0.7,
                label=f"{r['pod'] if len(r['pod']) <= 8 else r['pod'][-5:]} p50")

    # Phase averages: count-weighted p50/tail across ALL replicas over the
    # p2 (contention, disarmed) and p3 (mitigated) windows. Small settle
    # margins skip the phase-edge transients (driver ramp-in, scale-out).
    def phase_avg(key, lo_rel, hi_rel):
        vals, wts = [], []
        for r in reps:
            rel = r['t'] - t0
            k = (rel >= lo_rel) & (rel < hi_rel)
            vals.append(r[key][k]); wts.append(r['rq'][k])
        vals = np.concatenate(vals) if vals else np.array([])
        wts = np.concatenate(wts) if wts else np.array([])
        if len(vals) == 0:
            return float('nan')
        return float(np.average(vals, weights=np.maximum(wts, 1e-9)))

    if args.phase_avgs:
        arm_rel = next((x for x, l in marks if l == 'ARM'), None)
        cont_rel = next((x for x, l in marks if l in ('contention', 'overload')), 20)
        if arm_rel is not None:
            phases = [('p2 (contention)', cont_rel + 5, arm_rel),
                      ('p3 (mitigated)', arm_rel + 10, args.duration)]
            trans = ax.get_xaxis_transform()  # x in data, y in axes fraction
            for name, a, b in phases:
                ap50 = phase_avg('p50', a, b)
                atail = phase_avg(tail_key, a, b)
                ax.text((a + b) / 2, 0.92,
                        f"{name}\navg p50 {ap50:.1f} · {tail_lbl} {atail:.1f} ms",
                        transform=trans, ha='center', va='top', fontsize=8,
                        color='0.15', linespacing=1.4,
                        bbox=dict(boxstyle='round', fc='white', ec='0.7', alpha=0.85))

    ax.set_ylabel('latency (ms)')
    ax.legend(fontsize=7, ncol=max(1, len(reps)), loc='upper left')
    ax.set_title(f'Victim latency per replica (total_time p50/{tail_lbl} from {win_ms:g}ms '
                 f'windows, count-weighted 1s mean; series start = replica creation)',
                 fontsize=9)

    # ── panel 1: per-replica arrival rate ──
    ax = axes[1]
    for i, r in enumerate(reps):
        col = PALETTE[i % len(PALETTE)]
        rel = r['t'] - t0
        k = clip(rel)
        at, av = agg_1s(rel[k], r['rps'][k], np.ones_like(r['rps'][k]))
        ax.plot(at, av, lw=1.1, c=col,
                label=r['pod'] if len(r['pod']) <= 8 else r['pod'][-5:])
    ax.set_ylabel('arrival rps')
    ax.legend(fontsize=7, ncol=max(1, len(reps)))
    ax.set_title('Load distribution across replicas', fontsize=9)

    # ── panel 2: Eq.(1) horizontal — driving scores + replica response ──
    # The prediction channel ŷ50 is deliberately NOT plotted: horizontal
    # scaling is driven by the intrinsic-weighted formula signal e_horz =
    # y50·(1−ext50) (the ablation this figure documents), and the pinned
    # online ŷ50 only clutters. Pass --show-yhat to bring it back.
    ax = axes[2]
    has_ehorz = 'e_horz' in sc
    chans1 = []
    if args.show_yhat:
        chans1.append(('yhat50', 'ŷ50 (prediction, unused)', PALETTE[3], 0.4))
    chans1.append(('y50', 'y50_current (formula)', PALETTE[0], 0.5 if has_ehorz else 0.9))
    if has_ehorz:
        chans1.append(('e_horz', 'e_horz = y50·(1−ext50) (drives Eq.1)', PALETTE[7], 0.95))
    for key, lbl, col, alpha in chans1:
        bt, bv = bucket_median(sc['t'][m] - t0, sc[key][m])
        ax.plot(bt, bv, lw=1.0 if alpha > 0.6 else 0.9, c=col, label=lbl, alpha=alpha)
    if has_ehorz:
        ax.axhline(args.horz_theta_on, ls='--', c='gray', lw=0.7,
                   label=f'θ_on^horz={args.horz_theta_on}')
        ax.axhline(args.horz_theta_off, ls=':', c='gray', lw=0.7,
                   label=f'θ_off^horz={args.horz_theta_off}')
    else:
        ax.axhline(args.theta_on, ls='--', c='gray', lw=0.7, label=f'θ_on={args.theta_on}')
        ax.axhline(args.theta_off, ls=':', c='gray', lw=0.7, label=f'θ_off={args.theta_off}')
    ax.set_ylabel('score')
    ax.set_ylim(-0.05, 1.12)
    ax2 = ax.twinx()
    nt, nv = bucket_median(sc['t'][m] - t0, sc['n'][m])
    ax2.step(nt, nv + 1, where='post', lw=1.6, c=PALETTE[1], alpha=0.85)
    ax2.set_ylabel('replicas', color=PALETTE[1])
    ax2.tick_params(axis='y', labelcolor=PALETTE[1])
    ax2.set_ylim(0.5, max(4.5, nv.max() + 1.8))
    ax.legend(fontsize=7, ncol=4, loc='upper left')
    lead = 'e_horz = y50·(1−ext50)' if has_ehorz else 'y50'
    ax.set_title(f'Eq.(1) horizontal bang-bang: {lead} vs thresholds (left) '
                 '→ total replicas (right, orange)', fontsize=9)

    # ── panel 3: Eq.(2) isolation — driving signal + victim-side budget ──
    # c(t) is the aggressors' discretionary budget; the victim gains the
    # remainder c_base - c(t).
    ax = axes[3]
    for key, lbl, col, alpha in [('e_iso', 'e_iso = y90·ext90 (drives Eq.2)', PALETTE[6], 0.95),
                                 ('y90', 'y90 (tail formula)', PALETTE[2], 0.45),
                                 ('ext90', 'ext_pct90', PALETTE[4], 0.45)]:
        bt, bv = bucket_median(sc['t'][m] - t0, sc[key][m])
        ax.plot(bt, bv, lw=1.0, c=col, label=lbl, alpha=alpha)
    ax.axhline(args.theta_ref, ls='--', c='gray', lw=0.7, label=f'θ_ref={args.theta_ref}')
    ax.set_ylabel('score')
    ax.set_ylim(-0.05, 1.12)
    ax2 = ax.twinx()
    if mitigated:
        ct, cv = bucket_median(sc['t'][m] - t0, sc['applied_cap'][m])
        ax2.step(ct, args.cap_base - cv, where='post', lw=1.6, c=PALETTE[1], alpha=0.85)
        ax2.set_ylim(-0.3, args.cap_base * 1.15)
    ax2.set_ylabel('cores to victim', color=PALETTE[1])
    ax2.tick_params(axis='y', labelcolor=PALETTE[1])
    ax.legend(fontsize=7, ncol=4, loc='upper left')
    ax.set_title(f'Eq.(2) saturated-P isolation: e_iso vs θ_ref (left) → shared-pool '
                 f'cores returned to the victim, c_base−c(t), c_base={args.cap_base:g} '
                 f'(right, orange)', fontsize=9)
    ax.set_xlabel('time (s)')

    for ax in axes:
        for x, lbl in marks:
            ax.axvline(x, ls='-', c='crimson', lw=0.9, alpha=0.55)
        ax.set_xlim(x_start, hi - t0)
    for x, lbl in marks:
        if lbl:
            axes[0].annotate(lbl, xy=(x, 1.06), xycoords=('data', 'axes fraction'),
                             ha='center', fontsize=8, color='crimson')

    plt.tight_layout()
    os.makedirs(RESULTS_DIR, exist_ok=True)
    out = os.path.join(RESULTS_DIR, f'real_run_{args.label}.png')
    fig.savefig(out, dpi=130)
    print('[saved]', out)


if __name__ == '__main__':
    main()
