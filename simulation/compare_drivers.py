#!/usr/bin/env python3
"""Compare how well each contention-score model DRIVES the mitigation
controllers, per contention arm.

Inputs: per-(arm, score) sim traces produced by score_replay.py --sim-json
(files named <arm>_<score>_sim.json), e.g. from the stage-3 capture sweep:
arms baseline..severe, scores kg (Gordion k=0.025) + the five competitors.

Normalization: the controllers' thresholds live on a [0,1] contention scale
that only Gordion natively inhabits (competitors are ratios ~0.9-1.8). To
compare *driving quality* rather than units, every score is affinely mapped
so its own baseline-arm steady mean -> 0 and severe-arm steady mean -> 1
(clipped). This is the single place to swap in a different formula.
'binary' is already 0/1 and passes through raw.

For each (score, arm) the three controllers run at simulation.py defaults;
outputs:
  driving_signals.png  - per-score panels: normalized signal vs time, all arms
  driving_quality.png  - per-score panels: controller duty fractions vs arm
  driving_quality.csv  - full metrics table
  stdout               - metrics + monotonicity summary

Usage:
  python compare_drivers.py --traces-dir sim-traces [--dt 0.1]
"""
import argparse
import csv
import os
import sys

import numpy as np
import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from simulation import (load_json, ctrl_horizontal, ctrl_isolating,
                        ctrl_harvesting)

ARMS = ['baseline', 'mildA', 'mildB', 'mildC', 'moderate', 'severe']
SCORES = [
    ('kg',             'Gordion (k=0.025)'),
    ('slowdown_ratio', 'Slowdown Ratio'),
    ('ci',             'CI (p90/p50)'),
    ('cpi',            'CPI'),
    ('rolling_pctl',   'Rolling Percentile'),
    ('binary',         'Binary Classifier'),
]
PASSTHROUGH = {'binary'}      # already on the [0,1] controller scale
STEADY_S = 15.0               # skip warmup ramp when computing anchors/stats

# palette (light surface): ordinal blue ramp for arm severity,
# categorical slots for the three controllers
RAMP = ['#86b6ef', '#5598e7', '#2a78d6', '#1c5cab', '#104281', '#0d366b']
C_HORZ, C_ISO, C_HARV = '#2a78d6', '#1baf7a', '#eb6834'
SURFACE, INK, INK2 = '#fcfcfb', '#0b0b0b', '#52514e'
MUTED, GRID, BASE = '#898781', '#e1e0d9', '#c3c2b7'

# controller defaults (simulation.py fig_reference)
THETA_ON, THETA_OFF = 0.3, 0.1
THETA_REF, K_P, CAP_BASE, CAP_MIN = 0.3, 6.4, 4.0, 0.5
TH_SAFE, ALPHA, BETA, DELTA = 0.7, 0.05, 0.5, 0.05


def load_traces(d, dt):
    """traces[score][arm] = dict(t, p50, p90, ext90); missing files skipped."""
    traces = {}
    for score, _ in SCORES:
        traces[score] = {}
        for arm in ARMS:
            path = os.path.join(d, f'{arm}_{score}_sim.json')
            if not os.path.exists(path):
                continue
            vals = load_json(path, dt=dt)
            if len(vals) == 7:          # t, arr, p50, p90, ext90, dt, svc
                t, _, p50, p90, ext90 = vals[:5]
            else:                        # older 6-tuple without ext90
                t, _, p50, p90 = vals[:4]
                ext90 = np.ones_like(p90)
            traces[score][arm] = dict(t=t, p50=p50, p90=p90, ext90=ext90)
        if not traces[score]:
            del traces[score]
    return traces


def steady(tr, sig):
    return tr[sig][tr['t'] >= STEADY_S]


def normalize(traces):
    """Anchor each score: baseline steady mean -> 0, severe -> 1 (clipped).
    Returns anchors {score: (lo, hi)} and mutates traces in place, adding
    p50n/p90n."""
    anchors = {}
    for score, arms in traces.items():
        if score in PASSTHROUGH or 'baseline' not in arms or 'severe' not in arms:
            lo, hi = 0.0, 1.0
        else:
            lo = steady(arms['baseline'], 'p50').mean()
            hi = steady(arms['severe'], 'p50').mean()
            if hi - lo < 1e-9:
                lo, hi = 0.0, 1.0
        anchors[score] = (lo, hi)
        for tr in arms.values():
            for sig in ('p50', 'p90'):
                tr[sig + 'n'] = np.clip((tr[sig] - lo) / (hi - lo), 0.0, 1.0)
    return anchors


def run_controllers(tr, dt):
    h_horz = max(1, int(1.0 / dt))
    h_harv = max(1, int(0.5 / dt))
    cmd, n = ctrl_horizontal(tr['p50n'], THETA_ON, THETA_OFF, h_horz)
    e_iso = tr['p90n'] * tr['ext90']    # Eq. 2; ext90=1 for competitors
    cap = ctrl_isolating(e_iso, THETA_REF, K_P, CAP_BASE, CAP_MIN, 0)
    hc, _, sl = ctrl_harvesting(tr['p90n'], TH_SAFE, ALPHA, BETA, DELTA, h_harv)
    on = (n > 0).astype(int)
    return dict(
        sig_mean=float(steady(tr, 'p50n').mean()),
        horz_duty=float(on.mean()),
        horz_up=int((cmd == 1).sum()),
        horz_switches=int(np.abs(np.diff(on)).sum()),
        iso_cap_mean=float(cap.mean()),
        iso_squeeze=float((cap < 2.0).mean()),
        harv_h_mean=float(hc.mean()),
        harv_release=float((sl <= 0).mean()),
    )


def spearman(y):
    """Rank correlation of y against arm order (monotone grading check)."""
    y = np.asarray(y, dtype=float)
    if len(y) < 3 or np.allclose(y, y[0]):
        return float('nan')
    rx = np.arange(len(y), dtype=float)
    ry = np.argsort(np.argsort(y)).astype(float)
    return float(np.corrcoef(rx, ry)[0, 1])


def style_axes(ax):
    ax.set_facecolor(SURFACE)
    ax.grid(axis='y', color=GRID, lw=0.8)
    ax.set_axisbelow(True)
    for side in ('top', 'right', 'left'):
        ax.spines[side].set_visible(False)
    ax.spines['bottom'].set_color(BASE)
    ax.tick_params(length=0, labelsize=8.5, colors=INK2)


def fig_signals(traces, out):
    scores = [(k, l) for k, l in SCORES if k in traces]
    fig, axes = plt.subplots(2, 3, figsize=(13.5, 6.6), sharey=True)
    fig.patch.set_facecolor(SURFACE)
    for (score, label), ax in zip(scores, axes.flat):
        for arm, color in zip(ARMS, RAMP):
            tr = traces[score].get(arm)
            if tr is None:
                continue
            # raw as a faint band (noise-dominated scores would otherwise
            # render as solid ink), 2s moving average as the readable line
            ax.plot(tr['t'], tr['p50n'], color=color, lw=0.5, alpha=0.18)
            w = max(1, int(round(2.0 / (tr['t'][1] - tr['t'][0]))))
            sm = np.convolve(tr['p50n'], np.ones(w) / w, mode='same')
            ax.plot(tr['t'], sm, color=color, lw=1.5)
        ax.set_title(label, fontsize=10.5, color=INK, pad=5)
        ax.set_ylim(-0.04, 1.06)
        style_axes(ax)
    for ax in axes.flat[len(scores):]:
        ax.set_visible(False)
    for ax in axes[-1]:
        ax.set_xlabel('Time (s)', fontsize=9, color=INK2)
    handles = [plt.Line2D([], [], color=c, lw=2.5) for c in RAMP]
    fig.legend(handles, ARMS, loc='upper right', ncol=6, fontsize=8.5,
               frameon=False, bbox_to_anchor=(0.99, 0.985))
    fig.suptitle('Normalized driving signal per score model, all arms\n',
                 fontsize=13, color=INK, x=0.02, ha='left')
    fig.text(0.02, 0.925,
             f'anchored: own baseline steady mean = 0, severe = 1 '
             f'(binary raw); steady = t > {STEADY_S:.0f}s',
             fontsize=9, color=MUTED)
    fig.tight_layout(rect=(0, 0, 1, 0.90))
    fig.savefig(out, dpi=180, facecolor=SURFACE)
    plt.close(fig)
    print(f'[saved] {out}')


def fig_quality(metrics, traces, out):
    scores = [(k, l) for k, l in SCORES if k in traces]
    series = [('horz_duty', 'scale-out duty', C_HORZ),
              ('iso_squeeze', 'squeeze duty', C_ISO),
              ('harv_release', 'harvest-release duty', C_HARV)]
    fig, axes = plt.subplots(2, 3, figsize=(13.5, 6.6), sharey=True)
    fig.patch.set_facecolor(SURFACE)
    for (score, label), ax in zip(scores, axes.flat):
        x = np.arange(len(ARMS))
        for key, _, color in series:
            y = [metrics.get((score, a), {}).get(key, np.nan) for a in ARMS]
            ax.plot(x, y, color=color, lw=2, marker='o', ms=5)
        ax.set_title(label, fontsize=10.5, color=INK, pad=5)
        ax.set_ylim(-0.04, 1.06)
        ax.set_xticks(x)
        ax.set_xticklabels(ARMS, rotation=30, ha='right', fontsize=8)
        style_axes(ax)
    for ax in axes.flat[len(scores):]:
        ax.set_visible(False)
    handles = [plt.Line2D([], [], color=c, lw=2.5, marker='o', ms=5)
               for _, _, c in series]
    fig.legend(handles, [lbl for _, lbl, _ in series], loc='upper right',
               ncol=3, fontsize=8.5, frameon=False,
               bbox_to_anchor=(0.99, 0.985))
    fig.suptitle('Controller actuation vs contention level, per score model\n',
                 fontsize=13, color=INK, x=0.02, ha='left')
    fig.text(0.02, 0.925,
             'duty = fraction of run the actuator is engaged '
             '(n>0 / cap<2 cores / slack<=0); a good driver grades '
             'monotonically with severity',
             fontsize=9, color=MUTED)
    fig.tight_layout(rect=(0, 0, 1, 0.90))
    fig.savefig(out, dpi=180, facecolor=SURFACE)
    plt.close(fig)
    print(f'[saved] {out}')


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--traces-dir', default='sim-traces')
    ap.add_argument('--dt', type=float, default=0.1)
    ap.add_argument('--out-dir', default='.')
    args = ap.parse_args()

    traces = load_traces(args.traces_dir, args.dt)
    if not traces:
        sys.exit(f'no <arm>_<score>_sim.json traces found in {args.traces_dir}')
    anchors = normalize(traces)

    print('normalization anchors (raw baseline -> 0, raw severe -> 1):')
    for score, (lo, hi) in anchors.items():
        tag = ' (passthrough)' if score in PASSTHROUGH else ''
        print(f'  {score:<15} lo={lo:.4f} hi={hi:.4f}{tag}')

    metrics = {}
    for score in traces:
        for arm, tr in traces[score].items():
            metrics[(score, arm)] = run_controllers(tr, args.dt)

    keys = ['sig_mean', 'horz_duty', 'horz_up', 'horz_switches',
            'iso_cap_mean', 'iso_squeeze', 'harv_h_mean', 'harv_release']
    csv_path = os.path.join(args.out_dir, 'driving_quality.csv')
    with open(csv_path, 'w', newline='') as f:
        w = csv.writer(f)
        w.writerow(['score', 'arm'] + keys)
        for score, _ in SCORES:
            for arm in ARMS:
                m = metrics.get((score, arm))
                if m:
                    w.writerow([score, arm] + [m[k] for k in keys])
    print(f'[saved] {csv_path}\n')

    hdr = f'{"score":<15} {"arm":<9} ' + ' '.join(f'{k:>13}' for k in keys)
    print(hdr)
    for score, _ in SCORES:
        for arm in ARMS:
            m = metrics.get((score, arm))
            if m:
                print(f'{score:<15} {arm:<9} ' +
                      ' '.join(f'{m[k]:>13.3f}' for k in keys))

    print('\nmonotonicity across arms (Spearman rank corr vs severity; '
          '1.0 = perfectly graded):')
    for score in traces:
        arms = [a for a in ARMS if (score, a) in metrics]
        line = []
        for key in ('sig_mean', 'horz_duty', 'iso_squeeze', 'harv_release'):
            rho = spearman([metrics[(score, a)][key] for a in arms])
            line.append(f'{key}={rho:.2f}' if rho == rho else f'{key}=flat')
        print(f'  {score:<15} ' + '  '.join(line))

    fig_signals(traces, os.path.join(args.out_dir, 'driving_signals.png'))
    fig_quality(metrics, traces, os.path.join(args.out_dir, 'driving_quality.png'))


if __name__ == '__main__':
    main()
