#!/bin/bash
# queue-eq.sh -- c6320 budget sweep for the evaluation questions (user 2026-11-11):
#   EQ1  at the same SLO, how much degradation do the matched and the mismatched mitigation remove?
#   EQ2  to reach the same SLO, how many cores does each need?
# Same quiet load (2000 rps) in both cells.
#   STEADY intrinsic (1-P victim thread 4, quiet 2000 rps + 4000 extra from t = 20 s = 6000 offered), 3 trials:
#     none, rep1f..rep4f (1-P replicas on node-3)   configs/motiv3-c6320-int1p-q2000-6000.yaml, GOMAXPROCS 1
#     hc-grow1 (thread 4 own + 6 shared)            -int1p2-q2000-6000, GOMAXPROCS 2, burner 12,6
#     hc-grow2 (4 own + 6,8 shared)                 -int1p3-q2000-6000, GOMAXPROCS 3, burner 12,6,8
#     hc-grow3 (4 own + 6,8,10 shared)              -int1p4-q2000-6000, GOMAXPROCS 4, burner 12,6,8,10, search limit 3 -> 4
#     -> data/eq/steady-t<T>/<cell>-quiet
#   TAIL extrinsic (2-P victim 4,6 at 2000 rps, 10 % co-tenant bursts; configs/motiv3-c6320.yaml), 2 trials:
#     rep1f rep3f rep6f (pre-provisioned 2-P replicas on node-3), rep6p (reactive, cap 6)
#     -> data/eq/tail-t<T>/<cell>-quiet      (none, hc-hold, rep3p: reuse data/motiv3-c6320{,-t2})
# Resumable (complete cells skipped, partial ones dropped), second pass, tgz + /proj mirror. Detached use:
#   unset SSH_AUTH_SOCK; setsid nohup bash /tmp/queue-eq.sh > /tmp/queue-eq.out 2>&1 < /dev/null &
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
unset SSH_AUTH_SOCK
S3=/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
SM1=/local/simple-mitigation-1/deploy/stage3
K=/local/dsb_hd_counter/hotelReservation/kubernetes/search/search-deployment.yaml
LOG=/tmp/queue-eq.log
log() { echo "[$(date -u '+%F %T')] $*" | tee -a $LOG; }
run() { ./run-motiv3.sh </dev/null 2>&1 | tee -a $LOG | grep --line-buffered -E "motiv3\]|FATAL|FAILED|WARNING"; return ${PIPESTATUS[0]}; }
cd $S3 || exit 1
export AGGR_CPUSET="0,2,4,6,8,10,12,14,16,18,20,22,28,30,32,34,36,38,40,42,44,46,48,50"
export LOAD_MANIFEST=motiv2/node3-load-c6320.yaml
export STAGE3_PERF_EVENTS=cycles,ref-cycles,instructions,LLC-loads,LLC-load-misses,branch-instructions,branch-misses,page-faults
STEADY_TRIALS=${STEADY_TRIALS:-3}; TAIL_TRIALS=${TAIL_TRIALS:-2}
STEADY_REP="${STEADY_REP:-none rep1f rep2f rep3f rep4f}"
TAIL_CELLS="${TAIL_CELLS:-rep1f:quiet rep3f:quiet rep6f:quiet rep6p:quiet}"
for c in configs/motiv3-c6320-int1p-q2000-6000.yaml configs/motiv3-c6320-int1p2-q2000-6000.yaml configs/motiv3-c6320-int1p3-q2000-6000.yaml configs/motiv3-c6320-int1p4-q2000-6000.yaml \
         motiv2/burner-node4-shared2-c6320.yaml motiv2/burner-node4-shared3-c6320.yaml $SM1/arms-c6320/hc-grow2.env $SM1/arms-c6320/hc-grow3.env $SM1/arms/rep4f.env; do
    [ -s $c ] || { log "FATAL: $c missing"; exit 1; }
done

limit() {   # 3 | 4 : the search deployment's cpu limit in the manifest hr_deploy re-applies
    local want=$1 have; have=$(grep -A1 'limits:' $K | grep -oE 'cpu: "[0-9]+"' | grep -oE '[0-9]+' | head -1)
    [ "$have" = "$want" ] && return 0
    sed -i "s/^\(\s*\)cpu: \"$have\"$/\1cpu: \"$want\"/" $K && log "search manifest limit $have -> $want"
}
cleanup() {   # kill leftovers of a previous attempt, drop partial cells, put the node back
    pkill -TERM -f 'run-motiv[3].sh'; pkill -TERM -f 'run-phased-stage[3].sh'; sleep 2
    pkill -TERM -f 'stage3-eva[l].sh'; pkill -f 'wrk2/wr[k]'; sleep 2
    pkill -9 -f 'run-motiv[3].sh|run-phased-stage[3].sh|stage3-eva[l].sh' 2>/dev/null
    ssh -o BatchMode=yes node-4 'sudo pkill -f "[m]itigation-controller"; sudo pkill -f "[r]un-mitigation"; sudo systemctl stop hc-balancer-dyn 2>/dev/null; sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
    ssh -o BatchMode=yes node-3 'sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
    rm -rf data/stage3eval_*
    for d in data/eq/*/*-quiet/; do
        [ -d "$d" ] || continue
        r=$(ls -d "$d"stage3eval_*/runs/run_1 2>/dev/null | head -1)
        if [ -z "$r" ] || [ ! -s "$r/run_data.json" ] || [ ! -s "$r/mitigation/cpu_usage.csv" ]; then log "dropping partial cell $d"; rm -rf "$d"; continue; fi
        case "$d" in *"/rep"*) ls "$r"/run_data-overflow-*.json >/dev/null 2>&1 || { log "dropping replica cell without replica data $d"; rm -rf "$d"; };; esac
    done
    limit 3
    kubectl set resources deployment search --limits=cpu=3 >/dev/null 2>&1
    kubectl scale deployment search-overflow --replicas=0 >/dev/null 2>&1
    kubectl scale deployment node3-load --replicas=0 >/dev/null 2>&1
    kubectl delete deployment hc-secondary-burner --ignore-not-found --wait=false >/dev/null 2>&1
}
steady() {   # <trial>
    local T=$1 OUTD=data/eq/steady-t$1 miss=0
    for c in $STEADY_REP hc-grow1 hc-grow2 hc-grow3; do [ -d "$OUTD/$c-quiet" ] || miss=1; done
    [ $miss = 0 ] && { log "skip steady t$T (complete)"; return 0; }
    log "## steady t$T: $STEADY_REP"
    OUT=$OUTD CFG=configs/motiv3-c6320-int1p-q2000-6000.yaml OVF=overflow-node3clean-c6320-1p ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=1 CELLS="$STEADY_REP" run; log "## steady t$T rep rc=$?"
    log "## steady t$T: hc-grow1"
    OUT=$OUTD CFG=configs/motiv3-c6320-int1p2-q2000-6000.yaml OVF=overflow-node3clean-c6320-1p ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared1-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=2 CELLS="hc-grow1" run; log "## steady t$T hc-grow1 rc=$?"
    log "## steady t$T: hc-grow2"
    OUT=$OUTD CFG=configs/motiv3-c6320-int1p3-q2000-6000.yaml OVF=overflow-node3clean-c6320-1p ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared2-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=3 CELLS="hc-grow2" run; log "## steady t$T hc-grow2 rc=$?"
    log "## steady t$T: hc-grow3 (search limit 4)"
    limit 4
    OUT=$OUTD CFG=configs/motiv3-c6320-int1p4-q2000-6000.yaml OVF=overflow-node3clean-c6320-1p ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared3-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=4 CELLS="hc-grow3" run; log "## steady t$T hc-grow3 rc=$?"
    limit 3; kubectl set resources deployment search --limits=cpu=3 >/dev/null 2>&1
}
tail_() {   # <trial>
    local T=$1 OUTD=data/eq/tail-t$1 miss=0
    for c in $TAIL_CELLS; do [ -d "$OUTD/${c%%:*}-${c#*:}" ] || miss=1; done
    [ $miss = 0 ] && { log "skip tail t$T (complete)"; return 0; }
    log "## tail t$T: $TAIL_CELLS"
    OUT=$OUTD CFG=configs/motiv3-c6320.yaml OVF=overflow-node3clean-c6320 CELLS="$TAIL_CELLS" run; log "## tail t$T rc=$?"
}
pass() {
    steady 1; tail_ 1; steady 2; tail_ 2
    for T in $(seq 3 $STEADY_TRIALS); do steady $T; done
    for T in $(seq 3 $TAIL_TRIALS); do tail_ $T; done
}
log "## EQ START (hops: $(ssh -o BatchMode=yes node-4 hostname 2>/dev/null || echo NO-KEY) / $(ssh -o BatchMode=yes node-3 hostname 2>/dev/null || echo NO-KEY))"
# arms into node-0's arms dir and node-4's kit
cp $SM1/arms-c6320/hc-grow1.env $SM1/arms-c6320/hc-grow2.env $SM1/arms-c6320/hc-grow3.env $SM1/arms/
for a in hc-grow1 hc-grow2 hc-grow3 rep4f; do
    scp -q -o BatchMode=yes $SM1/arms/$a.env node-4:/tmp/$a.env && ssh -o BatchMode=yes node-4 "sudo mkdir -p /tmp/mitig/arms && sudo cp /tmp/$a.env /tmp/mitig/arms/$a.env" 2>&1 | grep -v Warning
    ssh -o BatchMode=yes node-4 "ls /tmp/mitig/arms/$a.env" >/dev/null 2>&1 || { log "FATAL: $a arm missing in the node-4 kit"; exit 1; }
done
cleanup
pass
log "## second pass over anything that failed"
cleanup
pass
cleanup
tar czf /tmp/motiv3-c6320-eq.tgz data/eq $LOG 2>/dev/null && log "## motiv3-c6320-eq.tgz ready ($(du -h /tmp/motiv3-c6320-eq.tgz | cut -f1))"
DST=/proj/tracing-pythia-PG0/royno7-hc-archive-c6320-2026-11-10; mkdir -p $DST/archive $DST/data && cp /tmp/motiv3-c6320-eq.tgz $DST/archive/ && rsync -a data/eq $DST/data/ && (cd $DST && md5sum archive/*.tgz > MD5SUMS) && log "## /proj mirror updated"
log "## EQ DONE"
