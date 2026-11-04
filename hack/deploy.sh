#!/usr/bin/env bash
# Builds the images, pushes them to the local registry, and installs everything into the kind cluster.
set -euo pipefail
cd "$(dirname "$0")/.."
KCTX=${KCTX:-kind-copa}
MODE=${MODE:-builder}
K="kubectl --context ${KCTX}"

make images

# The in-cluster names use kind-registry:5000; pin by digest so Jobs never run a stale worker.
ctrl_digest=$(crane digest --insecure localhost:5001/copa/copa-rollout-controller:dev)
worker_digest=$(crane digest --insecure localhost:5001/copa/copa-worker:dev)
CONTROLLER_IMAGE="kind-registry:5000/copa/copa-rollout-controller@${ctrl_digest}"
WORKER_IMAGE="kind-registry:5000/copa/copa-worker@${worker_digest}"

# Signing key for the verify Job, public key for the controller. Generated once.
mkdir -p hack/.keys
if [ ! -f hack/.keys/signing.key ]; then
  go run ./cmd/worker keygen --out hack/.keys
fi

$K apply -f config/crd
$K apply -f config/manager/namespaces.yaml
$K -n copa-verifier create secret generic copa-signing-key --from-file=signing.key=hack/.keys/signing.key \
  --dry-run=client -o yaml | $K apply -f -
$K -n copa-system create secret generic copa-attestation-pubkey --from-file=signing.pub=hack/.keys/signing.pub \
  --dry-run=client -o yaml | $K apply -f -
$K apply -f config/rbac
$K apply -f config/builder
$K apply -f config/admission
sed -e "s|CONTROLLER_IMAGE|${CONTROLLER_IMAGE}|" -e "s|WORKER_IMAGE|${WORKER_IMAGE}|" -e "s|MODE|${MODE}|" \
  config/manager/controller.yaml | $K apply -f -

$K -n copa-builder rollout status deploy/trivy-server --timeout=10m
$K -n copa-system rollout status deploy/copa-rollout-controller --timeout=3m
echo "deployed in ${MODE} mode"
