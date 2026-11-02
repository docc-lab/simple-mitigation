# simulation/results — figure index

One sub-directory per experiment, prefixed with the month it was run. The
directory is gitignored; figures that are referenced from the runbooks are
tracked with `git add -f`.

| directory | cluster / period | what | produced by | documented in |
|---|---|---|---|---|
| `2026-07-eval-8360y-stage2/` | July 2026, sm110p (original stage-2 eval) | eval bar charts, predictor-on/off real-run panels, early compare_* plots (`old/`) and raw run_data captures | `plot_real_run.py`, `make_eval_barchart.py` | `STAGE3-RUNBOOK.md`, `COLUMN-EVAL.md` (untracked, local only) |
| `2026-10-motiv-8360y/` | 2026-10-23/24, Clemson 8360Y | motivation experiment v1: `motiv_ext_bursty_timeseries` (none / hc-bang / horizontal-any), `motiv_ext_bursty_armed` | `plot_motiv.py` | `deploy/stage3/RUNBOOK.md` sec 14 |
| `2026-11-motiv2-sm110p/` | 2026-11-01/02, Wisconsin sm110p | motivation pass 2 (cores needed for the same SLO): `motiv2_bars`, `motiv2_cores_vs_slo`, `motiv2_ts_ext`, `motiv2_ts_int`, `motiv2_ts_mixed` | dsb `stage3-eval/tools/motiv2_plot.py` | `deploy/stage3/MOTIV2-RESULTS.md` sec 8b, `RUNBOOK.md` sec 15 |

New experiments get their own `YYYY-MM-<topic>-<cluster>/` directory; do not
drop files at the top level.
