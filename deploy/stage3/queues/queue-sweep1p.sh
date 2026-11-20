#!/bin/bash
# queue-sweep1p.sh -- unattended 1-P intrinsic load sweep on the c6320 (user 2026-11-10, ~10 h):
# for each offered load L and trial T: cells none, rep1f (1-P victim on thread 4, GOMAXPROCS 1, 1-P replica on
# node-3) and hc-grow1 (victim thread 4 own + 6 shared with the burner, GOMAXPROCS 2) ->
# data/sweep1p/L<L>-t<T>/{none,rep1f,hc-grow1}-quiet. Resumable (complete cells are skipped, partial ones dropped);
# a second pass at the end redoes anything that failed. Runs detached (setsid nohup) with node-0's own ssh key.
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
unset SSH_AUTH_SOCK
S3=/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
LOG=/tmp/queue-sweep1p.log
log() { echo "[$(date -u '+%F %T')] $*" | tee -a $LOG; }
run() { ./run-motiv3.sh </dev/null 2>&1 | tee -a $LOG | grep --line-buffered -E "motiv3\]|FATAL|FAILED|WARNING"; return ${PIPESTATUS[0]}; }
cd $S3 || exit 1
export AGGR_CPUSET="0,2,4,6,8,10,12,14,16,18,20,22,28,30,32,34,36,38,40,42,44,46,48,50"
export OVF=overflow-node3clean-c6320-1p LOAD_MANIFEST=motiv2/node3-load-c6320.yaml
export STAGE3_PERF_EVENTS=cycles,ref-cycles,instructions,LLC-loads,LLC-load-misses,branch-instructions,branch-misses,page-faults
LOADS_ALL="${LOADS_ALL:-3000 3500 4000 4500 5000 5500 6000 7000 8000}"
LOADS_T3="${LOADS_T3:-4000 4500 5000 5500 6000}"

cleanup() {   # kill leftovers of a previous attempt, drop partial cells
    pkill -TERM -f 'run-motiv[3].sh'; pkill -TERM -f 'run-phased-stage[3].sh'; sleep 2
    pkill -TERM -f 'stage3-eva[l].sh'; pkill -f 'wrk2/wr[k]'; sleep 2
    pkill -9 -f 'run-motiv[3].sh|run-phased-stage[3].sh|stage3-eva[l].sh' 2>/dev/null
    ssh -o BatchMode=yes node-4 'sudo pkill -f "[m]itigation-controller"; sudo pkill -f "[r]un-mitigation"; sudo systemctl stop hc-balancer-dyn 2>/dev/null; sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
    ssh -o BatchMode=yes node-3 'sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
    rm -rf data/stage3eval_*
    for d in data/sweep1p/*/*-quiet/; do
        [ -d "$d" ] || continue
        r=$(ls -d "$d"stage3eval_*/runs/run_1 2>/dev/null | head -1)
        if [ -z "$r" ] || [ ! -s "$r/run_data.json" ] || [ ! -s "$r/mitigation/cpu_usage.csv" ]; then log "dropping partial cell $d"; rm -rf "$d"; continue; fi
        case "$d" in *rep1f*) ls "$r"/run_data-overflow-*.json >/dev/null 2>&1 || { log "dropping replica cell without replica data $d"; rm -rf "$d"; };; esac
    done
    kubectl scale deployment search-overflow --replicas=0 >/dev/null 2>&1
    kubectl set resources deployment search --limits=cpu=3 >/dev/null 2>&1
}
one() {   # <load> <trial>
    local L=$1 T=$2 OUTD=data/sweep1p/L$1-t$2
    [ -d "$OUTD/none-quiet" ] && [ -d "$OUTD/rep1f-quiet" ] && [ -d "$OUTD/hc-grow1-quiet" ] && { log "skip L$L t$T (complete)"; return 0; }
    log "## L$L t$T: none rep1f"
    OUT=$OUTD CFG=configs/motiv3-c6320-int1p-$L.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=1 CELLS="none rep1f" run; log "## L$L t$T none/rep1f rc=$?"
    log "## L$L t$T: hc-grow1"
    OUT=$OUTD CFG=configs/motiv3-c6320-int1p2-$L.yaml ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared1-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=2 CELLS="hc-grow1" run; log "## L$L t$T hc-grow1 rc=$?"
}
pass() {
    for T in 1 2; do for L in $LOADS_ALL; do one $L $T; done; done
    for L in $LOADS_T3; do one $L 3; done
}
log "## SWEEP1P START (hops: $(ssh -o BatchMode=yes node-4 hostname 2>/dev/null || echo NO-KEY) / $(ssh -o BatchMode=yes node-3 hostname 2>/dev/null || echo NO-KEY))"
ssh -o BatchMode=yes node-4 'ls /tmp/mitig/arms/hc-grow1.env' >/dev/null 2>&1 || { log "FATAL: hc-grow1 arm missing in the node-4 kit"; exit 1; }
cleanup
pass
log "## second pass over anything that failed"
cleanup
pass
cleanup
tar czf /tmp/motiv3-c6320-sweep1p.tgz data/sweep1p $LOG 2>/dev/null && log "## motiv3-c6320-sweep1p.tgz ready ($(du -h /tmp/motiv3-c6320-sweep1p.tgz | cut -f1))"
DST=/proj/tracing-pythia-PG0/royno7-hc-archive-c6320-2026-11-10; mkdir -p $DST/archive $DST/data && cp /tmp/motiv3-c6320-sweep1p.tgz $DST/archive/ && rsync -a data/sweep1p $DST/data/ && (cd $DST && md5sum archive/*.tgz > MD5SUMS) && log "## /proj mirror updated"
log "## SWEEP1P DONE"
