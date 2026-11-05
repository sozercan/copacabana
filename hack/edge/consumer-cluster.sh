#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
K="kubectl --context kind-copa-consumer"
$K create ns c-app --dry-run=client -o yaml | $K apply -f - >/dev/null
$K label ns c-app copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
for pair in web=kind-registry:5000/library/nginx:1.25.3-alpine only=kind-registry:5000/consumer-only/nginx:1.27.0-alpine; do
n=${pair%%=*}; img=${pair#*=}
cat <<YAML | $K apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: $n, namespace: c-app}
spec:
  replicas: 2
  selector: {matchLabels: {app: $n}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: $n, copa.sh/rollout: enabled}}
    spec:
      containers: [{name: web, image: "$img", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}]
YAML
done
timeout 400 bash -c "until $K -n c-app get patchrollout --no-headers 2>/dev/null | grep -qE 'Succeeded|RolledBack|Recovery'; do sleep 10; done" || echo TIMEOUT
$K -n c-app get patchrollout -o custom-columns=SRC:.spec.source,CAND:.spec.candidate.tag,PHASE:.status.phase --no-headers
$K get sourceimages -o custom-columns=REPO:.spec.repository,PATCH:.status.current.tag,READY:.status.conditions[0].reason --no-headers
echo "patchbuilds in consumer: $($K get patchbuilds -A --no-headers 2>/dev/null | wc -l); jobs: $($K get jobs -A --no-headers 2>/dev/null | wc -l); signing key secret: $($K -n copa-verifier get secret copa-signing-key -o name 2>&1 | grep -c '^secret')"
echo "web pods: $($K -n c-app get pods -l app=web -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{" "}{end}' | sed 's|registry.copa-registry.svc.cluster.local:5000/patched/[^@]*|PATCHED|g; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|g')"
echo "only pods: $($K -n c-app get pods -l app=only -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}')"
