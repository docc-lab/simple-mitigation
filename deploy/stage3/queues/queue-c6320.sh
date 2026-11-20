#!/bin/bash
# queue-c6320.sh -- motivation pass 3 re-run on the Clemson c6320 "icelake-perf1" (2026-11-10): every cell
# the figures use, two trials each.
#   EXT (configs/motiv3-c6320.yaml, 10 % bursts, enable at 120 s): none, hc-hold, rep3p:medium, rep3p:quiet
#       -> data/motiv3-c6320{,-t2}
#   INT (configs/motiv3-c6320-int.yaml, enable at 2 s, burner 12,14,16,18): none, rep1f -> data/motiv3-c6320-int{,-t2}
#   GROW (configs/motiv3-c6320-int4.yaml, 4-thread victim 4,6,8,10, search limit 3 -> 4, burner 12 + shared 8,10):
#       hc-grow -> data/motiv3-c6320-grow{,-t2}
# c6320 map: pool = socket 0 minus the HC daemons' 24,26 and their siblings 52,54; node-3 replicas on 4-26 even,
# load dial on 32-54 even. Needs a live `ssh -A` session (node-0 -> node-3/node-4 hops use the forwarded agent).
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
S3=/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
K=/local/dsb_hd_counter/hotelReservation/kubernetes/search/search-deployment.yaml
LOG=/tmp/queue-c6320.log
log() { echo "[$(date -u '+%F %T')] $*" | tee -a $LOG; }
run() { ./run-motiv3.sh </dev/null 2>&1 | tee -a $LOG | grep --line-buffered -E "motiv3\]|FATAL|FAILED|WARNING"; return ${PIPESTATUS[0]}; }
cd $S3 || exit 1
ls -d data/stage3eval_* >/dev/null 2>&1 && { log "FATAL: data/stage3eval_* exists"; exit 1; }
export AGGR_CPUSET="0,2,4,6,8,10,12,14,16,18,20,22,28,30,32,34,36,38,40,42,44,46,48,50"
export OVF=overflow-node3clean-c6320 LOAD_MANIFEST=motiv2/node3-load-c6320.yaml
# Haswell with SMT on has 4 programmable counters per thread: the trainer's 10-event set (6 programmable) does
# not fit (dTLB-load-misses: EINVAL), so the victim samples 4 programmable events + the fixed ones.
export STAGE3_PERF_EVENTS=cycles,ref-cycles,instructions,LLC-loads,LLC-load-misses,branch-instructions,branch-misses,page-faults
EXT_CELLS="${EXT_CELLS:-none hc-hold rep3p:medium rep3p:quiet}"
INT_CELLS="${INT_CELLS:-none rep1f}"
for T in "" "-t2"; do
    log "## EXT trial${T:--1}: $EXT_CELLS"
    OUT=data/motiv3-c6320$T CFG=configs/motiv3-c6320.yaml CELLS="$EXT_CELLS" run; log "## EXT trial${T:--1} rc=$?"
    log "## INT trial${T:--1}: $INT_CELLS"
    OUT=data/motiv3-c6320-int$T CFG=configs/motiv3-c6320-int.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-c6320.yaml CELLS="$INT_CELLS" run; log "## INT trial${T:--1} rc=$?"
    log "## GROW trial${T:--1}: hc-grow (search limit 3 -> 4 for this cell)"
    cp $K /tmp/search-deployment.yaml.bak
    sed -i 's/^\(\s*\)cpu: "3"$/\1cpu: "4"/' $K
    OUT=data/motiv3-c6320-grow$T CFG=configs/motiv3-c6320-int4.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=4 CELLS="hc-grow" run; log "## GROW trial${T:--1} rc=$?"
    cp /tmp/search-deployment.yaml.bak $K && log "## manifest restored: $(grep -A1 limits: $K | tail -1 | tr -d ' ')"
    kubectl set resources deployment search --limits=cpu=3 >/dev/null 2>&1
done
tar czf /tmp/motiv3-c6320.tgz data/motiv3-c6320 data/motiv3-c6320-t2 data/motiv3-c6320-int data/motiv3-c6320-int-t2 data/motiv3-c6320-grow data/motiv3-c6320-grow-t2 \
    data/smoke-c6320 score-verify/calibration-node4-c6320-2p-proc.log score-verify/gordion-node4-c6320-*.json score-verify/curve-node4-c6320-*.csv $LOG 2>/dev/null \
    && log "## motiv3-c6320.tgz ready ($(du -h /tmp/motiv3-c6320.tgz | cut -f1))"
log "## QUEUE-C6320 DONE"
