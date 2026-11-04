#!/usr/bin/env bash
# Sets up two opted-in namespaces running an old nginx image:
#   demo-dev   approves patches automatically
#   demo-prod  needs a PatchApproval (kubectl copa approve)
set -euo pipefail
cd "$(dirname "$0")/.."
KCTX=${KCTX:-kind-copa}
K="kubectl --context ${KCTX}"
SOURCE_TAG=${SOURCE_TAG:-1.25.3-alpine}

# Seed the local registry with an old upstream image, plus a floating tag pointing at the same digest.
crane copy "nginx:${SOURCE_TAG}" "localhost:5001/library/nginx:${SOURCE_TAG}"
crane tag "localhost:5001/library/nginx:${SOURCE_TAG}" 1.25-alpine

deploy() {
  local ns=$1 name=$2 image=$3
  cat <<YAML | $K apply -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${name}
  namespace: ${ns}
spec:
  replicas: 2
  selector:
    matchLabels: {app: ${name}}
  strategy:
    type: RollingUpdate
    rollingUpdate: {maxUnavailable: 0, maxSurge: 1}
  template:
    metadata:
      labels: {app: ${name}, copa.sh/rollout: enabled}
    spec:
      containers:
      - name: web
        image: ${image}
        ports: [{containerPort: 80}]
        readinessProbe:
          httpGet: {path: /, port: 80}
          periodSeconds: 3
YAML
}

for ns in demo-dev demo-prod; do
  $K create namespace "$ns" --dry-run=client -o yaml | $K apply -f -
  $K label namespace "$ns" copa.sh/patching=enabled --overwrite
done
$K label namespace demo-dev copa.sh/approval=automatic --overwrite

deploy demo-dev web "kind-registry:5000/library/nginx:${SOURCE_TAG}"
deploy demo-dev web-floating "kind-registry:5000/library/nginx:1.25-alpine"
deploy demo-prod web "kind-registry:5000/library/nginx:${SOURCE_TAG}"

cat <<MSG

Demo workloads created. Watch progress with:
  kubectl get sourceimages
  kubectl get patchbuilds -n copa-builder -w
  kubectl get patchrollouts -A -w
Approve production:
  PATH=\$PWD/hack:\$PATH kubectl copa approve -n demo-prod --all
Fail a rollout as a team (incident back-out):
  kubectl -n demo-dev annotate deploy web copa.sh/fail-rollout=true
MSG
