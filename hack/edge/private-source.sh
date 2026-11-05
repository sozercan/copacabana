#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NS=t-priv
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null; kubectl label ns $NS copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
# the team's own pull secret, so nodes can pull the private source
kubectl -n $NS create secret docker-registry pull --docker-server=private-registry:5000 --docker-username=team --docker-password="$(cat ${PRIVREG_PW_FILE:-$ROOT/hack/.keys/privreg/pw})" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
for pair in web=private-registry:5000/team/nginx:1.25.3-alpine typo=private-registry:5000/team/nginx:does-not-exist; do
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
      imagePullSecrets: [{name: pull}]
      containers: [{name: web, image: "$img", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}]
YAML
done
timeout 900 bash -c "until kubectl -n $NS get patchrollout --no-headers 2>/dev/null | grep -qE 'Succeeded|RolledBack|Recovery'; do sleep 10; done" || echo TIMEOUT
kubectl get patchbuilds -n copa-builder -o json | python3 -c '
import json,sys
for i in json.load(sys.stdin)["items"]:
    if "private-registry" in i["spec"]["repository"]: print("build:", i["spec"]["reason"], i["status"].get("phase"), i["status"].get("promoted",{}).get("tag",""), i["status"].get("message","")[:150])'
kubectl -n $NS get patchrollout -o custom-columns=SRC:.spec.source,CAND:.spec.candidate.tag,PHASE:.status.phase --no-headers
echo "web pods: $(kubectl -n $NS get pods -l app=web -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | sed 's|registry.copa-registry.svc.cluster.local:5000/patched/|PATCHED:|g; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|g')"
echo "typo event: $(kubectl -n $NS get events --field-selector reason=ImageNotResolved -o jsonpath='{.items[0].message}' | cut -c1-200)"
