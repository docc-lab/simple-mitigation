# Motivation pass 2 — results and how to read them

Wisconsin sm110p cluster (`harvest-k612`), 2026-11-01 21:50 → 2026-11-02 02:37 UTC.
Five sweeps, 46 phased runs, one trial each unless stated. Raw data, per-sweep
`report.csv` and the calibration files are in the laptop archive
`simple-mitigation-1/motiv2-sm110p.tgz` (105 MB, md5 `63ec4df3…`), extracted
at `C:\tmp\motiv2\`. Every table below is regenerated with
`stage3-eval/tools/motiv2_report.py <sweep dir> [flags]` (section 8).

The question this pass answers (user, 2026-11-01): **how many cores does each
mitigation need for the victim to meet the same SLO**, with replication tried
in three placements, and with the CPU time actually consumed by the victim,
every replica and the aggressors measured, not assumed.

---

## 1. Setup

**Hardware / software.** 5× sm110p, one Xeon Silver 4314 per node (16C/32T,
sibling of CPU N is N+16), kernel 6.12.115-harvest, k8s 1.25.6, cgroup v2.
node-4 is the target: SMT **on**, all cores pinned at 2.4 GHz
(`no_turbo=1`, min=max=2400000), swap off. node-0 runs the orchestrator and
wrk2; node-2/3 host the rest of DSB (frontend, geo, rate, memcached, Mongo).

**CPU map on node-4 = July's layout** (user's rule: this is the July hardware,
use the July setup, not the R650 adaptations).

| CPUs | use |
|---|---|
| 18, 19, 20 | victim `search`, `GOMAXPROCS=3`, CFS limit 3 |
| 2, 3, 4 | the victim's SMT siblings — the only path by which contention reaches `processing_time` |
| 0–13, 16–29 | aggressor pool (`AGGR_CPUSET`), i.e. the whole node minus the HC daemons' cores: July-like unpinned aggressors |
| 21–29 | "other cores" replica placement (nine threads: up to three 3-P replicas) |
| node-3 CPUs 2, 3, 4 | "other node" replica placement (shares node-3 with unpinned DSB services) |
| 14 / 15 | HC Balancer / Monitor (siblings 30, 31 left idle) |
| 0 / 1 | HC Listener / mitigation controller (shared with the pool) |
| HC Secondary | pool minus victim threads and siblings: `0,1,5-13,16,17,21-29` |

**Score calibration (processing_time, victim alone on 18–20, 2.4 GHz,
2026-11-01 21:22–21:46).** Baseline at 3600 rps: p50 58.5 kcyc (24.4 µs),
p90 80.3 kcyc (33.5 µs); σ50 2.73, σ90 1.56 kcyc at smoothing window 10
(1 s); k = 0.0425 (tail-matched: a 1.5× p90 scores y90 = 0.8). The
latency-vs-rate curve is flat from 300 to 5600 rps offered (d50 64 → 58
kcyc), arrivals at `search` ≈ 60 % of the offered end-to-end rate, no knee.
Files: `stage3-eval/score-verify/{gordion,curve}-node4-sm110p-smt-proc-3600.*`,
`calibration-node4-sm110p-proc.log`. A first calibration was polluted by a
concurrent sweep (k 0.0184) and discarded.

**Protocol.** Phased stage-3 run, 140 s: p1 0–20 s victim only; p2 20–80 s
contention present, mitigation disarmed; ARM at 80 s; p3 80–140 s armed.
Victim offered 3600 rps (exp) in the extrinsic scenarios. Per run the
orchestrator also records, new in this pass:

- `mitigation/cpu_usage.csv` — every second, per-CPU busy/total jiffies and
  every pod cgroup's `cpu.stat usage_usec` (`deploy/stage3/cpu-sampler.sh`);
  a second sampler on node-3 for other-node cells (`cpu_usage-node-3.csv`,
  present from the SHORT sweep on — sweep A's other-node replicas are
  **unsampled**);
- `pod_uids_{p3,end}.txt` — uid → pod/node, to attribute cgroup rows;
- `run_data-overflow-<pod>.json` — the windowed latency stream of every
  replica, so service-level latency can be pooled over victim + replicas.

**Arms** (`deploy/stage3/arms/*.env`):

| arm | what happens at ARM |
|---|---|
| `none` | nothing |
| `hc-bang` | HC bang-bang on e_iso (θ_on 0.15, release after 50 windows below θ_off); victim's three physical cores become exclusive while on, aggressors confined to Secondary + idle victim threads |
| `hc-hold` | HC applied unconditionally at ARM and never released (no control logic) |
| `hc-iso` | hc-hold + `HC_TARGET_IDLE_CORES=2` |
| `rep{1,2,3}` | horizontal scale-out on the e_iso > 0.15 trigger, `N_MAX=k`, 2 s cooldown, no scale-in |
| `rep{1,2,3}f` | the same k replicas forced at ARM (θ_on = −1), for scenarios the score cannot see |
| `hcrep3` | hc-bang + 3 forced replicas |

Replica placement is a manifest swap (`stage3-eval/motiv2/overflow-*.yaml`
over `kubernetes/search/search-overflow-deployment.yaml`): **samecores** =
victim's own threads 18–20, **othercores** = 21–29 on node-4, **othernode** =
node-3 CPUs 2–4. 3-P replicas (`GOMAXPROCS 3`, limit 3) in the 3-P
scenarios; 1-P replicas (`overflow1p-*.yaml`, one thread each: 18 / 21,22,23 /
node-3 2,3,4) in the 1-P overload scenarios.

**Metrics.** All ratios are armed window ÷ p1 of the same run (p1 = [2, 20) s,
armed = the aggressor-burst windows after ARM, or the whole p3 for steady
scenarios). Percentiles are per-100 ms-window percentiles, count-weighted.

- *handler* = `processing_time` of the victim **instance** (what the score sees);
- *service total* = `total_time` pooled over victim + replicas (what the
  caller sees from `search`; it is >99 % blocking on the geo/rate calls);
- *e2e* = wrk2 at the frontend (whole-run aggregate, see caveats);
- *cores* = cgroup CPU-seconds ÷ wall over the armed windows, per pod class
  (victim / overflow replicas on node-4 / replicas on node-3 / aggressors);
- *busy* = per-CPU-group busy fraction (victim 18–20, siblings 2–4, replica
  threads 21–29, rest of the pool).

**SLO used in the tables** (a reporting choice, stated here once):
handler p90 ≤ 1.2× p1 ("mechanism"), service total p90 ≤ 1.5× p1
("symptom"). Change with `--slo-proc` / `--slo-total`.

---

## 2. Sweep A — extrinsic, 10 s bursts (3-P victim), two trials

Config `configs/motiv-sm110p.yaml`: victim 3600 rps; SN 7000 / SS 8000 rps
bursts 10 s on / 10 s off from t = 20 s. Unmitigated: e_iso idle 0.02–0.09,
burst plateau 0.40–0.55 (θ 0.15 is comfortably in between). Aggressors burn
≈ 11–12 cores during a burst; their own throughput is kept at 0.93–1.06 in
every cell (harvesting does not starve them).

Trial 1 = `data/motiv2` (21:50–22:57), trial 2 = `data/motiv2-t2`
(01:18–02:37, adds hc-hold and hc-iso). Handler p90 ratio, armed bursts / p1:

| cell | trial 1 | trial 2 | handler p90 µs (t2, p1 → armed) | victim cores t1 / t2 | replica cores t1 / t2 |
|---|---|---|---|---|---|
| none | 1.72 | 1.66 | 32.9 → 54.6 | 1.27 / 1.21 | – |
| hc-bang | 1.42 | 1.43 | 32.6 → 46.7 | 1.08 / 1.10 | – |
| hc-hold | 1.36 | 1.35 | 34.0 → 45.9 | 1.07 / 1.10 | – |
| hc-iso | – | 1.37 | 32.6 → 44.8 | – / 1.09 | – |
| rep1 samecores | 2.19 | 2.10 | 32.6 → 68.4 | 0.84 / 0.81 | 0.81 / 0.77 |
| rep1 othercores | 1.87 | 1.88 | 32.7 → 61.7 | 0.84 / 0.84 | 0.86 / 0.92 |
| rep1 othernode | 1.92 | 1.76 | 34.0 → 59.8 | 0.84 / 0.86 | n/a / 0.72 |
| rep2 samecores | 2.27 | 2.46 | 32.6 → 80.4 | 0.69 / 0.69 | 1.16 / 1.15 |
| rep2 othercores | 1.81 | 2.07 | 33.9 → 71.0 | 0.81 / 0.75 | 1.21 / 1.39 |
| rep2 othernode | 1.93 | 1.83 | 32.9 → 60.3 | 0.73 / 0.80 | n/a / 0.79 |
| rep3 samecores | 2.98 | 2.84 | 32.6 → 92.7 | 0.60 / 0.62 | 1.52 / 1.43 |
| rep3 othercores | 2.01 | 2.01 | 32.7 → 65.9 | 0.65 / 0.65 | 1.76 / 1.68 |
| rep3 othernode | 1.90 | 1.77 | 36.3 → 64.1 | 0.64 / 0.72 | n/a / 1.16 |

Handler p50 ratio: none 1.26 / 1.31; hc arms 1.12–1.17; replication 1.39–1.52.
Siblings busy fraction during bursts: none 0.50; hc arms 0.12–0.18 (that is the
mechanism: the siblings are cleared); every replication cell 0.49–0.58
(nothing changes on the siblings).

Service total p90: none 4.56 / 4.50 ms (1.19 / 1.23×); hc arms 3.6–4.2 ms
(0.91–1.11×); replication 3.5–4.6 ms. End-to-end p50 is 2.9–3.4 ms in every
cell: at 3600 rps the frontend path is far from saturation, so **neither
total nor e2e latency discriminates in this scenario**; only the handler does.

**Reading.**

1. Harvesting is the matched mitigation and the only one that moves the
   handler: 1.7× → 1.35–1.43×, repeatable to ±0.02 across trials. It still
   misses a 1.2× handler SLO; it meets 1.5×. hc-hold (no control loop) is as
   good as or slightly better than bang-bang (bang-bang released and
   re-applied 6–7 times per run, first action 2.4–3.2 s after ARM; hc-hold
   fires once at +1.7 s). hc-iso brings nothing over hc-hold.
2. Replication never meets any handler SLO and makes the **instance** worse
   the more replicas there are: the victim's own handler slows from 1.7× to
   1.8–3.0× because the replicas add load to the same node (othercores: the
   aggressors also roam 21–29; samecores: 2–4 Go processes time-share threads
   18–20) while the siblings stay loaded. Other-node replicas (1.76–1.93×)
   are the least harmful but still worse than doing nothing, because the
   per-instance handler slowdown is a per-core phenomenon the replica cannot
   touch. Replication does cut the victim's *consumed* cores (1.27 → 0.6–0.84)
   by moving 45–64 % of requests away, which is exactly why the service-level
   total latency looks fine while the handler is at 2–3×.
3. Cores-to-SLO (the apple): harvesting reaches 1.5× with **zero extra cores
   consumed** by the victim (1.08 vs 1.27 unmitigated, the handler is faster)
   and three sibling threads reclaimed from the aggressors (≈ 1.1 cores of
   aggressor work displaced to the rest of the pool; aggressor throughput
   unchanged). Replication consumes 0.8–1.8 extra cores (k × 3 threads
   allocated) and reaches no handler SLO in any placement.

---

## 3. INT — intrinsic overload (1-P victim), no aggressors

Why 1-P: the 3-P victim cannot be overloaded through the frontend chain on
this cluster. Knee probe (no aggressors, wrk2 8 threads / 512 conns): 14k rps
offered → victim 2.21 cores; 20k → 2.32 cores (19.1k achieved); 28k → 2.35
cores (19.4k achieved), no CFS throttling: the chain tops out at ≈ 19.4k
before a 3-P `search` does. A first INT sweep at 10k offered with the 3-P
victim showed nothing (total p90 5.1 ms flat) and is kept only as
`data/.motiv2-int-invalid-10k`. The overload scenarios therefore use
`cpu_set "18"` + `STAGE3_VICTIM_GOMAXPROCS=1` (per-config, the 3-P scenarios
are untouched), 1-P replicas, and 800 rps base + 10200 rps extra from t = 20 s
(`configs/motiv-sm110p-int.yaml`). Replication arms are forced at ARM
(`rep*f`) because processing_time is blind to run-queue wait — the score stays
at 0 through the whole overload, which is the documented limitation, not a
bug of this pass.

`data/motiv2-int`, 23:11–00:06. Armed = whole p3 (`--steady`).

| cell | handler p90 ratio | total p90 p1 → armed (ms) | service total p90 ratio | victim cores | replica cores | extra driver e2e p50/p90/p99 (ms) @ rps |
|---|---|---|---|---|---|---|
| none | 0.87 | 5.0 → 18.0 | 3.56 | 1.00 | – | 22 / 66 / 125 @ 10161 |
| hc-hold | 0.88 | 4.9 → 23.8 | 4.90 | 0.99 | – | 37 / 118 / 292 @ 10177 |
| rep1f othercores | 1.05 | 5.0 → 4.6 | 0.89 | 0.87 | 0.88 | 6.9 / 48 / 112 @ 10180 |
| rep1f othernode | 1.04 | 4.9 → 4.6 | 0.99 | 0.87 | 0.91 (node-3) | 7.6 / 56 / 127 @ 10181 |
| rep2f othercores | 1.13 | 5.0 → 3.2 | 0.62 | 0.75 | 1.52 | 10.5 / 1350 / 2680 @ 10161 |
| rep2f othernode | 1.07 | 4.9 → 3.4 | 0.71 | 0.72 | 1.56 (node-3) | 7.6 / 242 / 1380 @ 10161 |
| rep3f othercores | 1.06 | 5.1 → 3.0 | 0.60 | 0.61 | 2.05 | 6.1 / 48 / 111 @ 10180 |
| rep3f othernode | 1.10 | 5.0 → 4.3 | 0.84 | 0.63 | 1.79 (node-3) | 12.4 / 176 / 464 @ 10161 |
| rep3f samecores | 0.92 | 4.9 → 49.3 | 9.12 | 0.26 | 0.75 | 2200 / 10380 / 14340 @ 9097 |

**Reading.**

1. The overload is real and invisible to the handler: total p90 3.6×, handler
   0.87× (faster: batching at saturation), e_iso 0.
2. One replica off the victim's thread is sufficient (total p90 back to 0.9×,
   e2e p50 22 → 6.9 ms at the full 10.2k rps). Cost: **0.75 extra cores
   consumed** (0.87 + 0.88 = 1.75 vs 1.00). More replicas lower total latency
   further (3.0–3.4 ms) at 2.3–2.7 cores but are not needed for a 1.5× SLO.
   Other-node replicas are as effective as same-node other-core ones here.
3. Harvesting is the unmatched mitigation: the victim already owns its whole
   thread (0.99–1.00 cores) and there are no aggressors, so there is nothing
   to reclaim; hc-hold's 23.8 ms vs none's 18.0 ms is within what a saturated
   queue shows run to run (no socket errors or non-2xx in any cell).
4. Same-thread replicas are catastrophic: four 1-P processes time-share thread
   18 (victim 0.26 cores + replicas 0.75 = one thread), total p90 49 ms, e2e
   p50 2.2 s, driver falls to 9.1k rps.
5. The rep2f cells show whole-run e2e p99 tails of 1.4–2.7 s with no errors
   while their in-service totals are the best of the sweep; the likely cause
   is the scale-up transient at ARM (endpoint changes, cold replicas). Not
   verified from a time series.

---

## 4. SHORT — extrinsic, 3 s bursts (3-P victim)

`configs/motiv-sm110p-short.yaml` (burst 3 s / idle 17 s), `data/motiv2-short`,
00:06–00:36, report with `--burst-s 3`. **Inconclusive; do not use for the
comparison.** The SN/SS load drivers only ramp to ≈ 0.8 aggressor cores
within 3 s (11–12 cores in 10 s bursts), so the unmitigated handler p90 rises
only 1.14× (32.4 → 36.9 µs) and every cell meets both SLOs:

| cell | handler p90 ratio | total p90 ratio | victim + replica cores | aggressor cores | notes |
|---|---|---|---|---|---|
| none | 1.14 | 0.90 | 0.99 | 0.81 | |
| hc-bang | 1.19 | 0.92 | 1.03 | 0.80 | first action 4.2 s after ARM — after the burst ended; 6 toggles |
| hc-hold | 1.14 | 0.90 | 1.01 | 3.05 | aggressor cores value unexplained (window alignment?) |
| rep3 othercores | 1.21 | 0.91 | 0.76 + 1.28 = 2.04 | 1.20 | only cell to break the 1.2× handler SLO |
| rep3 othernode | 1.18 | 0.91 | 0.77 + 1.06 = 1.83 | 1.41 | 2 of 3 replicas came up in time |

Usable fact: bang-bang reaction time (1 s smoothing + threshold) is ≈ 4 s,
longer than a 3 s burst. The aggressor model in this testbed cannot produce
sub-5 s contention bursts of the eval intensity.

---

## 5. MIXED — 1-P victim overload + 10 s aggressor bursts

`configs/motiv-sm110p-mixed.yaml`: as INT (800 + 10200 rps, 1-P victim on 18)
plus the SN/SS bursts of sweep A. `data/motiv2-mixed`, 00:36–01:18, report with
`--steady` (the overload is continuous; the handler ratios are therefore
p3-wide, bursts and idle gaps averaged — sweep A's handler numbers are
burst-only and not directly comparable).

| cell | handler p90 ratio | service total p90 (ms, ratio) | victim + replica cores | aggressor cores | handler SLO 1.2× | total SLO 1.5× |
|---|---|---|---|---|---|---|
| none | 1.18 | 42.8 (8.5×) | 0.98 | 7.7 | yes | no |
| hc-bang | 1.04 | 43.9 (8.7×) | 0.99 | 6.9 | yes | no |
| hc-hold | 1.10 | 43.1 (8.5×) | 0.99 | 6.7 | yes | no |
| rep3f othercores | 1.62 | 15.9 (3.1×) | 0.78 + 2.25 = 3.02 | 6.9 | no | no |
| rep3f othernode | 1.39 | 16.7 (3.4×) | 0.71 + 2.13 = 2.84 | 6.8 | no | no |
| hcrep3 othercores | 1.36 | 13.4 (2.7×) | 0.72 + 2.25 = 2.97 | 6.9 | no | no |
| hcrep3 othernode | 1.26 | 7.1 (1.44×) | 0.69 + 1.93 = 2.62 | 6.5 | no | **yes** |

**Reading.**

1. Matched vs unmatched in one table. Harvesting removes the sibling
   contention (siblings busy 0.34 → 0.09–0.20, handler 1.18 → 1.04–1.10×)
   and leaves the overload untouched (total p90 43 ms in all three). It also
   does not recover throughput: the extra driver completes 8.0–8.4k rps in
   none and the hc cells vs 10.1–10.2k in every replicated cell — the
   saturated 1-P `search` throttles the whole chain.
2. Replication removes the overload (total 43 → 16 ms) but makes the handler
   *worse* (1.4–1.6×): the 1-P replicas on 21–23 sit on cores whose siblings
   5–7 carry aggressors, and HC's Primary set covers only 18–20, so in
   `hcrep3-othercores` the replicas are unprotected (the victim instance
   alone improves, 1.62 → 1.36×). Other-node replicas avoid the node-4
   aggressors entirely, which is why hcrep3-othernode is the only cell under
   the total SLO (7.1 ms, 1.44×) — at 2.62 consumed cores, and still 1.26× on
   the handler.
3. Nothing meets the 1.2× handler SLO except the hc-only arms, which fail
   the total SLO. A fair same-node combination would need HC's Primary set
   extended to the replica threads (`HC_VICTIM_CPULIST=18,19,20,21,22,23`); not
   run.
4. The extra driver's e2e numbers for this sweep (p50 4–17 s in every cell)
   are **not usable**: wrk2 aggregates t = 20–140 s and a backlog builds during
   the 60 unmitigated seconds of p2; only the in-service total p90 of the armed
   window is meaningful here. Neighbour columns (`SS p50` 9.2 in
   hcrep3-othercores) are from a single aggressor pod's own stream and noisy.

---

## 6. Cores-to-SLO summary (the apple)

| scenario | matched | unmatched | cheapest cell meeting the SLO | consumed cores (victim + replicas) | notes |
|---|---|---|---|---|---|
| extrinsic bursts (A) | harvesting | replication | hc-hold / hc-bang at 1.5× handler | 1.08–1.10 (vs 1.21–1.27 none) + 3 sibling threads reclaimed | no cell meets 1.2×; replication 1.8–3.0× at 1.6–2.3 cores |
| intrinsic overload (INT) | replication | harvesting | rep1f othercores/othernode at 1.5× total | 1.75 (vs 1.00 none) | hc-hold 0.99 cores, no effect; samecores 10× worse |
| both (MIXED) | hc + replication | either alone | hcrep3 othernode at 1.5× total | 2.62 | handler still 1.26×; same-node variant needs HC Primary over the replica threads |
| 3 s bursts (SHORT) | – | – | everything (no contention materialises) | – | inconclusive |

Budget view (allocated rather than consumed): harvesting = the victim's 3
physical cores made exclusive (3 sibling threads taken from a 28-thread pool);
replication = k × 3 threads (3-P) or k × 1 thread (1-P) added.

---

## 7. Caveats and anomalies to keep in mind

- **p1 total p90 outliers.** In trial 1 `rep1-othernode`, `rep2-othercores`,
  `rep2-othernode` and in trial 2 `rep2-othernode` the p1 total p90 was
  6.1–7.3 ms instead of the usual 3.6–3.9 ms, so their total-ratio columns
  (0.55–0.62) are artifacts of an inflated baseline, not improvements. Handler
  p1 values are normal in those runs (32.6–33.9 µs).
- **Trial 1 `rep2-samecores`** neighbour columns (SS p50 ratio 0.03, aggressor
  cores 8.98 vs ≈ 11.5 elsewhere) are suspect; the victim-side numbers of that
  cell (2.27×, repeated at 2.46× in trial 2) are consistent.
- **Sweep A other-node replica cores are unsampled** (node-3 sampler added
  for the later sweeps; trial 2 has them: 0.72 / 0.79 / 1.16 cores for rep1/2/3).
- **Handler SLO is blind in the overload scenarios** (processing_time cannot
  see run-queue wait), and **total/e2e SLO is uninformative in the extrinsic
  scenario** (3600 rps is far from saturation). Each scenario has one
  discriminating metric; the tables state both.
- **Extra-driver wrk2 e2e is a whole-run aggregate** (t = 20–140 s). Fine in
  INT (no backlog), meaningless in MIXED.
- **MIXED handler ratios are p3-wide averages** (bursts + gaps), sweep A's
  are burst-only. Compare within a sweep only.
- **1-P victim** in INT/MIXED is a deliberate departure from the July layout
  (user asked for July's 3-P victim; it cannot be overloaded here, section 3).
- Single trial everywhere except sweep A. Sweep A repeatability: handler ratios
  within ±0.05 for none/hc, within ±0.25 for replication cells.
- hc-bang's limit cycle (6–12 apply/release cycles per run) is unchanged from
  October; hc-hold avoids it and performs the same or better — the "no control
  logic" arm the user asked for is the better reference for the paper.

---

## 8. Data layout and regenerating the tables

```
C:\tmp\motiv2\                     (= simple-mitigation-1/motiv2-sm110p.tgz)
  data/motiv2/<cell>/stage3eval_*/   sweep A trial 1 (12 cells), report.csv
  data/motiv2-t2/<cell>/...          sweep A trial 2 (13 cells, + hc-hold, hc-iso)
  data/motiv2-int/<cell>/...         INT (9 cells)
  data/motiv2-short/<cell>/...       SHORT (5 cells)
  data/motiv2-mixed/<cell>/...       MIXED (7 cells)
  data/.motiv2-int-invalid-10k/      discarded 3-P INT attempt
  data/motiv2-queue.log              sweep timestamps
  score-verify/                      calibration json/csv/log (also committed in dsb stage3-eval/score-verify)
per run dir:
  runs/run_1/wrk2/hotelReservation{,-extra}.txt       e2e (base / extra driver)
  runs/run_1/.../run_data.json, run_data-overflow-*.json  victim / replica windowed streams
  mitigation/cpu_usage.csv[, cpu_usage-node-3.csv]    sampler rows (unix_ms,key,a,b)
  mitigation/pod_uids_{p3,end}.txt                    uid -> ns, pod, node
  mitigation/*.log, scores.csv, actions                controller / HC logs, score stream, scale actions
  <cell>/placement.txt                                  which overflow manifest was used
```

```bash
cd dsb_hd_counter-s3/hotelReservation/noisy-neighbors/stage3-eval
python3 tools/motiv2_report.py C:/tmp/motiv2/data/motiv2        # sweep A (10 s bursts)
python3 tools/motiv2_report.py C:/tmp/motiv2/data/motiv2-t2
python3 tools/motiv2_report.py C:/tmp/motiv2/data/motiv2-int   --steady
python3 tools/motiv2_report.py C:/tmp/motiv2/data/motiv2-short --burst-s 3
python3 tools/motiv2_report.py C:/tmp/motiv2/data/motiv2-mixed --steady
#   --slo-proc 1.2 --slo-total 1.5   SLO thresholds;  --csv out.csv;  --phase-cores  per-phase cores
```

Re-running a sweep on a live cluster: `stage3-eval/run-motiv2.sh` with
`CFG=`, `OUT=`, `CELLS="arm[:placement] ..."`, `STAGE3_VICTIM_GOMAXPROCS=1
OVF_PREFIX=overflow1p` for the 1-P scenarios (RUNBOOK section 15).

---

## 8b. Figures

`simulation/results/motiv2_*.{png,pdf}`, rendered by
`stage3-eval/tools/motiv2_plot.py --root C:/tmp/motiv2/data --out simulation/results`
(reads the per-sweep `report.csv` for the bar/scatter figures and the raw run
dirs for the time series):

| file | what |
|---|---|
| `motiv2_bars` | per scenario (A / INT / MIXED): latency ratio per cell with the SLO lines, over the consumed cores stacked as victim + replicas (hatched = on node-3); sweep A shows both trials |
| `motiv2_cores_vs_slo` | the headline: consumed cores (x) vs the scenario's discriminating latency ratio (y, log); dashed 1.5×, dotted 1.2×; colour = mitigation family, marker = placement |
| `motiv2_ts_ext` | sweep A trial 2, six cells: handler p90 per second; siblings / victim-thread busy fraction and replica cores; e_iso with the actuator state (HC held or replica count). Shows the siblings being cleared by HC, the bang-bang limit cycle, and the handler getting worse as replicas arrive |
| `motiv2_ts_int` | INT, six cells: pooled service total p90 (log), victim / replica cores per second, handler p90 (flat: blind to the overload) |
| `motiv2_ts_mixed` | MIXED, six cells: handler p90, service total p90 (log), sibling busy + replica cores. Harvesting flattens row 1, replication drops row 2, only the combination does both |

## 9. Open follow-ups

1. Same-node fair combination for MIXED: HC Primary extended over the replica
   threads (one cell, 6 min).
2. A second trial of INT and MIXED (the only single-trial scenarios that
   matter for the paper).
3. Time series of the rep2f INT cells to confirm the scale-up transient
   explanation for the e2e tails.
4. Scenario 4 of the design discussion (memory-bandwidth / LLC aggressor where
   harvesting is the unmatched mitigation) — not implemented.
5. Label percentile for the predictor (p50 vs p90) is still open; this pass's
   unmitigated handler p50 is 1.26–1.31× under the July pool (was 1.05× under
   the 6-thread R650 pool), which makes p50 labels viable again.
