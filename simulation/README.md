# Simulation — mitigation controllers on recorded score traces

Offline reference implementation of the two finalized mitigation controllers,
driven by recorded contention-score traces. This is the parity oracle for the
Go controllers in `pkg/controllers` and the tool for comparing scoring
algorithms by the mitigation they drive.

**Eq. (1) — Horizontal scaling**: bang-bang on the predicted median score
ŷ50 with a replica counter n(t) ∈ {0..n_max} guarding scale-down.

**Eq. (2) — Isolation**: saturated proportional controller on the extrinsic
tail magnitude e_iso(t) = y90(t)·%ext,90(t):
`c(t) = min(c_base, max(c_min, c_base − k_p·(e_iso − θ_ref)))`, written as
cgroup v2 quota `c(t)·P` per period P = 100,000 µs. (%ext,90 falls back to 1
when the field is absent from the data.)

## Layout

```
simulation.py     the script (all paths resolve relative to this file)
data/             input JSON traces
results/          generated figures (created on demand)
```

Data files follow `data/{arm}_{score}_sim.json`:

- **arm** — the experiment: `baseline`, `mildA`, `mildB`, `mildC`,
  `moderate`, `severe`. (`spike`/`staircase` are stitched traces with no
  score suffix.)
- **score** — the scoring algorithm: `binary`, `ci`, `cpi`, `kg`,
  `rolling_pctl`, `slowdown_ratio`, … (anything after the first `_` is the
  score name; new score files are picked up automatically).

Each file holds `samples[]` with `offset_ms`, `p50_contention_score`,
`p90_contention_score`, and `timing_window.arrival_rps_1s`. Curves are
resampled to a uniform `--dt` grid (default 100 ms) with time starting at 0.

## Setup

```bash
pip install numpy scipy matplotlib   # one-time
cd simulation                        # or run simulation/simulation.py from anywhere
```

## Mode 1 — single-trace parameter sweeps

```bash
python simulation.py                               # synthetic signals
python simulation.py --data severe_kg_sim.json     # bare names resolve in data/
python simulation.py --data path/to/run.json       # explicit path works too
```

Writes `results/sweep_horizontal.png` (lead-time sweep),
`results/sweep_isolating.png` (θ_ref/k_p sweep),
`results/sweep_harvesting.png` (AIMD sweep), and
`results/ctrl_reference_run.png` (all three controllers at defaults), plus a
numeric summary on stdout.

## Mode 2 — cross-arm comparison (one score, all arms)

```bash
python simulation.py --compare                     # default score: kg (+ spike/staircase)
python simulation.py --compare --score cpi         # another score across the arms
python simulation.py --compare a.json b.json       # explicit file list (any curves)
```

Writes `results/compare_horizontal.png` (p50 inputs overlaid + one n(t)
strip per arm) and `results/compare_isolating.png` (e_iso overlaid + cap
c(t) overlaid), plus a per-arm stats table on stdout.

## Mode 3 — same-arm score comparison (the score-quality view)

```bash
python simulation.py --compare-scores                            # all arms, all scores
python simulation.py --compare-scores severe mildB               # chosen arms
python simulation.py --compare-scores severe --scores kg cpi ci  # chosen scores, plot order = list order
```

Writes one combined figure per arm, `results/compare_scores_<arm>.png`, in
two sections captioned with their own formula parameters:

- **Horizontal scaling** — input: the p50 score curves overlaid; response:
  one replica-counter strip n(t) per score.
- **Isolation** — input: the e_iso = y90·%ext,90 curves overlaid (%ext
  falls back to 1 when absent from the data); response: all cap curves
  c(t) overlaid.

Scores keep a fixed color in every figure (CVD-validated palette), so
figures stay comparable side by side. Unknown score names error with the
list of available ones; arms missing a requested score warn and continue.

## Scaling scores to a common [0,1] magnitude (`--scale`)

The scores have different native units (kg is tanh-bounded [0,1], ci lives
at 1.5–1.8, slowdown_ratio straddles 1.0, …), so shared thresholds are not
directly comparable. `--scale` applies a **pure gain, no offset** — per
score, the p50 series and the p90 series each get their own

```
k = min(1, 1 / max(series over every arm))         scaled(t) = k · s(t)
```

i.e. the smallest correction that brings an out-of-range series back into
[0,1]; a series already inside [0,1] keeps k = 1 and is untouched (Gordion's
kg is natively tanh-bounded [0,1], so it always passes through unchanged).
The gains used are printed as `[gain]` lines *and* shown per score in the
input-panel legends (e.g. `ci (k=0.50)`); the figure title records the
scaling and outputs get a `_scaled` suffix so raw-unit figures are never
overwritten.

```bash
python simulation.py --compare-scores --scale
python simulation.py --compare --scale --score ci
```

Reading note: k is anchored to each score's global max — for a noisy score
that max is a noise peak, so its steady-state levels get compressed downward
relative to a clean score. That is the intended consequence of fitting
everything under 1.0 with a single gain: range spent on noise is range lost
for signal.

(This scaling is figure-side magnitude alignment only — unrelated to the
normalization steps inside the scoring algorithms.)

## Controller parameters (modes 2 and 3)

Every parameter of the two formulas is a flag; the values used are echoed in
each figure's title.

| Actuator | Symbol | Flag | Default |
|---|---|---|---|
| Horizontal scaling | θ_on (scale-up threshold) | `--theta-on` | 0.3 |
| Horizontal scaling | θ_off (scale-down threshold) | `--theta-off` | 0.1 |
| Horizontal scaling | n_max (replica ceiling) | `--n-max` | 10 |
| Horizontal scaling | Δ_horz (predictor lead, s) | `--h-horz` | 1.0 |
| Isolation | θ_ref (reference threshold) | `--theta-ref` | 0.3 |
| Isolation | k_p (gain, cores per unit score) | `--k-p` | 6.4 |
| Isolation | c_base (nominal allocation) | `--cap-base` | 4.0 |
| Isolation | c_min (liveness floor) | `--cap-min` | 0.5 |

```bash
python simulation.py --compare-scores severe --scale --theta-on 0.75 --k-p 12
```

`--dt` (default 0.1 s) sets the resample grid in all modes. The single-run
sweep figures (mode 1) deliberately ignore these flags — their purpose is to
sweep parameters over the combos hardcoded in `fig_horizontal` /
`fig_isolating`.

## Notes on the current trace set

- Within any single arm the score is essentially flat (steady-state
  contention runs); the dynamics live across arms and in the stitched
  `spike`/`staircase` traces.
- `binary` is all-zero in every arm, including severe — it drives no
  mitigation anywhere. Worth checking the exporter.
- The p50/p90 split barely exists in these traces (p90 ≈ p50 for most
  scores); kg is the exception (p90 runs below p50).
