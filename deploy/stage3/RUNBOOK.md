# Stage-3 + HarvestContainers runbook (fresh CloudLab cluster)

From an empty CloudLab experiment to the full stage-3 sweep with the `hc` arm.
Written 2026-10-14, after the first cluster (Wisconsin sm110p, Xeon Silver
4314) expired mid-sweep. Nothing from that cluster carries over except what
is in git and on Docker Hub. Updated 2026-10-20 for the second cluster
(Clemson, 2× Xeon Platinum 8360Y): all 72 cores online, SMT off, turbo on
with no OS frequency control (steps 2–3).

Repos and branches:

| repo | branch | what it holds |
|---|---|---|
| `dsb_k8s_perf` | `harvest-k612` | CloudLab profile: 5 nodes, 6.12 kernel built from source, nosmt on the target, HC packages/units |
| `HarvestContainers-docclab-mod` | `main` | HC ported to 6.12 + cgroup v2 (Monitor kmod, Listener, Balancer) |
| `simple-mitigation` (`simple-mitigation-1`) | `main` | controller, `hc` actuator, this kit (`deploy/stage3/`) |
| `dsb_hd_counter` | `stage3-predictor` | stage3-eval runner, `run-phased-stage3.sh`, phased configs, `search-overflow` |

Node roles: node-0 control plane + orchestrator + wrk2; node-1..3 the rest of
DSB and the aggressor testbeds' other services; **node-4 the target** (victim
`search`, aggressors, HC, controller).

Time budget on a fresh cluster: ~1.5–2 h provisioning (kernel build + reboot +
Kubespray), ~1 h of steps 2–7, ~15 min calibration, then ~35–45 min per arm
(9 configs × 3.5–5 min). Six arms ≈ 4–5 h. **Copy results off before the
reservation ends** (step 12): `/local` is wiped with the experiment.

---

## 0. Before you start (laptop)

Everything below must be pushed, since every node clones from GitHub:

```bash
git -C C:\coding-workspace\dsb_k8s_perf status                   # harvest-k612
git -C C:\coding-workspace\HarvestContainers-docclab-mod status  # main
git -C C:\coding-workspace\simple-mitigation-1 status            # main
git -C C:\coding-workspace\dsb_hd_counter-s3 status              # stage3-predictor
```

If the new hardware type differs from sm110p, decide the CPU map first (step 2)
and update the files listed there in git **before** instantiating, so the
profile writes the right `/etc/default/hc-harvest`.

## 1. Instantiate the profile

CloudLab → start experiment with the `dsb_k8s_perf` profile at branch
`harvest-k612`. Parameters: `nodeCount=5`, `nodeType=<Ice Lake type>`,
`harvestEnable=true`, `harvestKernel=6.12`, `harvestWorkers=node-4`,
`harvestBootArgs=nosmt=force intel_idle.max_cstate=0 processor.max_cstate=0`.

Every node builds the kernel (30–60 min) and reboots once; then Kubespray runs.
Watch progress from node-0 with `/local/repository/setup-status.sh` (or
`tail -f /local/logs/setup.log`). When done, on node-0:

```bash
kubectl get nodes                                             # 5 Ready
kubectl get node node-4 --show-labels | grep -o podtype=harvest
for n in node-0 node-1 node-2 node-3 node-4; do ssh $n 'echo $(hostname) $(uname -r) $(stat -fc %T /sys/fs/cgroup)'; done
# every node: <ver>-harvest cgroup2fs
ssh node-4 'lscpu | grep -E "Model name|Socket|Thread|^CPU\(s\)"; cat /proc/cmdline; grep SCHED_CLASS_EXT /boot/config-$(uname -r)'
```

## 2. CPU map on node-4

```bash
ssh node-4 lscpu -e
```

Rules: victim, HC Secondary, HC daemons and controller on **one socket**; CPU
ids in HC's lists **< 64** (HC uses 64-bit masks); SMT off on node-4
(`nosmt=force`; `lscpu` shows 1 thread per core, the sibling threads listed
offline).

**Current cluster (Clemson, 2× Xeon Platinum 8360Y, 36 cores/socket, SMT
off, all 72 cores online).** Socket 0 is the even CPU ids, socket 1 the odd.
The map is the sm110p one moved onto socket 0's even ids:

| CPUs | use | sm110p (old) |
|---|---|---|
| 0, 2 | housekeeping, IRQs | 0–1 |
| 4, 6 | victim `search` (HC Primary; `GOMAXPROCS=1`, limit 2: one P so it saturates at ~4.3k rps, before the gRPC transport does) | 2–4 (3 Ps on 3 HTs at 2.4 GHz) |
| 8, 10, 12, 14 | HC Secondary (aggressors' dedicated cores when HC is on) | 5–8 |
| 16, 18, 20, 22 | unused (were in the first, 10-core pool) | 9–11 |
| 24 | mitigation controller | 12 |
| 26 | HC Listener | 13 |
| 28 | HC Balancer | 14 |
| 30 | HC Monitor kthread | 15 |
| everything else (62 cores incl. all of socket 1) | nothing (node-4 is tainted) | — |

**Aggressor pool (decided 2026-10-21, resized 2026-10-22):** the orchestrator
writes `cpuset.cpus = 4,6,8,10,12,14` (`AGGR_CPUSET` in
`run-phased-stage3.sh`) into every aggressor pod on node-4, re-applied every
10 s with the `gordion-aggr` label; `aggr_affinity_p3.txt` in each run
records that it took. A 2-core victim on a 72-core node is otherwise never
touched by unpinned neighbours (measured: e2e p99 unchanged, nothing to
mitigate). Sizing: the ext-severe aggressors burn ~9 cores on node-4 (two
`catalogue` pods at ~4 each) and a 10-core pool cost the victim only +25%
latency, so the pool is the victim's cores plus HC's Secondary cores, ~1.5x
oversubscribed under severe load. `CAP_BASE_CORES` = pool = 6, `K_P` 14.
Set `AGGR_CPUSET=` (empty) to run unconfined.

The map appears in these places (already set for the 8360Y in git):

| file | keys |
|---|---|
| node-4 `/etc/default/hc-harvest` (written once by `setup-harvest.sh` with sm110p values — **fix by hand**, below) | `HC_CPULIST`, `HC_SECONDARY_CPULIST`, `HC_MONITOR_CPU`, `HC_BALANCER_CPU`, `HC_LISTENER_CPU` |
| simple-mitigation-1 `deploy/stage3/arms/hc.env`, `hc-combined.env` | `HC_VICTIM_CPULIST`, `HC_SECONDARY_CPULIST`, `HC_BALANCER_CPU` |
| simple-mitigation-1 `deploy/stage3/arms/common.env` | `CONTROLLER_CPU`; `CAP_BASE_CORES` 6 (= the pool) and `K_P` 14 (cap reaches 0 at e_iso 0.79 as in July) |
| dsb_hd_counter `stage3-eval/run-phased-stage3.sh` | `AGGR_CPUSET` (the pool) |
| dsb_hd_counter `kubernetes/search/search*-deployment.yaml` | `limits.cpu` 2, `GOMAXPROCS` 2 |
| dsb_hd_counter `stage3-eval/configs/phased-*.yaml` (9 files) | `victim.cpu_set` |
| HarvestContainers `TestFramework/Config/SYSTEM.sh` (only for step 5's sanity check) | `ENV_TYPE=ICELAKE_8360Y` block |

Fix `/etc/default/hc-harvest` on node-4:
```bash
sudo sed -i -e 's/^HC_CPULIST=.*/HC_CPULIST=4,6/' \
  -e 's/^HC_SECONDARY_CPULIST=.*/HC_SECONDARY_CPULIST=8,10,12,14/' \
  -e 's/^HC_MONITOR_CPU=.*/HC_MONITOR_CPU=30/' \
  -e 's/^HC_BALANCER_CPU=.*/HC_BALANCER_CPU=28/' \
  -e 's/^HC_LISTENER_CPU=.*/HC_LISTENER_CPU=26/' /etc/default/hc-harvest
grep -E '^HC_' /etc/default/hc-harvest
```

## 3. Node setup the profile does not do

**All nodes** (the search and overflow manifests mount `/dev/cpu`, which only
exists with the `msr` module; without it the victim pod is stuck in
`FailedMount`):
```bash
for n in node-0 node-1 node-2 node-3 node-4; do
  ssh $n 'sudo modprobe msr && echo msr | sudo tee /etc/modules-load.d/msr.conf >/dev/null && ls -d /dev/cpu/0'
done
```

**node-4 frequency: not pinned, turbo on (decided 2026-10-20).** On the
8360Y the BIOS runs HWP itself (`intel_pstate: P-states controlled by the
platform`, no `cpufreq/` in sysfs), so the OS cannot pin it: writes to
`IA32_MISC_ENABLE` bit 38 and `IA32_HWP_REQUEST` are accepted but ignored.
Measured busy-core frequency: 3.1 GHz (all-core turbo) from 1 to 36 busy
cores, 2.87–2.92 GHz with all 72 busy. Runs are at that realistic,
uncontrolled frequency; calibration (step 9) is done under the same
conditions. To re-check:
```bash
ssh node-4 'sudo dmesg | grep -i "P-states"; for n in 1 16 72; do pids=""; \
  for c in $(seq 0 $((n-1))); do taskset -c $c sh -c "while :; do :; done" & pids="$pids $!"; done; sleep 4; \
  awk -v n=$n "/^cpu MHz/{i++; if(i<=n){s+=\$4}} END{printf \"busy=%d avg MHz=%.0f\n\",n,s/n}" /proc/cpuinfo; kill $pids; wait 2>/dev/null; done'
```
(On a node type with OS frequency control, pin it instead: `no_turbo=1` and
`cpupower frequency-set -g userspace -f <base>`, re-applied after every reboot.)

**node-0, tools** (check first; install only what is missing):
```bash
command -v kubectl helm jq yq go protoc
sudo apt-get install -y jq protobuf-compiler
sudo wget -qO /usr/local/bin/yq https://github.com/mikefarah/yq/releases/latest/download/yq_linux_amd64 && sudo chmod +x /usr/local/bin/yq
curl -fsSL https://go.dev/dl/go1.23.4.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc && source ~/.bashrc
go install github.com/fullstorydev/grpcurl/cmd/grpcurl@latest        # used by calibration
curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash   # if helm is missing (socialNetwork)
```

## 4. HC binaries and Monitor on node-4

`setup-harvest.sh` already tried `setup-bins.sh`; rebuild to be sure, then
start the Listener (its `ExecStartPre` loads `qidlecpu.ko` and binds the
Monitor):
```bash
cd /local/HarvestContainers && git pull
(cd TestFramework/Experiments && ./setup-bins.sh) 2>&1 | grep -iE 'error|warning: .*undefined' ; ls TestFramework/bin
sudo systemctl start hc-listener
systemctl is-active hc-listener; cat /proc/idlecpu/cpulist      # active / 4,6
systemctl is-active hc-balancer                                 # inactive (the hc actuator runs hc-balancer-dyn)
ps -eLo pid,psr,stat,pcpu,comm | grep check_idle                # psr 30, R, ~100%
dmesg | tail -5                                                 # no "Buffer overflow detected"
```
Leave `hc-listener` running for **every** arm, baselines included: the Monitor
spins on its CPU whenever the module is loaded, so all arms lose that core
equally.

## 5. Optional: HC sanity reproduction (~45 min)

Confirms the kmod + Listener work on the new kernel/hardware before stage-3.
Skip it if time is short and the hardware is the same as before.

node-0:
```bash
sudo mkdir -p /local/HarvestContainers && sudo chown $USER /local/HarvestContainers
git clone https://github.com/docc-lab/HarvestContainers-docclab-mod /local/HarvestContainers
ln -s /local/HarvestContainers ~/HarvestContainers
sudo mkdir -p /mnt/extra/config /mnt/extra/logs /mnt/extra/results && sudo chown -R $USER /mnt/extra
```
node-0 and node-4:
```bash
sudo apt-get install -y python-is-python3 python3-pip && pip install scipy pandas matplotlib
grep -q clabsvr ~/.ssh/config 2>/dev/null || cat >> ~/.ssh/config <<'EOF'
Host clabsvr
  Hostname node-0
  StrictHostKeyChecking no
Host clabcl1
  Hostname node-4
  StrictHostKeyChecking no
EOF
ssh clabsvr hostname && ssh clabcl1 hostname
```
node-4 (the sanity scripts load the module and start the Listener themselves):
```bash
sudo systemctl stop hc-balancer hc-listener 2>/dev/null; sudo rmmod qidlecpu
```
node-0, reserve node-4:
```bash
kubectl taint nodes node-4 dedicated=special:NoSchedule --overwrite
kubectl get pods -A --field-selector spec.nodeName=node-4 --no-headers \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,KIND:.metadata.ownerReferences[0].kind \
  | awk '$3!="DaemonSet"' | while read ns n k; do kubectl delete pod -n $ns $n; done
```
node-4, inside tmux:
```bash
cd ~/HarvestContainers/TestFramework/Experiments
./setup_workload.sh 2>&1 | tee /mnt/extra/logs/setup_workload.log     # 15-25 min
./sanity.sh 2>&1 | tee /mnt/extra/logs/sanity.log                      # ~15 min
grep -E 'Found container|Adding' /tmp/listener-debug.log | head
for a in memcached mysql xapian; do echo "== $a"; cat /mnt/extra/logs/$a/sanity.summary; done
```
Pass: harvest p99 within ~10% of baseline, `Adding harvest_launcher PID` in
the Listener log, CPUBully rate well above 1. sm110p reference: memcached
408→408 µs, Xapian 2.97→2.97 ms, CPUBully 2.1 / 6.1 cores. 8360Y
(2026-10-21, turbo on): memcached 657→656 µs, Xapian 8.09→7.48 ms, MySQL
1699→1654, CPUBully 4.5 / 7.2 / 7.5 cores, Primary-core use 0.5→2.7,
1.4→4.9, 0.8→4.8.

Clean up (node-0), list first and delete by exact name:
```bash
kubectl get pods,svc -o wide | grep -E 'primary|secondary|mutilate|ycsb|bully|dedup|nwbully|x264'
kubectl delete pod,svc <names from the list> --ignore-not-found
kubectl delete configmap ycsb-db-properties --ignore-not-found
# (the taint stays; step 7 re-asserts it and clears node-4 of leftover pods)
```
node-4: `sudo systemctl start hc-listener` again (step 4 checks).

## 6. Code on node-0

```bash
sudo mkdir -p /local/dsb_hd_counter /local/simple-mitigation-1 && sudo chown $USER /local/dsb_hd_counter /local/simple-mitigation-1
git clone -b stage3-predictor https://github.com/docc-lab/dsb_hd_counter.git /local/dsb_hd_counter     # or `git pull` if the profile cloned it
git clone https://github.com/docc-lab/simple-mitigation.git /local/simple-mitigation-1
chmod +x /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval/*.sh /local/simple-mitigation-1/deploy/stage3/*.sh
cd /local/dsb_hd_counter && git submodule update --init wrk2/deps/luajit   # wrk2's LuaJIT is a submodule
sudo apt-get install -y libssl-dev zlib1g-dev
cd /local/dsb_hd_counter/wrk2 && make            # stage3-eval expects ../../../wrk2/wrk
```

Controller build, tests, and kit install on node-4 (re-run `install-kit.sh`
after any node-4 reboot — `/tmp` is cleared — and after any change to the
controller or `arms/*.env`):
```bash
cd /local/simple-mitigation-1
./deploy/stage3/install-kit.sh node-4 /tmp/mitig       # runs make deps/proto, builds, installs
go test ./cmd/mitigation-controller/ ./pkg/actuators/hc/
ssh node-4 'ls -la /tmp/mitig /tmp/mitig/arms'
```
node-4 has no `kubectl`; that is fine (the controller uses client-go with
`/tmp/mitig/kubeconfig-admin`). The kubeconfig points at node-0's API server
(`https://10.10.1.1:6443`), reachable from node-4.

## 7. hotelReservation from dsb_hd_counter

The profile deploys stock DeathStarBench; replace it with dsb_hd_counter's
manifests (adds the score sidecar wiring and `search-overflow`):
```bash
kubectl delete -Rf /local/DeathStarBench/hotelReservation/kubernetes --wait=true
kubectl apply  -Rf /local/dsb_hd_counter/hotelReservation/kubernetes
kubectl scale deployment hr-client --replicas=0 2>/dev/null
# Keep everything but the victim and the configured aggressors off node-4.
# The runner's placement (lib/aggressor-place.sh) adds the matching
# toleration, so this taint stays on for the whole sweep. Pods already on
# node-4 are not evicted by the taint: delete them so they reschedule.
kubectl taint nodes node-4 dedicated=special:NoSchedule --overwrite
kubectl get pods -o wide --field-selector spec.nodeName=node-4 --no-headers | awk '{print $1}' | xargs -r kubectl delete pod
kubectl get pods -o wide                       # all Running; search-overflow 0/0
for n in node-1 node-2 node-3 node-4; do ssh $n sudo docker pull docclabgroup/search-online:v2.3.0; done
```

Temporary score calibration (the July one) so the victim can start; step 9
replaces it:
```bash
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval/score-verify
kubectl create configmap gordion-search --from-file=gordion.json --from-file=curve.csv \
    --dry-run=client -o yaml | kubectl apply -f -
```

Pre-flight checklist before any run:
```bash
ssh node-4 'nproc; lscpu | grep "Thread(s) per core"'            # 72 / 1
ssh node-4 'systemctl is-active hc-listener hc-balancer; cat /proc/idlecpu/cpulist; ls /tmp/mitig'
kubectl describe node node-4 | grep Taints     # dedicated=special:NoSchedule
kubectl get pods -A -o wide | grep -E 'primary|secondary|bully' || echo clean
```

## 8. Smoke run (node-0, `ssh -A`, inside tmux)

```bash
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
ARM=none ./run-phased-stage3.sh configs/phased-baseline.yaml 2>&1 | tee data/.smoke-baseline.orch.log
```
Expect, ~3.5 min: `Starting controller DISARMED` → `t0 found` → `ARMED at` →
`stage3-eval.sh exited rc=0` → `Collected -> …`. Second pane:
`tail -f data/.phased-baseline.orchestrator.eval.log`.

During p2/p3:
```bash
kubectl get pod -l 'io.kompose.service=search,overflow!=true' -o wide          # node-4
ssh node-4 'for p in $(pgrep -x search); do grep Cpus_allowed_list /proc/$p/status; done'   # 4,6
```
After:
```bash
d=$(ls -dt data/stage3eval_* | head -1)/runs/run_1
ls $d $d/mitigation
grep -c score_event $d/score_events.log                                      # > 0
grep -m1 'FORMULA mode' $d/mitigation/*.ctrl.log | grep -o 'iso_actuator[^ ,]*'
head -3 $d/wrk2/*.txt                                                        # shape/rps/140s
mkdir -p data/smoke && mv data/stage3eval_* data/smoke/                      # keep sweeps clean
```

## 9. Calibrate the score on node-4 (~15 min) — do not skip

The score is calibrated against the victim's per-request cycles, which depend
on CPU model, frequency, kernel and pinning. The July calibration is from
another node; arms compared under a wrong calibration fire at the wrong times.

The victim must be on node-4 **and pinned** with nothing else loading the
cluster. A finished stage3-eval run puts `search` back to normal scheduling,
so place it explicitly:
```bash
kubectl patch deployment search --type=merge -p '{"spec":{"template":{"spec":{
  "nodeSelector":{"kubernetes.io/hostname":"node-4"},
  "tolerations":[{"key":"dedicated","operator":"Equal","value":"special","effect":"NoSchedule"}]}}}}'
kubectl get deployment search -o jsonpath='{.spec.template.spec.containers[0].command}'; echo
# if that printed a command, drop it so the image's taskset launcher runs:
kubectl patch deployment search --type=json -p '[{"op":"remove","path":"/spec/template/spec/containers/0/command"}]'
kubectl set env deployment/search CPU_SET=4,6
kubectl rollout status deployment/search
ssh node-4 'for p in $(pgrep -x search); do grep Cpus_allowed_list /proc/$p/status; done'   # 4,6
```
Run with levels past the victim's knee (~4.8k e2e with one P) so the curve
bends with it; otherwise an overload is misattributed as extrinsic. Score
`total_time`, not `processing_time`: the interceptor's timer starts when the
handler goroutine runs, so run-queue waiting under self-overload is invisible
to `processing_time` (measured flat to 5600 rps while e2e p50 went 5 -> 40 ms);
`total_time` shows it. Downstream services sit on unloaded nodes, so their
latency noise is small. **Provisional:** `processing_time` is the metric the
design and July's results are built on; `total_time` is a workaround until the
contention setup is changed so that `processing_time` reacts (contention that
inflates the handler's own execution, not run-queue waiting of a mostly idle
thread). Treat total_time sweeps accordingly. Operating point 3600 rps (~75% of the single P's
~4.8k capacity): at 2400 the victim was ~65% idle and, on 6.12's EEVDF with
SMT off, CPU-bound neighbours only cost it a few ms of wake-up latency
(+70% tail with the 6-core pool 1.5x oversubscribed); contention damage
scales with the victim's utilization, so the ext configs drive it at 3600.
Re-running only the calibrator on recorded levels is enough to move the
operating point (`calibrate_from_stream.py --operating-rps 3600 ...`). The
tanh gain `k` must track sigma/baseline (y = tanh(k·dT/sigma)): July's
k = 0.025 went with sigma90 = 4.3% of baseline p90 (y90 = 0.8 at a 2.9x
tail); this calibration has 7%, so `calibration_sweep.sh` now defaults to
k = 0.06 (at 0.025 a 2.7x tail scored only y90 = 0.35). The arm thresholds
are unchanged. ~23 min:
```bash
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval/score-verify
URL=http://$(kubectl get node node-4 -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}'):$(kubectl get svc frontend -o jsonpath='{.spec.ports[0].nodePort}')
./calibration_sweep.sh --target-node node-4 --url "$URL" --section total_time \
    --rates "300 600 1200 2400 3000 3600 4200 4800 5600" 2>&1 | tee calibration-node4.log
```
Review the level table it prints (arrival_mean scales with rate; ~1000
windows per level; sigma in the tens of kcyc), then run the ConfigMap apply
command it prints. Keep the new `gordion.json`/`curve.csv`: commit them on
`stage3-predictor` (or copy them off) so the results are reproducible.

Check the calibration with one more smoke baseline (no aggressors → `e_iso`
low in every phase) and one contention config (`e_iso` clearly higher in
p2/p3 than p1, crossing 0.35 at least part of the time):
```bash
ARM=none ./run-phased-stage3.sh configs/phased-baseline.yaml configs/phased-ext-steady-moderate.yaml \
    2>&1 | tee data/.smoke-calib.orch.log
for d in $(ls -dt data/stage3eval_* | head -2); do
  r=$d/runs/run_1; t0=$(( $(cat $r/started_epoch_ns.txt) / 1000000 )); echo "== $(cat $r/mitigation/config-as-run.yaml | grep -m1 name)"
  awk -F, -v t0=$t0 'NR>1{p=($1<t0+20000)?"p1":($1<t0+80000)?"p2":"p3"; n[p]++; y[p]+=$6; x[p]+=$7; e[p]+=$8; h[p]+=$12; if($8>m[p])m[p]=$8}
   END{for(p in n) printf "%s rows=%d y90=%.3f ext90=%.3f e_iso=%.3f (max %.3f) i_horz=%.3f\n",p,n[p],y[p]/n[p],x[p]/n[p],e[p]/n[p],m[p],h[p]/n[p]}' $r/mitigation/*.scores.csv | sort
done
mv data/stage3eval_* data/smoke/
```
(`scores.csv` columns: `unix_ms,target,pod,yhat50,y50,y90,ext90,e_iso,n,applied_cap,ext50,i_horz`.)
For the int configs, `i_horz` should rise in p2/p3 while `e_iso` stays low.

## 10. Sweep: none / isolate / horizontal / combined

`CAP_BASE_CORES` (6) and `K_P` (14) in `deploy/stage3/arms/common.env`
match the 6-core aggressor pool (cap = pool, zero at e_iso 0.79 like July). If step 9 shows contention much weaker or
stronger than July's (p2 e_iso ≈ 0.5 in its ext configs), revisit the pool or the configs' load levels before
the sweep; after any change, commit, `git pull` on node-0, and re-run
`install-kit.sh`.

```bash
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
ls -d data/stage3eval_* 2>/dev/null && echo "move these out first"
CFGS="configs/phased-baseline.yaml $(ls configs/phased-ext-*.yaml configs/phased-int-*.yaml | tr '\n' ' ')"
set -o pipefail
for arm in none isolate horizontal combined; do
    ARM=$arm ./run-phased-stage3.sh $CFGS 2>&1 | tee data/.sweep-$arm.orch.log || break
    mkdir -p data/sweep-$arm && mv data/stage3eval_* data/sweep-$arm/
    cp data/.phased-*.orchestrator.eval.log data/sweep-$arm/ 2>/dev/null
done
```
`set -o pipefail` makes `|| break` see the runner's exit status rather than
`tee`'s. The `cp` keeps each arm's per-config eval logs, which the next arm
would overwrite. Check each `.sweep-$arm.orch.log` for `FATAL`/`WARNING`.

Arm order, if time is short: `none`, `isolate`, `hc`, then `horizontal`,
`combined`, `hc-combined`. Arms are only comparable within one cluster.

## 11. hc arm

Pilot first (~10 min):
```bash
ARM=hc ./run-phased-stage3.sh configs/phased-ext-steady-moderate.yaml configs/phased-int-bursty-severe.yaml \
    2>&1 | tee data/.hc-pilot.orch.log
```
node-4, during p3 of the ext run:
```bash
grep 'formula: hc' /tmp/mitig/phased-ext-steady-moderate.ctrl.log      # "hc apply" ... applied=true
systemctl is-active hc-balancer-dyn                                     # active
watch -n1 'for p in $(pgrep -f "[c]ompose-post|[c]atalogue"); do grep Cpus_allowed_list /proc/$p/status; done | sort | uniq -c'
# 8,10,12,14 plus whichever of 4,6 are idle, changing over time
```
After the run:
```bash
systemctl is-active hc-balancer-dyn                                     # inactive (restored at controller shutdown)
for p in $(pgrep -f "[c]ompose-post|[c]atalogue"); do grep Cpus_allowed_list /proc/$p/status; done   # all CPUs again
```
In the int run `hc` should stay off (`e_iso` ≈ 0); `hc apply` there means
`HC_THETA_ON` needs looking at. If the pilot looks right:
```bash
mkdir -p data/hc-pilot && mv data/stage3eval_* data/hc-pilot/
for arm in hc hc-combined; do
    ARM=$arm ./run-phased-stage3.sh $CFGS 2>&1 | tee data/.sweep-$arm.orch.log || break
    mkdir -p data/sweep-$arm && mv data/stage3eval_* data/sweep-$arm/
    cp data/.phased-*.orchestrator.eval.log data/sweep-$arm/ 2>/dev/null
done
```
hc arms additionally collect `hc-balancer.tgz` and `hc-listener.tail.log` into
each run's `mitigation/`.

## 12. Results off the cluster (do this after every arm, not only at the end)

node-0:
```bash
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
tar -czf /tmp/stage3-results.tgz data/sweep-* data/smoke data/hc-pilot data/.sweep-*.orch.log score-verify/calibration-node4.log score-verify/gordion.json score-verify/curve.csv
```
laptop:
```bash
scp <user>@<node-0 public host>:/tmp/stage3-results.tgz .
```
Each run's `mitigation/` has `arm.txt`, `<name>.env.txt` (exact controller
settings), `config-as-run.yaml`, `node_freq_floors`, `armed_epoch_ns.txt`,
`<name>.ctrl.log` and `<name>.scores.csv`.

## 13. Predictor training data from the stage-3 testbed (SMT ON, arm none)

Done 2026-10-23 on the Clemson cluster. The GRU predictor
(dsb_hd_counter `origin/master`
`hotelReservation/noisy-neighbors/ml_training_deployment/gru_train.py`) trains
on the victim's windowed `run_data.json` (73 features per 100 ms window) with
labels computed offline from `processing_time` by
`contention_scoring/act_func_label2.py`. Stage-3 run dirs already contain
that file; three things had to change:

- the victim must sample the trainer's 10 perf events (LLC-loads,
  LLC-load-misses, dTLB-loads were missing from the sampler default) —
  `stage3-eval.sh` now sets `PERF_EVENTS` (override `STAGE3_PERF_EVENTS`);
- the capture must wait for the sampler's 30 s flush (every sweep file lost
  its last ~20 s) — `capture_victim_run_data` now re-reads until the last
  sample passes `ended_epoch_ns`;
- **`processing_time` only reacts to SMT siblings.** Measured p2/p1 on
  ext-steady-severe with arm none: pool 4,6,8-14 → processing p50 1.03× /
  p90 1.18× (total_time 3×); pool 4,6 (5× oversubscribed) → 1.03× / 1.23×
  (total 20×); SMT on, aggressors on the siblings 76,78 → **1.64× / 1.53×**.
  A 50 µs handler is almost never preempted mid-execution, so time-sharing
  lands entirely in run-queue wait (total_time). July's signal came from
  hyperthreads on 5.15; `nosmt=force` (an HC requirement) removed it.

Boot node-4 with SMT on for collection (and back to `nosmt=force` for any
HC arm):
```bash
ssh node-4 'sudo sed -i "s/ nosmt=force//" /etc/default/grub.d/99-harvest.cfg && sudo update-grub && sudo reboot'
until ssh -o ConnectTimeout=5 node-4 true 2>/dev/null; do sleep 10; done
# the CloudLab image re-enables the swap partition on reboot and kubelet refuses to start:
ssh node-4 'sudo swapoff -a && sudo sed -i "/swap/s/^/#/" /etc/fstab && sudo systemctl restart kubelet'
kubectl get nodes node-4 -w                    # Ready within a minute
ssh node-4 'cat /sys/devices/system/cpu/smt/active; cat /sys/devices/system/cpu/cpu4/topology/thread_siblings_list'   # 1; 4,76
./deploy/stage3/install-kit.sh node-4 /tmp/mitig   # /tmp is cleared by the reboot
```
To restore SMT off: `sudo sed -i 's/max_cstate=0"$/max_cstate=0 nosmt=force"/' /etc/default/grub.d/99-harvest.cfg`, `update-grub`, reboot, same swap check, re-install the kit.

Collection sweep (node-0, tmux, agent-forwarded):
```bash
cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
python3 tools/gen_collect_configs.py            # 25 points -> configs/collect/, plan.txt
mkdir -p data/collect-smt
DEADLINE="HH:MM" ./run-collect.sh 2>&1 | tee -a data/collect-smt/console.log   # ~5.5 min/point, resumable
python3 tools/run_data_phases.py data/collect-smt/*                            # p1 vs p2 per point
tar -czf /tmp/collect-smt.tgz data/collect-smt data/probe-*
```
Points: baselines at 1500/2500/3200/4000 (labeler baseline stats), ext
light/mid/heavy × steady/bursty/exp, SN-only, SS-only, replicas 3, three
points with pool `4,6,76,78`, int extras 2000/3000/4200 and a bursty victim,
two mixed. None reuse the nine eval configs' load points. Converting the run
dirs into the trainer's `run_data_iterN.json` layout:
`tools/stage3_to_training.py` (slices by the epoch files, adds
`offset_from_workload_ms`, drops zero-request windows).

## Known pitfalls (all hit on the first two clusters)

- node-4 `NotReady` after a reboot, kubelet log `running with swap on is not
  supported`: the image re-enables `/dev/sda99` swap. `swapoff -a`, comment
  the fstab line, restart kubelet (section 13).

- wrk2 output is just `unable to connect to <node-4>:<nodePort> Connection
  refused` and 0 score events: the frontend was restarted (Consul recovery)
  and not yet listening when the driver started (no readinessProbe). Fixed
  in `stage3-eval.sh` (`wait_http_ready`); if it recurs, check
  `curl http://<node-4 IP>:<nodePort>/` by hand.

- `FailedMount … /dev/cpu`: `msr` not loaded on the node the pod landed on (step 3).
- `install-kit.sh` permission denied reading `/tmp/mitig`: fixed in the script
  (kit dir 755, kubeconfig 600); on an old copy, `sudo chmod 755 /tmp/mitig`.
- Search unpinned: the base manifest's `command:` override bypasses the
  image's `taskset`; the runner drops it when `victim.cpu_set` is set.
- `calibration_sweep.sh` refuses to run: the victim is off node-4 after a
  stage3-eval run; re-place it (step 9).
- HC Listener picking an empty `cpuset/kubepods` decoy cgroup: fixed in
  `Listener/getpids.c`; rebuild with `setup-bins.sh` if the binary is stale.
- HC's own runners `rmmod` the Monitor when they finish; restart `hc-listener`.
- Baseline saturates far below 2400 rps with seconds of latency while
  `kubectl top` looks idle: HR pods at their `limits.cpu` (frontend,
  reservation and rate hit 3 cores at 1200 rps on the 8360Y; `docker stats`
  during load shows it, `kubectl top` lags). Root cause: the HR services
  are Go 1.18 and only `search` set `GOMAXPROCS`; the others sized their
  scheduler from the host's 144 CPUs and burned ~3 ms CPU/request under
  an 8-core quota. Fixed in the manifests: `GOMAXPROCS` = `limits.cpu` on
  every Go service, limits raised (frontend 16, other non-victim services
  8, search stays 3). Jaeger client-vs-server span gaps of tens of ms are
  the CFS-throttling signature.
- The int configs never overloaded the victim (July included: p2 e_iso 0.08).
  Two causes on the 8360Y: a 2- or 3-P victim at 3.1 GHz is too fast for the
  chain, and the frontend->search gRPC connection caps at 100 concurrent
  streams (grpc-go 1.56.3), so the chain tops out at 100/latency = 6-9k rps
  with requests queueing outside the victim's handler (processing-time curve
  flat to 7200 rps). Fix: `GOMAXPROCS=1` on search, knee ~4.3k, int extras
  3600/4800 on a base of 800. Also: a memcached restart empties the cache and
  `mongodb-reservation` saturates for minutes; warm up before measuring.
- Controller substituted 1 for a genuine `ext_pct_90 = 0` (and the p50
  prediction for `y50_current = 0`), inflating e_iso on every clamped window
  (baseline ext90 ~0.9 with y90 = 0). Fixed in `pkg/features/compute.go`
  (fallbacks only for producers without `source_kind=gordion`); July's e_iso
  values carry this inflation. Re-run `install-kit.sh` after pulling, the kit
  is not rebuilt by `go test`.
- Weak ext contention despite a saturated pool: a victim at ~65% of one P is
  barely hurt by CPU-bound neighbours on EEVDF with SMT off (wake-up latency
  only). Run the victim near ~75% (operating point 3600) so time-sharing
  turns into queueing; July's 3x tails came from SMT siblings on 5.15.
- `search-overflow` replicas crash-loop (`Failed to open perf event cycles on
  CPU -1`): an empty `CPU_SET` makes the windowed sampler open counters on no
  CPU. The overflow manifest now pins to `0-3`; a horizontal arm whose
  `pod_placement_p3.txt` shows overflow pods in Error/CrashLoopBackOff is void.
- A non-victim HR pod (`rate`) scheduled onto node-4 unpinned and burned
  2.6 cores there in the baseline: keep the `dedicated=special` taint on
  node-4 (step 7).
- Results in `data/stage3eval_*` from earlier runs get swept into the next
  arm's directory by the loop's `mv`; move them out first.

## 14. processing_time scoring (SMT ON) and the motivation experiment (2026-10-23/24)

Section 9 scored `total_time` because `processing_time` did not react with
SMT off. With node-4 booted SMT ON (section 13) the aggressor pool moves onto
the victim's **sibling threads** and `processing_time` becomes the score input
again, as in the design and in July. Everything below was done on the cluster
and is in git (dsb_hd_counter `stage3-predictor`, simple-mitigation-1 `main`).

**Setup changes**

| where | change |
|---|---|
| `run-phased-stage3.sh` | default `AGGR_CPUSET=8,10,12,14,76,78` (76,78 = siblings of the victim's 4,6; was `4,6,8,10,12,14`, which only inflated total_time). `confine_aggressors` skips pods annotated `mitigation/hc-cpuset-set-by-node` (held by hc). |
| `score-verify/` | processing_time calibration, SMT on, victim alone on 4,6: `calibration_sweep.sh --section processing_time --rates "300 600 1200 2400 3000 3600 4200 4800 5600"`, then `calibrate_from_stream.py ... --window 10 --k K` (see below). Kept as `gordion-node4-8360y-smt-proc-3600.json` / `curve-node4-8360y-smt-proc-3600.csv`; the total_time ones are `calibration/total_time-20261022/`. |
| `pkg/actuators/hc/hc.go` | `confine` also widens the **pod-level** cpuset (v2 effective cpus are the intersection down the tree; with the pool excluding 4,6 the Balancer could never lend them). `Restore` writes parents first and, for a container that inherited its cpuset (saved value ""), writes the parent's `cpuset.cpus.effective`: cgroup v2 refuses to empty a populated cgroup's `cpuset.cpus` (ENOSPC), which every first Restore of the October sweep hit and silently left that container confined. |
| `cmd/mitigation-controller/formula.go` | `HC_THETA_OFF == HC_THETA_ON` accepted (bang-bang). |
| `deploy/stage3/arms/hc-bang.env` | hc, `HC_UP_WINDOWS=1`, single threshold, `HC_DOWN_WINDOWS=50` (release 5 s after the score drops; HC removes the contention that triggered it, so a zero-delay release would limit-cycle at the tick). |
| `deploy/stage3/arms/horizontal-any.env` | scale-out on y50 without the intrinsic weighting (`HORZ_FROM_EXT=0`, `HORZ_UP_WINDOWS=1`): replication applied to extrinsic contention = the unmatched mitigation. |
| `configs/motiv-ext-bursty.yaml` | victim 3600 exp; SN/SS `{burst_s 10, idle_s 10, peak 7000/8000, base 500/600}` from t=20 → bursts 20-30, 40-50, 60-70 (p2) and 80-90, 100-110, 120-130 (p3). |
| `tools/motiv_phases.py` | per-phase and per-burst processing/total p50/p90, e_iso/y50 per phase, controller actions with offsets from t0. |

**Why `k` and `smooth_window` changed.** y = tanh(k·ΔT/σ). Under SMT-sibling
contention processing p90 rises ~1.5× (p50 ~1.65×), far less than
total_time's 3-6×, and the intrinsic curve D(λ) is flat, so
ext90 = 1 − D/T̃ ≈ 0.33 at 1.5×: **e_iso saturates near 0.27**, not July's
~0.5. k is set so a 1.5× tail scores y90 = 0.8
(k = atanh(0.8)·σ90/(0.5·p90)); the smoother is 10 windows (1 s) instead of
30 so the bang-bang arm sees a burst within ~0.3 s instead of ~1 s. Both are
in `gordion.json`; thresholds below were read off the ARM=none run.

**Calibration (processing_time, SMT on, 2026-10-23 23:03-23:25):** baseline at
3600 rps p50 108 kcyc / p90 188 kcyc at 2.8 GHz (38.7 / 67 us), sigma50 13.6 %,
sigma90 5.9 % of the baseline (window 10); k = 0.1304. The curve is flat
(d50 82-119 kcyc over 180-3000 rps arrival), so any inflation reads as
extrinsic.

**Threshold.** ARM=none on `motiv-ext-bursty`: e_iso idle troughs 0.03-0.08,
burst plateaus 0.30-0.44, first burst bin 0.16, p1 ~0 (one 0.5 spike in the
first second after t0: smoother warm-up). `HC_THETA_ON = HC_THETA_OFF = 0.15`,
`THETA_ON = 0.15` for horizontal-any. `hc-balancer` static unit must be
inactive and `hc-listener` active (`systemctl start hc-listener`).

**Result (`data/motiv/{none,hc-bang,horizontal-any}`, `tools/motiv_phases.py --actions data/motiv/*`):**

| arm | p2 bursts proc p90 | p3 bursts proc p90 | p2 bursts total p50/p90 | p3 bursts total p50/p90 | p3 e_iso | actions |
|---|---|---|---|---|---|---|
| none | 1.75x | 1.83x | 2.93x / 2.65x | 3.13x / 2.64x | 0.29 | - |
| hc-bang | 1.74x | **1.18x** | 3.02x / 2.42x | **1.13x / 1.11x** | **0.02** | apply 1.7 s after ARM; 6 apply/restore cycles (~6 s on, ~4 s off: the limit cycle), 0 restore errors |
| horizontal-any, replicas OFF node-4 (old manifest; invalid as a replication test) | 1.77x | 1.99x | 2.95x / 2.74x | 0.82x / 0.85x | 0.32 | 1 uncontended replica at ARM+1 s, 2 at +33 s, back to 1 at +63 s |
| horizontal-any, replicas ON node-4 pinned 4,6 (`data/motiv/horizontal-any-node4`) | 1.63x | **2.09x** | 2.19x / 1.92x | 0.82x / 0.79x | **0.38** | 1 replica at ARM+1 s, 2 at +33 s (actuator's 30 s per-deployment cooldown; the controller's n counter ran to 3) |

(x = relative to that run's p1; proc = victim handler execution, total = victim
service total incl. downstream; whole-run wrk2 e2e p50: none 43 ms, hc-bang
31 ms, horizontal-any 23 ms.)

Reading: with the replicas on the contended node the handler gets *slower*
(p90 2.09x vs 1.63x unmitigated) and the score rises (e_iso 0.38 vs 0.28):
replication adds a second contended instance on the same cores. Its lower
total latency is queueing relief from splitting the traffic, bought with
extra capacity, not mitigation. Harvesting removes the contention (processing p90 back to 1.18x,
score to ~0, total back to ~1.1x) within ~2 s of each burst. Replication
lowers the *end-to-end* latency because three overflow replicas on idle
nodes take 3/4 of the traffic, but the victim's own execution gets slower
(proc p90 1.99x) and the score stays where it was (e_iso 0.32): the
contention is untouched, the symptom is bought with 4x the capacity. The
bang-bang limit cycle (HC removes the signal it was triggered by, so it
releases 5 s later and re-applies ~4 s after that) is visible in the action
list; `HC_DOWN_WINDOWS` (hc-bang.env) is the knob if a longer hold is wanted.

**Pitfalls seen here.** (1) After a horizontal run `search-overflow` was left
at 1 replica (controller shutdown Restore refused with "cross-node"); check
`kubectl get deploy search-overflow` and scale to 0 before the next run.
(2) `install-kit.sh` and the orchestrator read stdin: when driving them from a
script fed through `bash -s`, add `</dev/null` or they eat the script.
(3) The deployed `gordion-search` ConfigMap is now the processing_time one;
the total_time one is `score-verify/calibration/total_time-20261022/`.

**Figure.** `simulation/plot_motiv.py --root <dir with none/ hc-bang/ horizontal-any/> --theta 0.15`
(derived from `plot_real_run.py`'s loaders) writes
`simulation/results/2026-10-motiv-8360y/motiv_ext_bursty_timeseries.{png,pdf}`
(moved there 2026-11-02; results/ has one dated sub-directory per experiment,
see its README): per arm, handler
execution (processing p50/p90), victim service latency (total p50/p90) and
e_iso with the actuator state (HC held / overflow replicas), bursts shaded.
The hc-bang column shows the limit cycle directly: processing p90 snaps back
to baseline while HC is held and spikes in the ~4 s release gaps.

**Predictor data after the pool change (2026-10-24).** The 25-point
collection (section 13, `data/collect-smt`) used the sibling threads alone
as the pool (22 points) or `4,6,76,78` (3 points). Only the ext/mixed points
depend on the pool; baselines and int configs put nothing on node-4. The 12
ext + 2 mixed points were re-collected with the eval pool
(`configs/collect/plan-evalpool.txt`, `OUT=collect-smt-evalpool`), ~80 min.
Dataset A = the 10 pool-independent points of `collect-smt` + the 14 of
`collect-smt-evalpool`; the old ext points stay as dataset B (stronger,
siblings-only contention) for augmentation only. Label caveat: the offline
labeller scores processing **p50**, which in the eval pool barely moves
(1.05x in bursts) while p90 moves 1.75x and the hc trigger is p90-based
(e_iso = y90·ext90). Decide the label percentile before training; both can be
derived from the same run_data.

**Replication placement (design change 2026-10-24).** `kubernetes/search/
search-overflow-deployment.yaml` now schedules the overflow replicas ON
node-4, pinned to the victim's cores (`CPU_SET 4,6`, siblings = aggressor
pool), with the taint toleration. The first horizontal-any run above used the
old manifest (replicas off node-4), which is why its e2e latency dropped:
every replica was uncontended. Replication adds capacity where the service
is; only migration moves it away from the contention. Re-apply the manifest
after pulling (`kubectl apply -f .../search-overflow-deployment.yaml`); the
pending horizontal re-run of the 9 phased configs uses it too. The same-node
run is `data/motiv/horizontal-any-node4`
(`plot_motiv.py --dirs horizontal-any=horizontal-any-node4`).

**Commit (from the laptop; the changed files were copied into the local
clones, node-0 holds the same edits uncommitted -- after pushing, run
`git checkout -- . && git clean -fd deploy/stage3/arms` in /local/simple-mitigation-1 and
`git checkout -- hotelReservation/noisy-neighbors/stage3-eval && git pull` in /local/dsb_hd_counter,
keeping score-verify's untracked captures if wanted):**
```bash
cd C:/coding-workspace/simple-mitigation-1
git add cmd/mitigation-controller/formula.go pkg/actuators/hc/hc.go deploy/stage3/arms/hc-bang.env deploy/stage3/arms/horizontal-any.env     deploy/stage3/RUNBOOK.md simulation/plot_motiv.py
git add -f simulation/results/motiv_ext_bursty_timeseries.png simulation/results/motiv_ext_bursty_timeseries.pdf   # results/ is gitignored
git commit -m "hc: pod-level cpuset + ENOSPC-safe restore; bang-bang hc arm; HORZ_SIGNAL=e_iso arm; motivation figure; runbook sec 14"
git push
cd C:/coding-workspace/dsb_hd_counter-s3
git add hotelReservation/noisy-neighbors/stage3-eval/run-phased-stage3.sh hotelReservation/noisy-neighbors/stage3-eval/configs/motiv-ext-bursty.yaml     hotelReservation/noisy-neighbors/stage3-eval/configs/collect/plan-evalpool.txt hotelReservation/noisy-neighbors/stage3-eval/tools/motiv_phases.py     hotelReservation/noisy-neighbors/stage3-eval/score-verify/gordion.json hotelReservation/noisy-neighbors/stage3-eval/score-verify/curve.csv     hotelReservation/noisy-neighbors/stage3-eval/score-verify/gordion-node4-8360y-smt-proc-3600.json hotelReservation/noisy-neighbors/stage3-eval/score-verify/curve-node4-8360y-smt-proc-3600.csv     hotelReservation/noisy-neighbors/stage3-eval/score-verify/calibration-node4-proc.log hotelReservation/noisy-neighbors/stage3-eval/score-verify/calibration/total_time-20261022/
git commit -m "stage3: SMT-sibling aggressor pool, processing_time calibration (SMT on), motivation config, eval-pool collection plan, phase tool; overflow replicas on the target node"
git push
```

## 15. Motivation pass 2 on the Wisconsin sm110p: cores needed for the same SLO (2026-11-01)

Third cluster (user instantiated it 2026-10-31 from `harvest-k612`; 5x sm110p,
one 16C/32T Xeon Silver 4314 per node, sibling of CPU N is N+16). Restructured
motivation experiment: (1) replication in three placements -- same node and
same cores as the victim, same node on other cores, other node; (2) compare
the arms by the cores they need to meet the same SLO and by the CPU time the
victim, every replica and the aggressors actually consume; (3) later, more
scenarios with their matched and unmatched mitigation.

**Bring-up deltas against steps 1-7** (everything else as written):

- node-4 must run **SMT ON** (processing_time only reacts to siblings, sec 13):
  the profile boots it with `nosmt=force`; drop it from
  `/etc/default/grub.d/99-harvest.cfg`, `update-grub`, reboot, then the swap /
  kubelet chores of sec 13. `/sys/devices/system/cpu/smt/active` = 1, 32 CPUs.
- Frequency: `intel_cpufreq` passive; **pinned at 2.4 GHz as in July**
  (`intel_pstate/no_turbo=1`, `scaling_min_freq = scaling_max_freq = 2400000`
  on every CPU of node-4; re-apply after a reboot).
- CPU map (sm110p, SMT ON) = **July's layout** (user, 2026-11-01: this is the
  July hardware, so use the July setup, not the R650 adaptations such as the
  6-thread aggressor pool or the 1-P victim):

  | CPUs | use |
  |---|---|
  | 18, 19, 20 | victim `search` (`GOMAXPROCS=3`, limit 3; data-collector's slot 6) |
  | 2, 3, 4 | the victim's SMT siblings = where the handler contention comes from |
  | 0-13, 16-29 | aggressor pool `AGGR_CPUSET=0-13,16-29` (whole node minus the daemons' cores: July-like unpinned) |
  | 21-29 | "other threads" replication placement (nine threads for up to three 3-P replicas) |
  | 14 / 15 (30, 31 idle) | HC Balancer / Monitor |
  | 0 / 1 | HC Listener / controller (`CONTROLLER_CPU=1`), shared with the pool |
  | HC Secondary | pool minus victim threads and siblings: `0,1,5-13,16,17,21-29` |

  `/etc/default/hc-harvest`: `HC_CPULIST=18,19,20 HC_SECONDARY_CPULIST=0,1,5,...,29
  HC_MONITOR_CPU=15 HC_BALANCER_CPU=14 HC_LISTENER_CPU=0`. The module reads
  its cpulist at insmod: after editing, `systemctl stop hc-listener; rmmod
  qidlecpu; systemctl start hc-listener` and check `/proc/idlecpu/cpulist`.
- Pitfalls hit: files edited on Windows arrive with CRLF and the sourced
  `arms/*.env` die with `$'\r': command not found` (strip with
  `sed -i 's/\r$//'` before scp); `ssh node-0 'bash -s' < script` stops at the
  script's first inner `ssh` (it eats stdin) -- scp the script and run it from
  a file; a `pkill -f` pattern that also appears literally in the launching
  command line kills the launching shell (the cpu-sampler start is two ssh
  calls for that reason).

**New pieces (in git after this pass):**

| where | what |
|---|---|
| `deploy/stage3/cpu-sampler.sh` (kit) | per second: per-CPU busy/total jiffies and every pod cgroup's `cpu.stat usage_usec`; started/stopped by the orchestrator, lands in `mitigation/cpu_usage.csv` |
| `deploy/stage3/arms/rep{1,2,3}.env` | replication with a fixed budget: horizontal-any trigger, `N_MAX=k`, `HORIZONTAL_COOLDOWN_SEC=2`, no scale-in during the run |
| dsb `stage3-eval/run-phased-stage3.sh` | cpu-sampler start/stop/fetch, `pod_uids_{p3,end}.txt` (uid -> pod, to join the sampler's rows) |
| dsb `stage3-eval/stage3-eval.sh` | also captures `run_data-overflow-<pod>.json` from every overflow replica |
| dsb `stage3-eval/motiv2/overflow-{samecores,othercores,othernode}.yaml` | the three replica placements; `run-motiv2.sh` copies one over `kubernetes/search/search-overflow-deployment.yaml` before each run (hr_deploy re-applies that file); replicas are 3-P / 3-thread like the victim |
| dsb `stage3-eval/configs/motiv-sm110p.yaml` | motiv-ext-bursty with the victim on 18,19,20 |
| dsb `stage3-eval/run-motiv2.sh` | the 11-cell driver (resumable; `OUT=`, `CELLS=`, `DEADLINE=`) |
| dsb `stage3-eval/tools/motiv2_report.py` | per-cell latency ratios (victim instance and service pooled over replicas), e2e, cores per pod class, CPU-group busy, neighbour ratios, SLO table |

Cells: `none`, `hc-bang`, `rep{1,2,3}:samecores`,
`rep{1,2,3}:othercores`, `rep{1,2,3}:othernode` (~5.5 min each).

**Added during the pass (2026-11-01/02):**

- Calibration on node-4 (processing_time, victim alone on 18-20, 2.4 GHz):
  baseline p50 58.5 / p90 80.3 kcyc, sigma50 2.73 / sigma90 1.56 (window 10),
  **k = 0.0425**; curve flat 300-5600 rps, arrivals at search ~60 % of the
  offered rate. Files `stage3-eval/score-verify/*node4-sm110p-smt-proc-3600*`.
  Unmitigated motiv-sm110p: e_iso idle 0.02-0.09, bursts 0.40-0.55 -> THETA 0.15.
- More arms: `hc-hold` (HC applied at ARM, never released, no control logic),
  `hc-iso` (hc-hold + `HC_TARGET_IDLE_CORES=2`), `rep{1,2,3}f` (k replicas
  forced at ARM: for scenarios processing_time cannot see), `hcrep3`
  (hc-bang + 3 forced replicas).
- **The 3-P victim cannot be overloaded through the chain** (knee probe: 14k /
  20k / 28k offered -> 2.21 / 2.32 / 2.35 victim cores, chain max ~19.4k, same
  finding as the R650). Overload scenarios use a 1-P victim on thread 18:
  config `cpu_set: "18"` + `STAGE3_VICTIM_GOMAXPROCS=1` (new stage3-eval.sh
  env, `kubectl set env` after deploy) and 1-P replicas
  (`motiv2/overflow1p-*.yaml`, `OVF_PREFIX=overflow1p`), 800 + 10200 rps.
- Other-node cells start a second cpu-sampler on node-3
  (`cpu_usage-node-3.csv`); `motiv2_report.py` reads `cpu_usage-*.csv`.
- Sweeps run: A (10 s bursts) three times, INT twice, SHORT (3 s bursts),
  MIXED, uncapped replication rep6 twice, replica-count iteration rep4/5/8 on
  node-3 twice; 81 runs. **Results, caveats and how to regenerate every table:
  `deploy/stage3/MOTIV2-RESULTS.md`.** Archives
  `archives/2026-11-wisc-sm110p/{motiv2-sm110p,round2,round3}.tgz` (laptop,
  see `archives/README.md`; copy on CloudLab's Clemson `/proj`). Figures
  `simulation/results/2026-11-motiv2-sm110p/` from
  `stage3-eval/tools/motiv2_plot.py`.
  Headline: harvesting is the only arm that improves the handler under
  extrinsic bursts (1.7x -> 1.35-1.44x, zero extra cores); replication stays
  at 1.7-3.0x at any replica count (3-8 replicas, up to 3.0 cores); one
  off-thread replica fixes the intrinsic overload at +0.75-0.80 cores while HC
  does nothing; in MIXED only hc + other-node replicas meet the total SLO
  (2.62 cores).
- Predictor collection on this cluster (sec 13 layout question): configs are
  generated per layout, `python3 tools/gen_collect_configs.py --layout sm110p`
  -> `configs/collect-sm110p/` (victim 18,19,20; pool = July's unpinned
  `0-13,16-29`, alt pool = siblings only `2,3,4`), run with
  `PLAN=configs/collect-sm110p/plan.txt OUT=collect-sm110p ./run-collect.sh`.
  Started 2026-11-02 03:05 UTC (25 points, ~2.5 h); live scores are valid
  processing_time scores this time (k 0.0425 ConfigMap deployed).
- **Keeping data off the node while unattended runs finish** (2026-11-02):
  `/local` and `/users/<you>` on CloudLab nodes are local disk and die with
  the experiment; `/proj/<project>` is the persistent NFS export, but **per
  site** (a Wisconsin copy is not visible from a Clemson experiment). On the
  node holding the data (node-0) run, detached, `stage3-eval/tools/proj-sync.sh`:
  ```bash
  cd /local/dsb_hd_counter/hotelReservation/noisy-neighbors/stage3-eval
  DST=/proj/tracing-pythia-PG0/royno7-hc-archive-$(date -u +%F) \
    SRC="data/<sweep1> data/<sweep2>" WHILE='queue-[a-z0-9]*\.sh' \
    setsid nohup tools/proj-sync.sh > /tmp/proj-sync.log 2>&1 < /dev/null &
  ```
  It rsyncs `SRC` (default: everything under `data/`) plus queue logs,
  calibration files and `/tmp/*.tgz` / `/local/archive` into `$DST` every
  10 min while a process matching `WHILE` runs, then once more and writes
  `MD5SUMS`. Verify from another node: `ls /proj/.../<dir>/data`. To keep a
  copy at the other site, from a node there:
  `rsync -a <user>@<this node>:/proj/<project>/<dir>/ /proj/<project>/<dir>-<site>/`
  (needs that node's ssh key in your `authorized_keys` on the source node, or
  agent forwarding; remove the key line afterwards). Retrieve to the laptop
  with Windows OpenSSH `scp` from any node that mounts the copy.

## 16. Motivation pass 3 on the Wisconsin `icelake-perf` sm110p (2026-11-08/09)

Fourth cluster (a teammate's experiment from `harvest-k612`, node-0
`sm110p-10s10611.wisc.cloudlab.us`; royno7 has no node-0 key accepted by the
other nodes, so every detached run must live inside an agent-forwarded
`ssh -A` session from the laptop). Pass 3 answers the objections to pass 2:
service-level p50/p90/p99 during bursts and overall, bursts 10 % of the time,
a reserved replica node with a load dial, and an HC Secondary tenant in the
intrinsic scenario. Full setup and results: `MOTIV3-RESULTS.md`.

**Bring-up deltas against sec 15:**

- 2-P victim on threads 18,19 in BOTH scenarios (`cpu_set: "18,19"`,
  `STAGE3_VICTIM_GOMAXPROCS=2`, `/etc/default/hc-harvest` `HC_CPULIST=18,19`,
  arms `hc-*.env` `HC_VICTIM_CPULIST=18,19`); a 3-P victim cannot be
  overloaded through the chain (~18k rps cap, frontend ~1000 connections).
- node-3 tainted `dedicated=special`, 2.4 GHz, SMT on, hosts only the replicas
  (`motiv2/overflow-node3clean.yaml`, threads 18-29) and the load dial
  (`motiv2/node3-load.yaml`, busy loops on siblings 2-13, `DUTY` 50/100).
- Calibrate with `calibrate_from_stream.py --window 10` and k from the rule
  k·p90/σ90 = 2.19 (window 30 / k 0.0425 is blind to 6 s bursts).
- Driver `stage3-eval/run-motiv3.sh` (`CELLS="arm[:quiet|medium|contended]"`,
  `BURNER=1` for the intrinsic config, `ARM_OFFSET_S`), queue
  `queues/queue-motiv3.sh` (ext t1 → int t1 → ext t2 → int t2 → tgz), report
  `tools/motiv3_report.py` (`--steady --duration 140` for the intrinsic
  sweeps), figures `tools/motiv3_plot.py --root C:/tmp/motiv3/data`.

**Pitfalls added:**

- A detached queue survives the ssh session but loses the agent: the next
  `ssh node-4` (ARM touch, cpu-sampler fetch) hangs or fails and the cell is
  silently invalid (`WARNING: failed to touch ARM`, bursts p2/p3 = 4/0,
  no `cpu_usage.csv`). Check `armed_epoch_ns.txt` lies inside the run and
  that `mitigation/cpu_usage.csv` exists before trusting a cell; relaunch
  with a script file (`bash /tmp/relaunch.sh`) that kills the leftovers
  first, never with an inline `pkill -f` whose pattern is in the command.
- The hc actuator's `Restore` resets Secondary thread affinity to the whole
  aggressor cpuset. A self-pinning Secondary (the burner) must be recreated
  per cell (`set_burner` deletes and re-applies the deployment).
- `kubectl scale` of the chain services to 2-3 replicas does not raise the
  knee and starves node-1/2 of CPU requests (sockshop pods Pending).

## 17. Motivation pass 3 re-run on the Clemson c6320 `icelake-perf1` (2026-11-10)

Fifth cluster (user's, from `harvest-k612`; node-0 `clnode170.clemson.cloudlab.us`).
**It is not Ice Lake:** 5x c6320 = 2x Xeon E5-2683 v3 (Haswell, 14C/28T each,
2.0 GHz base), so nothing is comparable with the sm110p numbers and every
figure says c6320. Goal (user): rerun every cell the motivation figures use,
two trials, including the reactive quiet-node replication cell the sm110p pass
lacked (`rep3p:quiet`). Results: `MOTIV3-RESULTS.md` section 3.

**CPU map (node-4, SMT ON; socket 0 = even ids 0-26, sibling of N = N+28):**

| CPUs | use |
|---|---|
| 4, 6 | victim `search` (2 Ps; siblings 32, 34 are where the contention comes from) |
| 4, 6, 8, 10 | hc-grow's 4-thread victim (8, 10 = the harvestable pair) |
| 12, 14, 16, 18 | intrinsic burner (`motiv2/burner-node4-c6320.yaml`); hc-grow's burner: 12 + 8, 10 |
| 0, 2, 4-22 even, 28-50 even | aggressor pool `AGGR_CPUSET` (socket 0 minus the daemons' 24, 26 and their siblings 52, 54) |
| 24 / 26 | HC Balancer / Monitor; Listener 0, controller 2 (shared with the pool) |
| node-3: 4-26 even / 32-54 even | replicas (`overflow-node3clean-c6320.yaml`) / load dial (`node3-load-c6320.yaml`) |

Arms with this map: `deploy/stage3/arms-c6320/{common,hc-hold,hc-grow,hc-int}.env`
(copy over `arms/` before `install-kit.sh`). Configs
`configs/motiv3-c6320{,-int,-int4}.yaml`; `run-motiv3.sh` takes
`LOAD_MANIFEST`, `OVF=overflow-node3clean-c6320`, `BURNER_MANIFEST`;
report tools take `MOTIV_LAYOUT=c6320`; figures `motiv3_plot.py --machine c6320`.

**Bring-up deltas against sec 15/16 (all hit on 2026-11-10):**

- node-4: drop `nosmt=force` from `/etc/default/grub.d/99-harvest.cfg`,
  `update-grub`, reboot; then swap off + kubelet restart; **2.0 GHz pinned**
  on node-3 and node-4 (`intel_cpufreq` passive: `no_turbo=1`, governor
  `performance`, `scaling_min_freq = scaling_max_freq = 2000000`).
- royno7 needs `sudo cp /etc/kubernetes/admin.conf ~/.kube/config`;
  `go.dev/dl` resets connections from Clemson, use
  `https://dl.google.com/go/go1.23.4.linux-amd64.tar.gz`; `install-kit.sh`
  needs `apt install protobuf-compiler`.
- **Haswell PMU:** 4 programmable + 3 fixed counters per thread with SMT on.
  The victim's 10-event `PERF_EVENTS` set fails outright
  (`Failed to open perf event dTLB-load-misses on CPU 0: Invalid argument`,
  CrashLoopBackOff); with 8 events (`cycles,ref-cycles,instructions,LLC-loads,
  LLC-load-misses,branch-instructions,branch-misses,page-faults`, via
  `STAGE3_PERF_EVENTS`) the group opens but, because the sampler opens all
  events as one group per CPU and the **NMI watchdog holds a counter**, every
  delta reads 0 and `freq.ok` is false (the frequency comes from
  cycles / ref-cycles), so the calibrator sees "0 clean windows". Fix:
  `kernel.nmi_watchdog = 0` on node-3 and node-4 (sysctl.d). Check a sample's
  `perfDeltas` before calibrating.
- `calibration_sweep.sh` and the knee probe need the frontend Service as
  NodePort (`kubectl patch svc frontend -p '{"spec":{"type":"NodePort"}}'`);
  the runner only does it inside a run.
- Knee (2-P victim, 2.0 GHz): offered 2000 -> 0.96 cores, e2e p50 3.9 ms;
  6000 -> 1.50 cores, 10 ms; 8000 -> 1.61 cores, 32 ms; 10000 collapses
  (e2e p50 4 s). Loads: extrinsic victim 2000 rps (the sm110p's 3600 by
  cores), aggressor peaks halved (SN 3500, SS 4000; the pool is 12 cores,
  not 14), intrinsic 800 + 7200 = 8000 (~1.2x knee), calibration operating
  point 2000.
- Pitfall repeat: a `pkill -f "knee3.sh"` typed in the same ssh command as
  `bash /tmp/knee3.sh` kills the shell (exit 127); kill in a separate command.
- Intrinsic scenario on this chain: the hotelReservation services saturate at
  ~9k rps (frontend 14 cores, reservation 12, rate 10.5/12 at 10k), so with the
  2-P victim both mitigations are chain-bound at 8000 and the figure cannot
  separate them. The intrinsic figure therefore uses a **1-P victim** (thread 4,
  `STAGE3_VICTIM_GOMAXPROCS=1`, knee ~4000) at 5000 rps: `configs/motiv3-c6320-int1p.yaml`
  (none, rep1f with 1-P replicas `motiv2/overflow-node3clean-c6320-1p.yaml`) and
  `motiv3-c6320-int1p2.yaml` + `arms-c6320/hc-grow1.env` + `motiv2/burner-node4-shared1-c6320.yaml`
  (hc-grow1: thread 4 own + 6 shared, GOMAXPROCS 2). Queue: `queues/queue-int1p.sh`.
- Unattended runs: node-0 -> node-3/4 hops need royno7's own key on node-0
  (`ssh-keygen` + `ssh-copy-id node-3 node-4`, done by the user 2026-11-10); with it
  a queue runs detached (`setsid nohup ... < /dev/null &`, `unset SSH_AUTH_SOCK`)
  and survives the laptop. The 1-P load sweep (`queues/queue-sweep1p.sh`: loads 3000-8000,
  2-3 trials, resumable, second pass, tgz + /proj at the end) ran this way, 9.3 h.
- All queue scripts of passes 3 (`queue-motiv3.sh`, `queue-c6320.sh` + `launch-c6320.sh`,
  `queue-intprobe.sh`, `queue-int1p.sh`, `queue-sweep1p.sh`, `check-cell.sh`) are kept in
  `deploy/stage3/queues/` (see its README); copy to node-0 `/tmp` to run.
- EQ1/EQ2 budget sweep (user's evaluation questions, 2026-11-11): `queues/queue-eq.sh`
  ran detached 2026-11-11 23:36 – 11-12 04:17 UTC (32 cells, all rc=0) with the same
  quiet load (2000 rps) in both scenarios; steady cell = 1-P victim at 2000 + 4000 extra
  (6000 offered) against none, rep1f-rep4f and hc-grow1/2/3 (`arms-c6320/hc-grow{2,3}.env`,
  `motiv2/burner-node4-shared{2,3}-c6320.yaml`, `configs/motiv3-c6320-int1p{,2,3,4}-q2000-6000.yaml`),
  tail cell adds rep1f / rep3f / rep6f / rep6p to the existing none / hc-hold / rep3p.
  Figure `simulation/results/2026-11-motiv3-c6320/motiv3_eq.{png,pdf,csv}` from
  `tools/motiv3_eq.py`; results in `MOTIV3-RESULTS.md` sec 3.4; data
  `archives/2026-11-clemson-c6320-pass3/motiv3-c6320-eq.tgz` (+ `/proj` mirror).

### 17.1 Gordion Phase G on the same cluster (2026-11-10, workstream 3) — environment changes

The ρ-indexing / three-signal work (`dsb_hd_counter/PHASEG-HANDOFF.md`, results in
`blueprint/examples/bigsvc/RESULTS-G.md`) shares this experiment. It stays off node-4 and
off the `default` namespace; nothing of sections 14–17 is modified. Changes made, in order:

- node-0: Go 1.25.3 installed **beside** the kit's 1.23.4, at `/usr/local/go1.25` (used only
  via `PATH=/usr/local/go1.25/bin:...` / `GOROOT_OVERRIDE` in the Gordion scripts; `/usr/local/go`
  untouched); `protoc-gen-go@v1.36.6`, `protoc-gen-go-grpc@v1.5.1` in `~/go/bin`; `royno7` added to
  the `docker` group (`sudo usermod -aG docker royno7`; the kit used sudo docker / root).
- node-0: `/local/blueprint` = clone of `RoyZhang7/blueprint` branch `per-endpoint-dictionary` plus the
  laptop's uncommitted edits (tar sync); `/local/hr/hotelReservation` = the dsb_hd_counter HR fork with
  the gordion recorder; `/local/results/{build,hr,...}` for Gordion runs.
- Registry `10.10.1.1:5000`: images `big_ctr:k123g3` (synthetic service) and `hr-gordion:gordion6`.
- **node-3 is the Gordion service node** (idle: no motivation pods, 2.0 GHz pinned, NMI watchdog off, as
  left by section 17). Socket 1 only, odd CPU ids: SUT on `1,3,5,7` (synthetic, k = 4) or `1,3` (HR
  recommendation, k = 2), SMT siblings `29,31,33,35` left idle; load generator pinned on
  `17,19,21,23,25,27`; stress-ng aggressors on the SUT cores. Socket 0 (the motivation's node-3 CPU map)
  is not used. Host dir `/var/tmp/gordion` (chmod 777) for the recorder files; `stress-ng` apt-installed
  on node-3 when the aggressor rows start.
- Kubernetes: namespace **`gordion`** with a second HotelReservation stack from
  `hotelReservation/kubernetes/` (PVCs re-pointed to the dynamic `nfs-client` class because the fork's
  static PVs are bound to `default`; CPU quotas stripped from the eight services; DBs/consul/jaeger on
  node-1/node-2). The `default` HR (the motivation victim with 8–24-core limits) is left as it is.
- node-4: untouched; a leftover `check_idle_cpus` process was found at 100 % on CPU 26 (not ours, not killed).
- node-3/node-4 carry the taint `dedicated=special:NoSchedule` (section 17 setup); the Gordion pods tolerate it
  (`tolerations: dedicated Exists NoSchedule` in `exp.sh`'s manifest and `hr.sh deploy_svc`). The taint itself is
  unchanged.
- HR gordion image builder bumped from `golang:1.21` to `golang:1.25` (`hotelReservation/Dockerfile`) so the recorder's
  Phase G runtime counters (`/sched/pauses/total/gc:seconds`, Go 1.22+) exist; image tag `hr-gordion:gordion7`.
- royno7's node-0 key (`~/.ssh/id_rsa.pub`, created by the user for section 17) copied to node-1 and node-2 as well
  (`ssh-copy-id`), so the Gordion HR driver can run detached (it places pods on node-1/2 and creates their host dirs).
- node-3: `bpftrace` (apt, 0.14.0) installed for the Phase G scheduler probe (`blueprint/examples/bigsvc/probe/`,
  copied to `/var/tmp/gordion/probe/`); the probe runs as root via sudo, attaches `kfunc:finish_task_switch`,
  `tracepoint:sched:sched_wakeup` and the futex syscall tracepoints, filtered to the SUT's CPUs; a cgroup
  `/sys/fs/cgroup/gordion-aggr` is created for stress-ng so the aggressor has a cgroup id.
- Phase G §2.4: SocialNetwork gordion stack installed by `/local/sn/sn.clean.sh install` in namespace **`sn`** (the
  motivation's `aggressor-sn` namespace is untouched), image `sn-gordion:g5` (recorder emits `rawt` = thread id per
  sample), datastores + nginx on node-1, post-storage SUT pinned on node-3 CPU 1 with the taint toleration; the
  socfb-Reed98 graph loaded through nginx. Probe v3 runs on node-3 for every Gordion run (`/var/tmp/gordion/probe/`).
  New mix files `/local/results/mix-memwalk30.json` (30 % VioMemWalk, `MEMWALK_MB=128`) and `mix-mutex10.json`
  (10 % VioMutex) on node-0 and node-3; stress-ng `--stream` ×4 on CPUs 9,11,13,15 for the LLC aggressor.
- Phase G §2.4 (v), 2026-11-11 06:14 UTC: the post-storage SUT pod in namespace `sn` now runs with **`hostPID: true`**
  (`sn.sh deploy` with `HOSTPID=1`; `/local/sn/sn.clean.sh` regenerated from the patched `socialNetwork/gordion/sn.sh`)
  so the recorder's per-request thread ids are host tids the scheduler probe can join. The deployment keeps the field
  after the run; patch `{"spec":{"template":{"spec":{"hostPID":false}}}}` to revert if the namespace is reused.
- Phase G §2.4 D: a stray second probe instance (`probe.py`/`bpftrace` on CPUs 1,3,5,7, left by a duplicate queue
  chain at 05:12) was found and stopped at 06:14; its files `/var/tmp/gordion/20261111-051210-*probe*` (10 MB) can be
  deleted. The (iv-b′) pair used `BIGSVC_MEMWALK_MB=24` as a pod environment variable only (no cluster change).
- Phase G §2.4 (v) run 3, 06:38 UTC: probe **v3.1** installed on node-3 (`/var/tmp/gordion/probe/`): `BPFTRACE_MAP_KEYS_MAX`
  raised to 1 048 576 and per-thread map entries dropped on `sched_process_exit` (the 4096-key default lost every
  post-storage thread created after ~90 s). Unit test (victim + hog on CPU 1) unchanged: aggressor credited 50.0 ms per
  100 ms window. No other change.
- Phase G Addendum D (overnight queue `queue-g2f.sh`, armed 07:15 UTC, detached on node-0): `msr-tools` apt-installed on
  node-3 and the `msr` module loaded, **read-only** use (`rdmsr 0x621`, the current uncore ratio, once per second into
  each run's `uncore.tsv`); the uncore is NOT pinned (`/sys/devices/system/cpu/intel_uncore_frequency` does not exist
  on this Haswell-EP kernel; pinning would need `wrmsr 0x620`, not done). cpufreq on socket 1 was found already pinned
  (`intel_cpufreq` passive, `performance`, min = max = 2 000 000 kHz on every core) and is **not changed**; each run
  records it in `freq.txt`. New aggressor method `stress-ng --cache 4 --cache-level 3 --cache-no-affinity` on CPUs
  9,11,13,15 in the `gordion-aggr` cgroup (A1/A4). New mix files `mix-memwalk{10,20,40,50}.json`, `mix-mutex40.json`
  on node-0 and node-3 `/local/results/`. The probe's userspace is pinned to CPU 9 (`PROBE_CPU`, v3.2). A stray smoke
  probe from 06:59 is killed by the queue after pair 4 ends.
- Phase G Addendum D, 07:50 UTC: `msr-tools` did NOT install (no `rdmsr`); the uncore ratio is read through
  `/dev/cpu/N/msr` from Python instead (`/var/tmp/gordion/uncore-trace.sh`, `freq-record.sh`), read-only. Found:
  `MSR 0x620 = 0x1b1b` on package 1 — uncore ratio min = max = 27 (2.7 GHz), i.e. already fixed by the platform;
  nothing was written. Order-control queue `queue-g2f2.sh` (24 MB memwalk pair in reverse order plus a second twin)
  armed after the G5.2 queue; both wait for the running queue and change no settings.
- Phase G Addendum D, end state (checked 21:45 UTC Nov 11): all three queues finished (08:45, 08:56, 09:20); node-3 has
  no stress-ng, probe or uncore-trace process left, the `gordion-aggr` cgroup is empty; cpufreq unchanged (final record
  `/local/results/g2f-final/freq.txt`: performance, 2.0 GHz min = max on every socket-1 core); uncore untouched (ratio 27,
  fixed by the platform); the `sn` post-storage deployment keeps `hostPID: true`. Nothing else was changed overnight.
- **Phase G closing, 2026-11-11 22:12–22:40 UTC — controls and end state.** Two idle-state controls ran on node-3
  (`queue-g2f3.sh`): a pure CPU spinner neighbour on CPUs 9,11,13,15 and a memwalk twin with C1E (`cpuidle/state2`)
  disabled on all 28 socket-1 CPUs; C1E was re-enabled afterwards and verified (disable flags 0 0 0 28 28 = as found:
  C3 and C6 were disabled before Phase G and were never touched). End state verified by `cleanup-g.sh` at 22:36 UTC:
  no stress-ng / bpftrace / probe / trace process on node-3; cgroups `gordion-aggr` and `gordion-victim` removed (none
  left); `/var/tmp/gordion/probe` and every probe, uncore and C-state trace file removed, then the whole
  `/var/tmp/gordion` host directory (2.2 GB of recorder copies, all already on node-0 `/local/results` and the laptop);
  cpufreq on all 28 socket-1 cores `performance`, min = max = 2 000 000 kHz (pre-Phase-G value, never changed); uncore
  `0x620 = 0x1b1b` (ratio 27 fixed, never written); no queue process on node-0. **Left in place, not asked:** the
  `gordion` (HotelReservation, 19 pods) and `sn` (SocialNetwork, 27 pods, post-storage with `hostPID: true`)
  namespaces, the apt packages `bpftrace` and `stress-ng`, the `msr` module, Go 1.25 on node-0, royno7's key on
  node-1/2, the motivation experiment on node-4 untouched. The nodes' frequency and idle-state settings are exactly
  what Phase G found.
- **Phase G Addendum E (2026-11-12, 05:00–05:25 UTC), environment changes on node-3 (nothing on node-4 / `default`):**
  (1) E.2 checks, read-only (`blueprint/examples/bigsvc/probe/e2_checks.sh`, output in RESULTS-G §6.2) plus one reversible
  check: `mount -t resctrl resctrl /sys/fs/resctrl` at 05:11 UTC to read `info/` (CMT `llc_occupancy` only, 56 RMIDs; no
  MBM, no CAT, no MBA on this Haswell), **unmounted again** in the same command (`mount | grep -c resctrl` = 0 after).
  (2) `/var/tmp/gordion` (chmod 777) and `/var/tmp/gordion/probe` recreated (the Phase G closing had removed them); probe
  **v4** installed there (`gordion_sched.bt.tmpl`, `probe.py`, `probe-ctl.sh`, `probe-unit-v4.sh`) with `cpustat.sh` and
  `freq-record.sh`; the `gordion-aggr` cgroup recreated (`/sys/fs/cgroup/gordion-aggr`). (3) Probe v4 unit test run once
  (05:21 UTC, ~45 s): a CPU-bound loop in a temporary `gordion-victim` cgroup on CPU 1 and a hog in `gordion-aggr` on CPU 29
  then CPU 1; the `gordion-victim` cgroup was removed afterwards. (4) E.4 queue `/local/results/queue-ge4.sh` armed detached
  on node-0 at 05:22 UTC (7 runs, ~50 min): bigsvc on CPUs 1,3,5,7 as before; **new:** stress-ng `matrixprod` ×4 pinned to the
  SMT siblings 29,31,33,35 (in `gordion-aggr`) for the E.4 (a) runs; `perf stat` in counting mode (cycles, instructions,
  LLC-loads, LLC-load-misses, 100 ms intervals) on the victim process and on the aggressor CPUs during the probe-on runs
  (`perf_event_paranoid` = 2 left as is; perf runs as root); probe userspace and perf pinned to CPU 53. The cache aggressor
  of E.4 (b) is the D.4 one (`--cache` ×4 on 9,11,13,15). cpufreq, idle states, uncore, NMI watchdog: untouched. The queue
  ends with the stressor stopped and a leftover-process check; the probe dir and `/var/tmp/gordion` are left in place this
  time (small) until the user says otherwise.
- **Phase G Addendum E, end state (checked 06:48 UTC Nov 12):** both queues finished (`queue-ge4.sh` 06:16, the corrected
  (b) pair `queue-ge4b.sh` 06:27–06:45 after `probe-ctl.sh` v4.1 switched the victim counters to perf cgroup mode); node-3 has
  no stress-ng / bpftrace / perf / probe process, `gordion-aggr` exists and is empty, cpufreq `performance` 2 000 000 kHz
  min = max on the cpuset and its siblings (unchanged), C1E enabled / C3 disabled as found, resctrl not mounted, the two
  Gordion pods (bigsvc in `default`, recommendation in `gordion`) and the `sn` post-storage pod as before. **Left in place:**
  `/var/tmp/gordion` (256 MB: the nine runs' recorder, probe, perf and cpustat copies, all fetched to node-0
  `/local/results` and the laptop) with probe v4.1 under `/var/tmp/gordion/probe/`; `/local/results/queue-ge4{,b}.sh` and
  their logs. Nothing on node-4 or in `default` HR / `aggressor-*` was touched.

- **Phase G Addendum F (2026-11-12, 07:00–08:30 UTC), node-0 / node-3 / namespace `sn`:** (1) image `sn-gordion:g6` built on
  node-0 from `/local/sn/gordion` (recorder emits `rawe`, the completion time per sample; `unique-id-service` gets the
  recorder) and pushed to the registry; `g5` kept. (2) Probe **v5** installed in `/var/tmp/gordion/probe/` (per-resume
  records, softirq tracepoints `irq:softirq_raise/entry/exit` on the cpuset; `probe-resume.jsonl` per run). (3) `sn.clean.sh`
  used with `INSTRUMENTED` extended by `unique-id-service`: the Run A deploys moved **unique-id-service to node-3 CPU 1 with
  `hostPID: true`** (image g6) and the other instrumented services, post-storage included, to node-1/2; the Run B deploys put
  post-storage back on node-3 CPU 1 (`hostPID: true`, image g6). (4) `stressor.sh` gained `METHOD=duty<N>` (stress-ng
  `--cpu-load N --cpu-load-slice 10`) for the 20 % duty-cycled hog on CPU 1 in `gordion-aggr`; Run B's hog is the pair-4
  `matrixprod` hog. Queue `/local/results/queue-gf.sh`, armed 07:39 UTC. End state and restore: see the next entry.
- **Phase G Addendum F, end state and restore (08:30 UTC Nov 12):** the 07:39 queue ran on image g5 (helm had set the image
  at install; `deploy` patches only command/env) and was stopped at 07:50; **every C++ service of `sn` was switched to
  `sn-gordion:g6`** (`kubectl set image`, 11 deployments) and the queue re-run 07:51–08:20 (five runs). Restore: node-3 has
  no probe / bpftrace / stress-ng / perf / hrgen; `gordion-aggr` cgroup removed; the run copies in `/var/tmp/gordion`
  deleted (969 MB → 64 KB: `cpustat.sh`, `freq-record.sh` and `probe/` v5 kept); cpufreq `performance` 2.0 GHz min = max,
  idle states and resctrl untouched; **`unique-id-service` moved back to node-1, unpinned, `hostPID: false`**;
  `post-storage-service` stays as the Phase G closing left it (node-3 CPU 1, `hostPID: true`), now on image g6 like the
  other ten C++ services (g5 remains in the registry); bigsvc (`default`) and recommendation (`gordion`) pods unchanged;
  no queue on node-0; `/local/results/sn` holds the five Addendum F runs and the three discarded g5-attempt directories.
- **Phase K1 (2026-11-13, from 03:30 UTC), eBPF hook overhead, node-0 / node-3 / namespace `sn`:** (1) node-3: **apt
  install `clang-14 llvm-14`** (27 packages, list in `/var/tmp/k1build/dpkg-added.txt`, to be removed at the end);
  `/var/tmp/k1build` with libbpf v1.4.7 and bpftool v7.4.0 sources from GitHub, built there (static libbpf, `bpftool`), and
  the K1 libbpf probe (`k1probe`, `k1probe.bpf.o`); nothing installed system-wide from them. (2) `/var/tmp/gordion/k1/`: the
  bpftrace program variants (`progs/`), the cell controller `k1ctl.py`, the Thrift load generator `ugen` (built on node-0
  with Go 1.25). (3) During every cell the controller sets **`kernel.bpf_stats_enabled=1`** (0 during `:nostats` arms) and
  restores the value it found (0) at the end of the cell. (4) Aggressors (stress-ng `matrixprod`, `--cpu-load 20`,
  `--switch`) run in a recreated `/sys/fs/cgroup/gordion-aggr` on the victim cores or on their SMT siblings, one cell at a
  time. (5) Victims: bigsvc `k123g4` redeployed per cell (node-3 CPUs 1,3,5,7, as Phase G); **`unique-id-service` moved to
  node-3 CPU 1 with `hostPID: true`** (image g6) for its cells, which moves **post-storage-service to node-1/2 unpinned**
  (`sn.sh deploy` places non-SUT C++ services off the service node); the post-storage cells move it back to node-3 CPU 1.
  cpufreq (performance, 2.0 GHz min = max), idle states, uncore untouched. Queue `/local/results/queue-k1.sh` with
  `/local/results/k1/cells.txt`, armed 04:05 UTC; results `/local/results/k1/`.
- **INCIDENT, node-4, 2026-11-13 06:03 UTC (Phase K0b, caused by the agent).** During a read-only check, `sudo cat
  /proc/idlecpu/irqlist` on node-4 hit a NULL dereference in HC's `irqlist_reader` (`vmalloc(0)`, never-reset global
  offset). Node-4 runs `panic_on_oops=1`, `panic=10`, so it panicked and rebooted, back up at 06:05:37. The pstore dump is
  at `/var/lib/systemd/pstore/7707532676273/` on node-4 and copied to node-0 `/local/results/k0b/`. **The reboot reset:**
  swap on, so the kubelet fails and node-4 is NotReady; cpufreq at schedutil 1.2-3.0 GHz with turbo on (was performance
  2.0 GHz min = max, `no_turbo=1`); `qidlecpu` unloaded and `hc-listener` inactive (was loaded, Monitor on CPU 26,
  cpulist `4,6,8,10`, Listener active since Nov 10). Persisted: NMI watchdog off, irqbalance off, C-state limit, SMT on.
  **Not yet restored:** the agent's restore attempt (swap off, kubelet restart, 2.0 GHz pin) was refused by its permission
  policy. To restore by hand on node-4:
  `sudo swapoff -a && sudo systemctl restart kubelet`; then for every CPU set `scaling_governor=performance`,
  `scaling_max_freq=2000000`, `scaling_min_freq=2000000`, and `intel_pstate/no_turbo=1`; then, only if HC should be back
  as found, `sudo systemctl start hc-listener` (its ExecStartPre `hc-load-module` reloads the module, binds CPU 26 and
  sets cpulist `4,6`) and `echo 4,6,8,10 | sudo tee /proc/idlecpu/cpulist`. **Do not read `/proc/idlecpu/{irqlist,cpulist,irqtime}`**
  until the readers are fixed (RESULTS-K section 0b.3). K1 on node-3 was not affected.
  **Restored by the agent with the user's permission:** swap off, kubelet restarted (node-4 `Ready`), all 56 CPUs
  `performance` at min = max = 2 000 000 kHz with `no_turbo=1`, `hc-listener` started (its `hc-load-module` reloaded
  `qidlecpu`: Monitor kthread on CPU 26, cpulist `4,6` → rewritten to `4,6,8,10` as found), `hc-balancer` inactive as found.
  Verified with `lsmod`, `systemctl`, `ps` and sysfs only. Node-4 runs the same pods as before the crash (calico-node,
  kube-proxy, nginx-proxy).
- **Phase K0b 3a, node-4, 2026-11-13 06:38-06:39 UTC (user-approved):** `hc-listener` stopped, `qidlecpu` unloaded, fresh
  `insmod` as shipped (cpulist `4,6`, bindcpu 26, control 1 by the sampler), 62 s of sampling through `mmap` of
  `/dev/qidlecpu` and `/proc/<kthread>/sched`, no HC procfs file read, module unloaded, then restored as found:
  `hc-listener` started (reloads the module, Monitor on CPU 26), cpulist `4,6,8,10`, `hc-balancer` inactive. No kernel
  warnings beyond the load/unload lines. Results in RESULTS-K section 0b.4 and node-0 `/local/results/k0b/`.
- **K1, 07:40 UTC:** the queue stops loads with `pkill -x wlgen`, but the binary runs as `wlgen-latest`, so a cell's load
  outlived the cell and the next cell's wlgen copy failed (`Text file busy`). Affected: a-syn-r05, b-syn-hog1,
  b-syn-duty20. Fix without touching the running queue: a watchdog `~/k1-loadwatch.sh` on node-0 stops any `wlgen-latest`
  on node-3 that does not belong to the current cell (log `/local/results/k1/loadwatch.log`; it exits when the rerun
  queue ends), and `~/k1c-chain.sh` reruns the three cells after `queue-k1b` (`/local/results/k1/cells-k1c.txt`, log
  `queue-k1c.log`).
- **K1 end, 16:19 UTC Nov 13:** `queue-k1c` printed DONE-K1; the watchdog exited; no K1 process on node-0 or node-3.
  On node-0 the recorders were reduced in place (`~/k1_reduce.py` → `lat_reduced.json` per run) and a small-file tarball
  `~/k1-small.tgz` was fetched to the laptop; `/local/results/k1/` keeps everything.
- **K1 reader check, node-3, 18:02–18:08 UTC (idle, after the queue):** bpftrace 0.14 run directly, pinned to CPU 53,
  twice per program, on the Phase G §2.3 program and the K1 `full_both` program, with `BPFTRACE_MAP_KEYS_MAX` unset
  (4096) and set to 1048576; `strace -c` (12 s) and `perf record -g` (10 s) on the bpftrace process. Files in
  `/var/tmp/gordion/k1check`, copied to node-0 `/local/results/k1/readercheck/` and removed in the restore below. Nothing
  else on node-3 was touched (`bpf_stats_enabled` stayed 0). Result: RESULTS-K §1.6.
- **K1 restore, 18:12 UTC Nov 13:** (1) node-0 keeps copies of node-3's two K1 recorder leftovers
  (`k1-uid-sweep-unique-id-service.jsonl`, the set-up sweep; `20261113-053122-k1-a-syn-knee.jsonl`, the aborted cell) in
  `/local/results/k1/node3-leftovers/`. (2) node-3: **`apt-get purge` of the 27 packages in `dpkg-added.txt`** after an
  `apt-get -s` simulation showed exactly those 27 and nothing else; 0 of them still installed, bpftrace v0.14.0 intact.
  **Removed:** `/var/tmp/k1build` (libbpf, bpftool, the libbpf probe), `/var/tmp/gordion/k1` (variants, controller,
  `ugen`), `/var/tmp/gordion/k1check`, the two leftovers, and the empty `/sys/fs/cgroup/gordion-aggr`.
  `kernel.bpf_stats_enabled = 0`; cpufreq `performance` 2 000 000 kHz min = max as found; `/var/tmp/gordion` is back to
  `cpustat.sh`, `freq-record.sh`, `probe/` (64 KB). (3) **`unique-id-service` back to node-1, unpinned, `hostPID: false`**
  (the Addendum F restore's `kubectl patch`; image g6); `post-storage-service` on node-3 CPU 1 with `hostPID: true`,
  image g6, as the Phase G closing left it (the e-ps cells had already put it there). bigsvc (`default`, node-3 CPUs
  1,3,5,7, `k123g4`) and `recommendation` (`gordion`) are unchanged. **Left as after Addendum F:** both SN pods keep the
  recorder env of their last run, so `unique-id-service` on node-1 still appends to
  `/var/tmp/gordion/20261113-135430-k1-d-uid-unique-id-service.jsonl` (47 MB at the restore; the two Addendum F files
  beside it are older). Node-0 keeps `/local/results/queue-k1*.sh`, their logs and `~/k1-*.sh`. Nothing on node-4 or in
  `default` HR / `aggressor-*` was touched by K1.
- **K1 step 0, 18:35–18:41 UTC Nov 13 (user):** every recorder still writing a K1-named file was switched off with
  `kubectl set env ... GORDION_DISABLE=1` (pods restarted in place): `sn` compose-post, home-timeline, post-storage, text,
  unique-id and user-timeline services, and `default/bigsvc` (bigsvc and post-storage were writing into K1 files the queue
  had already deleted). All 19 `20261113-*-k1-*` recorder files on node-1 (11, incl. unique-id's 47.7 MB) and node-2 (8)
  were copied gzipped to node-0 `/local/results/k1/archive-nodes/<node>/`, size-checked, then removed from the nodes.
  After: no process on node-1/2/3 holds a K1 file, no probe/aggressor/generator runs, `bpf_stats_enabled` 0 everywhere.
- **K1 addendum (lean libbpf), from 18:42 UTC Nov 13:** node-3 again: `apt-get install clang-14 llvm-14` (27 packages,
  `/var/tmp/k1build/dpkg-added.txt`), `/var/tmp/k1build` (libbpf v1.4.7 static, bpftool v7.4.0, `k1lean`, `k1lean.bpf.o`,
  `k1empty.bpf.o`), `/var/tmp/gordion/k1` (`k1ctl.py` with the lean arms, `ugen`); a 10 s functional test in a temporary
  cgroup `gordion-k1func` (removed). Queue `/local/results/queue-k1d.sh` with `k1/cells-k1d.txt` (8 cells), armed 18:48 UTC,
  log `/local/results/queue-k1d.log`; as in K1 it moves `unique-id-service` to node-3 CPU 1 with `hostPID` (post-storage to
  node-1/2) for the uid cells, redeploys bigsvc for the syn cells, recreates `gordion-aggr` and toggles `bpf_stats_enabled`
  per cell. Its generator stop uses bracketed `pkill -f` patterns (K1's `pkill -x wlgen` missed `wlgen-latest`, and an
  unbracketed `pkill -f` also matches the remote shell running it). **Restore pending** (as the 18:12 entry, plus the
  recorders off as after step 0).
- **K4.1 set-up and queue, from 18:46 UTC Nov 13 (user go):** (1) `hr.sh deploy` in namespace `gordion` (all eight HR
  services restarted, run `20261113-184647-k4-primary`): **recommendation on node-4 CPUs 4,6,8,10**, `GOMAXPROCS=4`,
  **`GORDION_GATE=0` on all eight services**, recorder to node-4 `/mnt/extra/gordion`; the other services on node-1/node-2
  as before. Pre-K4 specs saved in `/local/results/k4/pre-k4-gordion-deployments.yaml` (restore target: recommendation on
  node-3 CPUs 1,3, `GOMAXPROCS=2`, `GORDION_GATE=1`). (2) Namespace `k4` with deployment `hc-secondary-burner`
  (`python:3.11-slim`, replicas 0 or 1, own core 12). (3) node-4: helpers in `/var/tmp/k4/bin` (`/var/tmp/k4` made
  `chmod 777` at 18:59 after the first segment's sampler could not write its log); HC driven as the actuator does:
  `hc-listener` start/stop (its ExecStartPre loads `qidlecpu`, Monitor on CPU 26) and `rmmod qidlecpu` for the standalone
  twins, `/proc/idlecpu/cpulist` written (never read) to 4,6,8,10, Primary/Secondary/Reset messages on 10101, the Balancer
  as transient unit `hc-balancer-dyn` on CPU 24 with logs in `/mnt/extra/logs/hc/k4-*`, the Secondary pod's cpusets set to
  12 and widened to 4,6,8,10,12 while HC is on. Two driver tests at 18:54 and 18:57 UTC (the first left its pod's
  cpusets at 4,6,8,10,12 because an empty original cannot be written back; the pod was deleted right after, and the helper
  now restores an empty original from the parent's effective set). `hrgen` runs on node-4 CPUs 13–23 odd. Queue
  `~/k4/queue-k4.sh`, armed 18:58 UTC, log `/local/results/k4/queue-k4.log`; it ends with the burner at 0 replicas and HC
  loaded as found (Listener up, cpulist 4,6,8,10, Balancer inactive). The Primary stays on node-4 for K4.2. **Restore
  pending** after K4: recommendation back to node-3, gate back on, namespace `k4` deleted, `/var/tmp/k4` removed.
- **Both queues finished; reductions, Nov 13–14:** K4.1 ended 19:46 UTC (`DONE-K4.1`; burner at 0 replicas, HC loaded as
  found, Balancer inactive) and the K1 addendum 21:25 UTC (`DONE-K1D`, `leftover=0`). Checked Nov 14 06:09 UTC: no
  generator, probe, aggressor or sampler runs on node-0..4, and nothing writes on node-3. The eight HR services in
  `gordion` (K4 run) keep appending their recorder files while idle (node-1/2 `/var/tmp/gordion/20261113-184647-k4-primary-*`,
  node-4 `/mnt/extra/gordion/...-recommendation.jsonl`, about 300 B/s each); they stay on for K4.2. Reductions (files only,
  no configuration change): node-0 `~/k4reduce.py`, `~/k4-small.tgz`, `~/k1d-small.tgz`, `/local/results/k4/{windows,
  recorder_reduced,balancer_reduced,knee_recorder,sub_recorder}.json` and `node4-clocks.txt`, `lat_reduced.json` in each
  `/local/results/k1/20261113-*-k1-d*` run; node-4 `/var/tmp/k4/{k4reduce.py,windows.json,kneewin.json,subwin.json,
  *_recorder.json}` (removed with `/var/tmp/k4` at the K4 restore). Read-only: the QoS class of the two pods and
  `cpu.weight` of `kubepods-besteffort.slice` (1) and `kubepods-burstable.slice` (7) on node-4. node-3 is still in the
  K1-addendum state (packages, build, unique-id on node-3 CPU 1); its restore is the pending one above.
- **K1-addendum restore, 06:49–06:51 UTC Nov 14 (user go):** (0) node-0 keeps the lean-probe build
  (`/local/results/k1/k1lean-build/k1lean-build.tgz`: `src/` with `k1lean`, `k1lean.bpf.o`, `k1empty.bpf.o` and sources,
  `bpftool`, the dpkg lists) and a working copy in `~/k4b/` for K4.1b. (1) `unique-id-service` back to **node-1, unpinned
  (`UniqueIdService`), `hostPID: false`, `GORDION_DISABLE=1`**; `post-storage-service` back to **node-3** (its spec kept
  `taskset -c 1`, `hostPID: true` and `GORDION_DISABLE=1` while the uid cells had it on node-2); `default/bigsvc`
  `GORDION_DISABLE=1` (node-3 CPUs 1,3,5,7, as after step 0). compose-post, home-timeline, text and user-timeline were not
  restarted by the addendum and are unchanged (node-1/node-2, recorders off). (2) node-3: the four `*-ugen.jsonl` in
  `/var/tmp/gordion/k1` matched node-0's copies byte for byte; `apt-get purge` of the 27 packages after an `apt-get -s`
  simulation showed exactly those 27 (0 left, bpftrace v0.14.0 intact); removed `/var/tmp/k1build`, `/var/tmp/gordion/k1`
  and the empty `/sys/fs/cgroup/gordion-aggr`. After: `/var/tmp/gordion` = `cpustat.sh`, `freq-record.sh`, `probe/`
  (64 KB); `kernel.bpf_stats_enabled` 0 on node-1/2/3; no process holds a K1 file or a deleted recorder file; cpufreq
  `performance` 2 000 000 kHz min = max as found. **node-3 is restored.**
- **K4 recorders stopped and archived, 06:52–06:55 UTC Nov 14 (user):** `GORDION_DISABLE=1` on the eight HR services in
  `gordion` (run `20261113-184647-k4-primary`; pods restarted in place, recommendation kept its `taskset -c 4,6,8,10` pin);
  their ten files (node-1: frontend, frontend-client, geo, profile, reservation; node-2: rate, search, search-client,
  user; node-4: recommendation, 1.96 GB) copied gzipped to node-0 `/local/results/k4/archive-nodes/<node>/` (567 MB),
  size-checked, then removed from the nodes. After: no process holds a file of that run.
- **K4.1b set-up and queue, from 07:00 UTC Nov 14 (user go):** node-4 `/var/tmp/k4/bin` gained `k4probe.sh`, `k1lean` and
  `k1lean.bpf.o` (the lean probe; loader reader pinned to CPU 53) and updated `k4node.sh` (`hc-on-nosec`: Primary message
  and Balancer, no Secondary) and `k4sample.py` (pod CPU weights, Primary threads' migrations and switches, probe run
  time from fdinfo). Deployment `k4/hc-secondary-burner` re-applied from `k4b-secondary.yaml`: **BestEffort** (no CPU
  request), so both pods sit in `kubepods-besteffort.slice` with `cpu.weight` 1 (pod and container scopes alike).
  Functional tests 07:00–07:01 with the idle Primary toggled `kernel.bpf_stats_enabled` 1 → 0 and HC's Balancer on
  and off (HC left loaded as found). Finding: with no rebalance event the Balancer dies of SIGFPE on exit
  (`avgServiceTime = total / eventCount` in `writeLog`), losing its logs; it cannot happen under load. Queue
  `~/k4/queue-k4b.sh`, armed 07:02 UTC, log `/local/results/k4b/queue-k4b.log`: `hr.sh deploy` with recorders on (run
  `20261114-070229-k4b-primary`, K4.1's placement), `kernel.bpf_stats_enabled=1` on node-4 for its duration, a 60 s
  pre-warm, then 12 runs; the Balancer's big logs stay on node-4 `/mnt/extra/logs/hc/<run>`. It ends with the recorders
  off again, the burner at 0, HC loaded as found and `bpf_stats` 0. **Restore pending** after K4 (as the K4.1 entry, plus
  `/var/tmp/k4/bin` probe files).
- **K4.1b ran 07:02–08:17 UTC Nov 14** (12 runs, `DONE-K4.1b`). End state as designed: the burner at 0, HC loaded as
  found (Listener up, cpulist 4,6,8,10, Balancer inactive), `kernel.bpf_stats_enabled` 0, no probe, and the eight HR
  recorders off again (`GORDION_DISABLE=1`, pods restarted in place).
- **Unattended chain, armed 08:06 UTC Nov 14 (user: "chain after K4.1b, unattended; stop only if the cluster breaks"):**
  node-0 `~/k6/chain.sh` (detached, log `/local/results/chain.log`) does the following, in order:
  1. reduces K4.1b (`k4x-reduce.sh`; the Primary's recorder and the Balancer logs on node-4) and archives the K4.1b
     recorder files gzipped to `/local/results/k4b/archive-nodes/` (`archive-run.sh`, size-checked, removed from the
     nodes);
  2. snapshots eno2's 57 IRQ affinities on node-4 (`/local/results/k42-irq-before.txt`);
  3. runs the K4.2 functional test (`k42-test.sh`); it stops if any IRQ affinity differs from the snapshot afterwards;
  4. arms the K4.2 queue (`~/k4/queue-k42.sh`, results `/local/results/k42`);
  5. reduces and archives K4.2 the same way, and checks the end state against the snapshot.

  K4.2's changes on node-4:
  - `/var/tmp/k4/bin2/` holds the driver with the IRQ path, the helpers, and three Balancer builds made on node-0 from
    node-4's tree: `obs` (`-DOBSERVE60`, `k4/hc-patch/observe60.py`), `irq` (`-DBALANCE_INTERRUPTS` with the one-line
    compile fix `k4/hc-patch/irqfix.py`) and `obsirq` (both).
  - The unpatched build is byte-identical to the shipped `TestFramework/bin/balancer` (sha256 `d852c60c…`), which is not
    modified.
  - Per run, the Balancer runs as `hc-balancer-dyn` with `CPUAffinity=26`, the Monitor's CPU.
  - For steps c and d, eno2's IRQs are pointed at CPUs 12 and 14 through `/proc/idlecpu/irqlist`, `irqaffinity` and
    `irqcontrol` (write only). Every IRQ's `smp_affinity_list` is saved first and restored at `hc-off`.
  - `kernel.bpf_stats_enabled` is 1 for the queue.
  - The HR services are redeployed with recorders on (new run) and switched off at the end.

  **Restore at the end of K4.2** (the queue and chain): the burner at 0; HC loaded with the shipped binary and placement
  (the next Balancer start uses CPU 24 by default); IRQ affinities as in the snapshot; `bpf_stats` 0; recorders off.
  **Still pending after K4:** recommendation back to node-3, the gate back on, namespace `k4` and `/var/tmp/k4`
  (including `bin2`) removed.
- **Chain finished, 09:44 UTC Nov 14 (`DONE-CHAIN`).** It ran in order:
  - K4.1b reduced, and its 10 recorder files archived to `/local/results/k4b/archive-nodes/` (0 mismatches);
  - the eno2 IRQ snapshot (57 IRQs, all `1,3,…,55`);
  - the K4.2 test (08:23–08:25): the IRQ path set and restored, all 57 checked;
  - K4.2, 08:25–09:38: 10 runs, 0 failures;
  - K4.2 reduced, and its 10 recorder files archived to `/local/results/k42/archive-nodes/`.

  **End state, checked by the chain:**
  - the eno2 IRQ affinities exactly as in the snapshot;
  - HC loaded as found: module, `hc-listener` active, `hc-balancer-dyn` and `hc-balancer` inactive, Monitor on CPU 26;
  - the shipped `TestFramework/bin/balancer` unchanged (sha256 `d852c60c…`);
  - `kernel.bpf_stats_enabled` 0; no probe or sampler running; the burner at 0 replicas;
  - all eight HR recorders off.

  K4.2 left `BAL_CPU` 26 only per run; the driver's default is 24 again. Node-0 keeps `~/k6/`, `~/k4/`, `~/k4b/` (the
  lean-probe build and the HC variant sources and binaries) and `/local/results/{k4b,k42}`. Node-4 keeps
  `/var/tmp/k4/{bin,bin2}` and the Balancer logs in `/mnt/extra/logs/hc/2026111{3,4}-*`. These are removed at the final
  K4 restore, which is still pending.
- **K4.2e (reserve sweep), armed 02:25 UTC Nov 15 (user go):** node-0 `~/k6/chain2.sh`, detached, log
  `/local/results/chain2.log`. It runs `~/k4/queue-k42e.sh` (results `/local/results/k42e`), then reduces and archives
  as the first chain did, and checks the end state against an eno2 IRQ snapshot.
  - **The runs.** Shipped placement (Monitor CPU 26, Balancer CPU 24, shipped binary), the burner BestEffort, 5 000
    req/s, HC + burner only, 30 s warm-up + 300 s. Order: S0 | reserve 2 | reserve 3 | reserve 1 | S1.
  - **What changes, per HC + burner run only.** HC's reserve: the Primary message's `targetIdleCores` and
    `static_targetIdleCores`, the Balancer's argv target, and `LOW_TIC` in the message and the Balancer's environment,
    all set to 2, 3 or 1 through `k4node.sh`'s `TIC` and `LOW_TIC`. These are runtime values: nothing is rebuilt, and
    the shipped binary is untouched.
  - **The restore is the driver's default.** `TIC` and `LOW_TIC` revert to 1 on the next start, as our actuator's
    `DefaultTargetIdle` and `DefaultLowTIC`; the actuator's configuration is not changed.
  - **As in K4.1b and K4.2:** the HR services redeployed with recorders on (a new run) and switched off at the end;
    `kernel.bpf_stats_enabled` 1 for the queue and 0 at the end; the lean probe in every run.
  - **The K4 restore stays in place;** K4.5 follows.
- **K4.2e finished, 03:01 UTC Nov 15 (`DONE-CHAIN2`).** It ran 5 runs, 0 failures, reduced them, and archived its 10
  recorder files to `/local/results/k42e/archive-nodes/`.

  **End state, checked by the chain:**
  - the eno2 IRQs exactly as in the snapshot;
  - HC loaded as found: module, `hc-listener` active, `hc-balancer-dyn` and `hc-balancer` inactive, Monitor on CPU 26;
  - the shipped Balancer unchanged (sha256 `d852c60c…`), with the reserve back to the driver's default of 1 at the next
    start;
  - `bpf_stats` 0; no probe or sampler; the burner at 0;
  - all eight HR recorders off.

  The K4 restore (recommendation back to node-3, namespace `k4`, node-4 `/var/tmp/k4`, the Balancer logs) is left in
  place for K4.5.
- **Read-only check for RESULTS-K §4.2e, Nov 15 after K4.2e (no change):** on node-4, the `hc-balancer-dyn` journal
  since Nov 13 18:30 UTC (each Balancer start's argv and its print of the Primary message) and the md5 of the two
  driver copies in `/var/tmp/k4/bin` and `bin2`; on node-0, the md5 of `~/k4/k4node.sh`. Nothing was written, and
  no HC procfs file was read.
- **K4.2f, HC's own reserve controller (attended; user go Nov 15):** node-0 `~/k4/queue-k42f.sh` and `~/k6/chain3.sh`,
  one phase per call (log `/local/results/chain3.log`, results `/local/results/k42f`).
  - **Start phase:** an eno2 IRQ snapshot (`/local/results/k42f-irq-before.txt`); `hr.sh deploy` with the recorders on
    (a new run, K4's placement: recommendation on node-4 CPUs 4,6,8,10, `GOMAXPROCS` 4, gate off);
    `kernel.bpf_stats_enabled=1` on node-4; a 60 s pre-warm.
  - **Runs:** S0, the twin (HC unloaded, burner 0); then HC + burner with the cap at 3 and the floor at 1. That is
    `k4node.sh` with `TIC=3 LOW_TIC=1`: the Primary message's `targetIdleCores` = `static_targetIdleCores` = 3,
    `LOW_TIC` = 1 in the message and the Balancer's environment, and argv target 3. Only if the target settles at 3,
    one more run with `TIC=4`. Shipped placement and binary, burner BestEffort, 5 000 req/s, 30 s + 300 s, the lean
    probe in every run. These are run-time values: nothing is rebuilt.
  - **Restore (end phase):** the burner at 0; HC loaded as found, with `TIC` and `LOW_TIC` back to the driver's default
    of 1 at the next start and the shipped binary untouched; `bpf_stats` 0; the recorders off, their files archived to
    `/local/results/k42f/archive-nodes/`; the IRQs compared with the snapshot (K4.2f does not write them). The K4
    restore stays pending for K4.5.
- **K4.2f finished, 05:09 UTC Nov 15 (`DONE-CHAIN3-end`).** Start phase 04:44–04:58 (S0, cap 3); cap 4 05:01–05:07,
  run on the user's decision after cap 3 (the target cycled rather than settling at 3); end phase 05:07–05:09. Three
  runs, 0 failures, 0 client errors. Reduced; the run's 10 recorder files archived to `/local/results/k42f/archive-nodes/`
  (0 mismatches). **End state, checked by the chain:**
  - eno2's IRQs exactly as in the snapshot;
  - HC loaded as found: module, `hc-listener` active, `hc-balancer-dyn` and `hc-balancer` inactive, Monitor on CPU 26;
    the shipped Balancer unchanged (sha256 `d852c60c…`), with `TIC` and `LOW_TIC` back to the driver's default of 1 at
    the next start;
  - `bpf_stats` 0; no probe or sampler; the burner at 0;
  - all eight HR recorders off.

  Node-0 keeps `~/k4/queue-k42f.sh`, `~/k6/chain3.sh`, `/local/results/k42f` and `~/k42f-small.tgz`. The K4 restore
  (recommendation back to node-3, namespace `k4`, node-4 `/var/tmp/k4`, the Balancer logs) is still pending for K4.5.
- **K4.5, Gordion's scorer on HC's interference (attended; user go Nov 15):** node-0 `~/k4/queue-k45.sh` and
  `~/k6/chain4.sh`, one phase per call (log `/local/results/chain4.log`, results `/local/results/k45`).
  - **Start phase:** an eno2 IRQ snapshot (`/local/results/k45-irq-before.txt`); `hr.sh deploy` with the recorders on
    (a new run, K4's placement: recommendation on node-4 CPUs 4,6,8,10, `GOMAXPROCS` 4, gate off);
    `kernel.bpf_stats_enabled=1` on node-4; a 60 s pre-warm; then D, the dictionary sweep, standalone (HC unloaded,
    burner 0), 1 000 to 12 000 req/s in 45 s steps.
  - **arms5 phase:** a 60 s pre-warm at 2 000 req/s, added after the sweep: its 12 000 req/s step overloaded the chain
    (7 340 req/s served, 977 client errors), so S0 starts on a drained chain. Then S0, the twin (standalone, 5 000
    req/s); then HC + burner at reserves 2, 3 and 1 (`k4node.sh` with `TIC = LOW_TIC` = the reserve), 5 000 req/s.
  - **arms7 phase:** T7, the twin at 7 000 req/s; HC + burner at reserve 1, 7 000 req/s; the free controller
    (`TIC=3 LOW_TIC=1`: start and cap 3, floor 1), 7 000 req/s; S1, the twin again at 5 000 req/s.
  - **Every run:** shipped placement (Monitor CPU 26, Balancer CPU 24) and binary, burner BestEffort on core 12,
    30 s warm-up + 300 s, the lean probe and the sampler. These are run-time values: nothing is rebuilt.
  - **Between phases, read-only on node-4:** `~/k6/k45-early.sh` copies the live recorder file to node-0 and cuts the
    runs so far into `/local/results/k45/early/`.
  - **Restore (end phase):** the burner at 0; HC loaded as found, with `TIC` and `LOW_TIC` back to the driver's default
    of 1 at the next start and the shipped binary untouched; `bpf_stats` 0; the recorders off, their files archived to
    `/local/results/k45/archive-nodes/`, the Primary's file split per run on node-0 (`~/k6/k45-pack.sh`); the IRQs
    compared with the snapshot (K4.5 does not write them). The stall run, if armed, and the final K4 restore get their
    own entries.
- **K4.5 stall run, RESULTS-K 4.1b's deciding run (user's item 2), chained unattended with the end phase (user's
  decision 07:25 UTC, so the laptop can close):** node-0 `~/k6/k45-rest.sh`, detached. It waits for arms7's
  `DONE-CHAIN4-arms7`, then runs `chain4.sh stall` (`queue-k45.sh stall`: one run, `stall7`) and `chain4.sh end`.
  The run: reserve 1 (`TIC=1 LOW_TIC=1`), HC + burner, 7 000 req/s, 30 s warm-up + 300 s, the shipped placement
  (Monitor CPU 26, Balancer CPU 24), the lean probe and the sampler.
  - **The one change:** the Balancer binary is K4.2's `/var/tmp/k4/bin2/obs/balancer` (built with `-DOBSERVE60`, set
    through `k4node.sh`'s `BAL_BIN`). HC therefore lends nothing until 60 s after the Balancer starts.
  - **Restore:** `hc-off` at the end of the run, as after every HC run. The next Balancer start uses the shipped binary
    (the driver's default). The shipped binary is not modified.
- **K4.5 finished, 07:48 UTC Nov 15 (`DONE-CHAIN4-end`, then `k45-rest: DONE`).** Phases:
  - start 06:30–06:38: deploy, pre-warm, D;
  - arms5 06:41–07:06: pre-warm 2, S0, reserves 2, 3 and 1;
  - arms7 07:08–07:33: T7, reserve 1 at 7 000, the free controller, S1;
  - stall 07:33–07:39 and end 07:40–07:48, both unattended.

  Ten runs, 0 failures. Client errors: 977 in the sweep's collapsed 12 000 req/s step, and at most 53 in any warm-up.
  The runs were reduced, the run's 10 recorder files archived to `/local/results/k45/archive-nodes/` (943 MB gzipped,
  0 mismatches), and the Primary's file split per run into `~/k45-data.tgz`. **End state, checked by the chain:**
  - eno2's IRQs exactly as in the snapshot;
  - HC loaded as found: module, `hc-listener` active, `hc-balancer-dyn` and `hc-balancer` inactive, Monitor on CPU 26;
    the shipped Balancer unchanged (sha256 `d852c60c…`);
  - `bpf_stats` 0; no probe or sampler; the burner at 0;
  - all eight HR recorders off.

  Node-0 keeps `~/k4/queue-k45.sh`, the `~/k6/` scripts (`chain4.sh`, `k45-rest.sh`, `k45-arm.sh`, `k45-early.sh`,
  `k45-pack.sh`, `k45split.py`, `k45x-reduce.sh`), `/local/results/k45`, `~/k45-early.tgz` and `~/k45-data.tgz`. The
  final K4 restore is next, as its own entry.
- **Read-only checks during K4.5's analysis and the restore's preparation (nothing written on node-4):**
  - on node-0, the saved pre-K4 deployment specs (`/local/results/k4/pre-k4-gordion-deployments.yaml`);
  - on node-4, the listing of `/mnt/extra/logs/hc` and `/var/tmp/k4`. Its 41 directories with 13-digit names, dated
    Nov 10–12, are the motivation experiment's, not K4's;
  - free7's Balancer directory, the `hc-balancer-dyn` journal for 07:20–07:28 UTC, and two `balancer.log` files read in
    full (free7's and K4.2f cap 3's) to rebuild free7's target.

  On node-0, the snapshot of every namespace's pods and deployments for the restore's step 7 was written to
  `/local/results/k4restore/` (`pods-before-restore.txt`, `deploy-before-restore.txt`).
- **Final K4 restore (RESULTS-K 4.5's seven steps; user's item 3), attended, user go 21:25 UTC Nov 15:** node-0
  `~/k6/k4-restore.sh` (log `/local/results/k4restore/restore.log`), after K4.5's end phase. It never reads an HC procfs
  file.
  1. **HC:** `hc-off` if a Balancer still runs. Check: module loaded, `hc-listener` active, `hc-balancer-dyn` and
     `hc-balancer` inactive, the Monitor on CPU 26, the shipped Balancer's sha256.
  2. **Probe and sampler:** none running; `kernel.bpf_stats_enabled` 0 on node-4.
  3. **Recorders:** `GORDION_DISABLE=1` on the eight services; K4.5's recorder files archived to
     `/local/results/k45/archive-nodes/` and removed from the nodes (the end phase's `archive-run.sh`).
  4. **Burner:** scaled to 0; namespace `k4` deleted.
  5. **HR placement:** `hr.sh deploy` with the pre-K4 parameters: recommendation on node-3 CPUs 1,3 (`taskset`),
     `GOMAXPROCS` 2, `GORDION_GATE=1` on all eight services, the others on node-1 and node-2 as before. The recorders
     stay off: the pre-K4 specs had them on (`GORDION_DISABLE=0`, still writing Phase G iii's files), and RESULTS-K
     4.5's step 3 turns them off. Check: every service's spec against `/local/results/k4/pre-k4-gordion-deployments.yaml`
     (`~/k6/k4restore_check.py`), pods Running, a recommend request through the frontend returns 200.
  6. **node-4 files:** `/var/tmp/k4` removed (bin, bin2 with the patched Balancers, the lean probe, the sampler and
     reducer files); the K4 Balancer log directories under `/mnt/extra/logs/hc/` removed (K4.1 to K4.5 runs, plus
     `k4-test` and `k4-test2`). Kept there: the 41 directories with 13-digit names from Nov 10-12, which are the
     motivation experiment's, and HC's `listener.log`.
  7. **Final checks:** eno2's IRQs against the K4.2e snapshot; HC as found; the pods on node-4; the namespaces
     `default`, `sn`, `aggressor-sn`, `aggressor-ss` and `kube-system` unchanged against a snapshot taken right before
     the restore (`/local/results/k4restore/pods-before-restore.txt`, `deploy-before-restore.txt`, 21:25 UTC). The
     06:25 snapshot is kept beside it as `*-0625.txt`, and the two are identical for those namespaces.
- **Final K4 restore finished, 21:27 UTC Nov 15 (`DONE-K4-RESTORE`).** All seven checks passed:
  1. HC: module loaded, `hc-listener` active, `hc-balancer-dyn` and `hc-balancer` inactive, Monitor on CPU 26, the
     shipped Balancer unchanged (sha256 `d852c60c…`).
  2. No probe or sampler; `bpf_stats` 0.
  3. `GORDION_DISABLE=1` on the eight services; K4.5's 10 recorder files archived, 0 mismatches, none left on node-1,
     node-2 or node-4.
  4. Namespace `k4` deleted.
  5. `hr.sh deploy 20261115-212624-k4-restore` (rc 0):
     - 19 of 19 `gordion` deployments as before K4, except the recorder switch (pre-K4 `GORDION_DISABLE=0`, now 1);
     - every pod Running; recommendation on node-3 (3 threads on CPU 1, 3 on CPU 3);
     - a recommend request through the frontend returned 200.
  6. `/var/tmp/k4` removed from node-4, and the 31 K4 Balancer log directories removed from `/mnt/extra/logs/hc/` (0
     left). The 41 motivation directories and `listener.log` are kept; `/mnt/extra` has 657 GB free.
  7. eno2's 57 IRQs as in the K4.2e snapshot; node-4 runs only `calico-node`, `kube-proxy` and `nginx-proxy`; the
     namespaces `default`, `sn`, `aggressor-sn`, `aggressor-ss` and `kube-system` unchanged.

  Left in place:
  - on node-4, the empty `/mnt/extra/gordion` (created by K4.1's deploy, Nov 13 18:46; not one of the plan's steps);
  - on node-0, the K4 scripts (`~/k4`, `~/k6`), `/local/results/k4*` with the archives, and the packs in `~`.
- **Node-4 cleanup after the K4 restore (user's request, Nov 15):** the empty `/mnt/extra/gordion` removed with `rmdir`,
  which removes only an empty directory. K4.1's deploy created it on Nov 13 at 18:46 as the recommendation recorder's
  hostPath on node-4. Restore: none needed; `hr.sh deploy` recreates it (`mkdir -p`) the next time a service is placed
  on node-4. Nothing else on the cluster changes.
- **Phase K5/K6 build on node-0 (Nov 16, from 04:30 UTC; RESULTS-K 5 item A):** nothing on node-1 to node-4.
  - apt `clang-14` and `llvm-14` on node-0 (package lists before and after in `/local/k5build/dpkg-*.txt`). libbpf
    1.4.7 is built static in `/local/k5build`, and the probe `k5probe.bpf.o` and the agent `k5agent` are built there.
  - The sources are in `~/k5`. The node-0 trees get the K5 recorders: `/local/hr/hotelReservation/gordion/recorder.go`
    and `/local/sn/gordion/src/gordion/Gordion.h`, with the originals kept beside them as `*.pre-k5`.
  - New registry tags `hr-gordion:gordion8` and `sn-gordion:g7`, built from those trees. `hr.sh build` also rebuilds
    `hrgen`. The existing tags are untouched.
  - Restore, at the end of K6: purge the packages in `dpkg-added.txt`, remove `/local/k5build` and `~/k5`, and put the
    `*.pre-k5` sources back. The new tags stay in the registry; nothing deploys them after the restore.
- **Phase K5/K6 queue (Nov 16, from ~04:30 UTC, unattended; RESULTS-K 5 and 6; node-0 `~/k5/queue-k5.sh all`, log
  `/local/results/k5/queue-k5.log`, STOP file `/local/results/k5/STOP`):** every change below is undone by the queue's
  own `restore` phase, which also runs when a STOP file skips the rest.
  - Saved first: `kubectl get deploy -o json` of namespaces `gordion` and `sn` and of `default/bigsvc`
    (`/local/results/k5/pre-k5-*.json`), and the pod listing of every namespace. The restore re-applies them.
  - **node-3 (the victim node):** `unique-id-service` redeployed with image `sn-gordion:g7` on CPU 1 (`taskset`,
    `hostPID`), `recommendation` with `hr-gordion:gordion8` on CPUs 5,7 (`GOMAXPROCS` 2, gate on); both recorders on,
    starting with `GORDION_INTERCEPT=off GORDION_READS=none` and a control file (`GORDION_CTL=/data/k5-ctl-*`) that the
    cell controller writes per arm. `hr.sh deploy` re-deploys the other seven HR services on node-1/node-2 with the new
    run name as usual. `bigsvc` scaled to 0 (it is deployed again by K6 C3/C4 and scaled to 0 after).
    `/var/tmp/k5` holds the agent, the probe object, the controller, the scorer and `ugen`; `/sys/fs/resctrl` mounted
    if it was not (recorded, unmounted at the restore); cgroup `gordion-aggr` for the neighbors (stress-ng, killed at
    the end of every cell); `kernel.bpf_stats_enabled` 1 during a cell, back to the value found after it. Loads on CPUs
    17-27 odd (`ugen`, `hrgen`), agent and scorer on CPU 53.
  - **Namespace `k5`:** the A5 network neighbor, two python pods on node-3 CPUs 9 and 11, a UDP stream between them;
    RPS of their calico veths set to the victim's CPUs (`/sys/class/net/cali*/queues/rx-0/rps_cpus`), which disappears
    with the pods. Deleted after every use and at the restore.
  - **node-4 (K6 C5 only, ~10 min):** `recommendation` moved to node-4 CPUs 4,6,8,10 (`gordion8`, gate off), namespace
    `k4` with the K4.1b burner, `/var/tmp/k4/bin` (k4node.sh, k4sample.py) and `/var/tmp/k5` (agent) copied in; one
    standalone run with HC unloaded, one run with HC loaded, the shipped Balancer at reserve 1 and the burner; then
    `hc-off`, HC loaded as found, `k4` deleted, both directories removed, the Balancer logs of the two runs removed
    from `/mnt/extra/logs/hc`, the recorder file archived and `/mnt/extra/gordion` removed again. No HC procfs file is
    read. The motivation namespaces are not touched.
  - **Restore (the queue's last phase):** loads and neighbors stopped, agent/scorer/stress-ng killed, `gordion-aggr`
    removed, `bpf_stats` 0, recorder files archived to `/local/results/k5/recorders/` and removed from the nodes,
    `/var/tmp/k5` removed, resctrl unmounted where the queue mounted it, namespaces `k5` and `k4` deleted, the saved
    deployments re-applied (unique-id back to node-1 unpinned, recommendation to node-3 CPUs 1,3 `gordion7` with
    `GORDION_DISABLE=1`, bigsvc back as found), pods compared with the listing taken before; on node-0 the clang/llvm
    packages purged, `/local/k5build` removed (agent and object kept in `/local/results/k5/k5build.tgz`), the two
    recorder sources restored from `*.pre-k5`.
  - **A5 calibration re-run after the restore (chained, `~/k5/a5post.sh`, log `/local/results/k5/a5post.log`):** the
    queue's own A5 step failed (the pods lacked the `dedicated=special` toleration node-3 requires; fixed in
    `k5net.sh` before the first run that uses the neighbor). The re-run waits for `DONE-K5-K6`, creates namespace
    `k5` with the two pods on node-3 CPUs 9/11 for about three minutes (RPS to CPUs 1,5,7, idle after the restore),
    records `/proc/softirqs` per CPU at 20 000, 80 000 and 50 000 packets/s, and deletes the namespace again.
  - **K5/K6 outcome (Nov 16 11:03 UTC) and the leftovers it found:** `DONE-K5-K6` at 11:03, `DONE-A5POST` at 11:07.
    The restore ran (pods 119/119, only age differences; clang purged; `/var/tmp/k5` gone; resctrl unmounted). Two
    queue defects: (1) C5 redeployed recommendation to node-4 and never moved it back, so the eight recommendation
    rung-4 cells and the `r0c-rec` twin ran with no victim on node-3 (0 arm records); (2) C5 copied `k4node.sh` and
    `k4sample.py` but not `k4send.py`, so `hc-on` failed and the "reserve 1" segment ran with the Secondary pinned
    but no Balancer. One restore gap: `kubectl apply` of the saved deployments kept the three `GORDION_INTERCEPT`,
    `GORDION_READS`, `GORDION_CTL` env entries that the prep had added (harmless: recommendation runs
    `GORDION_DISABLE=1`, unique-id runs `g6` without the switches).
  - **K5/K6 redo, first attempt (Nov 16 23:42-23:47 UTC), stopped by hand:** during its first cell it showed that the
    recommendation load generator (`hrgen`) keeps 1 MB of output in memory and has no SIGINT handler, so every
    recommendation cell of the first run (rungs 0-4) has an empty client file. Stopped: queue process, controller,
    agent, `hrgen` and stress-ng killed on node-3; resctrl monitor groups empty afterwards. Recommendation stayed on
    node-3 CPUs 5,7 `gordion8` for the second attempt.
  - **K5/K6 redo, second attempt (started Nov 17 ~00:00 UTC, detached, `queue-k5.sh redo`, log
    `/local/results/k5/redo.log`; the first run's script kept as `~/k5/queue-k5.sh.run1`):** queue changes: the
    recommendation and synthetic generators get a schedule exactly as long as the cell and the queue waits for them
    to exit (up to 180 s) before any SIGINT; synthetic client files copied from `/local/results/<cell>` (the
    bigsvc scripts' results root) and the synthetic recorder archived to `results/k6/recorders` before the restore
    deletes it; C5 ships `k4send.py`. Cluster changes: `/local/k5build` re-extracted from `k5build.tgz` (agent and
    object only, no compiler); node-3 `/var/tmp/k5`, resctrl mount, `gordion-aggr` as in the prep; recommendation
    redeployed to node-3 CPUs 5,7 `gordion8` gate on with the K5 switches off (new recorder run, the first run's
    name kept in `run-rec-1.txt`). Cells: every recommendation cell of rungs 0-4 with the first run's names and
    order (the net neighbor namespace `k5` created and deleted around its three cells, as before), then K6 C3/C4 on
    bigsvc (deployed on node-3 CPUs 1,3,5,7, scaled to 0 after), then C5 on node-4 as the first time. Then the
    same restore, now also removing the three env entries from recommendation and unique-id and keeping the first
    `k5build.tgz`. Expected end about 04:30 UTC Nov 17, marker `DONE-REDO`. STOP file `/local/results/k5/STOP`
    skips the remaining cells and still restores.
  - **K5/K6 redo outcome (Nov 17 04:02 UTC) and end state:** `DONE-REDO` at 04:02:41, 37 cell-done lines, 0 errors.
    Every recommendation cell has its client file (384-456 per-second records); the C3/C4 synthetic cells have
    their client files and both synthetic recorders are in `results/k6/recorders`; C5 `hc-on` worked this time
    (node-4 units listener/balancer/balancer-dyn `active inactive active` during the reserve-1 segment, `active
    inactive inactive` after). The restore ran as above, env entries removed. Checked at 04:06-04:10: namespaces
    `k5`/`k4` gone; node-0 `/local/k5build` gone, no clang packages, `k5build.tgz` still holds `dpkg-added.txt`;
    node-3 `/var/tmp/k5` and `gordion-aggr` gone, resctrl not mounted, no agent/controller/generator/stress-ng
    process; node-4 HC module loaded, listener active, Balancer inactive, `/var/tmp/k4` gone; 133/133 pods Running,
    the same set as before K5. Pod templates of all 47 saved deployments compared field by field with
    `pre-k5-*.json`: one difference, `sn/unique-id-service` still had `hostPID: true` (the prep's sn.sh deploy set
    it; `kubectl apply` of a spec that omits a field does not remove it, as with the env entries; it was also left
    by the first restore). Fixed at 04:08:48: `kubectl -n sn patch deploy unique-id-service --type=json` removing
    `/spec/template/spec/hostPID`; one rollout, new pod Running on node-1 at 04:08:54; afterwards 0 differences.
    Restore it would take: none (this is the pre-K5 state). The queue's restore now also removes `hostPID` from
    unique-id when the saved spec has none (staged, for any later run). The motivation experiment was not touched.
  - **K6 C5 rerun (Nov 17, `queue-k5.sh c5redo`, log `/local/results/k5/c5redo.log`):** the result check found that
    in both C5 runs (Nov 16 10:05 and Nov 17 03:48) the agent exited at start, "no cgroup at
    /sys/fs/cgroup/gordion-aggr": the cell spec always lists the switch aggressor's cgroup as a holder, and the queue
    creates it only on node-3. Client data, the Balancer logs and the K4 sampler were recorded; the agent columns C5
    is about (φ, R_foreign, CPI, MPKI, CMT, softirq) were not. Queue change: C5 creates an empty `gordion-aggr` on
    node-4 and removes it afterwards; its done line now carries the error count. Cluster changes, the same as the C5
    of the redo: `/local/k5build` re-extracted (agent and object); on node-4 `/var/tmp/k4`, `/var/tmp/k5`, resctrl
    mounted, `bpf_stats` on, the empty `gordion-aggr`; recommendation redeployed to node-4 CPUs 4,6,8,10 `gordion8`;
    namespace `k4` with the burner; segment S0 with HC unloaded, then res1 with HC loaded, the Secondary pinned and the
    Balancer on, then off. Restore: the same queue restore as the redo (recommendation back to node-3 from the saved
    spec, `k4` deleted, node-4 back to HC loaded / listener active / Balancer inactive, `gordion-aggr` and the
    directories removed, resctrl unmounted, `/local/k5build` removed). About 20 minutes; marker `DONE-C5REDO`.
  - **K6 C5 rerun outcome and end state (Nov 17 04:29 UTC):** `DONE-C5REDO` at 04:29:27; segments
    `20261117-041512-k6-c5-twin-S0` and `20261117-042144-k6-c5-res1` each 1 arm record, 0 errors, agent records present
    (Balancer units `active inactive active` during res1). The restore ran as for the redo. Checked at 04:31: all 47
    saved deployments field-by-field equal to `pre-k5-*.json` (0 differences; the hostPID removal of 04:08 held);
    133/133 pods Running; recommendation back on node-3; namespaces `k4`/`k5` gone; node-0 `/local/k5build` gone;
    node-4 HC module loaded, listener active, Balancer inactive, `/var/tmp/k4`, `/var/tmp/k5`, `/mnt/extra/gordion` and
    `gordion-aggr` gone, resctrl not mounted, `bpf_stats` 0, no agent; node-3 `/var/tmp/k5` and `gordion-aggr` gone,
    resctrl not mounted, `bpf_stats` 0. K5/K6 cluster work is finished; nothing is left to restore. The motivation
    experiment was not touched.
  - **K6 C3 at 128 MB (Nov 17, `queue-k5.sh c3big`, log `/local/results/k5/c3big.log`):** one twin (uniform mix) and
    one run (30 % VioMemWalk) with `MEMWALK_MB=128`, 400 req/s, no neighbor, agent with everything on, as C3 at 24 MB.
    Cluster changes: `/local/k5build` re-extracted (agent and object); node-3 `/var/tmp/k5`, resctrl mounted,
    `bpf_stats` on, `gordion-aggr` (empty); `default/bigsvc` redeployed twice on node-3 CPUs 1,3,5,7 `k123g4` with
    `BIGSVC_MEMWALK_MB=128` (each deploy recalibrates; nothing else runs on the socket during calibration), then scaled
    to 0. Restore: the queue restore (bigsvc and the other saved deployments re-applied from `pre-k5-*.json`, node-3
    directories, cgroup, resctrl and `bpf_stats` back, `/local/k5build` removed); then the field-by-field template
    comparison against the snapshot. About 20 minutes; marker `DONE-C3BIG`.
  - **K6 C3 at 128 MB outcome and end state (Nov 17 05:26 UTC):** `DONE-C3BIG` at 05:25:57; cells
    `20261117-051059-k5-c3-syn-uniform128-twin` and `20261117-051909-k5-c3-syn-memwalk30-128mb`, 1 arm record and 0
    errors each, client files (384 / 3 781 records) and both recorders in `results/k6/recorders`. Pod calibrations 75.8
    and 78.4 ns per step at 128 MB. Checked at 05:28: all 47 saved deployments field-by-field equal to `pre-k5-*.json`;
    133/133 pods Running; node-0 `/local/k5build` gone; node-3 `/var/tmp/k5` and `gordion-aggr` gone, resctrl not
    mounted, `bpf_stats` 0, no agent. Nothing left to restore. Raw K5/K6 data archived on node-0 as
    `/local/results/k5k6-raw-20261117.tgz` (83 MB, 1 490 entries; `/local/results/k5` and `k6` left in place) and on
    the laptop outside the repositories (`C:/coding-workspace/archive/k5k6-raw-20261117-laptop.tgz`); not staged.
  - **K5 §5.7, default set B measured (Nov 17, `queue-k5.sh defb`, log `/local/results/k5/defb.log`):** arms
    `none`, `defB` (probe with φ and φ_sibling, no holder split; CMT; interceptor count + slot; window reads;
    sampler; scorer on-node, which sums the victim threads' run_delay from `/proc/<tid>/schedstat`) and `defBt` (the
    same with the interceptor's timers), 5 reps × 30 s each, paired per rep. Cells: unique-id and recommendation ×
    {none, switch, net} at mid, plus unique-id under the net neighbor at 7 000 req/s. Cluster changes, as in the K5
    prep (the `pre-k5-*.json` snapshots and `pods-before-k5.txt` are not retaken): `/local/k5build` re-extracted;
    node-3 `/var/tmp/k5`, resctrl mounted, `gordion-aggr`, `bpf_stats` on; `default/bigsvc` scaled to 0;
    unique-id-service redeployed to node-3 CPU 1 with `sn-gordion:g7`, hostPID and the K5 env switches;
    recommendation redeployed to node-3 CPUs 5,7 `gordion8` gate on with the switches; the switch aggressor in
    `gordion-aggr` and the net-neighbor namespace `k5` per cell. Restore: the queue restore (both services and bigsvc
    back from the snapshots, env switches and hostPID removed, `k5` deleted, node-3 back, `/local/k5build` removed),
    then the field-by-field template comparison. About 90 minutes; marker `DONE-DEFB`.
  - **K5 §5.7 outcome and end state (Nov 17 06:53 UTC):** `DONE-DEFB` at 06:53:19; seven cells, 15 arm records and
    0 errors each; both recorders archived to `results/k5/recorders`. Checked at 06:56: all 47 saved deployments
    field-by-field equal to `pre-k5-*.json` (env switches and hostPID removed again); 133/133 pods Running;
    namespace `k5` gone; node-0 `/local/k5build` gone; node-3 `/var/tmp/k5` and `gordion-aggr` gone, resctrl not
    mounted, `bpf_stats` 0, no agent or stress-ng. Nothing left to restore. Raw cells archived on node-0 as
    `/local/results/k5-setb-raw-20261117.tgz` and on the laptop in `C:/coding-workspace/archive/`; not staged. The
    motivation experiment was not touched.
  - **Recorder profiling on node-0 (Nov 17 07:02-07:20 UTC; no cluster change):** Go benchmarks and a paced test of
    `hotelReservation/gordion` and a C++ harness of `Gordion.h` (with the Thrift stub in `~/k5/stub`), pinned to node-0
    CPUs 40/42, in the scratch directory `~/k5bench` (a copy of the HR module's vendor tree; removed at the end of
    §5.8); `perf record` under sudo with `perf_event_paranoid` left at 2. Nothing on node-1 to node-4.
  - **K5 §5.8, set B′ (Nov 17, `queue-k5.sh defbp`, log `/local/results/k5/defbp.log`):** cluster changes:
    - node-0: apt `clang-14`/`llvm-14` again (lists in `/local/results/k5/build-k5p/`), libbpf 1.4.7 and the agent with
      the new F_RD flag built in `/local/k5build` (`~/k5/build-k5p.sh`); the first agent kept as `k5build-run1.tgz`;
      the new recorder sources (`recorder.go`, `grpc.go`, `Gordion.h`) in the node-0 trees with the originals as
      `*.pre-k5`; new registry tags `hr-gordion:gordion9` and `sn-gordion:g8` (existing tags untouched);
    - node-3: an agent smoke test (3 s, system.slice), then the K5 placement as in §5.7 except the images (`g8`,
      `gordion9`) and recommendation's admission gate off (`GATE=0`); arms `none` and `defBp`, 5 reps × 30 s, unique-id
      and recommendation × {none, switch, net} at mid plus unique-id under net at 7 000 req/s.
    - Restore: the queue restore (snapshots re-applied, env switches and hostPID removed, `k5` deleted, node-3 back,
      `/local/k5build` removed, the clang/llvm packages purged, the three sources put back from `*.pre-k5`), then the
      field-by-field template comparison. About 75 minutes after the build; marker `DONE-DEFBP`; a build or smoke
      failure logs `FAILED`, skips the cells and still restores.
  - **K5 §5.8 outcome and end state (Nov 17 08:18 UTC):** build done 07:26 (24 packages added, agent with F_RD,
    `hr-gordion:gordion9` and `sn-gordion:g8` pushed); agent smoke ok; seven cells 07:27-08:17, 10 arm records and 0
    errors each; `DONE-DEFBP` at 08:18:12. The queue restore ran (24 packages purged, `/local/k5build` removed, the
    three recorder sources back from `*.pre-k5`). Checked on return: all 47 saved deployments field-by-field equal to
    `pre-k5-*.json`; 133/133 pods Running; namespace `k5` gone; node-0 has no clang and the original `recorder.go`;
    node-3 `/var/tmp/k5` and `gordion-aggr` gone, resctrl not mounted, `bpf_stats` 0, no agent or stress-ng. The
    profiling scratch `~/k5bench` on node-0 removed. Raw cells archived on node-0 as
    `/local/results/k5-setbp-raw-20261117.tgz` and on the laptop in `C:/coding-workspace/archive/`; not staged. The new
    registry tags stay; nothing deploys them. The motivation experiment was not touched.

## 18. Ice Lake r650 cluster clnode270 (2026-11-17 on, Phase I; RESULTS-I.md)

Five Dell r650 nodes (2 × Xeon Platinum 8360Y), kernel `6.12.118-harvest`, Kubernetes 1.25.6 (node-0 and node-1
control plane, node-2 to node-4 workers). Every change below is logged with its restore before it is made; the
inventory comes first and changes nothing.

- **Before anything here (Nov 17 21:12-21:17 UTC, on clnode170):** clnode170's node-0 `/local/results` copied whole to
  `/proj/tracing-pythia-PG0/royno7-k-archive-clnode170-2026-11-17/` (`rsync -a`, 5 835 files, 26.5 GB; dry-run check
  0 differing files), recorded in `archives/README.md`. No change to clnode170 otherwise.
- **Read-only inventory (Nov 17 ~21:20-21:40 UTC; RESULTS-I §0):** on all five nodes, from `/tmp/i0inv` (a script and a
  CPUID reader compiled with gcc), removed afterwards on every node. A 4-second spin on CPUs 10 and 12 of node-3 and
  node-4 to read the clock under load. No mount, no module load, no sysfs or MSR write, nothing installed.
  Found as set up by the profile (not by us; left as found): node-4 boots `nosmt=force intel_idle.max_cstate=0
  processor.max_cstate=0`, runs `intel_cpufreq` (passive, `schedutil`) with turbo allowed and the uncore at min = max
  2.4 GHz, and has `/local/HarvestContainers`; node-0 to node-3 have SMT on, no cpufreq driver ("P-states controlled
  by the platform"), C6 enabled, uncore 0.8-2.4 GHz.
- **clnode170 environment export (user, on clnode170, Nov 17 22:17-23:03 UTC):** node-0 `/local/{hr,sn,blueprint,
  dsb_hd_counter,simple-mitigation-1}`, `~royno7` (without `go/`, `results/`) and a `docker save` of `hr-gordion:gordion9`,
  `sn-gordion:g8`, `big_ctr:k123g4` to `/proj/tracing-pythia-PG0/royno7-k-archive-clnode170-2026-11-17/env/` (three
  files, sizes and sha256 in the laptop manifest). Read-only on clnode170.
- **Images into node-0's registry (Nov 17 ~23:10 UTC):** `docker load` of the image archive on node-0, then `docker push`
  of the three tags to `10.10.1.1:5000` (empty before; the nodes already trust `10.10.1.0/24` through `DOCKER_OPTS`, as
  on clnode170). Image IDs before (clnode170) and after (clnode270) are in RESULTS-I §2.0. **Restore:** `docker rmi` the
  three tags on node-0 and every node that pulled them; delete the three repositories from the registry
  (`docker exec local-registry rm -rf /var/lib/registry/docker/registry/v2/repositories/{hr-gordion,sn-gordion,big_ctr}`,
  then `docker exec local-registry registry garbage-collect /etc/docker/registry/config.yml`).
- **Trees unpacked on node-0 (Nov 17 ~23:10 UTC):** `local-trees.tgz` into `/local` (creates `/local/hr`, `sn`, `blueprint`,
  `dsb_hd_counter`, `simple-mitigation-1`; none existed). The home archive's non-hidden entries into `~royno7` (none
  existed; the list is `~/c170home-entries.txt`); its hidden entries (`.ssh`, `.kube`, `.bashrc`, caches) are not
  unpacked, so node-0's own keys and kubeconfig are untouched. **Restore:** `rm -rf` the five `/local` trees and
  `xargs rm -rf < ~/c170home-entries.txt` in `~`.
- **node-3 frequency and idle states (Nov 17 from ~23:20 UTC; user's stop-point-1 decision, in its order):** from
  `/var/tmp/i2` on node-3 (`i2node3.sh` and `msr.py`, copies in the session scratchpad `i2/`), run as root; originals
  saved first in `/var/tmp/i2/orig/` (`uncore.txt`: both packages min 800 000, max 2 400 000 kHz; `cpuidle.txt`: every
  state's `disable` on CPUs 0-143, all 0; `misc_enable.txt`: `IA32_MISC_ENABLE` per CPU; `msr-loaded`: 0).
  1. uncore min = max = 2 400 000 kHz on both packages (`intel_uncore_frequency` sysfs). **Restore:**
     `i2node3.sh uncore-restore`.
  2. `modprobe msr`; turbo off by setting bit 38 of MSR 0x1A0 on CPUs 0-143; checked with a spin. **Restore:**
     `i2node3.sh turbo-restore` (writes back the saved values), then `i2node3.sh msr-unload` (only if it was not loaded).
  3. only if 2 does not hold: `HWP_REQUEST` (0x774) min = max = base on CPUs 0-143, original saved to
     `orig/hwp_request.txt`. **Restore:** `i2node3.sh hwp-restore`.
  4. if neither holds: nothing more; the clock is recorded per window.
  5. idle states: `disable` = 1 for every state other than POLL, C1, C1E on CPUs 0-143 (C6 is the only one).
     **Restore:** `i2node3.sh cstate-restore`.
  Clock checks are short busy loops (`taskset` + shell loop, 3-5 s) on idle CPUs, with the APERF/MPERF reading.
  **Done Nov 17 23:12-23:15 UTC; state now:** uncore min = max 2 400 000 kHz on both packages (reads 2.2 GHz); `msr`
  module loaded (was not); MSR 0x1A0 back to its saved value 0x850089 on all CPUs (bit 38 was ignored, restored before
  step 3); **`HWP_REQUEST` = 0x55001818 on CPUs 0-143** (was 0x5500ff01; holds 2.4 GHz busy); C6 `disable` = 1 on CPUs
  0-143. Full node-3 restore, in this order: `cd /var/tmp/i2; sudo bash i2node3.sh hwp-restore; sudo bash i2node3.sh
  cstate-restore; sudo bash i2node3.sh uncore-restore; sudo bash i2node3.sh msr-unload`, then `rm -rf /var/tmp/i2`.
  `HWP_REQUEST` is read back before every queue (`sudo python3 /var/tmp/i2/msr.py read 0x774 0-143`).
- **node-0 toolchain (Nov 17 from ~23:20 UTC):** apt packages that are missing among `clang-14 llvm-14 libelf-dev
  zlib1g-dev python3-numpy protobuf-compiler` (package lists before and after in `/local/k5build/dpkg-{before,after}.txt`,
  the difference in `dpkg-added.txt`); libbpf 1.4.7 static into `/local/k5build/root`; the probe and agent built in
  `/local/k5build` from the laptop's staged `k5/probe/` (bitmaps), copied over `~/k5/probe/` (the clnode170 versions
  kept as `~/k5/probe/*.c170`). Go 1.25.3 into `/usr/local/go1.25` (as clnode170's `setup170.sh`), `protoc-gen-go`
  v1.36.6 and `protoc-gen-go-grpc` v1.5.1 into `~/go/bin`. **Restore:** `sudo apt-get purge -y $(cat
  /local/k5build/dpkg-added.txt)`, `sudo rm -rf /usr/local/go1.25 /local/k5build ~/go`, put back `~/k5/probe/*.c170`.
  No image is rebuilt from this (the three loaded images are the measured ones).
- **node-3 `stress-ng` (Nov 17, with the toolchain):** `apt-get install stress-ng` (added packages in
  `/var/tmp/i2/dpkg-added-stress.txt` on node-3). **Restore:** purge them.
- **Kubernetes layout as on clnode170 (Nov 17 from ~23:25 UTC; step 2 bring-up):** (a) `/local/results/{hr,sn,build,i2}`
  on node-0 and the twelve `mix-*.json` in `/local/results` on node-0 and node-3 (as clnode170's `mixfix170.sh`);
  (b) taint `dedicated=special:NoSchedule` on node-3 and node-4 (clnode170's, read before it expired; our deployments
  tolerate the taint); node-4 already had `podtype=harvest` from the profile (`kubectl label` found it), so the label is
  not ours and stays at restore; (c) HotelReservation in namespace `gordion`
  (`NS=gordion DB_NODE=node-1 OTHER_NODES=node-1,node-2 hr.sh install` then `hr.sh place`: datastores, consul and
  jaeger on node-1, the services on node-1/node-2); (d) SocialNetwork in namespace `sn` (`NS=sn TAG=g8 DB_NODE=node-1
  OTHER_NODES=node-1,node-2 sn.clean.sh install`: helm release `sn`, every C++ service `sn-gordion:g8`, datastores and
  nginx on node-1; the install skipped the graph because `/local/sn/.graph-loaded` came with clnode170's tree, so the
  marker was moved to `/local/results/build/` and `sn.clean.sh graph` run at 23:33 UTC: socfb-Reed98, 9 424 calls succeeded; `aiohttp` pip-installed `--user` on node-0 by the script). Stock
  images come from Docker Hub; their digests are compared with clnode170's list. **Restore:** `helm uninstall -n sn sn;
  kubectl delete ns sn gordion`; `kubectl taint nodes node-3 node-4 dedicated-`;
  `rm -rf /local/results` (after the data is copied off) and `/local/results` on node-3; `pip3 uninstall -y aiohttp`
  and its dependencies (`~/.local`, listed by `pip3 list --user` before and after in `/local/results/build/`).
- **bigsvc image with the fixed step count (Nov 17 from ~23:25 UTC):** the laptop's staged `workflow/bigsvc/kernels.go`,
  `scripts/exp.sh`, `analysis/dictionary.py` and `analysis/b4_compare.py` copied into `/local/blueprint/examples/bigsvc`
  (clnode170's versions kept beside them as `*.c170`); `exp.sh build k123i1` (Go 1.25.3; modules fetched into `~/go`):
  `10.10.1.1:5000/big_ctr:k123i1` and `wiring/wlgen-k123i1`. **k123i1 is not used:** node-0's
  `runtime/plugins/gordion/recorder.go` was clnode170's k123g5 state (thread-change count emitted cumulatively); the
  laptop's staged file (count per window) copied over it (clnode170's kept as `recorder.go.c170`) and the image rebuilt
  as **`big_ctr:k123i2`** with `wiring/wlgen-k123i2`, the one Phase I uses. **Restore:** put the `*.c170` files back, delete the tags
  k123i1 and k123i2 from the registry (as for the loaded images), `rm wiring/wlgen-k123i{1,2}`, `rm -rf wiring/build-k8s` and untar it from
  the export if wanted (the build regenerates it).
- **Queue i2 (Phase I step 2; `~/i2/queue-i2.sh all`, log `/local/results/i2/queue-i2.log`, data `/local/results/i2`):**
  snapshots `pre-i2-{gordion,sn}.json` and the pod list first; node-3: `/var/tmp/k5` (agent, probe, controller, scorer,
  `ugen`), resctrl mounted (was not), `/sys/fs/cgroup/gordion-aggr`, `kernel.bpf_stats_enabled` = 1 (was 0);
  unique-id-service onto node-3 CPU 1 (`g8`, hostPID, `GORDION_CTL`), recommendation onto node-3 CPUs 5,7 (`gordion9`,
  gate off, `GORDION_CTL`), bigsvc pods (`k123i2`) on CPUs 1,3,5,7 per run; `stress-ng` neighbors only inside cells;
  `HWP_REQUEST` read before every cell (re-applied and logged if it drifted). **Restore (the queue's last phase,
  checked):** agents, scorer and neighbors stopped, cgroup removed, `bpf_stats_enabled` and resctrl back as before, the
  two namespaces' deployments `kubectl replace`d from the snapshots (env and hostPID removed), bigsvc deleted, node-3's
  recorder and controller files removed; deployments compared field by field and pods by node, before and after.
  **Queue i2 done Nov 18 02:00 UTC; restore checked:** 46 deployments equal to the snapshots field by field; 79/79 pods
  running, six unpinned HotelReservation services rescheduled between node-1 and node-2 (same nodes as before, other
  assignment); node-3: no agent, scorer or `stress-ng`, `/var/tmp/k5` gone, resctrl unmounted, `bpf_stats_enabled` 0;
  `HWP_REQUEST` 0x55001818 on all CPUs at all 17 checks (no re-apply); C6 still off (ours, kept).
- **Queue i3 (Phase I step 3; `~/i2/queue-i3.sh all`, log `/local/results/i3/queue-i3.log`):** the same prep and restore
  as i2 (snapshots `pre-i2-*.json` under `/local/results/i3`), plus the step-2 dictionaries `dict-uid.json`,
  `dict-rec.json` on node-3 for the scorer; the network neighbor: namespace `k5`, two `python:3.11-slim` pods on node-3
  CPUs 9/11, RPS on their host-side veths to the victim's CPUs (gone with the namespace). **Restore:** as i2, then
  `kubectl delete ns k5`.
  **Queue i3 done Nov 18 02:49 UTC; restore checked:** 46 deployments equal, pods on the same nodes, node-3 as after i2,
  namespace `k5` gone, `HWP_REQUEST` unchanged.
- **The profile's HotelReservation pods off node-3 (Nov 18 ~03:30 UTC; stop-point-2 decision 2):** the nine `default`
  pods on node-3 (frontend, geo, hr-client, memcached-rate, mongodb-profile, mongodb-rate, mongodb-recommendation,
  profile, user; list and node of every `default` pod before in `/local/results/i4/default-pods-before.txt`) deleted;
  their ReplicaSets recreate them on node-1/node-2 (node-3 and node-4 carry the `dedicated` taint). The three mongods
  use hostPath PVs without node affinity (`/data/volumes/<pv>` on the node they run on, `Retain`): their data stays on
  node-3, untouched, and they start empty on the new node, so `rate` and `recommendation` (whose databases moved
  without them) are restarted to fill them, as the others are by their own restart. Checked with a request through the
  profile's frontend. node-4's three `default` pods are left. **Restore:** remove the taint from node-3, delete the
  nine pods again so they may land there; the original data in node-3's `/data/volumes` is there for mongods that do.
- **Agent and controller for queue i4 (Nov 18 ~03:25 UTC):** `k5agent` rebuilt with `--mhz-cpus` (APERF/MPERF of the
  listed CPUs through `/dev/cpu/N/msr`, which needs the `msr` module node-3 already has, §18 above), `k5ctl.py`
  (`--resctrl-cg`, `--mhz-cpus`, schedstat per CPU and thread cpusets in each arm record), `k5spec.py` (the
  decomposition arms, `sweepF`); the earlier versions kept beside them as `*.pre-i4` (`~/k5`, `/local/k5build`).
- **Queue i4 (decisions 1 and 3; `~/i2/queue-i4.sh all`, log `/local/results/i4/queue-i4.log`):** the same prep, net
  neighbor and restore as i3; arms per RESULTS-I §4. **Restore:** as i3.
  Its first cell (unique-id, quiet) recorded nothing: right after prep the old unique-id pod (node-2, terminating) was
  still Running and the queue took its UID (no cgroup on node-3). The lookup now takes the pod on node-3 without a
  deletion timestamp (all three queues in the repository). The other cells ran. **Rerun (`queue-i4b.sh`, chained on node-0
  after `DONE-I4`):** `R=/local/results/i4b PHASES="prep decomp" DC_NB=none`, its own prep, snapshots and restore.
- **bigsvc image `big_ctr:k123i3` (Nov 18 03:25 UTC; decision of Nov 18):** blueprint's `runtime/plugins/gordion/recorder.go`
  reads the clock only when `GORDION_READS` contains `mhz` (default off; `exp.sh` passes `READS`); node-0's previous files
  kept as `*.pre-i4`; built and pushed with `wiring/wlgen-k123i3`. `k5ctl.py` now runs pinned to CPU 53 (queues).
  **Restore:** the `*.pre-i4` files back; delete the tag as for the others.
- **Queue i5 (`~/i2/queue-i5.sh all`, chained on node-0 after the i4b rerun; log `/local/results/i5/queue-i5.log`):**
  prep as i2; one 10 s cell with `Bp-fix2` on unique-id during which `strace` (0.3 s each, `openat`/`read`/`pread64`)
  is attached to unique-id's and recommendation's recorders, an idle bigsvc pod, the agent and the scorer (not a
  measurement); then the eight bigsvc sweeps on `k123i3` with `sweepF`. **Restore:** as i2.
- **i4b and i5 failed at their first cell (Nov 18 05:08 and 05:21 UTC), nothing measured:** the new pod lookup's JSONPath
  held a line break where `
` was meant (an editing artefact), so kubectl rejected it and every cell's spec lacked a
  pod UID. Their restores ran and checked clean (46 deployments equal, pods unchanged, node-3 clean). Queue i4 itself
  (switch decomposition, recommendation cells, both re-sweeps) ran from the copy started before the change and is
  complete. Fixed in all queues, the lookup tested on node-0 against the live pods; the failed directories kept as
  `/local/results/{i4b,i5}-failed-jsonpath`. **Rerun Nov 18 ~21:00 UTC:** i4b, then i5, then `post-i5.sh`, chained.
  **Reruns done Nov 18:** i4b 20:55-21:30 UTC and i5 21:30-22:52 UTC, both restores checked clean (46 deployments equal,
  pods unchanged, node-3 clean, HWP_REQUEST unchanged); analysis on node-0 (`post-i5.sh`), raw data
  `/local/results/i4i5-raw.tgz` copied to the laptop archive (sha256 equal). Cluster quiet since 22:53 UTC.
- **Step 4 apparatus (Nov 18 ~23:00 UTC):** `k5agent` rebuilt with `--perf-td` (three raw stall counters per cgroup
  group), `k5ctl.py` passes it, `k5spec.py` arm `sigF`; checked on node-3 with two temporary cgroups
  (`/sys/fs/cgroup/k5td-{spin,stream}`, a 4 s spin and a stress-ng stream on CPUs 11 and 13), removed.
- **Queue i6 (step 4; `~/i2/queue-i6.sh all`, log `/local/results/i6/queue-i6.log`):** prep as i2; a 24 MB memwalk
  calibration pod and sweep; the signature cells (RESULTS-I §5): unique-id twins and its SMT sibling under matrixprod on
  CPU 73; bigsvc pods (`k123i3`) at 256, 128 and 24 MB; neighbors in `/sys/fs/cgroup/gordion-aggr`: stress-ng
  `--stream` 2/4/8 on CPUs 9-23 odd, `--cache 4` on 9-15, only inside their cells. **Restore:** as i2.
  **End state (Nov 19 00:41 UTC):** ran 23:01-00:41, 15 cells, 0 errors; restore checked: 46 deployments equal to the
  snapshots, 79/79 pods on their nodes, node-3 with no agent, `/var/tmp/k5`, `gordion-aggr` cgroup or monitor group left,
  144 CPUs online, `HWP_REQUEST` 0x55001818 at every check (`/local/results/i6/hwp-check.txt`), C6 still disabled.
  Raw data `/local/results/i6-raw.tgz` (sha256 30bcd784…8bd1), the same on the laptop.
- **node-3 C1E off (stop-point-3 decision 1; Nov 19 01:01:48 UTC):** `/var/tmp/i2/i2node3.sh c1e-off` (new case, the previous
  script kept as `i2node3.sh.pre-c1e`): `disable` = 1 for `cpuidle/state2` (C1E, exit 4 µs) on CPUs 0-143 (verified: 144 × 1), in addition to
  C6 (step 0.2). States left on: POLL (exit 0 µs) and C1 (exit 1 µs, residency 1 µs). Reason: RESULTS-I §5.1, the unique-id
  core runs at 1.71 GHz effective at mid with C1E between its requests. **Restore:** `i2node3.sh c1e-restore` (writes 0
  to state2/disable on every CPU; the snapshot `orig/cpuidle.txt` has 0 for every state2) or `cstate-restore` for all states.
- **Step 5 apparatus (Nov 19 ~01:20 UTC):** `k5agent` rebuilt on node-0 with `--rdt-parent` (mon group inside a given resctrl ctrl
  group), `k5ctl.py` with `mitig_setup/teardown` (ctrl groups `/sys/fs/resctrl/k5v`, `k5n`, schemata L3 / MB per arm, removed
  at the arm's end), `k5score.py` row-3 sub-rule, `k5spec.py` arms `cat4 cat6 mba90 mba50 mba30 mba10`; previous versions kept
  as `*.pre-i7` (`~/k5`, `/local/k5build`). **Smoke test on node-3 (~1 min):** resctrl mounted (unmounted again if it was),
  cgroup `/sys/fs/cgroup/k5smoke` with one spin on CPU 11, ctrl group `k5v` with `L3:1=00f`, the agent 3 s with its mon group
  inside; then spin killed, mon group, `k5v` and the cgroup removed, `/tmp/k5smoke` removed. Ran 01:08:20-01:08:24 UTC: agent exit 0, mon group inside `k5v`, 1 task, 29 ticks;
  end state resctrl unmounted, no cgroup, agent or spin left.
- **Queue i7 (unique-id with C1E off; `~/i2/queue-i7.sh all`, log `/local/results/i7/queue-i7.log`, Nov 19 01:12 UTC on):** prep as i2
  (unique-id `g8` and recommendation `gordion9` pods on node-3, resctrl mounted, `gordion-aggr`, `bpf_stats`); unique-id's ρ ramp
  3 000-30 000 req/s, its dictionary sweep, twin / SMT sibling / twin cells, overhead cells quiet and switch (stress-ng `--switch`
  on CPU 1, matrixprod on CPU 73, only inside their cells). **Restore:** as i2 (deployments replaced from the snapshot, pods
  checked, node-3 cleaned; C6 and C1E stay off). **Abort and resume:** the queue exited at 01:19 UTC at the start of its sweep
  phase (`set -u`: the sweep called `sched_of` with queue-i4's two arguments instead of queue-i6's three, so the schedule was
  empty), after the ramp (mid 22 483 req/s) and before any instrumented cell; its restore did not run. The pods stayed deployed
  with no load, no neighbor and no agent on node-3 for 80 min (checked 02:38: `gordion-aggr` empty, no stress-ng/ugen,
  HWP 0x55001818 on 1 and 73, C1E off on all 144). Fixed (`sched_of uid uniform 30`), re-shipped, resumed 02:39:43 UTC with
  `PHASES="sweep sig ovh" bash ~/i2/queue-i7.sh all` appending to the same log (prep skipped; the restore runs at the end as
  before, and `chain-i7-i8.sh` still waits on `DONE-I7`). The mid phase's result stands; no cell is repeated.
  **End state (DONE-I7 03:20:46 UTC):** 6 cells, 34 arm records, 0 errors; HWP 144 × 0x55001818 at every check; restore
  clean (46 deployments equal to the snapshot, 79/79 pods, node-3 cleaned, C6 and C1E still off); chain started i8 03:21:30.
  Raw `/local/results/i7-raw.tgz` (18 MB, sha256 00c39307…a266) = laptop `archive/i7-raw-20261119-laptop.tgz`.
- **Queue i8 (step 5 mitigation; `~/i2/queue-i8.sh all` started by `~/i2/chain-i7-i8.sh` once i7's restore has checked clean,
  log `/local/results/i8/queue-i8.log`, expected Nov 19 ~02:15-04:30 UTC):** prep as i2; bigsvc pods (`k123i3`) at 24 and 128 MB;
  neighbors in `gordion-aggr` only inside their cells (stress-ng `--cache 4` on CPUs 9-15, `--stream 4` on 9-23 odd); per arm
  the resctrl ctrl groups `/sys/fs/resctrl/k5v` (victim, `L3:1=00f` / `03f`) and `k5n` (neighbor, the other ways or `MB:1=90/50/30/10`),
  removed at the arm's end by `k5ctl.py`; `~/k5/k5spec.py` replaced by the step 5 version (previous as `k5spec.py.pre-i8`).
  **Restore:** as i2, plus `rmdir` of `k5v`, `k5n` if left; resctrl unmounted again (it was not mounted before prep).
  **End state (DONE-I8 05:03:45 UTC; started 03:21:30 by the chain):** 8 cells (24 MB twin / cache × 5 arms / twin; 128 MB
  twin / stream ×4 × 7 arms / twin; the dense 128 MB sweep), 17 arm records, 0 errors; every CAT/MBA ctrl group built and
  removed per arm (`mitig` / `mitig-off` events; schemata `L3:1=00f` / `03f`, `MB:1=` 90-10 on `k5n`); HWP 144 × 0x55001818
  at every check; restore clean: 46 deployments equal to the snapshot, 79/79 pods, node-3 cleaned (`/var/tmp/k5` gone,
  resctrl unmounted, no `gordion-aggr`, no `k5v`/`k5n`), C6 and C1E still off (`state2`/`state3` disable 144 × 1), checked
  05:08 UTC from node-3. Raw `/local/results/i8-raw.tgz` (191 MB, sha256 a5e9f7bc…9e97) = laptop
  `archive/i8-raw-20261119-laptop.tgz`. `~/k5/k5spec.py` is the step-5 version (`k5spec.py.pre-i8` beside it). Analysis
  only on node-0 afterwards (i6_sig, i2_dict; the figs work in `/local/results/figs-work`: `pip --user` matplotlib + scipy
  and a Python-3.10 copy of `b4_compare.py`; nothing on node-3).
- **Queue i9 (step 6 escalation arm + the two-step policy test; `~/i2/queue-i9.sh all`, log `/local/results/i9/queue-i9.log`,
  Nov 19 ~05:50 UTC on):** prep as i8 (unique-id `g8` and recommendation `gordion9` pods on node-3, resctrl mounted,
  `gordion-aggr`, `bpf_stats`; dictionaries: queue i7's `dict-uid.json` and k5score's pooled `dict-syn-memwalk128.json`
  for the synthetic service's on-node scorer). Phases: `m6` = memwalk30 at 128 MB, twin / stream ×4 × [`sigF`,
  `cat6mba30`: ctrl groups `k5v` `L3:1=03f` and `k5n` `L3:1=fc0` + `MB:1=30`] / twin, 300 s each; `esc128` = a fresh
  memwalk128 pod under stream ×4, arms `none`, `Bp-fixedReads`, `Bp-esc` 3 × 60 s; `escnet` = unique-id at 22 483 req/s
  under the A5 network neighbor (namespace `k5`, pods on CPUs 9/11, RPS to CPU 1), the same arms. **New in `Bp-esc`**
  (`k5spec.py`, `k5ctl.py` class `Escalator`, shipped with `.pre-i9` backups in `~/k5`): a second `k5agent`
  (`--probe switch,softirq --rq 1 --perf 1 --perf-read 1 --perf-td 1`) on CPU 53 while the on-node scorer flags a window
  (E ≥ 0.15 or I_g > 1.15), stopped 5 s after the last flag; both agents exit with the arm (`esc` events in `cell.jsonl`;
  `leftover_agents` checked at the cell's end). **Restore:** as i8 (deployments replaced from the snapshot, pods checked,
  node-3 cleaned, `k5v`/`k5n` removed, resctrl unmounted, namespace `k5` deleted; C6 and C1E stay off).
  **End state (DONE-I9 06:13:02 UTC):** the m6 cells complete (4 arm records, 0 errors; CAT 6 + MBA 30 groups built and
  removed); **the two escalation cells aborted after their first `Bp-esc` arm:** the escalation itself ran (diagnostic
  agent on from the first flagged window to the arm's end, 62.8 s in both), but `k5ctl.py` crashed in `Escalator.finish()`
  (`_stop` shadowed `threading.Thread._stop`, which `join()` calls), so the arm record, the remaining reps and the agents'
  stop were skipped; the production and diagnostic agents and the scorer stayed up until the restore's `pkill`
  (`leftover_agents` 1 and 2 in the cells' end records). Restore clean: 46 deployments equal, 79/79 pods, node-3 cleaned
  (no agents, resctrl unmounted, `gordion-aggr` and `k5v`/`k5n` gone, namespace `k5` gone), C6 and C1E still off. Fixed
  (`_stop_diag`, offline join test), shipped to `~/k5/k5ctl.py` (06:20 UTC). **Rerun as queue i9b** (`~/i2/chain-i10-i9b.sh`:
  after i10's clean restore, `R=/local/results/i9b PHASES="prep esc128 escnet" ~/i2/queue-i9.sh all`, log
  `/local/results/i9b/queue-i9b.log`; prep and restore as i9). Raw `/local/results/i9-raw.tgz` (59 MB, sha256
  739a6e64…7817) = laptop `archive/i9-raw-20261119-laptop.tgz`.
- **Queue i10 (step 7, HarvestContainers on node-4; `~/i2/queue-i10.sh all`, log `/local/results/i10/queue-i10.log`,
  chained after i9's clean restore by `~/i2/chain-i9-i10.sh`):** **node-4 changes, all restorable and restored by the
  queue:** CPUs 64-71 offline (`echo 0 > cpu*/online`; snapshot `node4-online-before.txt`, back to `0-71` at the end);
  cpufreq pin of the experiment's 16 CPUs (4,6,8,10, 12, 13-15, 17-27 odd, 24, 30) to `scaling_min = scaling_max =
  2 400 000 kHz` (snapshot `node4-freq-before.txt`, 800 000-3 500 000 as found, restored per CPU); `/var/tmp/k4/bin2`
  (K4's `k4node.sh`, `k4send.py`) and `/var/tmp/k4/logs` (the Balancer's logs per cell), removed at the end;
  `/var/tmp/k5`, `/var/tmp/gordion`, resctrl mounted and `gordion-aggr` as on node-3 for every i queue, cleaned as
  there; namespace `k4` with the BestEffort burner (`k4b-secondary.yaml`, 0 or 1 replica on core 12), deleted at the end;
  HC: `hc-listener` started by `k4node.sh hc-load` (its `ExecStartPre` inserts `qidlecpu.ko` and writes `cpulist`,
  `bindcpu`, `control`), the Balancer as the transient unit `hc-balancer-dyn` on CPU 24 at `TIC = LOW_TIC` = 1 or 3, the
  burner pod's cpusets confined and restored by `hc-on` / `hc-off`, module removed by `hc-unload` after each HC cell and in
  the restore (as found: module absent, units inactive). **Never reads an HC procfs file** (`k4node.sh` writes only;
  `panic_on_oops` is 1 on node-4). The profile's pods on node-4 (jaeger, memcached-profile, mongodb-geo) are not touched.
  Recommendation is redeployed to node-4 CPUs 4,6,8,10 (`hr.sh deploy`, gate off) and put back from the pre-prep snapshot
  by the restore (node-3, CPUs 5,7). Cells: `sweep-rec` (dictionary), `c5-rec-twin-S0`, `c5-rec-res1`, `c5-rec-res3`,
  `c5-rec-twin-S1`, 300 s each at 5 000 req/s, arm `sigS`. **Restore check:** online `0-71`, the 16 CPUs' min/max as in the
  snapshot, `k4node.sh status` module absent, no `k5agent`, namespace `k4` gone, 46 deployments equal, pods counted.
  **Queue i9b (06:47-07:14 UTC, node-3):** the two escalation cells again with the fixed controller: 18 arm records, 0
  errors, one escalation per `Bp-esc` arm (on 62.8-62.9 s of 63), both agents and the scorer stopped at every arm's end
  (`leftover_agents` 0); restore clean (46 deployments equal, 79/79 pods, node-3 cleaned, namespace `k5` gone, C6 and C1E
  off). Raw `/local/results/i9b-raw.tgz` (23 MB, sha256 a53479ea…21b3) = laptop `archive/i9b-raw-20261119-laptop.tgz`.
  **Queue i10b (07:14-07:47 UTC, node-4):** sweep, dictionary fit and shipped, S0 / reserve 1 / reserve 3 / S1 with the
  scorer on-node (HC loaded and unloaded per reserve cell; Balancer logs fetched); restore: deployments 46 equal, 79/79
  pods, CPUs `0-71` online, module absent, units inactive, `/var/tmp/k4`, `/var/tmp/k5` and `/mnt/extra/gordion` gone,
  namespace `k4` gone; **but the restore's closing pin check (inherited from the node-3 queues' HWP check) re-applied the
  cpufreq pin after `freq_restore` had released it**, so the 16 CPUs sat at 2 400 000-2 400 000 kHz from 07:47 until the
  hand restore at **20:55 UTC** (the laptop's watcher had been stopped for low memory and the session was idle); released
  per CPU from `node4-freq-before.txt`, checked 16 × 800 000-3 500 000. `queue-i10.sh` no longer runs that check in its
  restore (staged). Note for the analysis: `i2_dict.py` fits recommendation at k = 2 (its K table), so the on-node
  dictionary was a 2-core one; the 4-core refit (`dict/dict-rec-k4.json`, `k5score.py --fit --k 4`) and the offline
  replays are in `results/i10b/an-k4` and each cell's `ctl/sigS-r00-score-replay.jsonl`. Raw `/local/results/i10-raw.tgz`
  (sha256 09d0dfe6…1a3f) and `i10b-raw.tgz` (0d5c2dde…0250) = the laptop's `archive/i10-raw-…`, `archive/i10b-raw-…`.
  **node-4 as left (20:55 UTC):** CPUs 0-71 online, cpufreq 800 000-3 500 000 on every CPU we touched, `qidlecpu` absent,
  `hc-listener` / `hc-balancer-dyn` inactive, no HC listener or balancer process (the stray pre-queue listener pid 36233 is
  gone, see i10), the profile's three pods untouched.
- **Queue i11 (step 8 on node-3; `~/i2/queue-i11.sh all`, log `/local/results/i11/queue-i11.log`, Nov 19 ~21:40 UTC on):** prep
  as i9 (unique-id `g8` and recommendation `gordion9` on node-3, resctrl mounted, `gordion-aggr`, `bpf_stats`; the unique-id
  dictionary refit with `e_thr`; a 40 s unique-id run at mid to read CPU 1's quiet softirq share), pre-state snapshots
  (`node3-pre.txt`: HWP on all 144 CPUs, `state2`/`state3`, resctrl mount; `node3-irq-pre.txt`: the 63 `mlx5_comp` IRQs of
  `ca:00.0` = `enp202s0f0np0` and their `smp_affinity_list`). Phases: `escnet` (namespace `k5` net pods on node-3 CPUs 9/11
  with RPS to CPU 1, cell `dc-uid-net-mid` arms `none`, `Bp-fixedReads`, `Bp-esc` 3 × 60 s); **`c2`: the 63 RX-queue IRQs of
  `enp202s0f0np0` pinned to CPU 1** (`smp_affinity_list`, restored from the snapshot after the phase and again in the
  restore), the stream's sender pod on **node-1** CPUs 9,11,13,15 (4 processes, namespace `k5`), receiver on node-3 CPU 11,
  rate raised 100 k → 800 k pps until NET_RX on CPU 1 ≥ 10 000/s, cells `c2-uid-twin-a`, `c2-uid-net-r1..r3`, `c2-uid-twin-b`
  (300 s, arm `Bp-sirq`); `figa2` (the standard net pods, cell `bp-uid-net-mid` arms `none`, `Bp-fixedReads` 5 × 300 s).
  **New in the code** (`~/k5/*.pre-s8` backups): the scorer's `e_thr`/`thr`, `--cpuset-file`, `--sirq-base` and row 8;
  the escalator fires on E only; `k5net.sh` sender on another node. **Restore from an `EXIT` trap** (once): as i9 plus the IRQ
  affinities from the snapshot and the net namespace; closing checks compare HWP, idle states, resctrl and the IRQ
  affinities with the pre-state and write nothing; the in-cell HWP check logs a drift and does not re-apply.
- **Queue i10c (step 8 on node-4; `~/i2/queue-i10c.sh all` chained after i11, log `/local/results/i10c/queue-i10c.log`):**
  as i10b (CPUs 64-71 offline, the 16 CPUs' cpufreq pin, `/var/tmp/k4/bin2`, namespace `k4`, HC loaded / unloaded per reserve
  cell, recommendation on CPUs 4,6,8,10 with `/mnt/extra/gordion` as its `/data`), the dictionary fit with k = 4 and `e_thr`
  and shipped before the HC cells, the scorer reading k from the pod's cpuset. **Restore from an `EXIT` trap**: HC off and
  unloaded, burner and namespace gone, cpufreq limits and online CPUs from the snapshots, `/var/tmp/k4`, `/var/tmp/k5`,
  `/mnt/extra/gordion` removed; closing checks compare the 16 CPUs' limits and the online list with the pre-state and write
  nothing (the pin check inside the cells logs only).
  **i11, first run (21:36 UTC on):** `escnet` complete (9 arm records, 0 errors; the escalator fired 0 times in the three
  `Bp-esc` arms). **The `c2` phase measured nothing:** its NET_RX reader matched `/proc/softirqs` lines without their leading
  spaces and its client-p50 reader looked for a `p50` field (`lat_p50_us`), so the calibration read 0 everywhere, set the
  stream to 0 pps, ran `c2-uid-twin-a`, one `c2-uid-net-r1` with the stream off and `c2-uid-twin-b`, then restored the 63
  IRQ affinities (the pin itself worked: irq 418 → CPU 1). The sender pods on node-1 started at every rate. Fixed in
  `queue-i11.sh` (shipped 22:19 UTC); **rerun as i11b** (`~/i2/chain-i10c-i11b.sh`: after i10c's clean restore,
  `R=/local/results/i11b PHASES="prep c2" ~/i2/queue-i11.sh all`, log `/local/results/i11b/queue-i11b.log`; prep and the
  trap restore as i11). `figa2` was still running at 22:19 (ends ~23:00), then i11's restore and i10c.
  **i10c ran 23:03-23:37 UTC** (after i11's closing check had refused the chain on my snapshot-order bug: the pre-state was
  recorded after prep had mounted resctrl, so "mounted" differed from the restored "unmounted" while node-3 was as before
  prep; the snapshot now precedes the mount, and i10c was started by hand). **Its live scorer read k = 64** from the pod
  cgroup's `cpuset.cpus.effective` (the HR pods are pinned by thread affinity, not by a cgroup cpuset), so its on-node rows
  are at ρ 0.012 and not the "4-core dictionary" reading; the agent columns stand. `k5ctl.py` now passes k from the
  victim threads' `Cpus_allowed_list` (shipped 23:29 UTC, before i11b's prep copied it). **Rerun as i10d**
  (`~/i2/chain-i11b-i10d.sh` after i11b's clean restore: `R=/local/results/i10d ~/i2/queue-i10c.sh all`).
  **End states.** i11 (DONE 23:00:23 UTC): escnet 9 arm records, figa2 10, the aborted c2 cells 3, 0 errors; restore from
  the trap, deployments 46 equal, 79/79 pods; closing check: HWP and idle states equal, resctrl "differs" only because the
  snapshot was taken after the mount (as before prep: unmounted), IRQ affinities equal. i11b (prep + c2, 23:37-23:58):
  calibration 100 k / 200 k / 400 k / 800 k pps (NET_RX on CPU 1 3 039 / 2 826 / 1 622 / 1 620 per s; pinned IRQs 3 330 /
  6 281 / 8 / 11 per s), run at 100 000 pps, `c2-uid-twin-a`, `c2-uid-net-r1` (the stream saturated CPU 1: client p50 24 s,
  served 18 968 of 22 483 req/s), remaining C2 arms skipped (in-handler p50 moved 0 %), `c2-uid-twin-b`; IRQ affinities
  restored (checked equal, 63 IRQs), HWP / idle states / resctrl equal to the pre-state, namespace `k5` gone. i10c
  (23:03-23:37): 5 cells, restore from the trap; cpufreq limits and online CPUs equal to the pre-state, module absent, units
  inactive, nothing re-applied. i10d (23:59-00:12): aborted after its first cell through `$R/STOP` and node-4's
  `/var/tmp/k5/STOP` (k from the pause process: 64); the HC load / on / off / unload of the skipped reserve cells still ran
  (the burner pod up and down twice), then the trap restore. i10e: the same queue with k = 4 from the Primary's threads.
  Raw archives on node-0 and the laptop: `i11-raw.tgz` (faccb764…), `i11b-raw.tgz` (2b75cb6b…), `i10c-raw.tgz` (32f210af…).
  **i10e (00:12-00:45 UTC Nov 20, k = 4 from the Primary's threads):** sweep, dictionary (`e_thr` 0.033 at ρ 0.185), S0, reserve
  1, reserve 3, S1 with the live scorer (res1 row 1 `burner` 3 028 / 3 030; res3 2 515 / 3 030; twins row 6 98-99 %), 0
  errors; restore from the trap: deployments 46 equal, 79/79 pods, CPUs `0-71`, cpufreq limits equal to the pre-state,
  module absent, units inactive, `/var/tmp/k4`, `/var/tmp/k5`, `/mnt/extra/gordion` gone, namespace `k4` gone (checked
  00:47 UTC). Raw `i10d-raw.tgz`, `i10e-raw.tgz` on node-0 = the laptop's `archive/i10d-raw-20261120-laptop.tgz`,
  `archive/i10e-raw-20261120-laptop.tgz`. **Cluster state at the end of step 8:** node-3 and node-4 as before Phase I's
  queues except node-3's C6 and C1E, which stay off (RUNBOOK entries of Nov 18-19); `~/k5/*.pre-s8` hold the step-7 code.
- **Queue i12 (step 9, the milder C2 on node-3; `~/i2/queue-i12.sh all`, log `/local/results/i12/queue-i12.log`, Nov 20 ~01:23 UTC
  on):** prep as i11 (pre-state snapshots now include `ethtool -c enp202s0f0np0`: adaptive RX on, rx-usecs 8, rx-frames 128).
  Phase `c2mild`: the 63 RX-queue IRQs of `enp202s0f0np0` → CPU 1; **`ethtool -C enp202s0f0np0 adaptive-rx off rx-usecs 0`**
  (restored from the snapshot after the phase and again in the trap restore: `rx-usecs 8 rx-frames 128 adaptive-rx on`);
  the stream's sender pods on node-1 CPUs 9,11,13,15 (namespace `k5`), receiver on node-3 CPU 11; calibration steps 5 k-60 k
  pps, each with a 40 s unique-id run at mid and a 20 s `/proc/stat` read on CPU 1, stopping at the first step over share 0.40
  or under served = offered; cells `c2m-uid-twin-a`, `c2m-uid-net-r1..r3`, `c2m-uid-twin-b` (300 s, arm `Bp-sirq`, the step-9
  scorer with `--sirq-base` from prep). **Restore from the EXIT trap**: net namespace, coalesce settings, IRQ affinities,
  then as i11; closing checks: HWP, idle states, resctrl, IRQ affinities and the coalesce settings against the pre-state,
  read-only. Code shipped with `~/k5/*.pre-s9` backups (`k5score.py` φ_irq / row 9, `k5ctl.py` no escalation on row 9).
  **End state (DONE-I12 01:57:59 UTC):** calibration 5 000-30 000 pps (share 0.185-0.419, served = offered throughout), run at
  20 000 pps; 5 cells, 0 errors; coalesce settings restored by the phase and checked by the trap restore ("Adaptive RX: on
  TX: on rx-usecs: 8 rx-frames: 128"), the 63 IRQ affinities equal to the snapshot, HWP / idle states / resctrl equal to the
  pre-state, deployments 46 equal, 79/79 pods, namespace `k5` gone. Raw `/local/results/i12-raw.tgz` (sha256 7f644875…) =
  laptop `archive/i12-raw-20261120-laptop.tgz`. **node-3 and node-4 at the end of step 9:** as before Phase I's queues
  except node-3's C6 and C1E, which stay off; `~/k5/*.pre-s9` hold the step-8 code, `*.pre-s8` the step-7 code.

  **End state (DONE-I10 06:47:02 UTC; ran 06:14-06:47):** 5 cells, 0 errors; HC loaded, Primary and Secondary acknowledged,
  Balancer on CPU 24 and unloaded again in both reserve cells; deployments 46 equal, 79/79 pods, CPUs `0-71` online,
  module absent, units inactive, `/var/tmp/k4` and the `k4` namespace gone. **Two faults, both corrected by hand at
  06:50-06:51 UTC:** (1) the restore's cpufreq loop read its snapshot through `ssh -n` (no stdin) and did nothing, so the 16
  CPUs stayed at 2 400 000-2 400 000 kHz; restored per CPU from `node4-freq-before.txt` to 800 000-3 500 000 (checked on all
  16); (2) `hr.sh` mounts `/mnt/extra/gordion` as the pod's `/data` on node-4 (not `/var/tmp/gordion`), so the control file
  the queue wrote was never read (interceptor stayed off: the recorder stream has no windows, the in-queue dictionary fit and
  the on-node scorer had nothing) and the recorder file sat under `/mnt/extra/gordion`; fetched to node-0 and the directory
  removed (absent before the queue). The agent-side columns of the four cells are complete (RESULTS-I 8.1). Also seen: an HC
  `listener` process (pid 36233, not a systemd unit, present before the queue beside the inactive units) was gone after the
  restore; the queue only started and stopped the `hc-listener` unit and the module. **Rerun as queue i10b**
  (`~/i2/chain-i9b-i10b.sh` after i9b: `R=/local/results/i10b ~/i2/queue-i10.sh all` with `V_CTL`/`V_REC` under
  `/mnt/extra/gordion`, `freq_restore` built without stdin, and `/mnt/extra/gordion` removed in the restore).