#!/usr/bin/env bash
# Creates workloads that exercise edge cases. Run after `make demo` has promoted a patch for nginx:1.25.3-alpine.
# Then run scenarios.sh, manual.sh, chain.sh, drift.sh, missed-swap-and-guard.sh and guard.sh. consumer.sh needs
# MODE=consumer make deploy first. Forged patches: go run ./hack/forge.
#   edge-health   automatic approval; readiness passes only on the unpatched image -> rollback to pinned source
#   edge-elig     automatic approval; Recreate and probe-less Deployments (ineligible), and a Deployment whose
#                 init container pins the source by digest while the main container uses the tag
#   edge-manual   manual approval; used for wrong-digest approval, team deploy, opt-out and delete mid-rollout
#   edge-misc     images that can't be patched: no OS (pause) and a platform the cluster doesn't patch (arm64 only)
set -euo pipefail
K="kubectl --context ${KCTX:-kind-copa}"
SRC=kind-registry:5000/library/nginx:1.25.3-alpine
SRC_DIGEST=$(crane digest --insecure localhost:5001/library/nginx:1.25.3-alpine)

crane copy registry.k8s.io/pause:3.10 localhost:5001/edge/pause:3.10.0
crane copy --platform linux/arm64 nginx:1.25.3-alpine localhost:5001/edge/arm-only:1.25.3-alpine

ns() {
  $K create namespace "$1" --dry-run=client -o yaml | $K apply -f -
  $K label namespace "$1" copa.sh/patching=enabled --overwrite
  if [ "${2:-}" = auto ]; then $K label namespace "$1" copa.sh/approval=automatic --overwrite; fi
}

# deploy NS NAME IMAGE [strategy-yaml] [readiness-yaml] [extra pod spec yaml]
deploy() {
  local ns=$1 name=$2 image=$3
  local strategy=${4:-'{type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}'}
  local readiness=${5:-'{httpGet: {path: /, port: 80}, periodSeconds: 3}'}
  local extra=${6:-}
  cat <<YAML | $K apply -f -
apiVersion: apps/v1
kind: Deployment
metadata: {name: ${name}, namespace: ${ns}}
spec:
  replicas: 2
  selector: {matchLabels: {app: ${name}}}
  strategy: ${strategy}
  template:
    metadata:
      labels: {app: ${name}, copa.sh/rollout: enabled}
    spec:
${extra}
      containers:
      - name: web
        image: ${image}
        readinessProbe: ${readiness}
YAML
}

ns edge-health auto
deploy edge-health fragile "$SRC" "" \
  '{exec: {command: [sh, -c, "apk info -v 2>/dev/null | grep -qx libcrypto3-3.1.4-r5"]}, periodSeconds: 3}'

ns edge-elig auto
deploy edge-elig recreate "$SRC" '{type: Recreate}'
deploy edge-elig noprobe "$SRC" "" 'null'
deploy edge-elig initdigest "$SRC" "" "" "      initContainers:
      - {name: init, image: kind-registry:5000/library/nginx@${SRC_DIGEST}, command: [sh, -c, 'apk info -v | grep libcrypto3']}"

ns edge-manual
for d in web web-team web-optout web-delete; do deploy edge-manual "$d" "$SRC"; done

ns edge-misc auto
deploy edge-misc pause kind-registry:5000/edge/pause:3.10.0 "" 'null'
deploy edge-misc arm kind-registry:5000/edge/arm-only:1.25.3-alpine "" 'null'
