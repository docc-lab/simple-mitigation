#!/bin/bash
# launch-c6320.sh -- (re)start the c6320 queue on node-0 inside a live `ssh -A` session: kill leftovers of a
# previous attempt, drop partial cells, start proj-sync, run queue-c6320.sh (resumable: complete cells are skipped).
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
log() { echo "[$(date -u '+%F %T')] $*"; }
S3=/local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
cd $S3 || exit 1
pkill -TERM -f 'queue-c632[0].sh' ; pkill -TERM -f 'run-motiv[3].sh' ; pkill -TERM -f 'run-phased-stage[3].sh'; sleep 2
pkill -TERM -f 'stage3-eva[l].sh'; pkill -f 'wrk2/wr[k]'; sleep 2
pkill -9 -f 'queue-c632[0].sh|run-motiv[3].sh|run-phased-stage[3].sh|stage3-eva[l].sh' 2>/dev/null
ssh -o BatchMode=yes node-4 'sudo pkill -f "[m]itigation-controller"; sudo pkill -f "[r]un-mitigation"; sudo systemctl stop hc-balancer-dyn 2>/dev/null; sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
ssh -o BatchMode=yes node-3 'sudo pkill -f "[c]pu-sampler"; true' 2>&1 | grep -v Warning
# a cell dir without run_data.json or mitigation/cpu_usage.csv is a partial/invalid cell: drop it so the queue redoes it
for d in data/motiv3-c6320*/*-*/; do
    [ -d "$d" ] || continue
    r=$(ls -d "$d"stage3eval_*/runs/run_1 2>/dev/null | head -1)
    if [ -z "$r" ] || [ ! -s "$r/run_data.json" ] || [ ! -s "$r/mitigation/cpu_usage.csv" ]; then log "dropping partial cell $d"; rm -rf "$d"; fi
    # a replication cell whose replicas never served (no run_data-overflow-*.json): the overflow pods crash-looped
    # before the c6320 manifest got PERF_EVENTS (2026-11-10 04:20 UTC) -> quarantine so the queue redoes it
    case "$d" in *"/rep"*) if [ -n "$r" ] && ! ls "$r"/run_data-overflow-*.json >/dev/null 2>&1; then
        log "quarantining replication cell without replica data: $d"; mkdir -p data/invalid-cells; mv "$d" "data/invalid-cells/$(basename "$(dirname "$d")")--$(basename "$d")--no-replicas-$(date -u +%H%M)"; fi;; esac
done
rm -rf data/stage3eval_*
# a GROW cell killed mid-run leaves the search manifest at limit 4: put it back to 3 (hr_deploy re-applies the file)
K=/local/dsb_hd_counter/hotelReservation/kubernetes/search/search-deployment.yaml
if grep -q 'cpu: "4"' $K; then sed -i 's/^\(\s*\)cpu: "4"$/\1cpu: "3"/' $K; log "search manifest limit restored to 3"; fi
kubectl set resources deployment search --limits=cpu=3 >/dev/null 2>&1
kubectl scale deployment search-overflow --replicas=0 >/dev/null 2>&1
kubectl scale deployment node3-load --replicas=0 >/dev/null 2>&1
pgrep -f 'proj-syn[c]' >/dev/null || {
    DST=/proj/tracing-pythia-PG0/royno7-hc-archive-c6320-$(date -u +%F) SRC="data/motiv3-c6320 data/motiv3-c6320-t2 data/motiv3-c6320-int data/motiv3-c6320-int-t2 data/motiv3-c6320-grow data/motiv3-c6320-grow-t2 data/smoke-c6320" WHILE='queue-c632[0]\.sh' \
      setsid nohup tools/proj-sync.sh > /tmp/proj-sync.log 2>&1 < /dev/null &
    log "proj-sync started"; }
log "starting queue-c6320.sh"
bash /tmp/queue-c6320.sh
