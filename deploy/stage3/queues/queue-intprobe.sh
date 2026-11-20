#!/bin/bash
# queue-intprobe.sh -- c6320 intrinsic load probe (user 2026-11-10: find the load where replication beats
# harvesting). For each offered load (10000, 12000): rep1f (2-P victim + 1 replica) and hc-grow (4-thread
# victim sharing 8,10 with the burner). Cells land in data/motiv3-c6320-int<K>k / data/motiv3-c6320-grow<K>k;
# LOADS / CELLS_REP / CELLS_GROW override. Needs a live ssh -A session.
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
S3=/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
K=/local/dsb_hd_counter/hotelReservation/kubernetes/search/search-deployment.yaml
LOG=/tmp/queue-intprobe.log
log() { echo "[$(date -u '+%F %T')] $*" | tee -a $LOG; }
run() { ./run-motiv3.sh </dev/null 2>&1 | tee -a $LOG | grep --line-buffered -E "motiv3\]|FATAL|FAILED|WARNING"; return ${PIPESTATUS[0]}; }
cd $S3 || exit 1
ls -d data/stage3eval_* >/dev/null 2>&1 && { log "FATAL: data/stage3eval_* exists"; exit 1; }
export AGGR_CPUSET="0,2,4,6,8,10,12,14,16,18,20,22,28,30,32,34,36,38,40,42,44,46,48,50"
export OVF=overflow-node3clean-c6320 LOAD_MANIFEST=motiv2/node3-load-c6320.yaml
export STAGE3_PERF_EVENTS=cycles,ref-cycles,instructions,LLC-loads,LLC-load-misses,branch-instructions,branch-misses,page-faults
SUFFIX="${SUFFIX:-}"   # e.g. -t2 for a second trial
for L in ${LOADS:-10 12}; do
    log "## load ${L}k: rep cells (${CELLS_REP:-rep1f})"
    OUT=data/motiv3-c6320-int${L}k$SUFFIX CFG=configs/motiv3-c6320-int${L}k.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-c6320.yaml CELLS="${CELLS_REP:-rep1f}" run; log "## load ${L}k rep rc=$?"
    log "## load ${L}k: hc-grow (search limit 3 -> 4)"
    cp $K /tmp/search-deployment.yaml.bak
    sed -i 's/^\(\s*\)cpu: "3"$/\1cpu: "4"/' $K
    OUT=data/motiv3-c6320-grow${L}k$SUFFIX CFG=configs/motiv3-c6320-int4${L}k.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=4 CELLS="${CELLS_GROW:-hc-grow}" run; log "## load ${L}k hc-grow rc=$?"
    cp /tmp/search-deployment.yaml.bak $K && log "## manifest restored: $(grep -A1 limits: $K | tail -1 | tr -d ' ')"
    kubectl set resources deployment search --limits=cpu=3 >/dev/null 2>&1
done
log "## QUEUE-INTPROBE DONE"
