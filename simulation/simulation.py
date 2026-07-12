#!/usr/bin/env python3
"""
Gordion mitigation controllers — minimal control formulas.

Three controllers (no vertical):
  • Horizontal : on p50  — bang-bang +/-1 replicas with counter guard (Eq. 1)
  • Isolating  : on e_iso = p90·%ext,90 — saturated proportional -> aggressor
                 core cap (Eq. 2); %ext,90 defaults to 1 when absent from data
  • Harvesting : on p90  — asymmetric AIMD on slack (theta_safe - score_tail)

Signal assignment rationale (validated on real data):
  • p50 is sparse/spiky (~7% of time > 0.5) — drives expensive scale-out only
    when contention spills into the median (real SLO violation)
  • p90 is broad/sustained (~65% > 0.5) — drives fast, cheap actuators
    (cgroup squeeze) and protects harvesting tail headroom

Layout (relative to this script):
  data/     raw score-curve JSON (input)
  results/  generated figures (output)

Data naming: data/{arm}_{score}_sim.json, where arm is the experiment
(baseline, mildA..C, moderate, severe) and score is the scoring algorithm
(binary, ci, cpi, kg, rolling_pctl, slowdown_ratio).  spike/staircase are
stitched traces without a score suffix.

Usage:
  python simulation.py                          # synthetic signals
  python simulation.py --data run.json          # single-run parameter sweeps
                                                #   (looked up in data/ too)
  python simulation.py --compare                # one score (kg) across arms
  python simulation.py --compare a.json b.json  # explicit curve list
  python simulation.py --compare-scores         # all scores on the same arm,
                                                #   one figure per arm
  python simulation.py --compare-scores severe  # just that arm
"""

import argparse
import glob as glob_module
import json
import os
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.gridspec as gridspec
from scipy.ndimage import gaussian_filter1d

SIM_DIR     = os.path.dirname(os.path.abspath(__file__))
DATA_DIR    = os.path.join(SIM_DIR, 'data')
RESULTS_DIR = os.path.join(SIM_DIR, 'results')


def _resolve_data(path):
    """Accept a bare filename and look it up in data/ as a fallback."""
    if os.path.exists(path):
        return path
    alt = os.path.join(DATA_DIR, path)
    return alt if os.path.exists(alt) else path


def _out(name):
    """Figure output path under results/ (created on demand)."""
    os.makedirs(RESULTS_DIR, exist_ok=True)
    return os.path.join(RESULTS_DIR, name)


# ═══════════════════════════════════════════════════════════════════════════════
# 1.  SIGNAL LOADING
# ═══════════════════════════════════════════════════════════════════════════════

def load_json(path, dt=0.1):
    """Load Gordion-format JSON; resample to uniform `dt` (100ms default)."""
    with open(path) as f:
        data = json.load(f)
    samples = data['samples']
    def _f(key, default=0.0):
        return np.array([(s.get(key) if s.get(key) is not None else default)
                         for s in samples], dtype=float)
    t_raw   = _f('offset_ms') / 1000.0
    p50_raw = _f('p50_contention_score')
    p90_raw = _f('p90_contention_score')
    # extrinsic tail fraction %ext,90 for e_iso (Eq. 2); 1.0 when the field
    # is absent (all tail degradation attributed to interference)
    ext_raw = _f('p90_extrinsic_pct', 1.0)
    arr_raw = np.array([(s['timing_window'].get('arrival_rps_1s') or 0)
                        for s in samples], dtype=float)
    # Normalize start to 0, resample to uniform grid
    t_norm = t_raw - t_raw.min()
    t      = np.arange(0, t_norm.max(), dt)
    p50    = np.interp(t, t_norm, p50_raw)
    p90    = np.interp(t, t_norm, p90_raw)
    ext90  = np.interp(t, t_norm, ext_raw)
    arr    = np.interp(t, t_norm, arr_raw)
    return t, arr, p50, p90, ext90, dt, data.get('service_name', 'unknown')


def make_synthetic(T=80.0, dt=0.1, seed=42):
    """
    Synthetic signals approximating real-data shape: sparse p50, broad p90.
    p50 = 2-3 isolated severe spikes; p90 = ~8 bursts of varying amplitude.
    """
    rng = np.random.default_rng(seed)
    t   = np.arange(0, T, dt)
    N   = len(t)
    arr = 100 + 10 * rng.standard_normal(N)
    arr[:int(2/dt)] = np.linspace(5, 105, int(2/dt))
    arr = np.clip(arr, 0, None)

    # p90 windows: (start, end, peak)
    p90_windows = [
        (2.5, 8.5, 0.85), (20.0, 33.0, 0.95), (38.0, 44.0, 0.92),
        (47.0, 60.0, 0.90), (62.0, 65.0, 0.80), (76.0, 80.0, 0.95),
    ]
    p90_raw = np.zeros(N)
    for s, e, amp in p90_windows:
        p90_raw[(t >= s) & (t < e)] = amp
    p90 = np.clip(gaussian_filter1d(p90_raw, sigma=1.5)
                  + 0.04*rng.standard_normal(N), 0, 1)

    # p50 spikes: only 2-3 brief severe events
    p50_windows = [(48.0, 51.0, 1.00), (77.0, 80.0, 1.00)]
    p50_raw = np.zeros(N)
    for s, e, amp in p50_windows:
        p50_raw[(t >= s) & (t < e)] = amp
    p50 = np.clip(gaussian_filter1d(p50_raw, sigma=1.0)
                  + 0.02*rng.standard_normal(N), 0, 1)
    return t, arr, p50, p90, np.ones(N), dt, 'synthetic'


# ═══════════════════════════════════════════════════════════════════════════════
# 2.  CONTROLLERS
# ═══════════════════════════════════════════════════════════════════════════════

def traffic_weighted(p, arr):
    """Per-endpoint -> service-level via traffic-share weighting."""
    if p.ndim == 1:
        return p.copy()
    denom = arr.sum(axis=1, keepdims=True).clip(min=1e-9)
    return ((arr / denom) * p).sum(axis=1)


def ctrl_horizontal(p50_svc, theta_on, theta_off, h_steps, n_max=10):
    """
    Eq. (1): bang-bang on the predicted median score ŷ50 with replica
    counter n(t) ∈ {0..n_max} guarding scale-down.

    Models the predictor as emitting, at each tick t, ONE scalar prediction
    ŷ50(t) of y50 at the future time t + Δ_horz:

        ŷ50(t) = y50(t + Δ_horz)       (perfect predictor at this horizon)

                 ⎧ +1  if ŷ50(t) > θ_on  ∧  n(t) < n_max            (anticipate)
    u_horz(t) =  ⎨ -1  if y50(t) < θ_off ∧ ŷ50(t) < θ_on ∧ n(t) > 0 (confirm idle)
                 ⎩  0  otherwise

    n(t+1) = min(n_max, max(0, n(t) + u_horz(t)))

    Asymmetric scale-up/scale-down logic encodes pod-lifecycle cost asymmetry:
    spinning up is slow and expensive (anticipate from prediction); tearing
    down is fast and reversible (wait for both current AND predicted to be
    calm before releasing capacity).

    Note: lead time Δ_horz = h_steps * dt is the predictor's horizon.  Spikes
    of duration < Δ_horz can be "looked past" — at t the prediction shows
    post-spike calm while the spike has not yet arrived.  Choose Δ_horz no
    longer than the shortest contention event the controller must cover.
    """
    N = len(p50_svc)
    cmd     = np.zeros(N, dtype=int)
    n_extra = np.zeros(N, dtype=int)
    n_cur   = 0
    for i in range(N):
        h      = min(i + h_steps, N - 1)
        p_pred = p50_svc[h]
        p_now  = p50_svc[i]
        if   p_pred > theta_on  and n_cur < n_max:
            cmd[i] = +1
        elif p_now < theta_off and p_pred < theta_on and n_cur > 0:
            cmd[i] = -1
        n_cur      = int(np.clip(n_cur + cmd[i], 0, n_max))
        n_extra[i] = n_cur
    return cmd, n_extra


def ctrl_isolating(e_iso, theta_ref, k_p, cap_base, cap_min, h_steps=0):
    """
    Eq. (2): saturated proportional controller on the extrinsic tail
    magnitude e_iso(t) = y90(t) · %ext,90(t) → core cap on aggressor cgroup.

      c(t) = min(c_base, max(c_min, c_base - k_p · (e_iso(t) - θ_ref)))

    c(t) is bounded by the nominal allocation c_base and a liveness floor
    c_min, and written as cgroup v2 quota c(t)·P per period P = 100,000 μs
    (see cores_to_cpu_max).  The %ext,90 factor throttles aggressors only
    when tail degradation is attributable to interference rather than the
    victim's own load; it is folded into e_iso by the caller.
    """
    assert cap_base >= cap_min
    N = len(e_iso)
    cap = np.zeros(N)
    for i in range(N):
        h = min(i + h_steps, N - 1)
        cap[i] = min(cap_base, max(cap_min, cap_base - k_p * (e_iso[h] - theta_ref)))
    return cap


def cores_to_cpu_max(cap_cores, period_us=100000, min_quota_us=1000):
    """
    Actuator mapping: cores -> cgroup v2 `cpu.max` string "<quota> <period>".
    Floor on quota prevents pathological hard-pause (cap_cores=0).
    """
    quota_us = max(min_quota_us, int(round(cap_cores * period_us)))
    return f"{quota_us} {period_us}"


def ctrl_harvesting(p90_svc, theta_safe, alpha, beta, delta, h_steps):
    """
    Asymmetric AIMD on slack = θ_safe - score_tail.

      slack ≤ 0      → h ← β·h          (safety release, multiplicative)
      slack > δ      → h ← h + α        (probe, additive)
      else (deadband)→ h unchanged       (hold)

    α is per-sample increment; with dt=0.1, α=0.05 → 0.5 cores/second probe rate.
    """
    N = len(p90_svc)
    score_tail = np.zeros(N)
    slack      = np.zeros(N)
    h_cores    = np.zeros(N)
    h_cur = 0.0
    for i in range(N):
        hp = min(i + h_steps, N - 1)
        score_tail[i] = p90_svc[hp]
        slack[i]      = theta_safe - score_tail[i]
        if   slack[i] <= 0:    h_cur = beta * h_cur
        elif slack[i] >  delta: h_cur = h_cur + alpha
        h_cores[i] = h_cur
    return h_cores, score_tail, slack


# ═══════════════════════════════════════════════════════════════════════════════
# 3.  FIGURES
# ═══════════════════════════════════════════════════════════════════════════════

def _p50_panel(ax, t, p50, thresholds=None):
    ax.plot(t, p50, c='steelblue', lw=0.85, label='p50_svc')
    ax.fill_between(t, 0, p50, color='steelblue', alpha=0.15, lw=0)
    if thresholds:
        for v, lbl, c, ls in thresholds:
            ax.axhline(v, ls=ls, lw=0.7, c=c, alpha=0.8, label=lbl)
    ax.set_ylabel('p50 [0–1]', fontsize=8)
    ax.set_ylim(-0.05, 1.05)
    ax.legend(fontsize=6, ncol=4)


def _p90_panel(ax, t, p90, thresholds=None, label='p90_svc'):
    ax.plot(t, p90, c='navy', lw=0.85, label=label)
    ax.fill_between(t, 0, p90, color='navy', alpha=0.15, lw=0)
    if thresholds:
        for v, lbl, c, ls in thresholds:
            ax.axhline(v, ls=ls, lw=0.7, c=c, alpha=0.8, label=lbl)
    ax.set_ylabel(f'{label} [0–1]', fontsize=8)
    ax.set_ylim(-0.05, 1.05)
    ax.legend(fontsize=6, ncol=4)


def fig_horizontal(t, p50_svc, dt, h_horz_unused, tag=''):
    """
    Sweep predictor lead time Δ_horz ∈ {0.1, 0.5, 1.0, 3.0, 5.0} s.
    Default thresholds: θ_on=0.3, θ_off=0.1.

    Shows the trade-off: short lead reacts late but never "looks past" the
    spike; long lead anticipates but risks acting on post-spike calm if the
    spike is shorter than the lead time.
    """
    THETA_ON, THETA_OFF = 0.3, 0.1
    leads_s = [0.1, 0.5, 1.0, 3.0, 5.0]
    colors  = plt.cm.viridis(np.linspace(0.05, 0.85, len(leads_s)))

    fig, axes = plt.subplots(3, 1, figsize=(14, 9), sharex=True)
    fig.suptitle(f'Horizontal scaler — lead-time sweep  |  signal=p50,  '
                 f'θ_on={THETA_ON}, θ_off={THETA_OFF}, n_max=10{tag}',
                 fontsize=10)

    th = [(THETA_ON, f"θ_on={THETA_ON}", 'orange', '--'),
          (THETA_OFF, f"θ_off={THETA_OFF}", 'gray', ':')]
    _p50_panel(axes[0], t, p50_svc, thresholds=th)

    for lead_s, col in zip(leads_s, colors):
        h_steps = max(1, int(round(lead_s / dt)))
        cmd, _  = ctrl_horizontal(p50_svc, THETA_ON, THETA_OFF, h_steps)
        axes[1].plot(t, cmd, lw=0.9, alpha=0.85, c=col,
                     label=f'Δ_horz={lead_s:.1f}s')
    axes[1].set_yticks([-1, 0, 1])
    axes[1].set_ylabel('cmd')
    axes[1].legend(fontsize=7, ncol=5, loc='upper left')
    axes[1].set_title('Command output (vertical separation = sample timing offset)',
                      fontsize=8)

    for lead_s, col in zip(leads_s, colors):
        h_steps = max(1, int(round(lead_s / dt)))
        _, n    = ctrl_horizontal(p50_svc, THETA_ON, THETA_OFF, h_steps)
        axes[2].plot(t, n, lw=0.95, alpha=0.85, c=col,
                     label=f'Δ_horz={lead_s:.1f}s')
    axes[2].set_ylabel('n_extra')
    axes[2].set_xlabel('Time (s)')
    axes[2].legend(fontsize=7, ncol=5, loc='upper left')
    axes[2].set_title('Replica counter n(t)', fontsize=8)

    plt.tight_layout()
    out = _out('sweep_horizontal.png')
    fig.savefig(out, dpi=130)
    print(f'[saved] {out}')
    plt.close(fig)

    # numerical summary
    print('\n── Horizontal lead-time sweep stats ──')
    for lead_s in leads_s:
        h_steps = max(1, int(round(lead_s / dt)))
        cmd, n  = ctrl_horizontal(p50_svc, THETA_ON, THETA_OFF, h_steps)
        print(f'  Δ_horz={lead_s:>4.1f}s: +1={(cmd==1).sum():3d} '
              f'-1={(cmd==-1).sum():3d}  n_max reached: {n.max():2d}  '
              f'time with n>0: {(n>0).mean():.1%}')


def fig_isolating(t, e_iso, dt, tag=''):
    """Sweep θ_ref, k_p, cap bounds on e_iso (Eq. 2)."""
    combos = [
        dict(ref=0.3, kp=6.4,  b=4.0, m=0.5, label='θ_ref=0.3, k_p=6.4, [0.5..4]  (default)'),
        dict(ref=0.3, kp=12.0, b=4.0, m=0.5, label='θ_ref=0.3, k_p=12,  [0.5..4]  (steeper)'),
        dict(ref=0.5, kp=6.4,  b=4.0, m=1.0, label='θ_ref=0.5, k_p=6.4, [1.0..4]  (later + gentler floor)'),
        dict(ref=0.3, kp=6.4,  b=8.0, m=0.5, label='θ_ref=0.3, k_p=6.4, [0.5..8]  (bigger aggressor)'),
    ]
    fig, axes = plt.subplots(3, 1, figsize=(14, 9), sharex=True)
    fig.suptitle(f'Isolating controller sweep  |  signal=e_iso=y90·%ext,  '
                 f'saturated proportional (Eq. 2){tag}', fontsize=10)

    th = [(0.3, 'θ_ref=0.3', 'green', '--'), (0.5, 'θ_ref=0.5', 'red', '--')]
    _p90_panel(axes[0], t, e_iso, thresholds=th, label='e_iso')

    for c in combos:
        cap = ctrl_isolating(e_iso, c['ref'], c['kp'], c['b'], c['m'])
        axes[1].plot(t, cap, lw=0.9, alpha=0.85, label=c['label'])
    axes[1].set_ylabel('cap (cores)')
    axes[1].legend(fontsize=7)
    axes[1].set_title('Cap on aggressor cores', fontsize=8)

    # Translate one combo to actual cpu.max strings (sample every ~5s)
    cap_ref = ctrl_isolating(e_iso, 0.3, 6.4, 4.0, 0.5)
    samp_idx = np.arange(0, len(t), int(5/dt))
    sampled_caps = cap_ref[samp_idx]
    sampled_quota = [int(round(c * 100000)) for c in sampled_caps]
    axes[2].step(t[samp_idx], sampled_quota, where='post', c='darkred', lw=1.1,
                 label='cgroup quota (μs / 100ms period)')
    axes[2].axhline(100000, ls=':', c='gray', lw=0.6, label='1 core baseline')
    axes[2].axhline(400000, ls=':', c='green', lw=0.6, label='cap_baseline=4 cores')
    axes[2].axhline(50000,  ls=':', c='red',   lw=0.6, label='cap_min=0.5 cores')
    axes[2].set_ylabel('cpu.max quota (μs)')
    axes[2].set_xlabel('Time (s)')
    axes[2].legend(fontsize=7)
    axes[2].set_title('Actuator mapping: cores → cpu.max quota (default combo, 5s sampling)', fontsize=8)

    plt.tight_layout()
    out = _out('sweep_isolating.png')
    fig.savefig(out, dpi=130)
    print(f'[saved] {out}')
    plt.close(fig)


def fig_harvesting(t, p90_svc, dt, h_harv, tag=''):
    """Sweep θ_safe, α, β on p90."""
    combos = [
        dict(s=0.70, a=0.05, b=0.5,  label='θ_safe=0.70, α=0.05 (0.5/s), β=0.5  (default)'),
        dict(s=0.85, a=0.05, b=0.5,  label='θ_safe=0.85, α=0.05 (0.5/s), β=0.5  (only severe releases)'),
        dict(s=0.70, a=0.10, b=0.3,  label='θ_safe=0.70, α=0.10 (1.0/s), β=0.3  (fast probe, fast release)'),
        dict(s=0.70, a=0.03, b=0.5,  label='θ_safe=0.70, α=0.03 (0.3/s), β=0.5  (slow probe)'),
    ]
    fig, axes = plt.subplots(4, 1, figsize=(14, 11), sharex=True)
    fig.suptitle(f'Harvesting AIMD sweep  |  signal=p90,  Δ_harv={h_harv*dt:.1f}s{tag}',
                 fontsize=10)

    th = [(c['s'], f"θ_safe={c['s']}", f"C{i}", '--') for i, c in enumerate(combos[:2])]
    _p90_panel(axes[0], t, p90_svc, thresholds=th)

    for c in combos:
        hc, _, _ = ctrl_harvesting(p90_svc, c['s'], c['a'], c['b'], 0.05, h_harv)
        axes[1].plot(t, hc, lw=0.9, alpha=0.85, label=c['label'])
    axes[1].set_ylabel('h(t) cores')
    axes[1].legend(fontsize=7)
    axes[1].set_title('Harvested cores', fontsize=8)

    for c in combos:
        _, _, sl = ctrl_harvesting(p90_svc, c['s'], c['a'], c['b'], 0.05, h_harv)
        axes[2].plot(t, sl, lw=0.9, alpha=0.85, label=c['label'])
    axes[2].axhline(0, ls=':', c='black', lw=0.5)
    axes[2].set_ylabel('slack s(t)')
    axes[2].legend(fontsize=7)
    axes[2].set_title('Slack signal', fontsize=8)

    # Phase relationship for the default combo
    ax3b = axes[3].twinx()
    axes[3].fill_between(t, 0, p90_svc, color='navy', alpha=0.15, lw=0)
    axes[3].plot(t, p90_svc, c='navy', lw=0.7, alpha=0.7)
    axes[3].set_ylabel('p90_svc', fontsize=8, color='navy')
    axes[3].set_ylim(0, 1.3)
    hc_ref, _, _ = ctrl_harvesting(p90_svc, 0.7, 0.05, 0.5, 0.05, h_harv)
    ax3b.plot(t, hc_ref, c='brown', lw=1.0)
    ax3b.set_ylabel('h(t)', fontsize=8, color='brown')
    axes[3].set_xlabel('Time (s)')
    axes[3].set_title('Phase: p90 (blue, left) vs harvested cores (brown, right)  — default combo',
                     fontsize=8)

    plt.tight_layout()
    out = _out('sweep_harvesting.png')
    fig.savefig(out, dpi=130)
    print(f'[saved] {out}')
    plt.close(fig)


def fig_reference(t, arr, p50_svc, p90_svc, e_iso, dt, h_horz, h_iso, h_harv, tag=''):
    """Full 3-controller stack at default settings."""
    THETA_ON, THETA_OFF = 0.3, 0.1
    THETA_REF, K_P      = 0.3, 6.4
    CAP_BASE, CAP_MIN   = 4.0, 0.5
    TH_SAFE, ALPHA, BETA, DELTA = 0.7, 0.05, 0.5, 0.05

    cmd, n_extra = ctrl_horizontal(p50_svc, THETA_ON, THETA_OFF, h_horz)
    cap          = ctrl_isolating (e_iso, THETA_REF, K_P, CAP_BASE, CAP_MIN, h_iso)
    hc, st, sl   = ctrl_harvesting(p90_svc, TH_SAFE, ALPHA, BETA, DELTA, h_harv)

    fig, axes = plt.subplots(7, 1, figsize=(14, 16), sharex=True)
    fig.suptitle(
        f'Reference run{tag}  |  Horz(p50,θ={THETA_ON}/{THETA_OFF})  '
        f'Iso(e_iso,θ_ref={THETA_REF},k_p={K_P},cap=[{CAP_MIN}..{CAP_BASE}])  '
        f'Harv(p90,θ_safe={TH_SAFE},α={ALPHA},β={BETA})',
        fontsize=9)

    # 0: arrival
    axes[0].fill_between(t, 0, arr, color='green', alpha=0.25, lw=0)
    axes[0].plot(t, arr, c='green', lw=0.6)
    axes[0].set_ylabel('Arrival\n(rps)')

    # 1: signals
    axes[1].plot(t, p50_svc, c='steelblue', lw=0.85, label='p50_svc (→ horizontal)')
    axes[1].plot(t, p90_svc, c='navy',      lw=0.85, alpha=0.8, label='p90_svc (→ iso + harv)')
    axes[1].axhline(THETA_ON,  ls='--', c='orange', lw=0.6, alpha=0.6)
    axes[1].axhline(THETA_REF, ls='--', c='red',    lw=0.6, alpha=0.6)
    axes[1].axhline(TH_SAFE,   ls=':',  c='brown',  lw=0.6, alpha=0.6)
    axes[1].set_ylabel('Score')
    axes[1].set_ylim(-0.05, 1.05)
    axes[1].legend(fontsize=7)

    # 2: horizontal cmd + n
    ax2b = axes[2].twinx()
    axes[2].step(t, cmd, c='darkred', where='post', lw=0.9, label='cmd')
    axes[2].set_yticks([-1, 0, 1])
    axes[2].set_ylabel('cmd', color='darkred')
    ax2b.step(t, n_extra, c='firebrick', where='post', lw=0.8, ls='--', alpha=0.7)
    ax2b.set_ylabel('n_extra', color='firebrick')
    ax2b.set_ylim(bottom=0)
    axes[2].set_title('Horizontal: ±1 replica commands and counter', fontsize=8)

    # 3: isolating cap
    axes[3].plot(t, cap, c='darkred', lw=0.9, label='cap (cores)')
    axes[3].axhline(CAP_BASE, ls=':', c='green', lw=0.6, label=f'baseline={CAP_BASE}')
    axes[3].axhline(CAP_MIN,  ls=':', c='red',   lw=0.6, label=f'min={CAP_MIN}')
    axes[3].set_ylabel('Cores')
    axes[3].legend(fontsize=7)
    axes[3].set_title('Isolating: aggressor core cap (saturated proportional on e_iso, Eq. 2)', fontsize=8)

    # 4: cpu.max quota equivalent (the actuator output)
    quota = np.array([int(round(c * 100000)) for c in cap])
    axes[4].step(t, quota, where='post', c='maroon', lw=0.85)
    axes[4].axhline(100000, ls=':', c='gray',  lw=0.5, label='1 core')
    axes[4].set_ylabel('quota (μs)')
    axes[4].legend(fontsize=7)
    axes[4].set_title('Actuator: cgroup v2 cpu.max quota (period=100000μs)', fontsize=8)

    # 5: harvested cores
    axes[5].plot(t, hc, c='brown', lw=0.95)
    axes[5].set_ylabel('h(t) cores')
    axes[5].set_title('Harvesting: cores released to best-effort', fontsize=8)

    # 6: slack
    axes[6].plot(t, sl, c='gray', lw=0.85, label='slack')
    axes[6].plot(t, st, c='navy', lw=0.6, alpha=0.5, label='score_tail')
    axes[6].axhline(0, ls=':', c='black', lw=0.5)
    axes[6].axhline(DELTA, ls='--', c='gray', lw=0.5, alpha=0.5, label=f'δ={DELTA}')
    axes[6].set_ylabel('Slack')
    axes[6].set_xlabel('Time (s)')
    axes[6].legend(fontsize=7)

    plt.tight_layout()
    out = _out('ctrl_reference_run.png')
    fig.savefig(out, dpi=130)
    print(f'[saved] {out}')

    # numeric summary
    print(f'\n── Reference run summary{tag} ──')
    print(f'  Signals: p50 mean={p50_svc.mean():.3f} (>0.3: {(p50_svc>0.3).mean():.1%})  '
          f'p90 mean={p90_svc.mean():.3f} (>0.7: {(p90_svc>0.7).mean():.1%})')
    print(f'  Horizontal: +1={(cmd==1).sum():3d} ({(cmd==1).mean():.1%})  '
          f'-1={(cmd==-1).sum():3d} ({(cmd==-1).mean():.1%})  n_max reached: {n_extra.max()}')
    print(f'  Isolating : cap range [{cap.min():.2f}, {cap.max():.2f}] cores  '
          f'mean={cap.mean():.2f}  fraction squeezed (cap<2): {(cap<2).mean():.1%}')
    print(f'  Harvesting: h(t) max={hc.max():.2f} cores  mean={hc.mean():.2f}  '
          f'safety releases: {(sl<=0).sum()} ({(sl<=0).mean():.1%})')
    plt.close(fig)


# ═══════════════════════════════════════════════════════════════════════════════
# 4.  CROSS-SCORE COMPARISON (one figure per controller, one curve per file)
# ═══════════════════════════════════════════════════════════════════════════════

# Fixed-order categorical palette (Tol-vibrant-based, CVD-validated: worst
# adjacent ΔE 20.1); slot i always belongs to dataset i regardless of how
# many are loaded.
PALETTE8 = ['#0077BB', '#EE7733', '#009988', '#EE3377',
            '#997700', '#33BBEE', '#CC3311', '#AA4499']


ARM_ORDER = ['baseline', 'mildA', 'mildB', 'mildC', 'moderate', 'severe']


def _split_stem(path):
    """'mildA_kg_sim.json' -> ('mildA', 'kg'); 'spike_sim.json' -> ('spike', None)."""
    stem = os.path.basename(path)
    if stem.endswith('_sim.json'):
        stem = stem[:-len('_sim.json')]
    else:
        stem = os.path.splitext(stem)[0]
    if '_' in stem:
        arm, score = stem.split('_', 1)
        return arm, score
    return stem, None


def _dataset_label(path):
    """Arm name: 'mildA_kg_sim.json' -> 'mildA', 'spike_sim.json' -> 'spike'."""
    return _split_stem(path)[0]


def discover_matrix():
    """data/{arm}_{score}_sim.json -> {arm: {score: path}}."""
    mat = {}
    for p in sorted(glob_module.glob(os.path.join(DATA_DIR, '*_sim.json'))):
        arm, score = _split_stem(p)
        if score is not None:
            mat.setdefault(arm, {})[score] = p
    return mat


def score_gains(mat, dt=0.1):
    """
    Per-score, per-series gains for --scale: scaled(t) = k * s(t), no offset.
    k is the smallest correction that brings an out-of-range series back into
    [0,1]: k = min(1, 1 / max(series over every arm)), computed separately
    for the p50 series (Eq. 1 signal) and the p90-based e_iso series (Eq. 2
    signal).  A series already inside [0,1] keeps k = 1 (untouched).
    Returns {score: (k50, k90, max50, max90)}.
    """
    maxes = {}
    for by_score in mat.values():
        for score, path in by_score.items():
            t, arr, p50, p90, ext90, _, _ = load_json(path, dt=dt)
            m50 = traffic_weighted(p50, arr).max()
            m90 = (traffic_weighted(p90, arr) * ext90).max()
            c50, c90 = maxes.get(score, (0.0, 0.0))
            maxes[score] = (max(c50, m50), max(c90, m90))
    return {s: (min(1.0, 1.0 / m50) if m50 > 0 else 1.0,
                min(1.0, 1.0 / m90) if m90 > 0 else 1.0,
                m50, m90)
            for s, (m50, m90) in maxes.items()}


def scale_datasets(datasets, gains):
    """Apply the per-series gains in place; labels must be score names."""
    for d in datasets:
        k50, k90 = gains[d['label']][:2]
        d['p50']   = d['p50'] * k50
        d['e_iso'] = d['e_iso'] * k90
        d['k50'], d['k90'] = k50, k90


def load_compare(paths, dt=0.1, labels=None):
    """Load each JSON as one dataset dict; time normalized to start at 0."""
    datasets = []
    for i, p in enumerate(paths):
        t, arr, p50, p90, ext90, _, svc = load_json(p, dt=dt)
        datasets.append(dict(
            label=labels[i] if labels else _dataset_label(p),
            t=t,
            p50=traffic_weighted(p50, arr),
            e_iso=traffic_weighted(p90, arr) * ext90,
        ))
    return datasets


SCALE_NOTE = '  ·  scores scaled into [0,1]: k·score, k = min(1, 1/max) per series'


def _input_ylim(ax, datasets, key):
    lo = min(0.0, min(d[key].min() for d in datasets))
    hi = max(1.0, max(d[key].max() for d in datasets))
    ax.set_ylim(lo - 0.05, hi + 0.05)


def fig_compare_horizontal(datasets, dt, h_horz, theta_on=0.3, theta_off=0.1,
                           n_max=10, scaled=False):
    """
    All score curves' p50 + the Eq. (1) replica response, one figure.
    The response is drawn as one small-multiple strip per curve: the
    bang-bang counter saturates at n_max on heavily-contended curves, so
    overlaid steps would collapse onto a single occluded line.
    """
    THETA_ON, THETA_OFF, N_MAX = theta_on, theta_off, n_max
    K = len(datasets)

    fig, axes = plt.subplots(1 + K, 1, figsize=(14, 4.5 + 0.85 * K), sharex=True,
                             gridspec_kw=dict(height_ratios=[4.5] + [1] * K))
    fig.suptitle(f'Horizontal scaling across score curves  |  bang-bang: '
                 f'θ_on={THETA_ON}, θ_off={THETA_OFF}, n_max={N_MAX}, '
                 f'Δ_horz={h_horz*dt:.1f}s'
                 + (SCALE_NOTE if scaled else ''), fontsize=10)

    print('\n── Horizontal cross-score comparison ──')
    for k, (d, col) in enumerate(zip(datasets, PALETTE8)):
        axes[0].plot(d['t'], d['p50'], c=col, lw=1.0, alpha=0.85, label=d['label'])
        cmd, n = ctrl_horizontal(d['p50'], THETA_ON, THETA_OFF, h_horz, N_MAX)
        ax = axes[1 + k]
        ax.step(d['t'], n, where='post', c=col, lw=1.1)
        ax.fill_between(d['t'], 0, n, step='post', color=col, alpha=0.15, lw=0)
        ax.set_ylim(-0.4, N_MAX + 0.9)
        ax.set_yticks([0, N_MAX])
        ax.tick_params(labelsize=6)
        ax.set_ylabel(d['label'], fontsize=7, rotation=0, ha='right', va='center')
        print(f"  {d['label']:>10s}: p50>θ_on {(d['p50']>THETA_ON).mean():6.1%}  "
              f"+1={(cmd==1).sum():3d}  -1={(cmd==-1).sum():3d}  "
              f"peak n={n.max():2d}  time n>0: {(n>0).mean():.1%}")

    axes[0].axhline(THETA_ON,  ls='--', c='orange', lw=0.7, alpha=0.8, label=f'θ_on={THETA_ON}')
    axes[0].axhline(THETA_OFF, ls=':',  c='gray',   lw=0.7, alpha=0.8, label=f'θ_off={THETA_OFF}')
    axes[0].set_ylabel('score (scaled)' if scaled else 'score', fontsize=8)
    _input_ylim(axes[0], datasets, 'p50')
    axes[0].legend(fontsize=7, ncol=5, loc='lower right')
    axes[0].set_title('Input: p50 score per experiment (horizontal-scaling signal)', fontsize=8)

    axes[1].set_title(f'Response: replica counter n(t) per curve  (0..{N_MAX})', fontsize=8)
    axes[-1].set_xlabel('Time (s)')

    plt.tight_layout()
    out = _out('compare_horizontal_scaled.png' if scaled else 'compare_horizontal.png')
    fig.savefig(out, dpi=130)
    print(f'[saved] {out}')
    plt.close(fig)


def fig_compare_isolating(datasets, dt, theta_ref=0.3, k_p=6.4,
                          cap_base=4.0, cap_min=0.5, scaled=False):
    """All score curves' e_iso + the Eq. (2) cap response, one figure."""
    THETA_REF, K_P, CAP_BASE, CAP_MIN = theta_ref, k_p, cap_base, cap_min

    fig, axes = plt.subplots(2, 1, figsize=(14, 7.5), sharex=True)
    fig.suptitle(f'Isolation across score curves  |  saturated proportional: '
                 f'θ_ref={THETA_REF}, k_p={K_P}, cap=[{CAP_MIN}..{CAP_BASE}] cores'
                 + (SCALE_NOTE if scaled else ''), fontsize=10)

    print('\n── Isolation cross-score comparison ──')
    for d, col in zip(datasets, PALETTE8):
        axes[0].plot(d['t'], d['e_iso'], c=col, lw=1.0, alpha=0.85, label=d['label'])
        cap = ctrl_isolating(d['e_iso'], THETA_REF, K_P, CAP_BASE, CAP_MIN)
        axes[1].plot(d['t'], cap, c=col, lw=1.1, alpha=0.85, label=d['label'])
        print(f"  {d['label']:>10s}: e_iso mean={d['e_iso'].mean():.3f}  "
              f"cap [{cap.min():.2f}, {cap.max():.2f}]  mean={cap.mean():.2f}  "
              f"squeezed (cap<2): {(cap<2).mean():6.1%}  floored: {(cap<=CAP_MIN).mean():.1%}")

    axes[0].axhline(THETA_REF, ls='--', c='green', lw=0.7, alpha=0.8, label=f'θ_ref={THETA_REF}')
    axes[0].set_ylabel('score (scaled)' if scaled else 'score', fontsize=8)
    _input_ylim(axes[0], datasets, 'e_iso')
    axes[0].legend(fontsize=7, ncol=5, loc='upper left')
    axes[0].set_title('Input: e_iso = y90·%ext,90 per experiment (isolation signal; '
                      '%ext=1 when absent)', fontsize=8)

    axes[1].axhline(CAP_BASE, ls=':', c='green', lw=0.7, label=f'c_base={CAP_BASE}')
    axes[1].axhline(CAP_MIN,  ls=':', c='red',   lw=0.7, label=f'c_min={CAP_MIN}')
    axes[1].set_ylabel('c(t) cores', fontsize=8)
    axes[1].set_xlabel('Time (s)')
    axes[1].legend(fontsize=7, ncol=5, loc='upper right')
    axes[1].set_title('Isolation — response: aggressor core cap c(t)  '
                      '(quota = c(t)·100,000 μs / 100 ms period)', fontsize=8)

    plt.tight_layout()
    out = _out('compare_isolating_scaled.png' if scaled else 'compare_isolating.png')
    fig.savefig(out, dpi=130)
    print(f'[saved] {out}')
    plt.close(fig)


def fig_compare_scores(arm, datasets, dt, h_horz, theta_on=0.3, theta_off=0.1,
                       n_max=10, theta_ref=0.3, k_p=6.4,
                       cap_base=4.0, cap_min=0.5, scaled=False):
    """
    Same arm, different scoring algorithms: how each score curve drives the
    two mitigation actuators.  Two sections, each captioned with its own
    formula parameters:
      Horizontal scaling — input p50 overlay, then one replica-counter
                           strip n(t) per score
      Isolation          — input e_iso = y90·%ext overlay, then the cap
                           c(t) overlay
    When scaled, the input legends state each score's gain k explicitly.
    """
    THETA_REF, K_P, CAP_BASE, CAP_MIN = theta_ref, k_p, cap_base, cap_min
    N_MAX = n_max
    K = len(datasets)

    fig, axes = plt.subplots(3 + K, 1, figsize=(14, 10 + 0.85 * K), sharex=True,
                             gridspec_kw=dict(height_ratios=[4] + [1] * K + [4, 4]))
    fig.suptitle(f'Score comparison on arm "{arm}"'
                 + (SCALE_NOTE if scaled else ''), fontsize=10)

    ax_p50  = axes[0]
    strips  = axes[1:1 + K]
    ax_eiso = axes[1 + K]
    ax_cap  = axes[2 + K]

    print(f'\n── Arm "{arm}": score-driven mitigation ──')
    for k, d in enumerate(datasets):
        col = d.get('color', PALETTE8[k])
        k_lbl50 = f"  (k={d['k50']:.2f})" if scaled else ''
        k_lbl90 = f"  (k={d['k90']:.2f})" if scaled else ''
        cmd, n = ctrl_horizontal(d['p50'], theta_on, theta_off, h_horz, N_MAX)
        cap = ctrl_isolating(d['e_iso'], THETA_REF, K_P, CAP_BASE, CAP_MIN)

        ax_p50.plot(d['t'], d['p50'], c=col, lw=1.0, alpha=0.85,
                    label=d['label'] + k_lbl50)
        ax = strips[k]
        ax.step(d['t'], n, where='post', c=col, lw=1.1)
        ax.fill_between(d['t'], 0, n, step='post', color=col, alpha=0.15, lw=0)
        ax.set_ylim(-0.4, N_MAX + 0.9)
        ax.set_yticks([0, N_MAX])
        ax.tick_params(labelsize=6)
        ax.set_ylabel(d['label'], fontsize=7, rotation=0, ha='right', va='center')

        ax_eiso.plot(d['t'], d['e_iso'], c=col, lw=1.0, alpha=0.85,
                     label=d['label'] + k_lbl90)
        ax_cap.plot(d['t'], cap, c=col, lw=1.1, alpha=0.85, label=d['label'])
        print(f"  {d['label']:>16s}: y50 mean={d['p50'].mean():.3f}  "
              f"time n>0: {(n>0).mean():6.1%}  mean n={n.mean():5.2f}  |  "
              f"e_iso mean={d['e_iso'].mean():.3f}  cap mean={cap.mean():.2f}  "
              f"floored: {(cap<=CAP_MIN).mean():6.1%}")

    # ── Horizontal scaling section ──
    ax_p50.axhline(theta_on,  ls='--', c='orange', lw=0.7, alpha=0.8, label=f'θ_on={theta_on}')
    ax_p50.axhline(theta_off, ls=':',  c='gray',   lw=0.7, alpha=0.8, label=f'θ_off={theta_off}')
    ax_p50.set_ylabel('score (scaled)' if scaled else 'score', fontsize=8)
    # ci & slowdown_ratio exceed 1.0 in raw units — widen the panel, not the data
    _input_ylim(ax_p50, datasets, 'p50')
    ax_p50.legend(fontsize=7, ncol=4, loc='upper right')
    ax_p50.set_title(f'Horizontal scaling — input: p50 score per algorithm  |  '
                     f'bang-bang: θ_on={theta_on}, θ_off={theta_off}, n_max={N_MAX}, '
                     f'Δ_horz={h_horz*dt:.1f}s', fontsize=8)
    strips[0].set_title(f'Horizontal scaling — response: replica counter n(t) per score  '
                        f'(0..{N_MAX})', fontsize=8)

    # ── Isolation section ──
    ax_eiso.axhline(THETA_REF, ls='--', c='green', lw=0.7, alpha=0.8,
                    label=f'θ_ref={THETA_REF}')
    ax_eiso.set_ylabel('score (scaled)' if scaled else 'score', fontsize=8)
    _input_ylim(ax_eiso, datasets, 'e_iso')
    ax_eiso.legend(fontsize=7, ncol=4, loc='upper right')
    ax_eiso.set_title(f'Isolation — input: e_iso = y90·%ext,90 per algorithm '
                      f'(%ext=1: field absent in data)  |  saturated proportional: '
                      f'θ_ref={THETA_REF}, k_p={K_P}, cap=[{CAP_MIN}..{CAP_BASE}] cores',
                      fontsize=8)
    ax_cap.axhline(CAP_BASE, ls=':', c='green', lw=0.7, label=f'c_base={CAP_BASE}')
    ax_cap.axhline(CAP_MIN,  ls=':', c='red',   lw=0.7, label=f'c_min={CAP_MIN}')
    ax_cap.set_ylabel('c(t) cores', fontsize=8)
    ax_cap.set_xlabel('Time (s)')
    ax_cap.legend(fontsize=7, ncol=4, loc='upper right')
    ax_cap.set_title('Isolation — response: aggressor core cap c(t)', fontsize=8)

    plt.tight_layout()
    out = _out(f'compare_scores_{arm}_scaled.png' if scaled else f'compare_scores_{arm}.png')
    fig.savefig(out, dpi=130)
    print(f'[saved] {out}')
    plt.close(fig)


# ═══════════════════════════════════════════════════════════════════════════════
# 5.  MAIN
# ═══════════════════════════════════════════════════════════════════════════════

def run(t, arr, p50_raw, p90_raw, ext90, dt, service):
    p50_svc = traffic_weighted(p50_raw, arr)
    p90_svc = traffic_weighted(p90_raw, arr)
    e_iso   = p90_svc * ext90
    h_horz  = max(1, int(1.0 / dt))   # Δ_horz = 1.0 s (default for ref run)
    h_iso   = 0                       # Δ_iso  = 0 s (instant actuator)
    h_harv  = max(1, int(0.5 / dt))   # Δ_harv = 0.5 s
    tag = f'  [{service}]'

    fig_horizontal(t, p50_svc, dt, h_horz, tag)
    fig_isolating (t, e_iso, dt, tag)
    fig_harvesting(t, p90_svc, dt, h_harv, tag)
    fig_reference (t, arr, p50_svc, p90_svc, e_iso, dt, h_horz, h_iso, h_harv, tag)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--data', default=None,
                    help='Gordion JSON output file (bare names resolve in data/)')
    ap.add_argument('--dt', type=float, default=0.1, help='resample interval (s)')
    ap.add_argument('--compare', nargs='*', default=None, metavar='JSON',
                    help='cross-arm comparison over the given JSON files '
                         '(no files -> the kg score across all arms + spike/staircase)')
    ap.add_argument('--compare-scores', nargs='*', default=None, metavar='ARM',
                    help='same-arm comparison of scoring algorithms, one '
                         'combined figure per arm (no arms -> every arm in data/)')
    ap.add_argument('--scores', nargs='+', default=None, metavar='SCORE',
                    help='with --compare-scores: only these scoring algorithms '
                         '(default: all found for the arm)')
    ap.add_argument('--score', default='kg', metavar='SCORE',
                    help='with --compare (no files): which score to compare '
                         'across arms (default: kg)')
    ap.add_argument('--scale', action='store_true',
                    help='scale out-of-range scores back into [0,1] with a '
                         'pure gain (scaled = k*score, k = min(1, 1/max over '
                         'all arms), p50 and p90 series each with their own '
                         'k; in-range scores are untouched)')
    eq1 = ap.add_argument_group('Eq. (1) horizontal bang-bang (compare modes)')
    eq1.add_argument('--h-horz', type=float, default=1.0,
                     help='predictor lead time Δ_horz in seconds (default 1.0)')
    eq1.add_argument('--theta-on', type=float, default=0.3,
                     help='scale-up threshold θ_on (default 0.3)')
    eq1.add_argument('--theta-off', type=float, default=0.1,
                     help='scale-down threshold θ_off (default 0.1)')
    eq1.add_argument('--n-max', type=int, default=10,
                     help='replica counter ceiling n_max (default 10)')
    eq2 = ap.add_argument_group('Eq. (2) isolation saturated-proportional (compare modes)')
    eq2.add_argument('--theta-ref', type=float, default=0.3,
                     help='reference threshold θ_ref (default 0.3)')
    eq2.add_argument('--k-p', type=float, default=6.4,
                     help='proportional gain k_p in cores per unit score (default 6.4)')
    eq2.add_argument('--cap-base', type=float, default=4.0,
                     help='nominal aggressor allocation c_base in cores (default 4.0)')
    eq2.add_argument('--cap-min', type=float, default=0.5,
                     help='liveness floor c_min in cores (default 0.5)')
    args = ap.parse_args()

    h_horz = max(1, int(round(args.h_horz / args.dt)))
    eq1_kw = dict(theta_on=args.theta_on, theta_off=args.theta_off,
                  n_max=args.n_max)
    eq2_kw = dict(theta_ref=args.theta_ref, k_p=args.k_p,
                  cap_base=args.cap_base, cap_min=args.cap_min)

    if args.compare_scores is not None:
        mat = discover_matrix()
        arms = args.compare_scores or (
            [a for a in ARM_ORDER if a in mat]
            + sorted(set(mat) - set(ARM_ORDER)))
        if not arms:
            ap.error(f'--compare-scores: no {{arm}}_{{score}}_sim.json files in {DATA_DIR}')
        # stable score -> color mapping across arms and across --scores subsets
        universe = sorted({s for by_score in mat.values() for s in by_score})
        color_of = {s: PALETTE8[i % len(PALETTE8)] for i, s in enumerate(universe)}
        gains = {}
        if args.scale:
            gains = score_gains(mat, dt=args.dt)
            for s, (k50, k90, m50, m90) in sorted(gains.items()):
                print(f'[gain] {s}: k50 = {k50:.3f} (max {m50:.3f})  '
                      f'k90 = {k90:.3f} (max {m90:.3f})'
                      + ('  [in range, untouched]' if k50 == 1.0 and k90 == 1.0
                         else ''))
        if args.scores:
            unknown = [s for s in args.scores if s not in universe]
            if unknown:
                ap.error(f'--scores: unknown score(s) {", ".join(unknown)} '
                         f'(have: {", ".join(universe)})')
        for arm in arms:
            if arm not in mat:
                ap.error(f'--compare-scores: no data for arm "{arm}" '
                         f'(have: {", ".join(sorted(mat))})')
            wanted = args.scores or sorted(mat[arm])
            missing = [s for s in wanted if s not in mat[arm]]
            if missing:
                print(f'[warn] arm "{arm}": no data for score(s) '
                      + ', '.join(missing) + ' — skipped')
            scores = [s for s in wanted if s in mat[arm]]
            if not scores:
                continue
            if len(scores) > len(PALETTE8):
                ap.error(f'--compare-scores: arm "{arm}" has {len(scores)} scores; '
                         f'at most {len(PALETTE8)} supported')
            print(f'Arm "{arm}": comparing scores ' + ', '.join(scores))
            datasets = load_compare([mat[arm][s] for s in scores], dt=args.dt,
                                    labels=scores)
            for d in datasets:
                d['color'] = color_of[d['label']]
            if args.scale:
                scale_datasets(datasets, gains)
            fig_compare_scores(arm, datasets, args.dt, h_horz,
                               scaled=args.scale, **eq1_kw, **eq2_kw)
        return

    if args.compare is not None:
        if args.compare:
            paths = [_resolve_data(p) for p in args.compare]
        else:
            mat = discover_matrix()
            paths = [mat[a][args.score] for a in ARM_ORDER
                     if args.score in mat.get(a, {})]
            if args.score == 'kg':   # the stitched traces are kg-based
                for extra in ('spike', 'staircase'):
                    p = os.path.join(DATA_DIR, f'{extra}_sim.json')
                    if os.path.exists(p):
                        paths.append(p)
        if not paths:
            ap.error(f'--compare: no files given and no "{args.score}"-score '
                     f'arms found in {DATA_DIR}')
        if len(paths) > len(PALETTE8):
            ap.error(f'--compare: at most {len(PALETTE8)} curves supported, got {len(paths)}')
        print(f'Comparing {len(paths)} score curves: '
              + ', '.join(_dataset_label(p) for p in paths))
        datasets = load_compare(paths, dt=args.dt)
        if args.scale:
            if args.compare:
                ap.error('--scale needs the {arm}_{score}_sim.json matrix; '
                         'use it without an explicit file list')
            g = score_gains(discover_matrix(), dt=args.dt).get(args.score)
            if not g:
                ap.error(f'--scale: no data found for score "{args.score}"')
            k50, k90, m50, m90 = g
            print(f'[gain] {args.score}: k50 = {k50:.3f} (max {m50:.3f})  '
                  f'k90 = {k90:.3f} (max {m90:.3f})')
            scale_datasets(datasets, {d['label']: g for d in datasets})
        fig_compare_horizontal(datasets, args.dt, h_horz,
                               scaled=args.scale, **eq1_kw)
        fig_compare_isolating(datasets, args.dt, scaled=args.scale, **eq2_kw)
        return

    if args.data:
        path = _resolve_data(args.data)
        print(f'Loading {path} ...')
        t, arr, p50, p90, ext90, dt, svc = load_json(path, dt=args.dt)
    else:
        print('Using synthetic signals (--data path.json for real data)\n')
        t, arr, p50, p90, ext90, dt, svc = make_synthetic(dt=args.dt)
    run(t, arr, p50, p90, ext90, dt, svc)


if __name__ == '__main__':
    main()