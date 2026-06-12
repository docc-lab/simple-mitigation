# simple-mitigation

Two K8s controllers that consume the per-pod `ContentionStream` gRPC API
(see [`Mitigation-interface.md`](Mitigation-interface.md)) and react to
contention by:

| Controller                | Surface                            | Cadence | Scope                          |
| ------------------------- | ---------------------------------- | ------- | ------------------------------ |
| `horizontal-cpa-sidecar`  | Deployment replicas (via CPA)      | ~1 s    | One CPA Pod per victim Deploy. |
| `vertical-scaler`         | `pods/resize` subresource (cpu)    | 100 ms  | One Deploy, multi-victim       |

The third mitigation from the plan (an isolating DaemonSet) is intentionally
out of scope for now.

## Architecture

```
   victim pod   100 ms      sidecar (in CPA pod)        1 s
   :7900  ───gRPC stream──▶  /metric  /evaluate  ◀──HTTP──  CPA bin ──scale──▶ Deployment
   :7900  ───gRPC stream──▶  vertical-scaler           ──PATCH pods/resize──▶ each pod
```

Both controllers share `pkg/scoreclient`, `pkg/podwatch`, `pkg/thresholder`,
and `pkg/aggregator`. The wire contract is vendored in
[`proto/contention.proto`](proto/contention.proto); Go stubs are generated
into `gen/go/contentionpb/` (gitignored) by `make proto`.

## Repo layout

```
proto/contention.proto                  vendored wire contract
gen/go/contentionpb/                    generated (gitignored) -- run `make proto`
pkg/targets/                            multi-victim config loader
pkg/scoreclient/                        gRPC subscriber w/ reconnect + multi-pod fan-in
pkg/aggregator/                         pluggable Max / Mean / P90 policy
pkg/thresholder/                        HI/LO + cooldown state machine
pkg/podwatch/                           client-go informer for a single target
cmd/horizontal-cpa-sidecar/             /metric + /evaluate HTTP server for CPA
cmd/vertical-scaler/                    multi-target pods/resize controller
deploy/horizontal/                      CustomPodAutoscaler CRs + RBAC
deploy/vertical/                        Deployment + RBAC + targets ConfigMap
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
make test         # runs the thresholder unit tests
```

To produce container images:

```bash
make docker-horizontal
make docker-vertical
```

The Dockerfiles run `make proto` inside the build stage so a fresh clone
builds end-to-end with `docker build` alone.

## Default thresholds (explicit)

Baked into both binaries as constants; every value is overridable by env
(horizontal) or env + ConfigMap (vertical). See the `defaultXxx` constants
in each `main.go` and the env table in `deploy/`.

| Param                  | Horizontal | Vertical |
| ---------------------- | ---------: | -------: |
| HI (p50 fire-up)       |        0.5 |      0.5 |
| LO (p50 fire-down)     |        0.2 |      0.2 |
| MIN_HOLD_WINDOWS       |          3 |        5 |
| COOLDOWN_SEC           |         30 |       60 |
| MIN_REPLICAS / MIN_CPU |          1 |     200m |
| MAX_REPLICAS / MAX_CPU |         10 |       4  |
| SCALE_UP_FACTOR        |          - |      1.5 |
| SCALE_DOWN_FACTOR      |          - |     0.75 |
| DOWNSCALE_BLOCK_TAIL   |        0.5 |      0.5 |
| AGG                    |        max |        - |

`p50_trend_pred` drives every scaling decision. `tail_trend_label` is
recorded on every log line and used as a downscale-safety guard: the
controller refuses to shrink while `tail > DOWNSCALE_BLOCK_TAIL`.

## Deploy

Prerequisite: K8s >= 1.35 (in-place pod resize GA -- see
<https://kubernetes.io/blog/2025/12/19/kubernetes-v1-35-in-place-pod-resize-ga/>).

### Sample victims

```bash
kubectl apply -f deploy/victim-sample/namespace.yaml
kubectl apply -f deploy/victim-sample/search.yaml
kubectl apply -f deploy/victim-sample/profile.yaml
```

Replace the placeholder `image: REGISTRY/...:tag` lines with your real
images. The container fields that matter for the mitigations to work:
named `score` port 7900, `resources.requests == resources.limits`,
`resizePolicy.cpu = NotRequired`.

### Vertical scaler

```bash
kubectl apply -f deploy/vertical/rbac.yaml
kubectl apply -f deploy/vertical/configmap.yaml
kubectl apply -f deploy/vertical/deployment.yaml
```

Adding a new victim service later is a single ConfigMap edit:

```bash
kubectl -n mitigation-system edit cm vertical-scaler-targets
# kubectl rollout restart deploy/vertical-scaler -n mitigation-system  # optional
```

### Horizontal scaler

Prerequisite: install the CPA operator
([repo](https://github.com/jthomperoo/custom-pod-autoscaler-operator)):

```bash
VERSION=v1.4.0
kubectl apply -f https://github.com/jthomperoo/custom-pod-autoscaler-operator/releases/download/${VERSION}/cluster.yaml
```

Then per victim:

```bash
kubectl apply -f deploy/horizontal/rbac.yaml
kubectl apply -f deploy/horizontal/cpa-search.yaml
kubectl apply -f deploy/horizontal/cpa-profile.yaml
```

Adding another victim = `cp cpa-search.yaml cpa-newvictim.yaml`, change
`metadata.name`, `scaleTargetRef.name`, and `TARGET_SELECTOR`.

## Smoke test the score API directly

Matches the path used during development; no controllers needed.

```bash
# terminal 1
kubectl -n hotelres port-forward pod/search-<id> 7900:7900

# terminal 2
grpcurl -plaintext -d '{}' localhost:7900 \
  gordion.contention.ContentionStream/Subscribe
```

You should see a stream of `ScoreEvent` JSON objects at roughly 10 Hz.

## Observability

Both binaries log JSON to stderr via `log/slog`. Every action-decision log
line carries `p50_agg`/`p50`, `tail_max`/`tail`, `dir`, `act`, the
before/after value, and the reason if an action was suppressed (cooldown,
tail safety, or pod-side infeasibility). No Prometheus exporter yet; that's
deliberately out of scope for this pass.

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
