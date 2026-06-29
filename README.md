# simple-mitigation

A single Go binary that consumes the per-pod `ContentionStream` gRPC API
(see [`Mitigation-interface.md`](Mitigation-interface.md)), evaluates a CEL
policy each tick, and fires one of three mitigation tiers:

| Tier         | Surface                                        | Timescale | Actuator               |
| ------------ | ---------------------------------------------- | --------: | ---------------------- |
| `isolate`    | cgroup v2 `cpu.max` on co-located aggressors   |  ~100 ms  | `pkg/actuators/isolate`  |
| `harvest`    | cgroup v2 `cpu.max` on co-located best-effort pods |  ~100 ms  | `pkg/actuators/harvest`  |
| `vertical`   | `pods/resize` subresource (cpu requests/limits) |   ~1 s    | `pkg/actuators/vertical` |
| `horizontal` | `apps/v1.Deployment/scale` subresource          |  ~10 s+   | `pkg/actuators/horizontal` |

The binary runs as a privileged `DaemonSet` -- one instance per node. Each
instance subscribes only to victim pods on its own node (field selector
`spec.nodeName=$NODE_NAME`), so node-local mitigations are race-free
without leader election. Horizontal scale is coordinated K8s-natively via
an idempotent `/scale` patch + a `mitigation/horizontal-last-scaled-at`
cooldown annotation on the Deployment.

See [`plan-v2-centralized.md`](plan-v2-centralized.md) for the full design.

## Architecture

```
   victim pod (this node)             mitigation-controller (this node, DaemonSet)
   :7900 ──gRPC stream──▶  scoreclient ──▶ features (rolling window per pod)
                                                    ↓
                                            policy (CEL rules)
                                                    ↓
                                ┌──────────┬──────────┬──────────┐
                                ▼          ▼          ▼          ▼
                            isolate     harvest    vertical   horizontal
                            (cpu.max)  (cpu.max)  (resize)   (scale)
```

The simulation's three simple control laws (horizontal bang-bang, isolating
saturated ramp, harvesting AIMD) are ported to `pkg/controllers` and validated
against [`simulation/simulation.py`](simulation/simulation.py). They are not
yet driving the per-tick loop — see [`plan.md`](plan.md) for the wiring status.

## Repo layout

```
proto/contention.proto                  vendored wire contract (3 spatial-horizon fields added)
gen/go/contentionpb/                    generated (gitignored) -- run `make proto`
pkg/targets/                            multi-victim config loader
pkg/scoreclient/                        gRPC subscriber w/ reconnect + multi-pod fan-in
pkg/podwatch/                           client-go informer (+ NewLocalNodeWatcher for the DaemonSet)
pkg/features/                           rolling window + spatial/temporal feature computation
pkg/policy/                             CEL env, YAML rule loader, fsnotify hot-reload, engine
pkg/cgroup/                             cgroup v2 path resolution + cpu.max read/write
pkg/actuators/                          shared interface + annotation key constants
pkg/actuators/isolate/                  throttles aggressor pods' cpu.max (fraction or absolute cap)
pkg/actuators/harvest/                  raises best-effort pods' cpu.max to lend victim idle cores
pkg/actuators/vertical/                 patches pods/resize for the victim pod
pkg/actuators/horizontal/               patches deployments/scale for the victim Deployment
pkg/controllers/                        the 3 simple control laws ported from simulation.py (cap / n / h)
pkg/aggregator/                         pluggable Max / Mean / P90 (callable from rules)
pkg/thresholder/                        HI/LO + cooldown state machine (also exposed to CEL via `band`)
cmd/mitigation-controller/              the only binary
deploy/controller/                      DaemonSet, RBAC, ConfigMap (targets + policy)
deploy/victim-sample/                   sample search + profile Deployments
```

## Build

Requires Go 1.23 and `protoc`. On Debian/Ubuntu:

```bash
sudo apt install protobuf-compiler
make deps         # installs protoc-gen-go + protoc-gen-go-grpc
make proto        # generates gen/go/contentionpb/*.pb.go
go mod tidy
make build        # equivalent to `go build ./...`
make test         # runs all unit tests
```

Build the container image:

```bash
make docker-controller
```

The Dockerfile runs `make proto` inside the build stage, so `docker build`
works from a fresh clone.

## Test

Three layers: Go unit tests, the offline control-law parity oracle, and an
in-cluster smoke test.

### Go unit tests

```bash
make test                          # go test ./... (all packages)
go test ./pkg/controllers/...      # the 3 ported control laws + streaming parity
go test ./pkg/cgroup/...           # cpu.max parse / path resolution
go test ./pkg/policy/...           # CEL compile + cooldown engine
```

`make test` needs the generated proto stubs (`make proto` once after a fresh
clone) because several packages import `gen/go/contentionpb`.
`pkg/controllers` has no proto dependency, so it tests standalone even before
`make proto`.

### Control-law parity (offline)

[`simulation/simulation.py`](simulation/simulation.py) is the reference
implementation the Go controllers in `pkg/controllers` are validated against —
the test expectations in `pkg/controllers/controllers_test.go` were
cross-checked against it. Run it to regenerate the sweep/figure PNGs or to
re-derive expected values:

```bash
cd simulation
pip install numpy scipy matplotlib            # one-time
python simulation.py                          # synthetic signals -> *.png
python simulation.py --data run_data_iter1_ready.json   # against a real Gordion trace
```

It writes `sweep_horizontal.png`, `sweep_isolating.png`, `sweep_harvesting.png`,
and `ctrl_reference_run.png`, plus a numeric summary to stdout.

### In-cluster smoke test

After [deploying](#deploy), confirm the pipeline end to end:

```bash
# controller is up, one pod per node, and loaded the policy:
kubectl -n mitigation-system rollout status ds/mitigation-controller
kubectl -n mitigation-system logs -l app.kubernetes.io/name=mitigation-controller --tail=20 | grep "policy reloaded"

# drive contention on a victim, then watch actions fire:
kubectl -n mitigation-system logs -l app.kubernetes.io/name=mitigation-controller -f | grep '"msg":"action"'

# verify a cgroup write landed on an aggressor (isolate) / best-effort pod (harvest):
kubectl -n hotelres get pod <aggressor> -o jsonpath='{.metadata.annotations.mitigation/cpu-max-original}'
kubectl -n hotelres get pod <be-pod>     -o jsonpath='{.metadata.annotations.mitigation/harvest-cpu-max-original}'
```

You can also exercise the score API alone without the controller — see
[Smoke test the score API directly](#smoke-test-the-score-api-directly).

> Note: this repo's CI/dev machine may not have a Go toolchain installed; if
> `go` is missing, the parity oracle (Python) still runs and is the primary
> way control-law changes are validated before pushing.

## Default policy (out of the box)

Three rules ship in [`deploy/controller/configmap.yaml`](deploy/controller/configmap.yaml),
matching plan-v2-centralized.md Section 5 verbatim:

```yaml
rules:
  - name: sharp_rising_spike
    when: "k_temporal > 0.3 || k_spatial > 0.3"
    fire:
      - kind: isolate
        params: { throttle_fraction: 0.5, aggressor_selector: "tier=batch" }
      - kind: vertical
        params: { scale_factor: 1.5 }
    cooldown: "30s"
    priority: 100

  - name: sustained_high_p50
    when: "p50_now > 0.5 && persistence_h >= 3 && duration_above_hi_ms >= 2000"
    fire:
      - kind: horizontal
        params: { delta: 1 }
    cooldown: "60s"
    priority: 50

  - name: clean_state
    when: "p50_now < 0.2 && k_temporal < 0.0 && tail_now < 0.5"
    fire:
      - kind: restore
        params: { tier: all }
    cooldown: "60s"
    priority: 10
```

`restore` is a meta-action: it fans out to every actuator's `Restore()`,
which reads the `mitigation/*` annotations on the corresponding object and
reverses the most recent action.

### CEL vocabulary

All feature fields are top-level identifiers (no wrapper object). Match the
field names in `features.FeatureVector`:

| Identifier              | Type            | Meaning                                                            |
| ----------------------- | --------------- | ------------------------------------------------------------------ |
| `target`                | string          | victim service name                                                |
| `pod`                   | string          | victim pod name                                                    |
| `p50_now`, `tail_now`   | double          | latest p50_trend_pred / tail_trend_label                           |
| `p50_h`, `tail_h`       | list(double)    | multi-horizon arrays (empty under a single-horizon predictor)      |
| `horizon_ms`            | list(int)       | parallel array of horizon offsets                                  |
| `k_spatial`             | double          | least-squares slope of p50_h vs horizon_ms                         |
| `accel_spatial`         | double          | mean second-difference of p50_h                                    |
| `p50_max_horizon_ms`    | int             | argmax horizon                                                     |
| `persistence_h`         | int             | count of p50_h entries >= HI_THRESHOLD                             |
| `k_temporal`            | double          | least-squares slope of p50 over the rolling window (per second)    |
| `accel_temporal`        | double          | mean second-difference over the window                             |
| `variance`              | double          | sample variance over the window                                    |
| `duration_above_hi_ms`  | int             | length of the most recent contiguous run above HI_THRESHOLD        |
| `window_size`           | int             | samples currently in the rolling window                            |
| `has_spatial`           | bool            | `true` iff the latest event populated `p50_horizons`               |
| `model_version`         | string          | latest event's model_version                                       |
| `source_kind`           | string          | latest event's source_kind ("onnx" / "formula" / ...)              |

Two helper functions are registered:

- `band(score, lo, hi) string` -> `"up"` / `"down"` / `"stable"`
- `count_at_least(list, threshold) int` -> count of list entries `>= threshold`

### Actuator params (`fire[].kind` + `params`)

| kind         | params                                                                                  |
| ------------ | --------------------------------------------------------------------------------------- |
| `isolate`    | `aggressor_selector` (req), and **either** `throttle_fraction` (default 0.5, one-shot) **or** absolute-cap mode: `cap_cores` / `cpu_max_quota_us` (+ `period_us`, `min_quota_us`). Optional `aggressor_namespace`. |
| `harvest`    | `be_selector` (req), `harvest_cores` (req, cores to lend on top of baseline). Optional `be_namespace`, `period_us`, `max_quota_us`. |
| `vertical`   | `scale_factor` (multiplicative) **or** `target_cpu` (absolute, e.g. `"750m"`). Clamped to `MIN_CPU`/`MAX_CPU`. |
| `horizontal` | exactly one of `delta` (additive) or `ensure_min` (idempotent floor); optional `min_replicas`/`max_replicas`. |
| `restore`    | meta-kind; fans out to every actuator's `Restore()`. |

The absolute-cap mode on `isolate` and the `harvest` kind are the actuation
surfaces the simulation's isolating (`cap`) and harvesting (`h`) controllers
drive; see [`plan.md`](plan.md).

### Authoring workflow

1. Edit `data.policy.yaml` in the ConfigMap.
2. Apply: `kubectl apply -f deploy/controller/configmap.yaml`.
3. The kubelet remounts the volume; `fsnotify` in `pkg/policy/loader.go`
   triggers `engine.Reload` within ~1s. Look for `policy reloaded` in the
   controller logs.

A typo in a CEL expression is rejected by `engine.Reload` and the previous
rules stay live -- the controller never goes silent on a bad rule.

## Default thresholds (explicit)

| Env var                   | Default | Meaning                                                              |
| ------------------------- | ------: | -------------------------------------------------------------------- |
| `TICK_MS`                 |   `100` | per-pod policy evaluation cadence                                    |
| `STALE_MS`                |  `1500` | a snapshot older than this is treated as missing                     |
| `WINDOW_SIZE`             |    `20` | rolling-window samples (~2 s at 100 ms cadence)                      |
| `HI_THRESHOLD`            |   `0.5` | what counts as "elevated" for PersistenceH / DurationAboveHiMs        |
| `MIN_CPU` / `MAX_CPU`     |  `200m` / `4` | vertical resize clamp                                          |
| `HORIZONTAL_COOLDOWN_SEC` |    `30` | cross-node Deployment scale gate                                     |
| `TARGETS_CONFIG`          | `/etc/mitigation/targets.yaml` | mounted from the ConfigMap                          |
| `POLICY_CONFIG`           | `/etc/mitigation/policy.yaml`  | same                                                |
| `NODE_NAME`               |    (none) | required; injected via `fieldRef: spec.nodeName`                  |

## Deploy

Prerequisite: K8s >= 1.35 (in-place pod resize GA -- see
<https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/>),
cgroup v2 on every node, and the
`pod-security.kubernetes.io/enforce=privileged` namespace label is honoured
(see `deploy/controller/namespace.yaml`).

### Sample victims

```bash
kubectl apply -f deploy/victim-sample/namespace.yaml
kubectl apply -f deploy/victim-sample/search.yaml
kubectl apply -f deploy/victim-sample/profile.yaml
```

Replace the placeholder `image: REGISTRY/...:tag` lines with your real
images. The fields that matter for mitigations to work: named `score` port
7900, `resources.requests == resources.limits`,
`resizePolicy.cpu = NotRequired`.

### Mitigation controller

**Automated (recommended):** [`build-push-deploy.sh`](build-push-deploy.sh)
does build → push → manifest rewrite → apply → rollout in one shot:

```bash
./build-push-deploy.sh --node=node-3                 # build, push to docclabgroup, deploy pinned to node-3
./build-push-deploy.sh --tag=v2 --node=node-3        # custom tag
./build-push-deploy.sh --no-build --node=node-3      # redeploy the current pushed image
./build-push-deploy.sh --help                        # all options (registry, pull-policy, no-push, ...)
```

It rewrites `deploy/controller/daemonset.yaml`'s image/pull-policy (and pins the
DaemonSet to `--node` via `nodeSelector`), then applies all four manifests and
waits for rollout. The manual steps below are the same thing unpacked.

First make the image reachable by **every node** (the DaemonSet runs one pod
per node). `make docker-controller` builds
`simple-mitigation/mitigation-controller:dev` into the local image store of the
node you built on; the others need it too. Check your runtime with
`kubectl get nodes -o wide` (CONTAINER-RUNTIME column):

```bash
# Save once on the build node:
docker save simple-mitigation/mitigation-controller:dev -o /tmp/mc.tar

# --- containerd runtime: import into the k8s.io namespace on each node ---
sudo ctr -n k8s.io images import /tmp/mc.tar
sudo ctr -n k8s.io images ls | grep mitigation

# --- docker runtime: load on each node (e.g. fan out over SSH) ---
for n in node-1 node-2 node-3 node-4; do
  scp /tmp/mc.tar "$n:/tmp/mc.tar" && ssh "$n" 'docker load -i /tmp/mc.tar'
done
```

(For a real registry instead, set the `image:` in `daemonset.yaml` to
`<registry>/simple-mitigation/mitigation-controller:<tag>` and `docker push` —
then no per-node loading is needed.)

> Version note: this design targets K8s >= 1.35 (in-place `pods/resize` GA). On
> older clusters the controller still runs and `isolate` / `harvest` /
> `horizontal` work, but the `vertical` actuator needs `pods/resize` (alpha
> 1.27, beta 1.33, GA 1.35) and will error there — drop the `vertical` fire
> from the policy on pre-1.33 clusters.

Then apply, in order:

```bash
kubectl apply -f deploy/controller/namespace.yaml
kubectl apply -f deploy/controller/rbac.yaml
kubectl apply -f deploy/controller/configmap.yaml
kubectl apply -f deploy/controller/daemonset.yaml

kubectl -n mitigation-system rollout status ds/mitigation-controller
kubectl -n mitigation-system logs -l app.kubernetes.io/name=mitigation-controller --tail=30
```

Adding a victim service later = single ConfigMap edit:

```bash
kubectl -n mitigation-system edit cm mitigation-controller-config
# Policy/targets reload via fsnotify within ~1s; no rollout needed.
```

## Crash-safe state (annotations only)

Every action stamps annotations on its target *before* the actual write so
`Reconcile()` at startup can find and complete an interrupted apply:

| Target               | Annotation keys                                                                              |
| -------------------- | -------------------------------------------------------------------------------------------- |
| Aggressor Pod        | `mitigation/cpu-max-original`, `mitigation/cpu-max-set-by-node`, `mitigation/cpu-max-set-at` |
| Best-effort Pod      | `mitigation/harvest-cpu-max-original`, `mitigation/harvest-set-by-node`, `mitigation/harvest-set-at` |
| Victim Pod           | `mitigation/cpu-limit-baseline`                                                              |
| Victim Deployment    | `mitigation/horizontal-last-scaled-at`, `mitigation/horizontal-baseline-replicas`            |

No extra storage backend (etcd, Redis, the controller's own CRD) is needed;
the API server is the source of truth.

## Smoke test the score API directly

Matches the path used during development; no controllers needed.

```bash
# terminal 1
kubectl -n hotelres port-forward pod/search-<id> 7900:7900

# terminal 2
grpcurl -plaintext -d '{}' localhost:7900 \
  gordion.contention.ContentionStream/Subscribe
```

You should see a stream of `ScoreEvent` JSON objects at roughly 10 Hz, now
including `p50_horizons` / `tail_horizons` / `horizon_ms` once the
predictor side ships the matching change.

## Observability

JSON `log/slog` on stderr. Every action emits a single line with
`rule`, `kind`, `pod`, `node`, `applied`, `reason`, `before`, `after`, and
`err` on failure. No Prometheus exporter yet; deliberately out of scope.

## Renaming the module

The module path is `github.com/coding-workspace/simple-mitigation-1`. To
change it (e.g. to your real GitHub org):

```bash
OLD=github.com/coding-workspace/simple-mitigation-1
NEW=github.com/your-org/your-repo
grep -rl "$OLD" . --include="*.go" --include="*.proto" --include="Makefile" \
  | xargs sed -i "s|$OLD|$NEW|g"
go mod edit -module "$NEW"
make proto && go mod tidy
```
