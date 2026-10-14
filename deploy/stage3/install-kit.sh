#!/bin/bash
# install-kit.sh [target-node] [kit-dir]
#
# Builds the mitigation controller on this host (node-0) and installs the
# stage-3 controller kit on the target node (default node-4:/tmp/mitig):
#   mitigation-controller, run-mitigation.sh, arms/*.env, targets.yaml,
#   policy-formula.yaml, kubeconfig-admin
# /tmp is node-local and cleared on reboot: re-run after a reboot, and after
# every controller code change. Needs go (>= 1.23) and protoc on PATH; uses
# your ssh agent to reach the target node.
set -euo pipefail

NODE="${1:-node-4}"
KIT="${2:-/tmp/mitig}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HERE="$REPO/deploy/stage3"
BUILD="$(mktemp -d)"
trap 'rm -rf "$BUILD"' EXIT

command -v go >/dev/null     || { echo "go not on PATH" >&2; exit 1; }
command -v protoc >/dev/null || { echo "protoc not on PATH (apt install protobuf-compiler)" >&2; exit 1; }

echo "[kit] building controller from $REPO"
cd "$REPO"
export PATH="$PATH:$(go env GOPATH)/bin"
command -v protoc-gen-go >/dev/null && command -v protoc-gen-go-grpc >/dev/null || make deps
make proto
CGO_ENABLED=0 go build -o "$BUILD/mitigation-controller" ./cmd/mitigation-controller

cp "$HERE/run-mitigation.sh" "$BUILD/"
cp -r "$HERE/arms" "$BUILD/"
cp "$REPO/sim/real/targets.yaml" "$BUILD/targets.yaml"
cp "$REPO/sim/offline/policy-formula.yaml" "$BUILD/policy-formula.yaml"
if [[ -r "$HOME/.kube/config" ]]; then
    cp "$HOME/.kube/config" "$BUILD/kubeconfig-admin"
else
    sudo cat /etc/kubernetes/admin.conf > "$BUILD/kubeconfig-admin"
fi
chmod +x "$BUILD/run-mitigation.sh" "$BUILD/mitigation-controller"
# mktemp -d is 0700 and tar applies "."'s mode to the kit dir; the
# orchestrator reads the kit and its logs as the normal user.
chmod 755 "$BUILD"
chmod 600 "$BUILD/kubeconfig-admin"

echo "[kit] installing to $NODE:$KIT"
tar -C "$BUILD" -czf - . | ssh "$NODE" "sudo mkdir -p '$KIT' && sudo tar -C '$KIT' -xzf - && sudo chown -R root:root '$KIT'"
ssh "$NODE" "ls -la '$KIT' '$KIT/arms'"
echo "[kit] done"
