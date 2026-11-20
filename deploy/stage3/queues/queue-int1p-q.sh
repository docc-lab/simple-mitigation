#!/bin/bash
# queue-int1p-q.sh -- c6320 intrinsic steady cells with the SAME quiet load as the extrinsic scenario
# (user 2026-11-11: "make sure all quiet load is the same across different scenarios"). Quiet phase Q rps
# (default 2000 = the extrinsic victim load) + extra to L rps offered from t = 20 s (default 6000, the figure's
# load). Cells none, rep1f (1-P victim on thread 4, 1-P replica on node-3) and hc-grow1 (thread 4 own + 6 shared
# with the burner) -> data/int1pq/Q<Q>-L<L>-t<T>/{none,rep1f,hc-grow1}-quiet, TRIALS trials (default 3).
# Resumable (complete cells skipped, partial ones dropped), second pass, tgz + /proj mirror at the end.
# Needs configs/motiv3-c6320-int1p-q<Q>-<L>.yaml and -int1p2-q<Q>-<L>.yaml (q2000-6000 exist). Detached use:
#   unset SSH_AUTH_SOCK; setsid nohup bash /tmp/queue-int1p-q.sh > /tmp/queue-int1p-q.out 2>&1 < /dev/null &
# DO NOT start while another experiment uses node-3/node-4 (it kills controllers, samplers and reapplies manifests).
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
unset SSH_AUTH_SOCK
S3=/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
SM1=/local/simple-mitigation-1/deploy/stage3
Q=${Q:-2000}; L=${L:-6000}; TRIALS=${TRIALS:-3}
LOG=/tmp/queue-int1p-q.log
log() { echo "[$(date -u '+%F %T')] $*" | tee -a $LOG; }
run() { ./run-motiv3.sh </dev/null 2>&1 | tee -a $LOG | grep --line-buffered -E "motiv3\]|FATAL|FAILED|WARNING"; return ${PIPESTATUS[0]}; }
cd $S3 || exit 1
export AGGR_CPUSET="0,2,4,6,8,10,12,14,16,18,20,22,28,30,32,34,36,38,40,42,44,46,48,50"
export OVF=overflow-node3clean-c6320-1p LOAD_MANIFEST=motiv2/node3-load-c6320.yaml
export STAGE3_PERF_EVENTS=cycles,ref-cycles,instructions,LLC-loads,LLC-load-misses,branch-instructions,branch-misses,page-faults
CFG1=configs/motiv3-c6320-int1p-q$Q-$L.yaml; CFG2=configs/motiv3-c6320-int1p2-q$Q-$L.yaml
[ -s $CFG1 ] && [ -s $CFG2 ] || { log "FATAL: $CFG1 / $CFG2 missing"; exit 1; }

cleanup() {   # kill leftovers of a previous attempt, drop partial cells
    pkill -TERM -f 'run-motiv[3].sh'; pkill -TERM -f 'run-phased-stage[3].sh'; sleep 2
    pkill -TERM -f 'stage3-eva[l].sh'; pkill -f 'wrk2/wr[k]'; sleep 2
    pkill -9 -f 'run-motiv[3].sh|run-phased-stage[3].sh|stage3-eva[l].sh' 2>/dev/null
    ssh -o BatchMode=yes node-4 'sudo pkill -f "[m]itigation-controller"; sudo pkill -f "[r]un-mitigation"; sudo systemctl stop hc-balancer-dyn 2>/dev/null; sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
    ssh -o BatchMode=yes node-3 'sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
    rm -rf data/stage3eval_*
    for d in data/int1pq/*/*-quiet/; do
        [ -d "$d" ] || continue
        r=$(ls -d "$d"stage3eval_*/runs/run_1 2>/dev/null | head -1)
        if [ -z "$r" ] || [ ! -s "$r/run_data.json" ] || [ ! -s "$r/mitigation/cpu_usage.csv" ]; then log "dropping partial cell $d"; rm -rf "$d"; continue; fi
        case "$d" in *rep1f*) ls "$r"/run_data-overflow-*.json >/dev/null 2>&1 || { log "dropping replica cell without replica data $d"; rm -rf "$d"; };; esac
    done
    kubectl scale deployment search-overflow --replicas=0 >/dev/null 2>&1
    kubectl set resources deployment search --limits=cpu=3 >/dev/null 2>&1
}
one() {   # <trial>
    local T=$1 OUTD=data/int1pq/Q$Q-L$L-t$1
    [ -d "$OUTD/none-quiet" ] && [ -d "$OUTD/rep1f-quiet" ] && [ -d "$OUTD/hc-grow1-quiet" ] && { log "skip Q$Q L$L t$T (complete)"; return 0; }
    log "## Q$Q L$L t$T: none rep1f"
    OUT=$OUTD CFG=$CFG1 ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=1 CELLS="none rep1f" run; log "## Q$Q L$L t$T none/rep1f rc=$?"
    log "## Q$Q L$L t$T: hc-grow1"
    OUT=$OUTD CFG=$CFG2 ARM_OFFSET_S=2 BURNER=1 BURNER_MANIFEST=motiv2/burner-node4-shared1-c6320.yaml STAGE3_VICTIM_GOMAXPROCS=2 CELLS="hc-grow1" run; log "## Q$Q L$L t$T hc-grow1 rc=$?"
}
pass() { for T in $(seq 1 $TRIALS); do one $T; done; }
log "## INT1P-Q START Q=$Q L=$L trials=$TRIALS (hops: $(ssh -o BatchMode=yes node-4 hostname 2>/dev/null || echo NO-KEY) / $(ssh -o BatchMode=yes node-3 hostname 2>/dev/null || echo NO-KEY))"
# the hc-grow1 arm into node-0's arms dir and node-4's kit
cp $SM1/arms-c6320/hc-grow1.env $SM1/arms/hc-grow1.env
scp -q -o BatchMode=yes $SM1/arms-c6320/hc-grow1.env node-4:/tmp/hc-grow1.env && ssh -o BatchMode=yes node-4 "sudo mkdir -p /tmp/mitig/arms && sudo cp /tmp/hc-grow1.env /tmp/mitig/arms/hc-grow1.env" 2>&1 | grep -v Warning
ssh -o BatchMode=yes node-4 'ls /tmp/mitig/arms/hc-grow1.env' >/dev/null 2>&1 || { log "FATAL: hc-grow1 arm missing in the node-4 kit"; exit 1; }
cleanup
pass
log "## second pass over anything that failed"
cleanup
pass
cleanup
tar czf /tmp/motiv3-c6320-int1pq.tgz data/int1pq $LOG 2>/dev/null && log "## motiv3-c6320-int1pq.tgz ready ($(du -h /tmp/motiv3-c6320-int1pq.tgz | cut -f1))"
DST=/proj/tracing-pythia-PG0/royno7-hc-archive-c6320-2026-11-10; mkdir -p $DST/archive $DST/data && cp /tmp/motiv3-c6320-int1pq.tgz $DST/archive/ && rsync -a data/int1pq $DST/data/ && (cd $DST && md5sum archive/*.tgz > MD5SUMS) && log "## /proj mirror updated"
log "## INT1P-Q DONE"
