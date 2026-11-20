#!/bin/bash
# queue-int1p.sh -- c6320 intrinsic scenario with a 1-P victim (user 2026-11-10: a load where one replica beats
# harvesting without overloading the chain). Cells: none (1 P on thread 4), rep1f (1-P victim + 1-P replica on
# node-3), hc-grow1 (victim threads 4 own + 6 shared with the burner, GOMAXPROCS 2). SUFFIX="-t2" for trial 2.
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
S3=/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
SM1=/local/simple-mitigation-1/deploy/stage3
LOG=/tmp/queue-int1p.log
log() { echo "[$(date -u '+%F %T')] $*" | tee -a $LOG; }
run() { ./run-motiv3.sh </dev/null 2>&1 | tee -a $LOG | grep --line-buffered -E "motiv3\]|FATAL|FAILED|WARNING"; return ${PIPESTATUS[0]}; }
cd $S3 || exit 1
ls -d data/stage3eval_* >/dev/null 2>&1 && { log "FATAL: data/stage3eval_* exists"; exit 1; }
export AGGR_CPUSET="0,2,4,6,8,10,12,14,16,18,20,22,28,30,32,34,36,38,40,42,44,46,48,50"
export OVF=overflow-node3clean-c6320-1p LOAD_MANIFEST=motiv2/node3-load-c6320.yaml
export STAGE3_PERF_EVENTS=cycles,ref-cycles,instructions,LLC-loads,LLC-load-misses,branch-instructions,branch-misses,page-faults
SUFFIX="${SUFFIX:-}"
# the hc-grow1 arm into node-0's arms dir and node-4's kit
cp $SM1/arms-c6320/hc-grow1.env $SM1/arms/hc-grow1.env
scp -q -o BatchMode=yes $SM1/arms-c6320/hc-grow1.env node-4:/tmp/hc-grow1.env && ssh -o BatchMode=yes node-4 "sudo cp /tmp/hc-grow1.env /tmp/mitig/arms/hc-grow1.env && grep -E '^HC_(VICTIM|SECONDARY)' /tmp/mitig/arms/hc-grow1.env | tr '\n' ' '" 2>&1 | grep -v Warning; echo
log "## 1-P cells: ${CELLS_1P:-none rep1f} (configs/motiv3-c6320-int1p.yaml, GOMAXPROCS 1)"
OUT=data/motiv3-c6320-int1p$SUFFIX CFG=configs/motiv3-c6320-int1p.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=1 CELLS="${CELLS_1P:-none rep1f}" run; log "## 1-P cells rc=$?"
log "## hc-grow1 (threads 4 + 6, GOMAXPROCS 2, burner 12 + 6)"
OUT=data/motiv3-c6320-grow1p$SUFFIX CFG=configs/motiv3-c6320-int1p2.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared1-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=2 CELLS="hc-grow1" run; log "## hc-grow1 rc=$?"
log "## QUEUE-INT1P DONE"
