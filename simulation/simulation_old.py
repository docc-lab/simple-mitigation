#!/usr/bin/env python3
"""
Gordion mitigation controller — math check / parameter sweep.

Figures produced
----------------
sweep_vertical.png     : PI vertical scaler sensitivity (3x3 grid)
sweep_horizontal.png   : Horizontal bang-bang — gated vs pure, side by side
sweep_harvesting.png   : AIMD harvesting sweep (separate from horizontal)
ctrl_reference_run.png : Full-stack reference run (both horizontal variants)
"""

import argparse
import numpy as np
import matplotlib.pyplot as plt
import matplotlib.gridspec as gridspec
from scipy.ndimage import gaussian_filter1d


# ═══════════════════════════════════════════════════════════════════════════════
# 1.  SIGNALS
# ═══════════════════════════════════════════════════════════════════════════════

def make_synthetic(T=80.0, dt=0.1, seed=42):
    """
    Approximate run_data_iter1_ready from visual inspection.
    Windows: (start, end, p90_peak_amp)
      - p90 amplitude varies per burst; sub-burst dips captured separately
      - p50 uses same envelope with heavier smoothing + ~78% amplitude
    """
    rng = np.random.default_rng(seed)
    t   = np.arange(0, T, dt)
    N   = len(t)

    # arrival rate
    arr = 100 + 10 * rng.standard_normal(N)
    ramp = int(5 / dt)
    arr[:ramp] = np.linspace(5, 110, ramp) + 3 * rng.standard_normal(ramp)
    drop = int(73 / dt)
    arr[drop:] = np.linspace(105, 20, N - drop) + 3 * rng.standard_normal(N - drop)
    arr = np.clip(arr, 0, None)

    # contention windows (start, end, p90_peak_amp)
    windows = [
        (2.0,  3.5,  0.35), (3.5,  6.0,  0.45),      # early partial
        (19.0, 21.0, 1.00), (21.8, 23.0, 0.95),       # burst 1: dip at ~21
        (25.0, 27.5, 0.92), (28.5, 34.0, 1.00),       # burst 2: two phases
        (37.0, 38.5, 0.90), (39.2, 40.8, 0.88),       # burst 3: oscillating
        (41.5, 44.0, 0.85),
        (47.0, 51.5, 1.00), (51.5, 52.5, 0.18),       # burst 4: strongest
        (58.0, 60.0, 0.18),                            # sub-threshold bump
        (70.0, 73.0, 0.65),                            # late moderate
        (74.5, 75.5, 0.30), (77.0, 78.5, 0.65),       # late spikes
        (79.2, 80.0, 1.00),
    ]
    p90_raw = np.zeros(N)
    for s, e, amp in windows:
        p90_raw[(t >= s) & (t < e)] = np.maximum(p90_raw[(t >= s) & (t < e)], amp)

    p90 = np.clip(gaussian_filter1d(p90_raw, sigma=1.2) + 0.03 * rng.standard_normal(N), 0, 1)

    p50_base = gaussian_filter1d(p90_raw, sigma=4.0) * 0.78
    early = (t >= 2.0) & (t <= 6.0)
    p50_base[early] = np.maximum(p50_base[early],
                                  gaussian_filter1d(p90_raw, sigma=2)[early] * 0.85)
    p50 = np.clip(p50_base + 0.05 * rng.standard_normal(N), 0, 1)

    return t, arr, p50, p90


def load_csv(path):
    import pandas as pd
    df  = pd.read_csv(path)
    low = {c.lower().replace(' ', '_').replace('-', '_'): c for c in df.columns}
    def pick(*keys):
        for k in keys:
            if k in low: return df[low[k]].values
        raise KeyError(f"None of {keys!r} found in {list(low)}")
    t   = pick('t', 'time', 'timestamp')
    arr = pick('arrival_rate', 'rps', 'rate', 'requests_per_second')
    p50 = pick('p50_score', 'p50', 'score_p50')
    p90 = pick('p90_score', 'p90', 'score_p90')
    return t, arr, p50, p90, float(np.median(np.diff(t)))


# ═══════════════════════════════════════════════════════════════════════════════
# 2.  CONTROLLERS
# ═══════════════════════════════════════════════════════════════════════════════

def ctrl_vertical(p50_svc, theta_slo, kp, ki, dt, h_steps,
                  windup=1.0, kff=0.0, arr=None, kd=0.0,
                  d_smooth=0.6, ff_smooth=0.8):
    """
    PI(D) + optional FF vertical scaler.

        u = kp·e + ki·∫e + kd·d̃e/dt + kff·d̃A/dt

    where d̃·/dt are EMA-smoothed derivatives:
        filt[i] = (1-α)·raw[i] + α·filt[i-1]

    Setting kd=0 disables D term (PI behavior).
    Setting kff=0 disables FF term.

    Args
    ----
    kd        : derivative gain on error (default 0 → PI only)
    kff       : feedforward gain on arrival rate (default 0 → no FF)
    d_smooth  : EMA factor for D-term derivative.  0=raw, 1=fully smoothed.
                Raw de/dt on noisy p50 is dominated by sample noise — use ≥0.5.
    ff_smooth : EMA factor for FF derivative.  Arrival rate is very noisy
                (σ~10 rps step-to-step at 100ms), so use ≥0.7.

    windup MUST stay ≤ 1.0: long quiescent windows accumulate large negative
    integrals with loose windup, preventing u_scale from going positive during bursts.
    """
    N = len(p50_svc)
    integ = 0.0
    e_all, u = np.zeros(N), np.zeros(N)
    de_filt = 0.0
    dA_filt = 0.0
    for i in range(N):
        h = min(i + h_steps, N - 1)
        e_all[i] = p50_svc[h] - theta_slo
        integ = np.clip(integ + e_all[i] * dt, -windup, windup)

        # D term (smoothed)
        if i > 0 and kd != 0:
            de_raw = (e_all[i] - e_all[i-1]) / dt
            de_filt = (1 - d_smooth) * de_raw + d_smooth * de_filt
        d_term = kd * de_filt

        # FF term (smoothed)
        if i > 0 and kff != 0 and arr is not None:
            dA_raw = (arr[i] - arr[i-1]) / dt
            dA_filt = (1 - ff_smooth) * dA_raw + ff_smooth * dA_filt
        ff_term = kff * dA_filt

        u[i] = kp * e_all[i] + ki * integ + d_term + ff_term
    return u, e_all


def ctrl_horizontal_gated(p50_svc, u_scale, u_max, theta_on, theta_off, h_steps, n_max=10):
    """
    Horizontal bang-bang WITH vertical saturation gate (escalation ladder).

      +1  if (u_scale(t) ≥ u_max) AND (p50_svc(t+Δ) > θ_on)
      -1  if p50_svc(t+Δ) < θ_off  AND  n(t) > 0    [scale-down guard]
       0  otherwise

    Gate enforces escalation: horizontal only fires after vertical saturates.
    n(t) = clamp(n(t-1) + cmd(t-1), 0, n_max)
    """
    N, n_cur = len(p50_svc), 0
    cmd, n_extra = np.zeros(N, dtype=int), np.zeros(N, dtype=int)
    for i in range(N):
        h = min(i + h_steps, N - 1)
        p_fwd = p50_svc[h]
        if (u_scale[i] >= u_max) and (p_fwd > theta_on):
            cmd[i] = +1
        elif (p_fwd < theta_off) and (n_cur > 0):
            cmd[i] = -1
        n_cur = int(np.clip(n_cur + cmd[i], 0, n_max))
        n_extra[i] = n_cur
    return cmd, n_extra


def ctrl_horizontal(p50_svc, theta_on, theta_off, h_steps, n_max=10):
    """
    Horizontal bang-bang WITHOUT vertical gate (pure threshold on p50).

      +1  if p50_svc(t+Δ) > θ_on
      -1  if p50_svc(t+Δ) < θ_off  AND  n(t) > 0    [scale-down guard]
       0  otherwise
    """
    N, n_cur = len(p50_svc), 0
    cmd, n_extra = np.zeros(N, dtype=int), np.zeros(N, dtype=int)
    for i in range(N):
        h = min(i + h_steps, N - 1)
        p_fwd = p50_svc[h]
        if p_fwd > theta_on:
            cmd[i] = +1
        elif (p_fwd < theta_off) and (n_cur > 0):
            cmd[i] = -1
        n_cur = int(np.clip(n_cur + cmd[i], 0, n_max))
        n_extra[i] = n_cur
    return cmd, n_extra


def ctrl_harvesting(p90_svc, theta_safe, alpha, beta, delta, h_steps):
    """
    Asymmetric AIMD harvesting.
    alpha = per-sample increment (= target_cores_per_second * dt).
    E.g. alpha=0.05 with dt=0.1 → 0.5 cores/second probe rate.
    """
    N = len(p90_svc)
    score_tail, slack, h_cores = np.zeros(N), np.zeros(N), np.zeros(N)
    h_cur = 0.0
    for i in range(N):
        hp = min(i + h_steps, N - 1)
        score_tail[i] = p90_svc[hp]
        slack[i] = theta_safe - score_tail[i]
        if slack[i] <= 0:
            h_cur = beta * h_cur
        elif slack[i] > delta:
            h_cur += alpha
        h_cores[i] = h_cur
    return h_cores, score_tail, slack


def traffic_weighted(p, arr):
    if p.ndim == 1: return p.copy()
    denom = arr.sum(axis=1, keepdims=True).clip(min=1e-9)
    return ((arr / denom) * p).sum(axis=1)


# ═══════════════════════════════════════════════════════════════════════════════
# 3.  FIGURE: VERTICAL SWEEP
# ═══════════════════════════════════════════════════════════════════════════════

def fig_vertical(t, p50_svc, dt, h_vert):
    kp_vals = [0.5, 1.0, 2.0]
    ki_vals = [0.05, 0.2, 0.5]
    th_vals = [0.3, 0.5, 0.7]
    U_MAX   = 1.0
    ki_cols = ['#1f77b4', '#ff7f0e', '#2ca02c']

    fig, axes = plt.subplots(3, 3, figsize=(15, 9), sharex=True, sharey='row')
    fig.suptitle(
        'Vertical scaler u_scale(t)  |  rows=kp, cols=θ_SLO, lines=ki  (windup=1.0)\n'
        'Red dashed = u_max=1.0  |  dotted = loose windup (10.0) — integrator never recovers',
        fontsize=9)

    for ri, kp in enumerate(kp_vals):
        for ci, th in enumerate(th_vals):
            ax = axes[ri, ci]
            ax.fill_between(t, 0, p50_svc, color='steelblue', alpha=0.10, lw=0)
            for ki, col in zip(ki_vals, ki_cols):
                u, _ = ctrl_vertical(p50_svc, th, kp, ki, dt, h_vert, windup=1.0)
                ax.plot(t, u, c=col, lw=0.9, alpha=0.9, label=f'ki={ki}')
                if ki == ki_vals[0]:
                    u_l, _ = ctrl_vertical(p50_svc, th, kp, ki, dt, h_vert, windup=10.0)
                    ax.plot(t, u_l, c=col, lw=0.7, alpha=0.35, ls=':', label=f'ki={ki} wu=10')
            ax.axhline(U_MAX, ls='--', c='red', lw=0.9)
            ax.axhline(0, ls=':', c='black', lw=0.5)
            ax.set_title(f'kp={kp}, θ_SLO={th}', fontsize=8)
            if ci == 0: ax.set_ylabel('u_scale', fontsize=7)
            if ri == 2: ax.set_xlabel('Time (s)', fontsize=7)
            if ri == 0 and ci == 0: ax.legend(fontsize=6, loc='upper left')

    plt.tight_layout()
    fig.savefig('sweep_vertical.png', dpi=130)
    print('[saved] sweep_vertical.png')
    plt.close(fig)


# ═══════════════════════════════════════════════════════════════════════════════
# 4.  FIGURE: HORIZONTAL SWEEP  (gated | pure, side by side)
# ═══════════════════════════════════════════════════════════════════════════════

def fig_vertical_ablation(t, arr, p50_svc, dt, h_vert):
    """
    Three ablation variants of the vertical scaler, all with kp=1, ki=0.2,
    θ_SLO=0.5, windup=1.0:
      Variant 1 (current) : PI + FF        — sweep kff
      Variant 2 (ablate FF): PI only        — baseline reference
      Variant 3 (swap FF→D): PID            — sweep kd
    Plus a direct comparison panel at representative settings.
    """
    KP, KI, TH_SLO = 1.0, 0.2, 0.5

    fig = plt.figure(figsize=(15, 16))
    gs  = gridspec.GridSpec(5, 1, figure=fig, hspace=0.45)

    # Row 0: context — p50_svc + arrival rate (twin axes)
    ax0  = fig.add_subplot(gs[0])
    ax0b = ax0.twinx()
    ax0.fill_between(t, 0, p50_svc, color='steelblue', alpha=0.15, lw=0)
    ax0.plot(t, p50_svc, c='steelblue', lw=0.85, label='p50_svc')
    ax0.axhline(TH_SLO, ls='--', c='orange', lw=0.8, label=f'θ_SLO={TH_SLO}')
    ax0.set_ylabel('Score [0–1]', color='steelblue', fontsize=8)
    ax0.legend(loc='upper left', fontsize=7)
    ax0b.plot(t, arr, c='green', lw=0.5, alpha=0.45, label='arrival rate')
    ax0b.set_ylabel('rps', color='green', fontsize=8)
    ax0b.legend(loc='upper right', fontsize=7)
    ax0.set_title('Context: p50_svc (PI input) and arrival rate (FF input)',
                  fontsize=9, fontweight='bold')

    # Row 1: Variant 1 — PI + FF, sweep kff
    ax1 = fig.add_subplot(gs[1], sharex=ax0)
    kff_vals = [0.0, 0.001, 0.003, 0.01]
    kff_cols = ['#888888', '#1f77b4', '#ff7f0e', '#d62728']
    for kff, col in zip(kff_vals, kff_cols):
        u, _ = ctrl_vertical(p50_svc, TH_SLO, KP, KI, dt, h_vert,
                              kff=kff, arr=arr)
        lbl = f'kff={kff}' if kff > 0 else 'kff=0 (PI baseline)'
        ax1.plot(t, u, c=col, lw=0.85, alpha=0.85, label=lbl)
    ax1.axhline(0, ls=':', c='black', lw=0.5)
    ax1.set_ylabel('u_scale', fontsize=8)
    ax1.legend(fontsize=7, ncol=4)
    ax1.set_title('Variant 1 (current):  u = kp·e + ki·∫e + kff·d̃A/dt   — kff sweep',
                  fontsize=9, fontweight='bold')

    # Row 2: Variant 2 — PI only (no FF), also show P-only and I-only for context
    ax2 = fig.add_subplot(gs[2], sharex=ax0)
    u_pi, _ = ctrl_vertical(p50_svc, TH_SLO, KP, KI,  dt, h_vert)
    u_p,  _ = ctrl_vertical(p50_svc, TH_SLO, KP, 0.0, dt, h_vert)
    u_i,  _ = ctrl_vertical(p50_svc, TH_SLO, 0.0, KI, dt, h_vert)
    ax2.plot(t, u_pi, c='#1f77b4', lw=0.95, label='PI: kp=1.0, ki=0.2')
    ax2.plot(t, u_p,  c='#888888', lw=0.75, ls='--', alpha=0.8,
             label='P only: kp=1.0, ki=0  (no memory)')
    ax2.plot(t, u_i,  c='#2ca02c', lw=0.75, ls=':',  alpha=0.8,
             label='I only: kp=0, ki=0.2  (no instant response)')
    ax2.axhline(0, ls=':', c='black', lw=0.5)
    ax2.set_ylabel('u_scale', fontsize=8)
    ax2.legend(fontsize=7, ncol=3)
    ax2.set_title('Variant 2 (ablate FF):  u = kp·e + ki·∫e   — PI baseline + P-only and I-only',
                  fontsize=9, fontweight='bold')

    # Row 3: Variant 3 — PID (no FF), sweep kd
    ax3 = fig.add_subplot(gs[3], sharex=ax0)
    kd_vals = [0.0, 0.02, 0.05, 0.15]
    kd_cols = ['#888888', '#1f77b4', '#ff7f0e', '#d62728']
    for kd, col in zip(kd_vals, kd_cols):
        u, _ = ctrl_vertical(p50_svc, TH_SLO, KP, KI, dt, h_vert, kd=kd)
        lbl = f'kd={kd}' if kd > 0 else 'kd=0 (PI baseline)'
        ax3.plot(t, u, c=col, lw=0.85, alpha=0.85, label=lbl)
    ax3.axhline(0, ls=':', c='black', lw=0.5)
    ax3.set_ylabel('u_scale', fontsize=8)
    ax3.legend(fontsize=7, ncol=4)
    ax3.set_title('Variant 3 (swap FF→D):  u = kp·e + ki·∫e + kd·d̃e/dt   — kd sweep',
                  fontsize=9, fontweight='bold')

    # Row 4: direct comparison at representative settings
    ax4 = fig.add_subplot(gs[4], sharex=ax0)
    u_pif, _ = ctrl_vertical(p50_svc, TH_SLO, KP, KI, dt, h_vert,
                              kff=0.003, arr=arr)
    u_pi,  _ = ctrl_vertical(p50_svc, TH_SLO, KP, KI, dt, h_vert)
    u_pid, _ = ctrl_vertical(p50_svc, TH_SLO, KP, KI, dt, h_vert, kd=0.05)
    ax4.plot(t, u_pif, c='#d62728', lw=0.95, alpha=0.9,
             label='PI + FF (kff=0.003)  — current')
    ax4.plot(t, u_pi,  c='#1f77b4', lw=0.95, alpha=0.9,
             label='PI                          — ablate FF')
    ax4.plot(t, u_pid, c='#2ca02c', lw=0.95, alpha=0.9,
             label='PID (kd=0.05)         — swap FF→D')
    ax4.axhline(0, ls=':', c='black', lw=0.5)
    ax4.set_ylabel('u_scale', fontsize=8)
    ax4.set_xlabel('Time (s)', fontsize=8)
    ax4.legend(fontsize=7)
    ax4.set_title('Direct comparison: three variants overlaid at representative settings',
                  fontsize=9, fontweight='bold')

    fig.suptitle(
        f'Vertical scaler ablations  |  kp={KP}, ki={KI}, θ_SLO={TH_SLO}, windup=1.0\n'
        f'D and FF use EMA-smoothed derivatives (d_smooth=0.6, ff_smooth=0.8)',
        fontsize=10, y=1.005)
    plt.savefig('sweep_vertical_ablation.png', dpi=130, bbox_inches='tight')
    print('[saved] sweep_vertical_ablation.png')

    # Quick numerical summary
    print('\n── Vertical ablation stats (range of u_scale) ──')
    for name, kff, kd in [('PI         ', 0,     0),
                          ('PI + FF    ', 0.003, 0),
                          ('PID (no FF)', 0,     0.05)]:
        u, _ = ctrl_vertical(p50_svc, TH_SLO, KP, KI, dt, h_vert,
                              kff=kff, arr=arr, kd=kd)
        print(f'  {name}: [{u.min():+.3f}, {u.max():+.3f}]  '
              f'mean={u.mean():+.3f}  std={u.std():.3f}')
    plt.close(fig)


def fig_horizontal(t, p50_svc, dt, h_vert, h_horz):
    # gated combos: vary what affects gate opening
    combos_g = [
        dict(kp=1.0, ki=0.2, th_slo=0.3, u_max=0.30, windup=1.0,
             label='kp=1.0, θ_SLO=0.3, u_max=0.30'),
        dict(kp=1.0, ki=0.2, th_slo=0.5, u_max=0.30, windup=1.0,
             label='kp=1.0, θ_SLO=0.5, u_max=0.30'),
        dict(kp=2.0, ki=0.2, th_slo=0.3, u_max=0.50, windup=1.0,
             label='kp=2.0, θ_SLO=0.3, u_max=0.50'),
        dict(kp=0.5, ki=0.2, th_slo=0.3, u_max=0.30, windup=1.0,
             label='kp=0.5, θ_SLO=0.3, u_max=0.30'),
    ]
    # pure combos: only theta_on/off matter
    combos_p = [
        dict(th_on=0.5, th_off=0.2, label='θ_on=0.5, θ_off=0.2'),
        dict(th_on=0.6, th_off=0.2, label='θ_on=0.6, θ_off=0.2  (fewer +1)'),
        dict(th_on=0.4, th_off=0.1, label='θ_on=0.4, θ_off=0.1  (more +1)'),
        dict(th_on=0.5, th_off=0.3, label='θ_on=0.5, θ_off=0.3  (narrow dead zone)'),
    ]
    THETA_ON, THETA_OFF = 0.5, 0.2   # reference lines on p50 panels

    fig = plt.figure(figsize=(17, 13))
    gs  = gridspec.GridSpec(4, 2, figure=fig, hspace=0.50, wspace=0.28)

    # ── helpers ──────────────────────────────────────────────────────────────
    def p50_panel(ax, title, extra_hlines=None):
        ax.plot(t, p50_svc, c='steelblue', lw=0.85, label='p50_svc')
        ax.axhline(THETA_ON,  ls='--', c='orange', lw=0.8, label=f'θ_on={THETA_ON}')
        ax.axhline(THETA_OFF, ls=':',  c='gray',   lw=0.8, label=f'θ_off={THETA_OFF}')
        if extra_hlines:
            for val, lbl in extra_hlines:
                ax.axhline(val, ls='--', lw=0.6, alpha=0.55, label=lbl)
        ax.set_ylabel('Score [0–1]', fontsize=7)
        ax.legend(fontsize=6, ncol=2)
        ax.set_title(title, fontsize=9, fontweight='bold')

    # ── LEFT: GATED ──────────────────────────────────────────────────────────
    ax_p50g = fig.add_subplot(gs[0, 0])
    ax_ug   = fig.add_subplot(gs[1, 0], sharex=ax_p50g)
    ax_cmdg = fig.add_subplot(gs[2, 0], sharex=ax_p50g)
    ax_ng   = fig.add_subplot(gs[3, 0], sharex=ax_p50g)

    p50_panel(ax_p50g, 'GATED  (u_scale ≥ u_max required for +1)')

    for c in combos_g:
        u, _ = ctrl_vertical(p50_svc, c['th_slo'], c['kp'], c['ki'],
                              dt, h_vert, windup=c['windup'])
        ax_ug.plot(t, u, lw=0.85, alpha=0.85, label=c['label'])
        ax_ug.axhline(c['u_max'], ls='--', lw=0.55, alpha=0.35)
    ax_ug.axhline(0, ls=':', c='black', lw=0.5)
    ax_ug.set_ylabel('u_scale', fontsize=7)
    ax_ug.legend(fontsize=6)
    ax_ug.set_title('Vertical scaler output  (u_max dashed per combo)', fontsize=8)

    for c in combos_g:
        u, _ = ctrl_vertical(p50_svc, c['th_slo'], c['kp'], c['ki'],
                              dt, h_vert, windup=c['windup'])
        cmd, _ = ctrl_horizontal_gated(p50_svc, u, c['u_max'],
                                        THETA_ON, THETA_OFF, h_horz)
        ax_cmdg.plot(t, cmd, lw=0.85, alpha=0.75, label=c['label'])
    ax_cmdg.set_yticks([-1, 0, 1])
    ax_cmdg.set_ylabel('cmd', fontsize=7)
    ax_cmdg.legend(fontsize=6)
    ax_cmdg.set_title('cmd (gated)', fontsize=8)

    for c in combos_g:
        u, _ = ctrl_vertical(p50_svc, c['th_slo'], c['kp'], c['ki'],
                              dt, h_vert, windup=c['windup'])
        _, n = ctrl_horizontal_gated(p50_svc, u, c['u_max'],
                                      THETA_ON, THETA_OFF, h_horz)
        ax_ng.plot(t, n, lw=0.85, alpha=0.75, label=c['label'])
    ax_ng.set_ylabel('n_extra', fontsize=7)
    ax_ng.legend(fontsize=6)
    ax_ng.set_title('Replica counter n(t)  [gated]', fontsize=8)
    ax_ng.set_xlabel('Time (s)', fontsize=7)

    # ── RIGHT: PURE ──────────────────────────────────────────────────────────
    ax_p50p = fig.add_subplot(gs[0, 1])
    ax_note = fig.add_subplot(gs[1, 1])
    ax_cmdp = fig.add_subplot(gs[2, 1], sharex=ax_p50p)
    ax_np   = fig.add_subplot(gs[3, 1], sharex=ax_p50p)

    # show all θ_on values on p50 panel
    extra = [(c['th_on'], f"θ_on={c['th_on']}") for c in combos_p]
    p50_panel(ax_p50p, 'PURE  (direct threshold, no u_scale gate)', extra_hlines=extra)

    ax_note.text(0.5, 0.5,
                 'No u_scale gate —\nvertical scaler output\nnot used here',
                 ha='center', va='center', transform=ax_note.transAxes,
                 fontsize=11, color='gray', style='italic')
    ax_note.set_axis_off()

    for c in combos_p:
        cmd, _ = ctrl_horizontal(p50_svc, c['th_on'], c['th_off'], h_horz)
        ax_cmdp.plot(t, cmd, lw=0.85, alpha=0.75, label=c['label'])
    ax_cmdp.set_yticks([-1, 0, 1])
    ax_cmdp.set_ylabel('cmd', fontsize=7)
    ax_cmdp.legend(fontsize=6)
    ax_cmdp.set_title('cmd (pure)', fontsize=8)

    for c in combos_p:
        _, n = ctrl_horizontal(p50_svc, c['th_on'], c['th_off'], h_horz)
        ax_np.plot(t, n, lw=0.85, alpha=0.75, label=c['label'])
    ax_np.set_ylabel('n_extra', fontsize=7)
    ax_np.legend(fontsize=6)
    ax_np.set_title('Replica counter n(t)  [pure]', fontsize=8)
    ax_np.set_xlabel('Time (s)', fontsize=7)

    fig.suptitle(
        f'Horizontal scaler  |  Δ_horz={h_horz*dt:.1f} s,  n_max=10,  scale-down guard: -1 only if n>0',
        fontsize=10, y=1.005)
    plt.savefig('sweep_horizontal.png', dpi=130, bbox_inches='tight')
    print('[saved] sweep_horizontal.png')
    plt.close(fig)


# ═══════════════════════════════════════════════════════════════════════════════
# 5.  FIGURE: HARVESTING SWEEP  (separate)
# ═══════════════════════════════════════════════════════════════════════════════

def fig_harvesting(t, p50_svc, p90_svc, dt, h_harv):
    combos = [
        dict(th_s=0.6, al=0.05, be=0.5, de=0.05,
             label='θ_safe=0.6, α=0.05 (0.5/s), β=0.5'),
        dict(th_s=0.4, al=0.03, be=0.7, de=0.05,
             label='θ_safe=0.4, α=0.03 (0.3/s), β=0.7'),
        dict(th_s=0.8, al=0.01, be=0.5, de=0.10,
             label='θ_safe=0.8, α=0.01 (0.1/s), β=0.5'),
        dict(th_s=0.6, al=0.05, be=0.3, de=0.05,
             label='θ_safe=0.6, α=0.05 (0.5/s), β=0.3  (faster release)'),
    ]

    fig, axes = plt.subplots(4, 1, figsize=(14, 13), sharex=True)
    fig.suptitle(f'Harvesting AIMD sweep  |  Δ_harv={h_harv*dt:.1f} s', fontsize=10)

    # Panel 0: p90_svc + p50_svc + θ_safe thresholds
    axes[0].plot(t, p50_svc, c='steelblue', lw=0.8, alpha=0.7, label='p50_svc')
    axes[0].plot(t, p90_svc, c='navy',      lw=0.85, label='p90_svc  (→ score_tail)')
    for c in combos:
        axes[0].axhline(c['th_s'], ls='--', lw=0.7, alpha=0.6,
                        label=f"θ_safe={c['th_s']}")
    axes[0].set_ylabel('Score [0–1]', fontsize=7)
    axes[0].legend(fontsize=6, ncol=2)
    axes[0].set_title(
        'Input signals — p90_svc drives score_tail; θ_safe lines show probe/release boundary',
        fontsize=8)

    # Panel 1: h(t) cores
    for c in combos:
        hc, _, _ = ctrl_harvesting(p90_svc, c['th_s'], c['al'], c['be'], c['de'], h_harv)
        axes[1].plot(t, hc, lw=0.9, alpha=0.85, label=c['label'])
    axes[1].set_ylabel('Cores h(t)', fontsize=7)
    axes[1].legend(fontsize=6)
    axes[1].set_title('Harvested cores  (probe = +α/sample, release = β×h)', fontsize=8)

    # Panel 2: slack s(t)
    for c in combos:
        _, _, sl = ctrl_harvesting(p90_svc, c['th_s'], c['al'], c['be'], c['de'], h_harv)
        axes[2].plot(t, sl, lw=0.9, alpha=0.85, label=c['label'])
    axes[2].axhline(0, ls=':', c='black', lw=0.6)
    axes[2].set_ylabel('Slack s(t)', fontsize=7)
    axes[2].legend(fontsize=6)
    axes[2].set_title(
        'Slack s(t) = θ_safe − score_tail  (< 0 → safety release, > δ → probe)',
        fontsize=8)

    # Panel 3: phase relationship — p90 overlay with reference h(t)
    ax3b = axes[3].twinx()
    axes[3].fill_between(t, 0, p90_svc, color='navy', alpha=0.12, lw=0)
    axes[3].plot(t, p90_svc, c='navy', lw=0.7, alpha=0.6, label='p90_svc')
    axes[3].set_ylabel('p90_svc', fontsize=7, color='navy')
    axes[3].set_ylim(0, 1.3)
    hc_ref, _, _ = ctrl_harvesting(p90_svc, 0.6, 0.05, 0.5, 0.05, h_harv)
    ax3b.plot(t, hc_ref, c='brown', lw=1.0, label='h(t)  θ_safe=0.6 ref')
    ax3b.set_ylabel('Cores h(t)', fontsize=7, color='brown')
    axes[3].set_xlabel('Time (s)', fontsize=7)
    axes[3].set_title(
        'Phase: p90_svc bursts vs harvested cores  (θ_safe=0.6 reference)',
        fontsize=8)
    handles = [plt.Line2D([0],[0],c='navy',lw=0.8,label='p90_svc'),
               plt.Line2D([0],[0],c='brown',lw=1.0,label='h(t)')]
    axes[3].legend(handles=handles, fontsize=6)

    plt.tight_layout()
    fig.savefig('sweep_harvesting.png', dpi=130)
    print('[saved] sweep_harvesting.png')
    plt.close(fig)


# ═══════════════════════════════════════════════════════════════════════════════
# 6.  FIGURE: REFERENCE RUN  (full stack, both horizontal variants)
# ═══════════════════════════════════════════════════════════════════════════════

def fig_reference(t, arr, p50_svc, p90_svc, dt, h_vert, h_horz, h_harv):
    KP, KI, TH_SLO  = 1.0, 0.2, 0.5
    THETA_ON, THETA_OFF = 0.5, 0.2
    U_MAX_GATED      = 0.3      # calibrated to actual u_scale range
    TH_SAFE, ALPHA, BETA, DELTA = 0.6, 0.05, 0.5, 0.05

    u_ref, e_ref = ctrl_vertical(p50_svc, TH_SLO, KP, KI, dt, h_vert, windup=1.0)
    cmd_g, n_g   = ctrl_horizontal_gated(p50_svc, u_ref, U_MAX_GATED,
                                          THETA_ON, THETA_OFF, h_horz)
    cmd_p, n_p   = ctrl_horizontal(p50_svc, THETA_ON, THETA_OFF, h_horz)
    hc, st, sl   = ctrl_harvesting(p90_svc, TH_SAFE, ALPHA, BETA, DELTA, h_harv)

    fig, axes = plt.subplots(8, 1, figsize=(14, 19), sharex=True)
    fig.suptitle(
        f'Reference run  kp={KP}, ki={KI}, θ_SLO={TH_SLO}, windup=1.0\n'
        f'Gated u_max={U_MAX_GATED}  |  Pure θ_on={THETA_ON}/θ_off={THETA_OFF}  |  '
        f'Harvest θ_safe={TH_SAFE}, α={ALPHA} ({ALPHA/dt:.1f} cores/s), β={BETA}',
        fontsize=9)

    # Row 0: arrival
    axes[0].fill_between(t, 0, arr, color='green', alpha=0.25, lw=0)
    axes[0].plot(t, arr, c='green', lw=0.7)
    axes[0].set_ylabel('Arrival\n(rps)')

    # Row 1: scores + all thresholds
    axes[1].plot(t, p50_svc, c='steelblue', lw=0.85, label='p50_svc')
    axes[1].plot(t, p90_svc, c='navy', lw=0.85, alpha=0.7, label='p90_svc')
    axes[1].axhline(TH_SLO,    ls='--', c='orange', lw=0.9, label=f'θ_SLO={TH_SLO}')
    axes[1].axhline(TH_SAFE,   ls=':',  c='brown',  lw=0.9, label=f'θ_safe={TH_SAFE}')
    axes[1].axhline(THETA_ON,  ls='--', c='red',    lw=0.6, alpha=0.5,
                    label=f'θ_on={THETA_ON}')
    axes[1].axhline(THETA_OFF, ls=':',  c='gray',   lw=0.6, alpha=0.5,
                    label=f'θ_off={THETA_OFF}')
    axes[1].legend(fontsize=6, ncol=3)
    axes[1].set_ylabel('Score [0–1]')

    # Row 2: error
    axes[2].plot(t, e_ref, c='darkorange', lw=0.85)
    axes[2].axhline(0, ls=':', c='black', lw=0.5)
    axes[2].set_ylabel('e_all')
    axes[2].set_title('PI error  e_all = p50_svc(t+Δ) − θ_SLO', fontsize=8)

    # Row 3: u_scale + gated saturation
    axes[3].plot(t, u_ref, c='purple', lw=0.85, label='u_scale')
    axes[3].fill_between(t, U_MAX_GATED, u_ref.clip(min=U_MAX_GATED),
                          color='red', alpha=0.25, label='above u_max (gate open)')
    axes[3].axhline(U_MAX_GATED, ls='--', c='red', lw=0.9, label=f'u_max={U_MAX_GATED}')
    axes[3].axhline(0, ls=':', c='black', lw=0.5)
    axes[3].legend(fontsize=7)
    axes[3].set_ylabel('u_scale')
    axes[3].set_title('Vertical scaler — red fill = gate open for gated horizontal', fontsize=8)

    # Row 4: gated horizontal
    ax4b = axes[4].twinx()
    axes[4].step(t, cmd_g, c='darkred', where='post', lw=1.0, label='cmd (gated)')
    axes[4].set_yticks([-1, 0, 1])
    axes[4].set_ylabel('cmd', fontsize=7, color='darkred')
    ax4b.step(t, n_g, c='firebrick', where='post', lw=0.8, ls='--',
              alpha=0.7, label='n_extra')
    ax4b.set_ylabel('n_extra', fontsize=7, color='firebrick')
    ax4b.set_ylim(bottom=0)
    h4 = [plt.Line2D([0],[0],c='darkred',lw=1.0,label='cmd'),
          plt.Line2D([0],[0],c='firebrick',lw=0.8,ls='--',label='n_extra')]
    axes[4].legend(handles=h4, fontsize=7)
    axes[4].set_title('Horizontal GATED  (escalation ladder)', fontsize=8)

    # Row 5: pure horizontal
    ax5b = axes[5].twinx()
    axes[5].step(t, cmd_p, c='steelblue', where='post', lw=1.0, label='cmd (pure)')
    axes[5].set_yticks([-1, 0, 1])
    axes[5].set_ylabel('cmd', fontsize=7, color='steelblue')
    ax5b.step(t, n_p, c='cornflowerblue', where='post', lw=0.8, ls='--',
              alpha=0.7, label='n_extra')
    ax5b.set_ylabel('n_extra', fontsize=7, color='cornflowerblue')
    ax5b.set_ylim(bottom=0)
    h5 = [plt.Line2D([0],[0],c='steelblue',lw=1.0,label='cmd'),
          plt.Line2D([0],[0],c='cornflowerblue',lw=0.8,ls='--',label='n_extra')]
    axes[5].legend(handles=h5, fontsize=7)
    axes[5].set_title('Horizontal PURE  (direct threshold)', fontsize=8)

    # Row 6: harvested cores
    axes[6].plot(t, hc, c='brown', lw=0.9, label='h(t) cores')
    axes[6].legend(fontsize=7)
    axes[6].set_ylabel('Cores')

    # Row 7: slack
    axes[7].plot(t, sl, c='gray',  lw=0.85, label='slack s(t)')
    axes[7].plot(t, st, c='navy',  lw=0.7, alpha=0.5, label='score_tail')
    axes[7].axhline(0, ls=':', c='black', lw=0.5)
    axes[7].legend(fontsize=7)
    axes[7].set_ylabel('Slack / tail')
    axes[7].set_xlabel('Time (s)')

    plt.tight_layout()
    fig.savefig('ctrl_reference_run.png', dpi=130)
    print('[saved] ctrl_reference_run.png')

    print('\n── Reference run summary ──')
    print(f'  p50_svc: mean={p50_svc.mean():.3f}, max={p50_svc.max():.3f}')
    print(f'  u_scale: [{u_ref.min():.3f}, {u_ref.max():.3f}]  '
          f'gate-open (u≥{U_MAX_GATED}): {(u_ref>=U_MAX_GATED).mean():.1%}')
    print(f'  Gated +1: {(cmd_g==1).sum():4d} ({(cmd_g==1).mean():.1%})  '
          f'-1: {(cmd_g==-1).sum():3d} ({(cmd_g==-1).mean():.1%})  '
          f'n_max reached: {n_g.max()}')
    print(f'  Pure  +1: {(cmd_p==1).sum():4d} ({(cmd_p==1).mean():.1%})  '
          f'-1: {(cmd_p==-1).sum():3d} ({(cmd_p==-1).mean():.1%})  '
          f'n_max reached: {n_p.max()}')
    print(f'  Harvest: max={hc.max():.2f} cores  '
          f'safety releases: {(sl<0).sum()} ({(sl<0).mean():.1%})')
    plt.close(fig)


# ═══════════════════════════════════════════════════════════════════════════════
# 7.  MAIN
# ═══════════════════════════════════════════════════════════════════════════════

def run(t, arr, p50_raw, p90_raw, dt):
    p50_svc = traffic_weighted(p50_raw, arr)
    p90_svc = traffic_weighted(p90_raw, arr)
    h_vert  = max(1, int(1.0 / dt))
    h_horz  = max(1, int(7.5 / dt))
    h_harv  = max(1, int(0.5 / dt))
    fig_vertical          (t, p50_svc, dt, h_vert)
    fig_vertical_ablation (t, arr, p50_svc, dt, h_vert)
    fig_horizontal(t, p50_svc, dt, h_vert, h_horz)
    fig_harvesting(t, p50_svc, p90_svc, dt, h_harv)
    fig_reference (t, arr, p50_svc, p90_svc, dt, h_vert, h_horz, h_harv)
    plt.show()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--data', default=None,
                    help='CSV: t, arrival_rate, p50_score, p90_score')
    ap.add_argument('--dt', type=float, default=0.1)
    args = ap.parse_args()
    if args.data:
        t, arr, p50, p90, dt = load_csv(args.data)
    else:
        print('Using synthetic signals (--data path.csv for real data)\n')
        t, arr, p50, p90 = make_synthetic(dt=args.dt)
        dt = args.dt
    run(t, arr, p50, p90, dt)


if __name__ == '__main__':
    main()