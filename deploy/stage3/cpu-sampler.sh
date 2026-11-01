#!/bin/bash
# cpu-sampler.sh <out.csv> [interval_s]
#
# Per-second CPU accounting on the target node, started/stopped by
# run-phased-stage3.sh around each run (needs no kubectl on the node). Long
# CSV, one row per CPU and per pod cgroup per sample:
#   unix_ms,key,a,b
#   key=cpuN      a=busy jiffies (user+nice+system+irq+softirq+steal), b=total jiffies
#   key=pod:<uid> a=cpu.stat usage_usec, b=cpu.stat nr_throttled
# Pod uids are normalised to the dashed form; the orchestrator saves
# pod_uids_*.txt (uid -> namespace/name/node) next to this file so the
# per-pod core-seconds of the victim, its replicas and the aggressors can be
# joined offline (tools/motiv2_report.py). Both cgroup layouts are handled
# (cgroupfs kubepods/<qos>/pod<uid>, systemd ...-pod<uid_>.slice).
set -u
out="${1:?usage: cpu-sampler.sh <out.csv> [interval_s]}"
iv="${2:-1}"
echo "unix_ms,key,a,b" > "$out"
while :; do
    now=$(date +%s%3N)
    awk -v t="$now" '/^cpu[0-9]+ /{busy=$2+$3+$4+$7+$8+$9; tot=busy+$5+$6; printf "%s,%s,%d,%d\n",t,$1,busy,tot}' /proc/stat >> "$out"
    for d in $(find /sys/fs/cgroup -maxdepth 4 -type d \( -name 'pod*' -o -name '*-pod*.slice' \) 2>/dev/null); do
        [ -f "$d/cpu.stat" ] || continue
        u=$(basename "$d"); u=${u%.slice}; u=${u##*pod}; u=${u//_/-}
        awk -v t="$now" -v u="$u" '/^usage_usec/{a=$2} /^nr_throttled/{b=$2} END{printf "%s,pod:%s,%d,%d\n",t,u,a,b}' "$d/cpu.stat" >> "$out"
    done
    sleep "$iv"
done
