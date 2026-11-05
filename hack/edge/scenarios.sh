#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
set -euo pipefail
SRC=kind-registry:5000/library/nginx:1.25.3-alpine
SRC_DIGEST=$(crane digest --insecure localhost:5001/library/nginx:1.25.3-alpine)
mk() { kubectl create ns "$1" --dry-run=client -o yaml | kubectl apply -f - >/dev/null; kubectl label ns "$1" copa.sh/patching=enabled --overwrite >/dev/null; [ "${2:-}" = auto ] && kubectl label ns "$1" copa.sh/approval=automatic --overwrite >/dev/null || true; }
dep() {
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: $2, namespace: $1}
spec:
  replicas: 2
  selector: {matchLabels: {app: $2}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: $2, copa.sh/rollout: enabled}}
    spec:
      containers:
      - {name: web, image: "$3", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}
YAML
}
# edge-chain: a digest reference and a tag for the same source in one namespace
mk edge-chain auto
dep edge-chain pinned "kind-registry:5000/library/nginx@${SRC_DIGEST}"
dep edge-chain tagged "$SRC"
# edge-drift: a tag that will move to a new digest
crane copy nginx:1.25.3-alpine localhost:5001/edge/app:1.0.0 2>/dev/null
mk edge-drift auto
dep edge-drift app kind-registry:5000/edge/app:1.0.0
# edge-manual2: Deployments arrive while the rollout waits for approval
mk edge-manual2
dep edge-manual2 web "$SRC"
timeout 60 bash -c 'until kubectl -n edge-manual2 get patchrollout -o name | grep -q .; do sleep 2; done'
echo "edge-manual2 rollout exists: $(kubectl -n edge-manual2 get patchrollout -o jsonpath='{.items[0].metadata.name} {.items[0].status.phase}')"
for d in web-team web-optout web-delete; do dep edge-manual2 $d "$SRC"; done
echo "created later Deployments at $(date -u +%T)"
