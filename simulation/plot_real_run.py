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
# The 12th column is the intrinsic-weighted horizontal signal. Renamed
# e_horz -> i_horz (e_iso's 'e' = extrinsic, so the intrinsic one is 'i');
# older traces wrote 'e_horz', so accept both and normalize to 'i_horz'.
EXT_COLS = ['ext50', 'i_horz']


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
    if any('i_horz' in r for r in rows):
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
                    help='Eq.1 threshold for the i_horz channel (traces with '
                         'ext50/i_horz columns)')
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
    ap.add_argument('--compact', action='store_true',
                    help='2-panel layout: latency merged with the firing '
                         "actuator's response + that actuator's score. Drops "
                         'the arrival-rate panel and the non-firing actuator.')
    ap.add_argument('--p3-panel', action='store_true',
                    help='single compact panel: only the firing actuator (scores + '
                         'replicas/cores) cropped to p3 (pass --x-start at the ARM offset). '
                         'No latency — performance is shown in the bar chart.')
    ap.add_argument('--actuator', choices=['auto', 'horizontal', 'isolation'],
                    default='auto',
                    help='which actuator to show in --compact (auto: horizontal '
                         'if n ever > 0, else isolation)')
    ap.add_argument('--no-legend', action='store_true',
                    help='suppress the per-panel legend (use an external aggregated legend)')
    ap.add_argument('--score-agg', choices=['tick-mean', 'bucket-median', 'bucket-max'],
                    default='tick-mean',
                    help='how to reduce the score channels for plotting. '
                         'tick-mean = the controller\'s actual per-tick decision '
                         'signal (pod-mean, no time smoothing → bursts visible); '
                         'bucket-median hides sub-second bursts.')
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

    kind = 'mitigated' if mitigated else 'unmitigated reference'
    win_ms = reps[0].get('win_ms', 100)
    tail_key = {90: 'p90', 99: 'p99'}[args.tail_pct]
    tail_lbl = f'p{args.tail_pct}'

    # Score-channel reducer. tick-mean reconstructs the controller's actual
    # per-tick decision signal (mean over the pods present at each ~100ms
    # tick, no time smoothing) so bursts that drive the bang-bang are visible
    # — a 1s median hides them and makes actuation look uncaused.
    def score_series(key):
        tt = sc['t'][m] - t0
        vv = sc[key][m]
        if args.score_agg == 'bucket-median':
            return bucket_median(tt, vv)
        tick = np.round(tt * 10) / 10.0  # 100ms bins
        ut = np.unique(tick)
        if args.score_agg == 'bucket-max':
            red = np.array([vv[tick == u].max() for u in ut])
        else:  # tick-mean (default): the decision signal
            red = np.array([vv[tick == u].mean() for u in ut])
        return ut, red

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

    def draw_latency(ax):
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
        if args.phase_avgs:
            arm_rel = next((x for x, l in marks if l == 'ARM'), None)
            cont_rel = next((x for x, l in marks if l in ('contention', 'overload')), 20)
            if arm_rel is not None:
                trans = ax.get_xaxis_transform()
                for name, a, b in [('p2 (contention)', cont_rel + 5, arm_rel),
                                   ('p3 (mitigated)', arm_rel + 10, args.duration)]:
                    ax.text((a + b) / 2, 0.80,
                            f"{name}\navg p50 {phase_avg('p50', a, b):.1f} · "
                            f"{tail_lbl} {phase_avg(tail_key, a, b):.1f} ms",
                            transform=trans, ha='center', va='top', fontsize=8,
                            color='0.15', linespacing=1.4,
                            bbox=dict(boxstyle='round', fc='white', ec='0.7', alpha=0.85))
        ax.set_ylabel('latency (ms)')
        ax.legend(fontsize=7, ncol=max(1, len(reps)), loc='upper left')

    def draw_rate(ax):
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

    has_ehorz = 'i_horz' in sc

    def draw_horiz_score(ax):
        chans = []
        if args.show_yhat:
            chans.append(('yhat50', 'ŷ50', PALETTE[3], 0.4))
        chans.append(('y50', 'y50', PALETTE[0], 0.5 if has_ehorz else 0.9))
        if has_ehorz:
            chans.append(('i_horz', 'i_horz = y50·(1−ext50)', PALETTE[7], 0.95))
        for key, lbl, col, alpha in chans:
            bt, bv = score_series(key)
            ax.plot(bt, bv, lw=1.0 if alpha > 0.6 else 0.9, c=col, label=lbl, alpha=alpha)
        if has_ehorz:
            ax.axhline(args.horz_theta_on, ls='--', c='gray', lw=0.7, label=f'θ_on={args.horz_theta_on}')
            ax.axhline(args.horz_theta_off, ls=':', c='gray', lw=0.7, label=f'θ_off={args.horz_theta_off}')
        else:
            ax.axhline(args.theta_on, ls='--', c='gray', lw=0.7, label=f'θ_on={args.theta_on}')
            ax.axhline(args.theta_off, ls=':', c='gray', lw=0.7, label=f'θ_off={args.theta_off}')
        ax.set_ylabel('score'); ax.set_ylim(-0.05, 1.12)
        if not args.no_legend:
            ax.legend(fontsize=7, ncol=4, loc='upper left')

    def draw_iso_score(ax):
        for key, lbl, col, alpha in [('e_iso', 'e_iso = y90·ext90', PALETTE[6], 0.95),
                                     ('y90', 'y90', PALETTE[2], 0.45),
                                     ('ext90', 'ext_pct90', PALETTE[4], 0.45)]:
            bt, bv = score_series(key)
            ax.plot(bt, bv, lw=1.0, c=col, label=lbl, alpha=alpha)
        ax.axhline(args.theta_ref, ls='--', c='gray', lw=0.7, label=f'θ_ref={args.theta_ref}')
        ax.set_ylabel('score'); ax.set_ylim(-0.05, 1.12)
        if not args.no_legend:
            ax.legend(fontsize=7, ncol=4, loc='upper left')

    def twin_replicas(ax):
        ax2 = ax.twinx()
        nt, nv = bucket_median(sc['t'][m] - t0, sc['n'][m])
        ax2.step(nt, nv + 1, where='post', lw=1.6, c=PALETTE[1], alpha=0.85)
        ax2.set_ylabel('replicas', color=PALETTE[1])
        ax2.tick_params(axis='y', labelcolor=PALETTE[1])
        ax2.set_ylim(0.5, max(4.5, nv.max() + 1.8))

    def twin_cores(ax):
        ax2 = ax.twinx()
        if mitigated:
            ct, cv = bucket_median(sc['t'][m] - t0, sc['applied_cap'][m])
            ax2.step(ct, args.cap_base - cv, where='post', lw=1.6, c=PALETTE[1], alpha=0.85)
            ax2.set_ylim(-0.3, args.cap_base * 1.15)
        ax2.set_ylabel('cores to victim', color=PALETTE[1])
        ax2.tick_params(axis='y', labelcolor=PALETTE[1])

    # Victim latency aggregated across ALL replicas (count-weighted 1s), for
    # the merged view: one p50 + one tail line rather than per-pod clutter.
    def agg_all_pods(key):
        ts = np.concatenate([r['t'] - t0 for r in reps])
        vs = np.concatenate([r[key] for r in reps])
        ws = np.concatenate([r['rq'] for r in reps])
        k = clip(ts)
        return agg_1s(ts[k], vs[k], ws[k])

    def twin_latency(ax):
        # Latency on the NON-firing actuator's freed right axis. Dark so it
        # reads as the outcome overlay, distinct from the coloured scores.
        ax2 = ax.twinx()
        at, av = agg_all_pods(tail_key)
        ax2.plot(at, av, lw=1.5, c='0.12', label=f'{tail_lbl} (all replicas)')
        at, av = agg_all_pods('p50')
        ax2.plot(at, av, lw=1.1, ls='--', c='0.12', alpha=0.65, label='p50 (all replicas)')
        ax2.set_ylabel('victim latency (ms)', color='0.12')
        ax2.tick_params(axis='y', labelcolor='0.12')
        if args.phase_avgs:
            arm_rel = next((x for x, l in marks if l == 'ARM'), None)
            cont_rel = next((x for x, l in marks if l in ('contention', 'overload')), 20)
            if arm_rel is not None:
                trans = ax2.get_xaxis_transform()
                for name, a, b in [('p2 (contention)', cont_rel + 5, arm_rel),
                                   ('p3 (mitigated)', arm_rel + 10, args.duration)]:
                    ax2.text((a + b) / 2, 0.62,
                             f"{name}: avg p50 {phase_avg('p50', a, b):.1f} · "
                             f"{tail_lbl} {phase_avg(tail_key, a, b):.1f} ms",
                             transform=trans, ha='center', va='top', fontsize=7.5,
                             color='0.12', bbox=dict(boxstyle='round', fc='white', ec='0.7', alpha=0.85))
        ax2.legend(fontsize=7, loc='upper right')

    # Which actuator actually fired? extrinsic contention → isolation only
    # (n stays 0); intrinsic overload → horizontal only (e_iso ≈ 0).
    horiz_fired = ('n' in sc) and (np.nanmax(sc['n']) > 0)
    fired = args.actuator if args.actuator != 'auto' else ('horizontal' if horiz_fired else 'isolation')

    if args.p3_panel:
        # Space-saving single panel: only the firing actuator, cropped to p3
        # (via --x-start). No latency (bar chart shows performance).
        fig, ax = plt.subplots(1, 1, figsize=(5.6, 2.9))
        if fired == 'horizontal':
            draw_horiz_score(ax); twin_replicas(ax)
            ax.set_title('time vs i_horz (left) → num of replicas (right)', fontsize=9)
        else:
            draw_iso_score(ax); twin_cores(ax)
            ax.set_title('time vs e_iso (left) → cpu share returned to victim (right)', fontsize=9)
        ax.set_xlabel('time (s)')
        axes = [ax]
    elif args.compact:
        # Two panels, BOTH control laws kept (the non-firing one proves the
        # ablation — its score correctly stays sub-threshold). The FIRING
        # actuator shows its actuation on the right axis; the NON-FIRING one's
        # right axis is otherwise flat, so latency is hosted there instead.
        # Arrival-rate + the standalone latency panel are dropped.
        fig, axes = plt.subplots(2, 1, figsize=(13, 8), sharex=True)
        fig.suptitle(f'{args.label}: {kind} — both control laws; '
                     f'{"horizontal" if fired == "horizontal" else "isolation"} fires, '
                     f'victim latency shares the idle axis of the other', fontsize=11)

        draw_horiz_score(axes[0])
        if fired == 'horizontal':
            twin_replicas(axes[0])
            axes[0].set_title('Eq.(1) horizontal: i_horz = y50·(1−ext50) vs thresholds (left) '
                              '→ total replicas (right)', fontsize=9)
        else:
            twin_latency(axes[0])
            axes[0].set_title('Eq.(1) horizontal: i_horz vs thresholds (left) — did NOT fire; '
                              'victim latency on the right', fontsize=9)

        draw_iso_score(axes[1])
        if fired == 'isolation':
            twin_cores(axes[1])
            axes[1].set_title(f'Eq.(2) isolation: e_iso = y90·ext90 vs θ_ref (left) → cores '
                              f'returned to victim (right, c_base={args.cap_base:g})', fontsize=9)
        else:
            twin_latency(axes[1])
            axes[1].set_title('Eq.(2) isolation: e_iso vs θ_ref (left) — did NOT fire; '
                              'victim latency on the right', fontsize=9)
        axes[1].set_xlabel('time (s)')
    else:
        fig, axes = plt.subplots(4, 1, figsize=(14, 12), sharex=True,
                                 gridspec_kw=dict(height_ratios=[3, 2, 2.6, 2.6]))
        fig.suptitle(f'{args.label}: live {kind} run — victim latency, load, and '
                     f'each control law with the signal it reacts to', fontsize=11)
        draw_latency(axes[0])
        axes[0].set_title(f'Victim latency per replica (total_time p50/{tail_lbl} from {win_ms:g}ms '
                          f'windows, count-weighted 1s mean)', fontsize=9)
        draw_rate(axes[1])
        draw_horiz_score(axes[2]); twin_replicas(axes[2])
        axes[2].set_title('Eq.(1) horizontal: i_horz vs thresholds → replicas (right)', fontsize=9)
        draw_iso_score(axes[3]); twin_cores(axes[3])
        axes[3].set_title(f'Eq.(2) isolation: e_iso vs θ_ref → cores to victim (right)', fontsize=9)
        axes[3].set_xlabel('time (s)')

    hi_x = args.duration if args.p3_panel else (hi - t0)
    for ax in axes:
        for x, lbl in marks:
            ax.axvline(x, ls='-', c='crimson', lw=0.9, alpha=0.55)
        ax.set_xlim(x_start, hi_x)
    for x, lbl in marks:
        if lbl:
            axes[0].annotate(lbl, xy=(x, 1.06), xycoords=('data', 'axes fraction'),
                             ha='center', fontsize=8, color='crimson')

    plt.tight_layout()
    os.makedirs(RESULTS_DIR, exist_ok=True)
    out = os.path.join(RESULTS_DIR, f'real_run_{args.label}.png')
    fig.savefig(out, dpi=400)
    print('[saved]', out)


if __name__ == '__main__':
    main()
