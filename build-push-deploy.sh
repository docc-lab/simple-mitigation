#!/bin/bash
#
# build-push-deploy.sh — one-shot build + push + deploy for the
# mitigation-controller DaemonSet. Mirrors the hotelReservation
# ims-build-push-rollout.sh flow, condensed to this repo's single image:
#
#   1. docker build the controller image (proto gen + go build happen in the
#      Dockerfile, so no host Go toolchain is needed)
#   2. push it to a registry (default docclabgroup on Docker Hub)
#   3. rewrite deploy/controller/daemonset.yaml's image + pull policy, and
#      (optionally) pin the DaemonSet to a single node via nodeSelector
#   4. kubectl apply namespace/rbac/configmap/daemonset
#   5. rollout restart + status, then tail logs and flag any startup ERROR
#
# Idempotent: safe to re-run. With a mutable tag (e.g. :dev) it forces a
# fresh pull via imagePullPolicy=Always + rollout restart.
#
# Usage:
#   ./build-push-deploy.sh [options]
#
# Options:
#   --registry=NAME     registry org/repo prefix (default: docclabgroup)
#   --image=NAME        image name             (default: mitigation-controller)
#   --tag=TAG           image tag              (default: dev)
#   --node=NODENAME     pin the DaemonSet to this node (kubernetes.io/hostname);
#                       omit to run on every node (true DaemonSet)
#   --pull-policy=P     Always | IfNotPresent | Never (default: Always)
#   --no-build          skip docker build (deploy an already-built/pushed image)
#   --no-push           skip docker push (e.g. local docker runtime, single node)
#   --no-deploy         build/push only; don't touch the cluster
#   -h | --help         show this help
#
# Examples:
#   ./build-push-deploy.sh --node=node-3                 # build+push+deploy, pinned to node-3
#   ./build-push-deploy.sh --tag=v2 --node=node-3
#   ./build-push-deploy.sh --no-build --node=node-3      # redeploy the current pushed image
#   ./build-push-deploy.sh --no-push --pull-policy=IfNotPresent  # single-node docker runtime

set -euo pipefail

# ---- Configuration (overridable via flags) ---------------------------------
REGISTRY="docclabgroup"
IMAGE_NAME="mitigation-controller"
TAG="dev"
NODE=""
PULL_POLICY="Always"
DO_BUILD=true
DO_PUSH=true
DO_DEPLOY=true

NAMESPACE="mitigation-system"
DS_NAME="mitigation-controller"
DEPLOY_DIR="deploy/controller"
DOCKERFILE="cmd/mitigation-controller/Dockerfile"
DOCKER="${DOCKER:-docker}"   # set DOCKER="sudo docker" if your daemon needs root
ROLLOUT_TIMEOUT="120s"

# ---- Logging helpers -------------------------------------------------------
log_info()    { echo -e "\n[INFO]    $(date '+%H:%M:%S') - $1"; }
log_error()   { echo -e "\n[ERROR]   $(date '+%H:%M:%S') - $1" >&2; }
log_success() { echo -e "\n[SUCCESS] $(date '+%H:%M:%S') - $1"; }

usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

# ---- Parse args ------------------------------------------------------------
for arg in "$@"; do
  case "$arg" in
    --registry=*)    REGISTRY="${arg#*=}" ;;
    --image=*)       IMAGE_NAME="${arg#*=}" ;;
    --tag=*)         TAG="${arg#*=}" ;;
    --node=*)        NODE="${arg#*=}" ;;
    --pull-policy=*) PULL_POLICY="${arg#*=}" ;;
    --no-build)      DO_BUILD=false ;;
    --no-push)       DO_PUSH=false ;;
    --no-deploy)     DO_DEPLOY=false ;;
    -h|--help)       usage 0 ;;
    *) log_error "Unknown option: $arg"; usage 1 ;;
  esac
done

# Run from the repo root (script lives there) so relative paths resolve.
cd "$(dirname "$0")"

FULL_IMAGE="${REGISTRY}/${IMAGE_NAME}:${TAG}"
DS_YAML="${DEPLOY_DIR}/daemonset.yaml"

echo "=========================================="
echo "mitigation-controller build-push-deploy"
echo "  image:        ${FULL_IMAGE}"
echo "  pull policy:  ${PULL_POLICY}"
echo "  node pin:     ${NODE:-<none> (all nodes)}"
echo "  build/push:   ${DO_BUILD}/${DO_PUSH}   deploy: ${DO_DEPLOY}"
echo "=========================================="

# ---- Phase 1: build --------------------------------------------------------
if [ "$DO_BUILD" = true ]; then
  log_info "Phase 1: building image ${FULL_IMAGE}"
  $DOCKER build -f "$DOCKERFILE" -t "$FULL_IMAGE" .
  log_success "Built ${FULL_IMAGE}"
else
  log_info "Phase 1: skipped (--no-build)"
fi

# ---- Phase 2: push ---------------------------------------------------------
if [ "$DO_PUSH" = true ]; then
  log_info "Phase 2: pushing ${FULL_IMAGE} (run 'docker login' first if needed)"
  $DOCKER push "$FULL_IMAGE"
  log_success "Pushed ${FULL_IMAGE}"
else
  log_info "Phase 2: skipped (--no-push)"
fi

if [ "$DO_DEPLOY" = false ]; then
  log_success "Done (--no-deploy): image ${FULL_IMAGE} ready."
  exit 0
fi

# ---- Phase 3: rewrite manifest ---------------------------------------------
log_info "Phase 3: updating ${DS_YAML}"
[ -f "$DS_YAML" ] || { log_error "manifest not found: $DS_YAML"; exit 1; }
cp "$DS_YAML" "${DS_YAML}.backup"

# Image line: replace whatever is there (placeholder or a previous value).
sed -i "/^[[:space:]]*image:/s|image:.*|image: ${FULL_IMAGE}|" "$DS_YAML"
# Pull policy.
sed -i "s|imagePullPolicy:.*|imagePullPolicy: ${PULL_POLICY}|" "$DS_YAML"

# Node pin (idempotent): set/insert nodeSelector: kubernetes.io/hostname.
if [ -n "$NODE" ]; then
  if grep -qE 'kubernetes\.io/hostname:' "$DS_YAML"; then
    sed -i "s|kubernetes.io/hostname:.*|kubernetes.io/hostname: ${NODE}|" "$DS_YAML"
  else
    sed -i "/serviceAccountName: ${DS_NAME}/a\\      nodeSelector:\n        kubernetes.io/hostname: ${NODE}" "$DS_YAML"
  fi
fi

grep -E 'image:|imagePullPolicy:|hostname:' "$DS_YAML" || true

# ---- Phase 4: apply --------------------------------------------------------
log_info "Phase 4: applying manifests"
kubectl apply -f "${DEPLOY_DIR}/namespace.yaml"
kubectl apply -f "${DEPLOY_DIR}/rbac.yaml"
kubectl apply -f "${DEPLOY_DIR}/configmap.yaml"
kubectl apply -f "$DS_YAML"

# Force a fresh pull/restart so a re-pushed mutable tag is actually picked up.
log_info "Restarting DaemonSet to pick up the image"
kubectl -n "$NAMESPACE" rollout restart "ds/${DS_NAME}"

# ---- Phase 5: rollout + verify ---------------------------------------------
log_info "Phase 5: waiting for rollout (timeout ${ROLLOUT_TIMEOUT})"
if ! kubectl -n "$NAMESPACE" rollout status "ds/${DS_NAME}" --timeout="$ROLLOUT_TIMEOUT"; then
  log_error "Rollout did not complete; recent pods + logs follow:"
  kubectl -n "$NAMESPACE" get pods -o wide || true
  kubectl -n "$NAMESPACE" logs -l "app.kubernetes.io/name=${DS_NAME}" --tail=40 || true
  exit 1
fi

kubectl -n "$NAMESPACE" get pods -o wide

log_info "Recent controller logs:"
logs="$(kubectl -n "$NAMESPACE" logs -l "app.kubernetes.io/name=${DS_NAME}" --tail=30 2>/dev/null || true)"
echo "$logs"

if echo "$logs" | grep -q '"level":"ERROR"'; then
  log_error "Controller logged an ERROR (e.g. a policy rule that failed to compile). Check above."
  exit 1
fi

log_success "mitigation-controller deployed: ${FULL_IMAGE}${NODE:+ on node ${NODE}}"
echo ""
echo "Tail logs:   kubectl -n ${NAMESPACE} logs -l app.kubernetes.io/name=${DS_NAME} -f"
echo "Watch acts:  kubectl -n ${NAMESPACE} logs -l app.kubernetes.io/name=${DS_NAME} -f | grep '\"msg\":\"action\"'"
