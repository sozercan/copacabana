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
# Private sources aren't patched: their patched copies would be readable without credentials.
timeout 120 bash -c "until kubectl -n $NS get events --field-selector reason=PrivateSource,involvedObject.name=web -o name | grep -q .; do sleep 5; done" || echo TIMEOUT
kubectl -n $NS get events --field-selector reason=PrivateSource -o custom-columns=OBJ:.involvedObject.name,MSG:.message --no-headers | sort -u
kubectl -n $NS get events --field-selector reason=ImageNotResolved -o custom-columns=OBJ:.involvedObject.name,MSG:.message --no-headers | sort -u | cut -c1-160
echo "source images from private-registry: $(kubectl get sourceimages -o jsonpath='{range .items[*]}{.spec.repository}{"\n"}{end}' | grep -c private-registry)"
