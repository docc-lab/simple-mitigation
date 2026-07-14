# Stage-3 phased sweep configs (reference copy)

The 8 phased experiment configs for the ext/int × steady/bursty × moderate/severe
sweep. **Canonical copies live on the score-side repo** at
`dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval/configs/` and are
run there by the phase orchestrator (`run-phased-stage3.sh`); this is a
read-only reference so the mitigation repo records exactly what was driven.
Full pipeline: see `../STAGE3-RUNBOOK.md`.

## Timeline (every config, one 140 s run)

| phase | window      | what happens |
|-------|-------------|--------------|
| p1    | t0 … +20 s  | victim only (baseline) |
| p2    | +20 … +80 s | contention on, mitigation **disarmed** (controller senses + traces) |
| p3    | +80 … +140 s| same contention, mitigation **armed** (orchestrator touches the ARM file) |

## The two axes

- **extrinsic** (`ext-*`): SN/SS co-tenants drive load from +20 s; victim fixed
  at 2400 rps throughout. Contention comes from interference. Aggressor list =
  SN/SS services only (HR profile/geo are NOT aggressors — they serve the
  victim's own frontend traffic).
- **intrinsic** (`int-*`): victim baseline 800 rps; a second wrk2 driver adds
  load from +20 s (→ 2200 moderate / 2400 severe). Co-tenant pods are deployed
  but **unloaded** (`rps: 0`). Contention comes from the victim's own overload.
- **steady** = `fixed` inter-arrival, **bursty** = `exp` (Poisson).
- **moderate/severe** = SN 5200/SS 6100 vs SN 5400/SS 6300 (extrinsic);
  →2200 vs →2400 rps (intrinsic).

## What the sweep validated (2026-07-14, e_horz control stack)

Horizontal scaling is gated on the **intrinsic-weighted** signal
`e_horz = y50·(1−ext50)` (θ_on 0.40 / θ_off 0.15, 8 s scale-down dwell);
isolation on the **extrinsic-weighted** `e_iso = y90·ext90`. Result:

| arm | horiz steps | max n | e_iso p2 | reading |
|-----|-------------|-------|----------|---------|
| ext × 4 | **0** | 0 | 0.47–0.55 | isolation only, no false scale-out |
| int × 4 | 5–17 | 3 | ~0.01 | scale-out only, isolation idle |

Each actuator responds to exactly the contention type it can fix. Victim
placement uses the overflow-deployment pattern (victim hard-pinned at
replicas=1; scale-out lands on other nodes via `search-overflow`). Figures:
`../simulation/results/real_run_sweep3-*.png`.
