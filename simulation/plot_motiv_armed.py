#!/usr/bin/env python3
"""Armed-phase-only view of the motivation experiment: the three arms
overlaid, two panels on one time axis (seconds since ARM):

  1. latency  -- victim per-window p90 (us), 1 s count-weighted mean
  2. p90 score -- the scorer's y90 (tanh of the smoothed p90 deviation)

Bursts of the aggressor load are shaded. Inputs/loaders as plot_motiv.py.

  python3 simulation/plot_motiv_armed.py --root <dir> --dirs horizontal-any=horizontal-any-node4
"""
import argparse
import glob
import os
import textwrap

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import numpy as np

from plot_motiv import ARMS, agg_1s, load_run, tick_mean

COLORS = {'none': '#555555', 'hc-bang': '#0077BB', 'horizontal-any': '#EE7733'}
LABELS = {'none': 'no mitigation', 'hc-bang': 'harvesting', 'horizontal-any': 'replication'}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--root', required=True)
    ap.add_argument('--dirs', default='', help='"arm=subdir,..." overrides')
    ap.add_argument('--onset', type=float, default=20.0)
    ap.add_argument('--burst-s', type=float, default=10.0)
    ap.add_argument('--idle-s', type=float, default=10.0)
    ap.add_argument('--duration', type=float, default=140.0)
    ap.add_argument('--pre', type=float, default=10.0, help='seconds shown before the ARM point')
    ap.add_argument('--title', default='Extrinsic bursty contention')
    ap.add_argument('--caption', default='', help='optional text under the figure')
    ap.add_argument('--out', default=os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                                  'results', 'motiv_ext_bursty_armed.png'))
    args = ap.parse_args()
    subdir = {k: k for k, _ in ARMS}
    for spec in filter(None, args.dirs.split(',')):
        k, _, v = spec.partition('=')
        subdir[k] = v

    runs = []
    for key, _ in ARMS:
        dirs = sorted(glob.glob(os.path.join(args.root, subdir[key], 'stage3eval_*')))
        if not dirs:
            raise SystemExit(f'no run dir under {args.root}/{subdir[key]}')
        runs.append((key, load_run(dirs[-1])))

    fig, (ax1, ax2) = plt.subplots(2, 1, figsize=(7.2, 4.6), sharex=True,
                                   gridspec_kw=dict(hspace=0.12))
    arm_ref = runs[0][1]['arm_t']
    x0, x1 = arm_ref - args.pre, args.duration
    # burst shading (bursts repeat every burst_s + idle_s from onset), time since t0
    period = args.burst_s + args.idle_s
    t = args.onset
    while t < args.duration:
        lo, hi = t, t + args.burst_s
        if hi > x0:
            for ax in (ax1, ax2):
                ax.axvspan(max(lo, x0), min(hi, x1), color='0.88', lw=0, zorder=0)
        t += period
    for ax in (ax1, ax2):
        ax.axvline(arm_ref, ls='--', c='0.3', lw=1.0, zorder=1)
    ax1.text(arm_ref + 0.6, 0.97, 'mitigation enabled', transform=ax1.get_xaxis_transform(),
             ha='left', va='top', fontsize=8, color='0.3')

    for key, R in runs:
        lat, sc, arm_t = R['lat'], R['sc'], R['arm_t']
        k = lat['t'] >= x0 - 1
        at, av = agg_1s(lat['t'][k], lat['pp90'][k], lat['rq'][k])
        ax1.plot(at, av, lw=1.5, c=COLORS[key], label=LABELS[key])
        m = sc['t'] >= x0 - 1
        st, sv = tick_mean(sc['t'][m], sc['y90'][m])
        ax2.plot(st, sv, lw=1.2, c=COLORS[key], label=LABELS[key])

    ax1.set_ylabel('latency p90 (µs)')
    ax1.grid(axis='y', lw=0.4, alpha=0.5)
    ax1.legend(fontsize=8, loc='lower center', ncol=3, bbox_to_anchor=(0.5, 1.0), frameon=False)
    ax1.set_title(args.title, fontsize=11, pad=22)
    ax2.set_ylabel('p90 score')
    ax2.set_ylim(-0.03, 1.05)
    ax2.grid(axis='y', lw=0.4, alpha=0.5)
    ax2.set_xlabel('time (s)')
    ax2.set_xlim(x0, x1)
    if args.caption:
        fig.text(0.5, -0.02, textwrap.fill(args.caption, 118), ha='center', va='top', fontsize=8, color='0.2')
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    fig.savefig(args.out, dpi=160, bbox_inches='tight')
    fig.savefig(os.path.splitext(args.out)[0] + '.pdf', bbox_inches='tight')
    print('wrote', args.out)


if __name__ == '__main__':
    main()
