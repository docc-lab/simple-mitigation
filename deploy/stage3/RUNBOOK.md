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
`simulation/results/motiv_ext_bursty_timeseries.{png,pdf}`: per arm, handler
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
