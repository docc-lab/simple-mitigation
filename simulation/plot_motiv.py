#!/usr/bin/env python3
"""Time-series figure for the stage-3 motivation experiment (2026-10-23):
one phased run per arm (none / hc-bang / horizontal-any) of
configs/motiv-ext-bursty.yaml, three rows per arm on a shared time axis:

  1. victim handler execution (processing_time p90, us)  -- the score input
  2. victim service latency   (total_time p90, ms)        -- what the user sees
  3. controller signal e_iso = y90*ext90 vs the trigger, with the actuator's
     state on the right axis (HC held 0/1, or overflow replicas n)

Bursts of the aggressor load are shaded (10 s on / 10 s off from t=20),
the ARM point is the dashed vertical line. Inputs are the run dirs the phase
orchestrator writes (runs/run_1/run_data.json, started/armed_epoch_ns.txt,
mitigation/*.scores.csv, mitigation/*.ctrl.log). Derived from
plot_real_run.py's loaders (same 1 s count-weighted reduction of the
per-window percentiles, same tick-mean score reduction).

  python3 simulation/plot_motiv.py --root <dir with none/ hc-bang/ horizontal-any/> \
      --theta 0.15 --out simulation/results/motiv_ext_bursty_timeseries.png
"""
import argparse
import csv
import glob
import json
import os
import re
from datetime import datetime

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import numpy as np

PALETTE = ['#0077BB', '#EE7733', '#009988', '#EE3377', '#997700', '#33BBEE', '#CC3311']
ARMS = [('none', 'no mitigation'),
        ('hc-bang', 'harvesting (HC, bang-bang)'),
        ('horizontal-any', 'replication (scale-out on the same trigger)')]


def ts_epoch(s):
    s = re.sub(r"\.(\d+)", lambda m: "." + (m.group(1) + "000000")[:6], s).replace("Z", "+00:00")
    return datetime.fromisoformat(s).timestamp()


def agg_1s(t, v, w):
    """Per-1s count-weighted mean of per-window percentiles."""
    if len(t) == 0:
        return t, v
    b = np.floor(t)
    ub = np.unique(b)
    out = np.array([np.average(v[b == x], weights=np.maximum(w[b == x], 1e-9)) for x in ub])
    return ub + 0.5, out


def tick_mean(t, v, dt=0.1):
    tick = np.round(t / dt) * dt
    ut = np.unique(tick)
    return ut, np.array([v[tick == u].mean() for u in ut])


def load_run(d):
    run = os.path.join(d, 'runs', 'run_1')
    mdir = os.path.join(run, 'mitigation')
    t0 = int(open(os.path.join(run, 'started_epoch_ns.txt')).read()) / 1e9
    arm_t = 80.0
    if os.path.isfile(os.path.join(mdir, 'armed_epoch_ns.txt')):
        arm_t = int(open(os.path.join(mdir, 'armed_epoch_ns.txt')).read()) / 1e9 - t0
    j = json.load(open(os.path.join(run, 'run_data.json')))
    t, pp50, pp90, tp50, tp90, rq = [], [], [], [], [], []
    for s in j['samples']:
        tw = s.get('timing_window') or {}
        if not tw.get('request_count'):
            continue
        t.append(ts_epoch(s['timestamp']) - t0)
        pp50.append(tw['processing_time']['p50_ns'] / 1e3)   # us
        pp90.append(tw['processing_time']['p90_ns'] / 1e3)
        tp50.append(tw['total_time']['p50_ns'] / 1e6)        # ms
        tp90.append(tw['total_time']['p90_ns'] / 1e6)
        rq.append(tw['request_count'])
    lat = dict(t=np.array(t), pp50=np.array(pp50), pp90=np.array(pp90),
               tp50=np.array(tp50), tp90=np.array(tp90), rq=np.array(rq, float))

    sc = dict(t=[], e_iso=[], y90=[], n=[], cap=[])
    for f in glob.glob(os.path.join(mdir, '*.scores.csv')):
        with open(f) as fh:
            for r in csv.DictReader(fh):
                try:
                    sc['t'].append(int(r['unix_ms']) / 1e3 - t0)
                    sc['e_iso'].append(float(r['e_iso']))
                    sc['y90'].append(float(r['y90']))
                    sc['n'].append(float(r['n']))
                    sc['cap'].append(float(r['applied_cap']))
                except (KeyError, ValueError):
                    continue
    sc = {k: np.array(v) for k, v in sc.items()}

    acts = []
    for f in glob.glob(os.path.join(mdir, '*.ctrl.log')):
        for line in open(f, errors='replace'):
            if '"formula: h' not in line:
                continue
            try:
                e = json.loads(line)
            except ValueError:
                continue
            acts.append((ts_epoch(e['time']) - t0, e.get('msg', '').replace('formula: ', ''), e))
    acts.sort(key=lambda a: a[0])
    return dict(lat=lat, sc=sc, acts=acts, arm_t=arm_t, t0=t0)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--root', required=True, help='dir containing <arm>/stage3eval_*/ for the three arms')
    ap.add_argument('--theta', type=float, default=0.15)
    ap.add_argument('--onset', type=float, default=20.0)
    ap.add_argument('--burst-s', type=float, default=10.0)
    ap.add_argument('--idle-s', type=float, default=10.0)
    ap.add_argument('--duration', type=float, default=140.0)
    ap.add_argument('--out', default=os.path.join(os.path.dirname(os.path.abspath(__file__)),
                                                  'results', 'motiv_ext_bursty_timeseries.png'))
    ap.add_argument('--dirs', default='',
                    help='override the per-arm subdir, "arm=subdir,..." '
                         '(e.g. horizontal-any=horizontal-any-node4)')
    args = ap.parse_args()
    subdir = {k: k for k, _ in ARMS}
    for spec in filter(None, args.dirs.split(',')):
        k, _, v = spec.partition('=')
        subdir[k] = v

    runs = []
    for key, title in ARMS:
        dirs = sorted(glob.glob(os.path.join(args.root, subdir[key], 'stage3eval_*')))
        if not dirs:
            raise SystemExit(f'no run dir under {args.root}/{key}')
        runs.append((key, title, load_run(dirs[-1])))

    bursts = []
    t = args.onset
    while t < args.duration:
        bursts.append((t, min(t + args.burst_s, args.duration)))
        t += args.burst_s + args.idle_s

    fig, axes = plt.subplots(3, len(runs), figsize=(4.6 * len(runs), 7.4), sharex=True,
                             sharey='row', gridspec_kw=dict(hspace=0.12, wspace=0.10))
    axes = np.atleast_2d(axes)
    for c, (key, title, R) in enumerate(runs):
        lat, sc, arm_t = R['lat'], R['sc'], R['arm_t']
        for r in range(3):
            ax = axes[r, c]
            for lo, hi in bursts:
                ax.axvspan(lo, hi, color='0.88', lw=0, zorder=0)
            ax.axvline(arm_t, ls='--', c='0.3', lw=0.9, zorder=1)
            ax.axvline(args.onset, ls=':', c='0.5', lw=0.8, zorder=1)
            ax.grid(axis='y', lw=0.4, alpha=0.5)
            ax.set_xlim(0, args.duration)

        # row 1: processing_time
        ax = axes[0, c]
        at, av = agg_1s(lat['t'], lat['pp90'], lat['rq'])
        ax.plot(at, av, lw=1.3, c=PALETTE[0], label='processing p90')
        ax.set_title(f'{title}\n({key})', fontsize=10)
        if c == 0:
            ax.set_ylabel('handler execution (µs)\n= score input')
            ax.legend(fontsize=7, loc='upper left')

        # row 2: total_time
        ax = axes[1, c]
        at, av = agg_1s(lat['t'], lat['tp90'], lat['rq'])
        ax.plot(at, av, lw=1.3, c=PALETTE[2], label='total p90')
        if c == 0:
            ax.set_ylabel('victim service latency (ms)')
            ax.legend(fontsize=7, loc='upper left')

        # row 3: score + actuator state
        ax = axes[2, c]
        if len(sc['t']):
            st, sv = tick_mean(sc['t'], sc['e_iso'])
            ax.plot(st, sv, lw=1.0, c=PALETTE[6], label='e_iso = y90·ext90')
        ax.axhline(args.theta, ls='--', c='gray', lw=0.8, label=f'θ = {args.theta}')
        ax.set_ylim(-0.03, 1.0)
        ax.set_xlabel('time since t0 (s)')
        if c == 0:
            ax.set_ylabel('controller signal')
        ax2 = ax.twinx()
        ax2.set_ylim(-0.15, 3.3)
        if key.startswith('hc'):
            if len(sc['t']):
                st, held = tick_mean(sc['t'], sc['cap'])
                ax2.step(st, held, where='post', lw=1.4, c=PALETTE[1], alpha=0.9, label='HC held')
            for ta, msg, _ in R['acts']:
                if msg == 'hc apply':
                    ax2.plot([ta], [1.08], marker='v', ms=5, c=PALETTE[1], ls='none')
            ax2.set_yticks([0, 1]); ax2.set_yticklabels(['off', 'on'], fontsize=8)
            if c == len(runs) - 1:
                ax2.set_ylabel('HC state', fontsize=9)
        elif key.startswith('horizontal'):
            if len(sc['t']):
                st, nv = tick_mean(sc['t'], sc['n'])
                ax2.step(st, nv, where='post', lw=1.4, c=PALETTE[1], alpha=0.9, label='overflow replicas n')
            ax2.set_yticks([0, 1, 2, 3])
            if c == len(runs) - 1:
                ax2.set_ylabel('overflow replicas', fontsize=9)
        else:
            ax2.set_yticks([])
        h1, l1 = ax.get_legend_handles_labels()
        h2, l2 = ax2.get_legend_handles_labels()
        ax.legend(h1 + h2, l1 + l2, fontsize=7, loc='upper left', ncol=1)

    # phase labels in the strip between the column title and the top axes
    for c, (key, title, R) in enumerate(runs):
        ax = axes[0, c]
        for lbl, a, b in [('p1', 0, args.onset), ('p2 contention, disarmed', args.onset, R['arm_t']),
                          ('p3 armed', R['arm_t'], args.duration)]:
            ax.text((a + b) / 2, 1.015, lbl, transform=ax.get_xaxis_transform(), ha='center',
                    va='bottom', fontsize=7.5, color='0.25', clip_on=False)
        ax.set_title(f'{title}\n({key})', fontsize=10, pad=16)
    fig.suptitle('Extrinsic bursty contention on the victim\'s SMT siblings (grey = aggressor bursts): '
                 'harvesting removes it, replication only hides it', fontsize=10.5, y=0.995)
    os.makedirs(os.path.dirname(os.path.abspath(args.out)), exist_ok=True)
    fig.savefig(args.out, dpi=160, bbox_inches='tight')
    root, _ = os.path.splitext(args.out)
    fig.savefig(root + '.pdf', bbox_inches='tight')
    print('wrote', args.out, 'and', root + '.pdf')


if __name__ == '__main__':
    main()
