#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
set -euo pipefail
NS=edge-consumer
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label ns $NS copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
for pair in web=kind-registry:5000/library/nginx:1.25.3-alpine unbuilt=kind-registry:5000/edge/unbuilt:1.27.0-alpine; do
n=${pair%%=*}; img=${pair#*=}
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: $n, namespace: $NS}
spec:
  replicas: 2
  selector: {matchLabels: {app: $n}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: $n, copa.sh/rollout: enabled}}
    spec:
      containers:
      - {name: web, image: "$img", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}
YAML
done
timeout 300 bash -c "until kubectl -n $NS get patchrollout -o jsonpath='{.items[*].status.phase}' | grep -qx Succeeded; do sleep 5; done"
kubectl -n $NS get patchrollout -o custom-columns=SRC:.spec.source,CAND:.spec.candidate.tag,PHASE:.status.phase --no-headers
kubectl get sourceimages -o custom-columns=NAME:.metadata.name,PATCH:.status.current.tag,READY:.status.conditions[0].reason,MSG:.status.conditions[0].message --no-headers | grep unbuilt
echo "patchbuilds for unbuilt: $(kubectl get patchbuilds -n copa-builder --no-headers 2>/dev/null | grep -c unbuilt || true)"
