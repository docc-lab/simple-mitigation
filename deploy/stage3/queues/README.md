# Motivation-experiment queue scripts

The shell queues that drove the motivation passes (`RUNBOOK.md` sec 16-17,
`MOTIV3-RESULTS.md`). They run on node-0 from
`/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval` and call
`run-motiv3.sh` once per (config, cell list); copies lived in `/tmp` on node-0
and in the session scratchpad, this directory is the kept version.

| script | cluster | what it runs | output dirs |
|---|---|---|---|
| `queue-motiv3.sh` | Wisconsin sm110p `icelake-perf` (pass 3, 2026-11-08) | extrinsic 12 cells (none, hc-bang, hc-hold, rep{1,3,6}f x quiet/medium/contended) and intrinsic 2-P (none, hc-int, rep1-3f), two trials | `data/motiv3{,-t2}`, `data/motiv3-int{,-t2}` |
| `queue-c6320.sh` | Clemson c6320 `icelake-perf1` (pass 3 re-run, 2026-11-10) | every plotted cell, two trials: extrinsic none / hc-hold / rep3p:medium / rep3p:quiet (`motiv3-c6320.yaml`), intrinsic 2-P none / rep1f (`-int.yaml`), hc-grow (`-int4.yaml`, search limit 3 -> 4) | `data/motiv3-c6320{,-t2}`, `-int{,-t2}`, `-grow{,-t2}` |
| `launch-c6320.sh` | c6320 | (re)starts `queue-c6320.sh` inside a live `ssh -A` session: kills leftovers, drops partial cells, quarantines replication cells without replica data, restores the search manifest, starts `proj-sync.sh` | - |
| `queue-intprobe.sh` | c6320 | intrinsic load probe with the 2-P victim at 10k / 12k rps: rep1f and hc-grow | `data/motiv3-c6320-{int,grow}{10,12}k` |
| `queue-int1p.sh` | c6320 | the 1-P intrinsic variant at one load: none, rep1f (`-int1p.yaml`), hc-grow1 (`-int1p2.yaml`, thread 4 own + 6 shared); `SUFFIX=-t2` for trial 2 | `data/motiv3-c6320-int1p$SUFFIX`, `-grow1p$SUFFIX` |
| `queue-sweep1p.sh` | c6320 | the unattended 1-P load sweep (3000-8000 rps, 2-3 trials, resumable, second pass, tgz + /proj mirror); ran detached 2026-11-10 09:15-18:31 UTC with node-0's own key | `data/sweep1p/L<rps>-t<trial>/` |
| `queue-int1p-q.sh` | c6320 | superseded by `queue-eq.sh` (never run): the steady-scenario cells with the same quiet load as the bursty scenario (`Q=2000` + extra to `L=6000`, 3 trials, resumable, detached-ready) | `data/int1pq/Q<Q>-L<L>-t<trial>/` |
| `queue-eq.sh` | c6320 | **done** (ran detached 2026-11-11 23:36 – 11-12 04:17 UTC, 32/32 cells rc=0, `archives/2026-11-clemson-c6320-pass3/motiv3-c6320-eq.tgz`, `MOTIV3-RESULTS.md` sec 3.4; resume with `STEADY_TRIALS=4` for a 4th steady trial): budget sweep for EQ1/EQ2 with the same quiet load (2000 rps) in both cells. Steady intrinsic (1-P victim, 2000 + 4000 extra = 6000 offered, 3 trials): none, rep1f-rep4f (1-P replicas), hc-grow1/2/3 (thread 4 own + 1/2/3 shared with the burner). Tail extrinsic (`motiv3-c6320.yaml`, 2 trials): rep1f, rep3f, rep6f pre-provisioned, rep6p reactive; none / hc-hold / rep3p reuse `data/motiv3-c6320{,-t2}` | `data/eq/steady-t<T>/`, `data/eq/tail-t<T>/` |
| `check-cell.sh` | c6320 | per-phase score summary (p1 / bursts / gaps, e_iso spurious triggers) of one extrinsic cell plus its `motiv3_report.py` rows | - |

Conventions shared by the c6320 queues:

- `AGGR_CPUSET` = socket 0 minus the HC daemons' threads 24,26 and their siblings
  52,54; `OVF` picks the replica manifest (`overflow-node3clean-c6320[-1p]`),
  `LOAD_MANIFEST` the node-3 load dial; `STAGE3_PERF_EVENTS` is the 8-event
  Haswell list (4 programmable counters per thread with SMT on).
- `run()` pipes `run-motiv3.sh` through the log and keeps only the `[motiv3]` /
  FATAL / FAILED / WARNING lines on the terminal.
- Detached use: `unset SSH_AUTH_SOCK; setsid nohup bash queue-X.sh > /tmp/queue-X.out 2>&1 < /dev/null &`
  once node-0 -> node-3/node-4 hops work without the forwarded agent (`ssh -o BatchMode=yes node-4 hostname`).
- Never `pkill` a queue from the same ssh command that launches it (self-match
  kills the shell); the `[k]` bracket trick in the patterns avoids matching the
  pkill itself.
