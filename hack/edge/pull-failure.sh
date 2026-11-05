#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NS=t-pull
until [ -z "$(kubectl get patchrollouts -A --no-headers | awk '$5=="Restarting"||$5=="Selecting"||$5=="Soaking"||$5=="RollingBack"')" ]; do sleep 10; done
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null; kubectl label ns $NS copa.sh/patching=enabled --overwrite >/dev/null
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: web, copa.sh/rollout: enabled}}
    spec:
      nodeSelector: {kubernetes.io/hostname: copa-control-plane}
      tolerations: [{key: node-role.kubernetes.io/control-plane, operator: Exists, effect: NoSchedule}]
      containers: [{name: web, image: "kind-registry:5000/library/nginx:1.25.3-alpine", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}]
YAML
kubectl -n $NS rollout status deploy/web --timeout=90s >/dev/null
timeout 90 bash -c "until kubectl -n $NS get patchrollout --no-headers 2>/dev/null | grep -q AwaitingApproval; do sleep 3; done"
kubectl -n copa-registry scale deploy/registry --replicas=0 >/dev/null; timeout 60 bash -c 'while kubectl -n copa-registry get pods -l app=registry -o name | grep -q .; do sleep 2; done'
echo "registry down at $(date -u +%T); approving"
PATH=$ROOT/hack:$PATH kubectl copa approve -n $NS --all | tail -1
RO=$(kubectl -n $NS get patchrollout -o jsonpath='{.items[0].metadata.name}')
timeout 120 bash -c "until kubectl -n $NS get pods -l app=web -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}' | grep -qE 'ImagePull|ErrImage'; do sleep 3; done"
echo "new pod: $(kubectl -n $NS get pods -l app=web -o jsonpath='{.items[*].status.containerStatuses[*].state.waiting.reason}')"
timeout 600 bash -c "until kubectl -n $NS get patchrollout $RO -o jsonpath='{.status.phase}' | grep -qE 'RolledBack|RecoveryFailed'; do sleep 10; done"
kubectl -n $NS get patchrollout $RO -o jsonpath='{.status.phase}: {.status.message}{"\n"}' | cut -c1-250
echo "pods: $(kubectl -n $NS get pods -l app=web -o jsonpath='{range .items[*]}{.spec.containers[0].image} ready={.status.containerStatuses[0].ready}{" "}{end}')"
kubectl -n copa-registry scale deploy/registry --replicas=1 >/dev/null; kubectl -n copa-registry rollout status deploy/registry --timeout=120s >/dev/null; echo "registry back $(date -u +%T)"
