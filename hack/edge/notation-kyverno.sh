#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd $ROOT
hack/edge/kyverno-policy.sh 2>&1 | tail -1
try() { if out=$("$@" 2>&1); then echo "ADMITTED  $(echo "$out" | sed 's|registry.copa-registry.svc.cluster.local:5000/patched/|PATCHED:|; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|')"; else echo "DENIED    $(echo "$out" | grep -oE 'failed to verify image[^:]*: [^.]{0,120}|no signature[^.]{0,80}|signature[^.]{0,100}|Error[^.]{0,140}' | head -1)"; fi; }
img() { echo -n "-o jsonpath={.spec.containers[0].image}"; }
echo "1. t-priv new pod, swapped to the Notation-signed patch:"
printf '   '; try kubectl -n t-priv run sig1 --image=private-registry:5000/team/nginx:1.25.3-alpine --labels=copa.sh/rollout=enabled --overrides='{"spec":{"imagePullSecrets":[{"name":"pull"}]}}' --dry-run=server -o jsonpath='{.spec.containers[0].image}'
echo "2. t-mp new pod, swapped to an unsigned patch (built before signing was on):"
printf '   '; try kubectl -n t-mp run sig2 --image=kind-registry:5000/mp/nginx:1.25.5-alpine --labels=copa.sh/rollout=enabled --dry-run=server -o jsonpath='{.spec.containers[0].image}'
UNSIGNED=$(kubectl get sourceimages -o json | python3 -c 'import json,sys; [print(i["status"]["current"]["ref"]) for i in json.load(sys.stdin)["items"] if "mp/nginx" in i["spec"]["repository"] and i["status"].get("current")]')
echo "3. pod naming the unsigned patched digest directly, no opt-in label:"
printf '   '; try kubectl -n default run sig3 --image=$UNSIGNED --dry-run=server -o jsonpath='{.spec.containers[0].image}'
echo "4. pod on a plain source image (not under patched/):"
printf '   '; try kubectl -n default run sig4 --image=kind-registry:5000/library/nginx:1.25.3-alpine --dry-run=server -o jsonpath='{.spec.containers[0].image}'
echo "5. new namespace on the signed private source: rollout (controller dry run goes through Kyverno too)"
kubectl create ns t-sig --dry-run=client -o yaml | kubectl apply -f - >/dev/null; kubectl label ns t-sig copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
kubectl -n t-sig create secret docker-registry pull --docker-server=private-registry:5000 --docker-username=team --docker-password="$(cat ${PRIVREG_PW_FILE:-$ROOT/hack/.keys/privreg/pw})" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: t-sig}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: web, copa.sh/rollout: enabled}}
    spec:
      imagePullSecrets: [{name: pull}]
      containers: [{name: web, image: "private-registry:5000/team/nginx:1.25.3-alpine", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}]
YAML
timeout 400 bash -c "until kubectl -n t-sig get patchrollout --no-headers 2>/dev/null | grep -qE 'Soaking|Succeeded|RolledBack|Recovery'; do sleep 5; done" || echo TIMEOUT
echo "   $(kubectl -n t-sig get patchrollout --no-headers | awk '{print $3,$4}') pods: $(kubectl -n t-sig get pods -l app=web -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | sed 's|registry.copa-registry.svc.cluster.local:5000/patched/|PATCHED:|g; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|g')"
echo "6. t-mp: what happens to a workload whose SwapList points at an unsigned patch when it scales up"
kubectl -n t-mp scale deploy web --replicas=3 >/dev/null; sleep 15
echo "   replicas ready: $(kubectl -n t-mp get deploy web -o jsonpath='{.status.readyReplicas}/{.spec.replicas}'); RS event: $(kubectl -n t-mp get events --field-selector reason=FailedCreate -o jsonpath='{.items[-1:].message}' | grep -oE 'admission webhook[^:]*|failed to verify[^:]*' | head -1)"
kubectl -n t-mp scale deploy web --replicas=2 >/dev/null
