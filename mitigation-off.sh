#!/bin/bash
#
# mitigation-off.sh — turn the mitigation-controller off and revert anything it
# changed, so you can debug the testbed with no interference.
#
# What mitigation changes, and how this reverts each:
#   • horizontal : patches Deployment /scale + annotates baseline replicas.
#                  -> read mitigation/horizontal-baseline-replicas, scale back,
#                     strip the horizontal annotations. (pure K8s API)
#   • isolate    : writes cpu.max on aggressor pods + annotates the original.
#   • harvest    : writes cpu.max on best-effort pods + annotates the original.
#                  -> recreate the annotated pods so their owner restarts them
#                     with the spec cpu.max and clean annotations. (cgroup writes
#                     live on the node; recreating is the reliable reset.)
#   • vertical   : pods/resize (no-op on K8s < 1.33); if any victim carries
#                  mitigation/cpu-limit-baseline it's reported / recreated too.
#
# Order matters: the controller is stopped FIRST so it can't re-apply while we
# revert.
#
# Usage:
#   ./mitigation-off.sh [options]
#
# Options:
#   --no-restart-pods   don't delete cgroup-annotated pods; just report them
#                       (their cpu.max stays modified until they restart)
#   --purge             also delete the namespace + ClusterRole/Binding
#   --dry-run           print what would happen, change nothing
#   -h | --help         show this help
#
# Re-enable later with:  ./build-push-deploy.sh --no-build --node=node-3

set -uo pipefail

NAMESPACE="mitigation-system"
DS_NAME="mitigation-controller"
RESTART_PODS=true
PURGE=false
DRYRUN=false

log_info()    { echo -e "\n[INFO]    $(date '+%H:%M:%S') - $1"; }
log_error()   { echo -e "\n[ERROR]   $(date '+%H:%M:%S') - $1" >&2; }
log_success() { echo -e "\n[SUCCESS] $(date '+%H:%M:%S') - $1"; }
usage() { sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

for arg in "$@"; do
  case "$arg" in
    --no-restart-pods) RESTART_PODS=false ;;
    --purge)           PURGE=true ;;
    --dry-run)         DRYRUN=true ;;
    -h|--help)         usage 0 ;;
    *) log_error "Unknown option: $arg"; usage 1 ;;
  esac
done

# run wraps mutating commands so --dry-run is a no-op preview.
run() { if $DRYRUN; then echo "DRY-RUN: $*"; else "$@"; fi; }

# pods_with_annotation <key> -> lines of "<ns> <name>" for pods carrying it.
pods_with_annotation() {
  local key="$1"
  kubectl get pods -A -o go-template='{{range .items}}{{if .metadata.annotations}}{{if index .metadata.annotations "'"$key"'"}}{{.metadata.namespace}}{{" "}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}{{end}}' 2>/dev/null
}

# deploys_with_baseline -> lines of "<ns> <name> <baseline-replicas>".
deploys_with_baseline() {
  kubectl get deploy -A -o go-template='{{range .items}}{{if .metadata.annotations}}{{$b := index .metadata.annotations "mitigation/horizontal-baseline-replicas"}}{{if $b}}{{.metadata.namespace}}{{" "}}{{.metadata.name}}{{" "}}{{$b}}{{"\n"}}{{end}}{{end}}{{end}}' 2>/dev/null
}

echo "=========================================="
echo "mitigation-off  (dry-run: ${DRYRUN}, restart-pods: ${RESTART_PODS}, purge: ${PURGE})"
echo "=========================================="

# ---- Phase 1: stop the controller -----------------------------------------
log_info "Phase 1: stopping the controller (delete DaemonSet) so it can't re-apply"
run kubectl -n "$NAMESPACE" delete daemonset "$DS_NAME" --ignore-not-found

# ---- Phase 2: revert horizontal scaling (API) -----------------------------
log_info "Phase 2: reverting horizontal scaling"
horiz_count=0
baselines="$(deploys_with_baseline)"
if [ -n "$baselines" ]; then
  while read -r ns name b; do
    [ -z "${ns:-}" ] && continue
    log_info "  scaling $ns/$name back to $b replicas"
    run kubectl -n "$ns" scale deploy "$name" --replicas="$b"
    run kubectl -n "$ns" annotate deploy "$name" \
      mitigation/horizontal-baseline-replicas- mitigation/horizontal-last-scaled-at- --overwrite
    horiz_count=$((horiz_count + 1))
  done <<< "$baselines"
fi
[ "$horiz_count" -eq 0 ] && log_info "  no deployments carry a horizontal baseline annotation"

# ---- Phase 3: revert cgroup mitigations (isolate + harvest) ----------------
log_info "Phase 3: reverting cgroup cpu.max changes (isolate + harvest)"
cgroup_pods="$( { pods_with_annotation "mitigation/cpu-max-set-by-node"; \
                  pods_with_annotation "mitigation/harvest-set-by-node"; \
                  pods_with_annotation "mitigation/cpu-limit-baseline"; } | sort -u )"
cg_count=0
if [ -n "$cgroup_pods" ]; then
  while read -r ns name; do
    [ -z "${ns:-}" ] && continue
    cg_count=$((cg_count + 1))
    if $RESTART_PODS; then
      log_info "  recreating $ns/$name (owner restarts it with original cpu.max)"
      run kubectl -n "$ns" delete pod "$name"
    else
      log_error "  $ns/$name still has modified cpu.max — NOT reset (--no-restart-pods). Restart it to clear."
    fi
  done <<< "$cgroup_pods"
fi
[ "$cg_count" -eq 0 ] && log_info "  no pods carry an isolate/harvest/vertical annotation"

# ---- Phase 4: optional purge ----------------------------------------------
if $PURGE; then
  log_info "Phase 4: purging namespace + cluster RBAC"
  run kubectl delete namespace "$NAMESPACE" --ignore-not-found
  run kubectl delete clusterrole "$DS_NAME" --ignore-not-found
  run kubectl delete clusterrolebinding "$DS_NAME" --ignore-not-found
fi

# ---- Verify ----------------------------------------------------------------
log_info "Verifying nothing remains annotated..."
remaining="$( { pods_with_annotation "mitigation/cpu-max-set-by-node"; \
                pods_with_annotation "mitigation/harvest-set-by-node"; \
                pods_with_annotation "mitigation/cpu-limit-baseline"; \
                deploys_with_baseline; } )"
if [ -n "$remaining" ] && ! $DRYRUN; then
  if $RESTART_PODS; then
    log_info "Some objects still show annotations (pods may still be terminating/recreating):"
  else
    log_info "Remaining (expected, since --no-restart-pods):"
  fi
  echo "$remaining"
else
  log_success "No mitigation annotations remain."
fi

log_success "Mitigation off. Reverted: ${horiz_count} deployment(s), ${cg_count} pod(s)."
echo "Re-enable: ./build-push-deploy.sh --no-build --node=node-3"
