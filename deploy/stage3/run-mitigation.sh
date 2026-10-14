#!/bin/bash
# run-mitigation.sh <arm> <name>
#
# Launches the mitigation controller in FORMULA mode for one stage-3 phased
# run, DISARMED until $KIT/ARM appears (run-phased-stage3.sh touches it at
# t0+80 s). Run as root on the target node from the kit dir (default
# /tmp/mitig, see install-kit.sh); the orchestrator calls it via sudo.
#
# Settings come from arms/common.env, then arms/<arm>.env. Outputs land in
# the kit dir: <name>.ctrl.log, <name>.scores.csv (SCORE_TRACE) and
# <name>.env.txt (the settings this run used).
set -euo pipefail

KIT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
arm="${1:?usage: run-mitigation.sh <arm> <name>}"
name="${2:?usage: run-mitigation.sh <arm> <name>}"
[[ -f "$KIT/arms/$arm.env" ]] || { echo "no arm file $KIT/arms/$arm.env" >&2; exit 2; }

set -a
. "$KIT/arms/common.env"
. "$KIT/arms/$arm.env"
set +a

export FORMULA_MODE=1
export FORMULA_ARM_FILE="$KIT/ARM"
export NODE_NAME="${NODE_NAME:-$(hostname -s)}"
export TARGETS_CONFIG="$KIT/targets.yaml"
export POLICY_CONFIG="$KIT/policy-formula.yaml"
export KUBECONFIG="$KIT/kubeconfig-admin"
export SCORE_TRACE="$KIT/$name.scores.csv"

rm -f "$SCORE_TRACE" "$KIT/$name.ctrl.log"
{
    echo "# arm=$arm name=$name started=$(date -Is)"
    env | grep -E '^(FORMULA_|THETA_|N_MAX|K_P|K_I|CAP_|YHAT_|HORZ_|HORIZONTAL_|AGGRESSOR_|ISO_|ISOLATE_|HC_|BASELINE_|NODE_NAME|CONTROLLER_CPU)' | sort
} > "$KIT/$name.env.txt"

cd "$KIT"
exec taskset -c "${CONTROLLER_CPU:-12}" "$KIT/mitigation-controller" >> "$KIT/$name.ctrl.log" 2>&1
