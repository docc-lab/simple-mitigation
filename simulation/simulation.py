#!/usr/bin/env python3
"""
Gordion mitigation controllers — minimal control formulas.

Three controllers (no vertical):
  • Horizontal : on p50  — bang-bang +/-1 replicas with counter guard
  • Isolating  : on p90  — P-only saturated linear ramp -> aggressor core cap
  • Harvesting : on p90  — asymmetric AIMD on slack (theta_safe - score_tail)

Signal assignment rationale (validated on real data):
  • p50 is sparse/spiky (~7% of time > 0.5) — drives expensive scale-out only
    when contention spills into the median (real SLO violation)
  • p90 is broad/sustained (~65% > 0.5) — drives fast, cheap actuators
    (cgroup squeeze) and protects harvesting tail headroom

Usage:
  python gordion_ctrl.py                      # synthetic signals
  python gordion_ctrl.py --data run.json      # Gordion JSON output
"""

import argparse
import json
import os
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.gridspec as gridspec
from scipy.ndimage import gaussian_filter1d


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
    arr_raw = np.array([(s['timing_window'].get('arrival_rps_1s') or 0)
                        for s in samples], dtype=float)
    # Normalize start to 0, resample to uniform grid
    t_norm = t_raw - t_raw.min()
    t      = np.arange(0, t_norm.max(), dt)
    p50    = np.interp(t, t_norm, p50_raw)
    p90    = np.interp(t, t_norm, p90_raw)
    arr    = np.interp(t, t_norm, arr_raw)
    return t, arr, p50, p90, dt, data.get('service_name', 'unknown')


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
    return t, arr, p50, p90, dt, 'synthetic'


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
    Window-max bang-bang on p50 with asymmetric scale-up/scale-down logic.

    Let  p_window(t) = max_{τ ∈ [0, Δ_horz]} p50_svc(t + τ)
         p_now(t)    = p50_svc(t)

      +1  if p_window > θ_on                          AND  n < n_max   (anticipate)
      -1  if p_now < θ_off  AND  p_window < θ_on      AND  n > 0       (confirm idle)
       0  otherwise

    n(t+1) = clamp(n(t) + cmd, 0, n_max)

    Three design choices baked in:

    1. WINDOW max instead of single-point look-ahead: a contention spike
       shorter than Δ_horz would be missed by a single-point lookup at
       p50_svc(t + Δ_horz) if it falls between t and t + Δ_horz.  The window
       max guarantees no in-horizon spike is missed.

    2. ASYMMETRIC look-ahead: scale-up anticipates (window), scale-down
       confirms idleness (current sample AND window).  Encodes the cost
       asymmetry of pod lifecycle: spinning up is slow and expensive, so
       anticipate; tearing down is fast and reversible, so wait for proof
       contention has passed.

    3. JOINT-CALM scale-down guard: -1 requires BOTH p_now < θ_off and
       p_window < θ_on.  Prevents oscillation when n_max blocks +1 but
       anticipated contention has not yet manifested.
    """
    N = len(p50_svc)
    cmd     = np.zeros(N, dtype=int)
    n_extra = np.zeros(N, dtype=int)
    n_cur   = 0
    for i in range(N):
        # WINDOW look-ahead: max(p50) over [t, t+Δ_horz], not a single
        # future point.  A single-point lookup loses spikes shorter than
        # Δ_horz that fall between now and the future sample.
        hi          = min(i + h_steps + 1, N)
        p_window    = p50_svc[i:hi].max()
        p_now       = p50_svc[i]
        # +1: scale up if ANY point in window crosses θ_on (anticipate)
        # -1: scale down only if current AND entire window are calm
        if   p_window > theta_on  and n_cur < n_max:
            cmd[i] = +1
        elif p_now < theta_off and p_window < theta_on and n_cur > 0:
            cmd[i] = -1
        n_cur      = int(np.clip(n_cur + cmd[i], 0, n_max))
        n_extra[i] = n_cur
    return cmd, n_extra


def ctrl_isolating(p90_svc, theta_release, theta_squeeze,
                   cap_baseline, cap_min, h_steps=0):
    """
    P-only saturated linear ramp on p90_svc → core cap on aggressor cgroup.

      p90_iso ≤ θ_release   → cap = cap_baseline
      p90_iso ≥ θ_squeeze   → cap = cap_min
      otherwise             → cap = cap_baseline - (cap_baseline - cap_min) * frac
                              where frac = (p90_iso - θ_release) / (θ_squeeze - θ_release)
    """
    assert theta_squeeze > theta_release
    assert cap_baseline >= cap_min
    span = theta_squeeze - theta_release
    drop = cap_baseline - cap_min
    N = len(p90_svc)
    cap = np.zeros(N)
    for i in range(N):
        h = min(i + h_steps, N - 1)
        p = p90_svc[h]
        if   p <= theta_release: cap[i] = cap_baseline
        elif p >= theta_squeeze: cap[i] = cap_min
        else:                    cap[i] = cap_baseline - drop * (p - theta_release) / span
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


def _p90_panel(ax, t, p90, thresholds=None):
    ax.plot(t, p90, c='navy', lw=0.85, label='p90_svc')
    ax.fill_between(t, 0, p90, color='navy', alpha=0.15, lw=0)
    if thresholds:
        for v, lbl, c, ls in thresholds:
            ax.axhline(v, ls=ls, lw=0.7, c=c, alpha=0.8, label=lbl)
    ax.set_ylabel('p90 [0–1]', fontsize=8)
    ax.set_ylim(-0.05, 1.05)
    ax.legend(fontsize=6, ncol=4)


def fig_horizontal(t, p50_svc, dt, h_horz, tag=''):
    """Sweep θ_on, θ_off on p50."""
    combos = [
        dict(on=0.3, off=0.1, label='θ_on=0.3, θ_off=0.1  (default)'),
        dict(on=0.5, off=0.2, label='θ_on=0.5, θ_off=0.2  (stricter)'),
        dict(on=0.2, off=0.05, label='θ_on=0.2, θ_off=0.05 (laxer)'),
        dict(on=0.4, off=0.3, label='θ_on=0.4, θ_off=0.3  (narrow band)'),
    ]
    fig, axes = plt.subplots(3, 1, figsize=(14, 9), sharex=True)
    fig.suptitle(f'Horizontal scaler sweep  |  signal=p50,  Δ_horz={h_horz*dt:.1f}s,  '
                 f'n_max=10{tag}', fontsize=10)

    th = [(c['on'], f"θ_on={c['on']}", 'orange', '--') for c in combos[:2]]
    th += [(c['off'], f"θ_off={c['off']}", 'gray', ':') for c in combos[:2]]
    _p50_panel(axes[0], t, p50_svc, thresholds=th)

    for c in combos:
        cmd, _ = ctrl_horizontal(p50_svc, c['on'], c['off'], h_horz)
        axes[1].plot(t, cmd, lw=0.9, alpha=0.8, label=c['label'])
    axes[1].set_yticks([-1, 0, 1])
    axes[1].set_ylabel('cmd')
    axes[1].legend(fontsize=7)
    axes[1].set_title('Command output', fontsize=8)

    for c in combos:
        _, n = ctrl_horizontal(p50_svc, c['on'], c['off'], h_horz)
        axes[2].plot(t, n, lw=0.9, alpha=0.8, label=c['label'])
    axes[2].set_ylabel('n_extra')
    axes[2].set_xlabel('Time (s)')
    axes[2].legend(fontsize=7)
    axes[2].set_title('Replica counter n(t)', fontsize=8)

    plt.tight_layout()
    fig.savefig('sweep_horizontal.png', dpi=130)
    print('[saved] sweep_horizontal.png')
    plt.close(fig)


def fig_isolating(t, p90_svc, dt, tag=''):
    """Sweep θ_release, θ_squeeze, cap_min on p90."""
    combos = [
        dict(rel=0.3, sq=0.85, b=4.0, m=0.5, label='θ_rel=0.3, θ_sq=0.85, [0.5..4]  (default)'),
        dict(rel=0.2, sq=0.70, b=4.0, m=0.5, label='θ_rel=0.2, θ_sq=0.70, [0.5..4]  (earlier+steeper)'),
        dict(rel=0.4, sq=0.90, b=4.0, m=1.0, label='θ_rel=0.4, θ_sq=0.90, [1.0..4]  (gentler floor)'),
        dict(rel=0.3, sq=0.85, b=8.0, m=0.5, label='θ_rel=0.3, θ_sq=0.85, [0.5..8]  (bigger aggressor)'),
    ]
    fig, axes = plt.subplots(3, 1, figsize=(14, 9), sharex=True)
    fig.suptitle(f'Isolating controller sweep  |  signal=p90, P-only saturated linear{tag}',
                 fontsize=10)

    th = [(0.3, 'θ_release=0.3', 'green', '--'), (0.85, 'θ_squeeze=0.85', 'red', '--')]
    _p90_panel(axes[0], t, p90_svc, thresholds=th)

    for c in combos:
        cap = ctrl_isolating(p90_svc, c['rel'], c['sq'], c['b'], c['m'])
        axes[1].plot(t, cap, lw=0.9, alpha=0.85, label=c['label'])
    axes[1].set_ylabel('cap (cores)')
    axes[1].legend(fontsize=7)
    axes[1].set_title('Cap on aggressor cores', fontsize=8)

    # Translate one combo to actual cpu.max strings (sample every ~5s)
    cap_ref = ctrl_isolating(p90_svc, 0.3, 0.85, 4.0, 0.5)
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
    fig.savefig('sweep_isolating.png', dpi=130)
    print('[saved] sweep_isolating.png')
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
    fig.savefig('sweep_harvesting.png', dpi=130)
    print('[saved] sweep_harvesting.png')
    plt.close(fig)


def fig_reference(t, arr, p50_svc, p90_svc, dt, h_horz, h_iso, h_harv, tag=''):
    """Full 3-controller stack at default settings."""
    THETA_ON, THETA_OFF = 0.3, 0.1
    THETA_REL, THETA_SQ = 0.3, 0.85
    CAP_BASE, CAP_MIN   = 4.0, 0.5
    TH_SAFE, ALPHA, BETA, DELTA = 0.7, 0.05, 0.5, 0.05

    cmd, n_extra = ctrl_horizontal(p50_svc, THETA_ON, THETA_OFF, h_horz)
    cap          = ctrl_isolating (p90_svc, THETA_REL, THETA_SQ, CAP_BASE, CAP_MIN, h_iso)
    hc, st, sl   = ctrl_harvesting(p90_svc, TH_SAFE, ALPHA, BETA, DELTA, h_harv)

    fig, axes = plt.subplots(7, 1, figsize=(14, 16), sharex=True)
    fig.suptitle(
        f'Reference run{tag}  |  Horz(p50,θ={THETA_ON}/{THETA_OFF})  '
        f'Iso(p90,θ={THETA_REL}/{THETA_SQ},cap=[{CAP_MIN}..{CAP_BASE}])  '
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
    axes[1].axhline(THETA_SQ,  ls='--', c='red',    lw=0.6, alpha=0.6)
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
    axes[3].set_title('Isolating: aggressor core cap (P-only saturated linear on p90)', fontsize=8)

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
    fig.savefig('ctrl_reference_run.png', dpi=130)
    print('[saved] ctrl_reference_run.png')

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
# 4.  MAIN
# ═══════════════════════════════════════════════════════════════════════════════

def run(t, arr, p50_raw, p90_raw, dt, service):
    p50_svc = traffic_weighted(p50_raw, arr)
    p90_svc = traffic_weighted(p90_raw, arr)
    h_horz  = max(1, int(7.5 / dt))   # Δ_horz = 7.5 s
    h_iso   = 0                       # Δ_iso  = 0 s (instant actuator)
    h_harv  = max(1, int(0.5 / dt))   # Δ_harv = 0.5 s
    tag = f'  [{service}]'

    fig_horizontal(t, p50_svc, dt, h_horz, tag)
    fig_isolating (t, p90_svc, dt, tag)
    fig_harvesting(t, p90_svc, dt, h_harv, tag)
    fig_reference (t, arr, p50_svc, p90_svc, dt, h_horz, h_iso, h_harv, tag)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--data', default=None, help='Gordion JSON output file')
    ap.add_argument('--dt', type=float, default=0.1, help='resample interval (s)')
    args = ap.parse_args()
    if args.data:
        print(f'Loading {args.data} ...')
        t, arr, p50, p90, dt, svc = load_json(args.data, dt=args.dt)
    else:
        print('Using synthetic signals (--data path.json for real data)\n')
        t, arr, p50, p90, dt, svc = make_synthetic(dt=args.dt)
    run(t, arr, p50, p90, dt, svc)


if __name__ == '__main__':
    main()