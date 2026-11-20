#!/bin/bash
# check-cell.sh <sweep dir> [cell]  -- score by phase (p1 / bursts / gaps) of one extrinsic cell + the motiv3 report rows.
# e.g. bash /tmp/check-cell.sh data/motiv3-c6320 none-quiet
export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval || exit 1
SW=${1:-data/motiv3-c6320}; CELL=${2:-none-quiet}
python3 - "$SW" "$CELL" <<'EOF'
import glob, csv, sys, statistics as st
sys.path.insert(0, "tools"); import motiv2_report as R
sw, cell = sys.argv[1], sys.argv[2]
run = sorted(glob.glob(f"{sw}/{cell}/stage3eval_*/runs/run_1"))[-1]
t0 = int(open(run+"/started_epoch_ns.txt").read())/1e9
bl = [b for b in R.parse_wrk2_blocks(run+"/wrk2/socialNetwork.txt") if b["kind"]=="BURST"]
spans = [(b["t"]-t0+1, b["t"]-t0+6) for b in bl]
print("  bursts (s):", ["%.0f-%.0f" % s for s in spans])
rows = list(csv.DictReader(open(glob.glob(run+"/mitigation/*.scores.csv")[0])))
cls = {"p1": [], "bursts": [], "gaps": []}
for r in rows:
    t = int(r["unix_ms"])/1e3 - t0
    k = "p1" if 2 <= t < 29 else ("bursts" if R.in_any(t, spans) else ("gaps" if t >= 29 else None))
    if k: cls[k].append(r)
for k, rs in cls.items():
    out = []
    for c in ("y90", "ext90", "e_iso"):
        v = [float(r[c]) for r in rs]; out.append("%s=%.3f(p90 %.3f max %.3f)" % (c, st.mean(v), sorted(v)[int(0.9*len(v))], max(v)))
    print("  %-7s n=%4d " % (k, len(rs)) + " ".join(out))
# e_iso ticks above 0.15 outside the bursts (spurious reactive triggers)
sp = [int(r["unix_ms"])/1e3 - t0 for r in cls["gaps"] if float(r["e_iso"]) > 0.15]
print("  gap ticks with e_iso > 0.15: %d %s" % (len(sp), ["%.0f" % t for t in sp[:12]]))
EOF
MOTIV_LAYOUT=c6320 python3 tools/motiv3_report.py "$SW" 2>&1 | grep -E "^cell|^$CELL|^== " | cut -c1-230
