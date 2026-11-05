#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
# Docker Hub short names, long forms, and tag@digest, all in one namespace.
NS=${NS:-t-hub}
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label ns $NS copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
D=$(crane digest nginx:1.25.4-alpine)
i=0
for img in nginx:1.25.4-alpine docker.io/library/nginx:1.25.4-alpine index.docker.io/library/nginx:1.25.4-alpine "nginx:1.25.4-alpine@$D"; do
i=$((i+1))
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: hub$i, namespace: $NS, annotations: {image: "$img"}}
spec:
  replicas: 1
  selector: {matchLabels: {app: hub$i}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: hub$i, copa.sh/rollout: enabled}}
    spec:
      containers:
      - {name: web, image: "$img", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}
YAML
done
timeout 600 bash -c "until [ \$(kubectl -n $NS get patchrollout --no-headers 2>/dev/null | grep -cE 'Succeeded|RolledBack|Failed') -ge 1 ] && ! kubectl -n $NS get patchrollout --no-headers | grep -qvE 'Succeeded|RolledBack|RecoveryFailed'; do sleep 10; done" || echo TIMEOUT
kubectl get sourceimages -o custom-columns=NAME:.metadata.name,REPO:.spec.repository,REFS:.status.references,PATCH:.status.current.tag,READY:.status.conditions[0].reason --no-headers | grep -v kind-registry
kubectl -n $NS get patchrollout -o custom-columns=SRC:.spec.source,CAND:.spec.candidate.tag,PHASE:.status.phase --no-headers
kubectl -n $NS get swaplist copa -o jsonpath='{range .spec.entries[*]}{.source} aliases={.aliases}{"\n"}{end}'
for n in 1 2 3 4; do echo "hub$n ($(kubectl -n $NS get deploy hub$n -o jsonpath='{.metadata.annotations.image}' | cut -c1-60)): $(kubectl -n $NS get pods -l app=hub$n -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | sed 's|registry.copa-registry.svc.cluster.local:5000/||g; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|g')"; done
