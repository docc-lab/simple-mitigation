#!/bin/bash
# queue-motiv3.sh -- motivation pass 3 sweep on node-0 (2026-11-08/09), unattended.
#   round 1: extrinsic 10 % bursts, 12 cells (none, hc-bang, hc-hold, rep{1,3,6}f x
#            {quiet, medium, contended} on node-3)            -> data/motiv3
#   round 2: intrinsic steady overload, 2-P victim, HC Secondary burner, enabled at
#            t=2 s: none, hc-int, rep1f, rep2f, rep3f (node-3 quiet) -> data/motiv3-int
#   round 3/4: second trials                                    -> data/motiv3-t2, data/motiv3-int-t2
# Reports after each round; proj-sync.sh runs alongside (started separately).
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval || exit 1
log() { echo "[$(date -u "+%F %H:%M:%S")] $*"; }
run() { ./run-motiv3.sh </dev/null 2>&1 | grep --line-buffered -E "motiv3\]|FATAL|FAILED|WARNING"; return ${PIPESTATUS[0]}; }
EXT_CELLS="none hc-bang hc-hold rep1f:quiet rep3f:quiet rep6f:quiet rep1f:medium rep3f:medium rep6f:medium rep1f:contended rep3f:contended rep6f:contended"
INT_CELLS="none hc-int rep1f rep2f rep3f"
log "## ROUND 1: extrinsic trial 1"
OUT=data/motiv3 CELLS="$EXT_CELLS" run; log "## extrinsic trial 1 rc=$?"
log "## ROUND 2: intrinsic trial 1"
OUT=data/motiv3-int CFG=configs/motiv3-sm110p-int.yaml ARM_OFFSET_S=2 BURNER=1 CELLS="$INT_CELLS" run; log "## intrinsic trial 1 rc=$?"
log "## ROUND 3: extrinsic trial 2"
OUT=data/motiv3-t2 CELLS="$EXT_CELLS" run; log "## extrinsic trial 2 rc=$?"
log "## ROUND 4: intrinsic trial 2"
OUT=data/motiv3-int-t2 CFG=configs/motiv3-sm110p-int.yaml ARM_OFFSET_S=2 BURNER=1 CELLS="$INT_CELLS" run; log "## intrinsic trial 2 rc=$?"
tar -czf /tmp/motiv3.tgz data/motiv3 data/motiv3-int data/motiv3-t2 data/motiv3-int-t2 score-verify/calibration-node4-sm110p-2p-proc.log score-verify/gordion.json score-verify/curve.csv /tmp/queue-motiv3.log 2>/dev/null
log "## motiv3.tgz ready ($(du -h /tmp/motiv3.tgz | cut -f1))"
log "## QUEUE-MOTIV3 DONE"
