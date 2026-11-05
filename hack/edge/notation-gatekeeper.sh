#!/usr/bin/env bash
# Notation signatures on patched images, enforced by Gatekeeper with Ratify as its external data provider
# (hack/edge/gatekeeper-policy.sh). Needs a deploy with NOTATION=true, and a source that hasn't been patched yet so
# its patch is built and signed during the run. UNSIGNED_SRC must already be swapped in UNSIGNED_NS to a patch
# built before signing was turned on.
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
SIGNED_SRC=${SIGNED_SRC:-kind-registry:5000/library/nginx:1.25.4-alpine}
UNSIGNED_SRC=${UNSIGNED_SRC:-kind-registry:5000/library/nginx:1.25.3-alpine}
UNSIGNED_NS=${UNSIGNED_NS:-shop-prod}
NS=t-gk
hack/edge/gatekeeper-policy.sh 2>&1 | tail -1
short() { sed 's|registry.copa-registry.svc.cluster.local:5000/patched/|PATCHED:|g; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|g'; }
try() { if out=$("$@" 2>&1); then echo "ADMITTED  $(echo "$out" | short)"; else echo "DENIED    $(echo "$out" | grep -oE '\[copa-patched-images-signed\][^"]{0,200}' | head -1 | short)"; fi; }

echo "1. new namespace on $SIGNED_SRC: the patch is built and signed, and the rollout (including the controller's dry run) goes through Gatekeeper"
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label ns $NS copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: $NS}
spec:
  replicas: 2
  selector: {matchLabels: {app: web}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: web, copa.sh/rollout: enabled}}
    spec:
      containers: [{name: web, image: "$SIGNED_SRC", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}]
YAML
timeout 900 bash -c "until kubectl -n $NS get patchrollout --no-headers 2>/dev/null | grep -qE 'Soaking|Succeeded|RolledBack|Recovery'; do sleep 5; done" || echo "   TIMEOUT"
echo "   rollout: $(kubectl -n $NS get patchrollout -o jsonpath='{.items[0].spec.candidate.tag} {.items[0].status.phase}')"
echo "   pods: $(kubectl -n $NS get pods -l app=web -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | short)"

echo "2. new pod in $NS, swapped to the signed patch:"
printf '   '; try kubectl -n $NS run sig2 --image="$SIGNED_SRC" --labels=copa.sh/rollout=enabled --dry-run=server -o jsonpath='{.spec.containers[0].image}'
echo "3. new pod in $UNSIGNED_NS, swapped to a patch built before signing was on:"
printf '   '; try kubectl -n "$UNSIGNED_NS" run sig3 --image="$UNSIGNED_SRC" --labels=copa.sh/rollout=enabled --dry-run=server -o jsonpath='{.spec.containers[0].image}'
UNSIGNED=$(kubectl -n "$UNSIGNED_NS" get swaplist copa -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["spec"]["images"][sys.argv[1]])' "$UNSIGNED_SRC")
echo "4. pod naming that unsigned patch by digest, no opt-in label:"
printf '   '; try kubectl -n default run sig4 --image="$UNSIGNED" --dry-run=server -o jsonpath='{.spec.containers[0].image}'
echo "5. pod on a source image (not under patched/, so never sent to Ratify):"
printf '   '; try kubectl -n default run sig5 --image="$UNSIGNED_SRC" --dry-run=server -o jsonpath='{.spec.containers[0].image}'

echo "6. $UNSIGNED_NS scales up while its SwapList points at the unsigned patch:"
deploy=$(kubectl -n "$UNSIGNED_NS" get deploy -o json | python3 -c 'import json,sys; print(next(d["metadata"]["name"] for d in json.load(sys.stdin)["items"] if any(c["image"]==sys.argv[1] for c in d["spec"]["template"]["spec"]["containers"])))' "$UNSIGNED_SRC")
replicas=$(kubectl -n "$UNSIGNED_NS" get deploy "$deploy" -o jsonpath='{.spec.replicas}')
kubectl -n "$UNSIGNED_NS" scale deploy "$deploy" --replicas=$((replicas + 1)) >/dev/null; sleep 15
echo "   ready: $(kubectl -n "$UNSIGNED_NS" get deploy "$deploy" -o jsonpath='{.status.readyReplicas}/{.spec.replicas}'); ReplicaSet: $(kubectl -n "$UNSIGNED_NS" get events --field-selector reason=FailedCreate -o jsonpath='{.items[-1:].message}' | grep -oE '\[copa-patched-images-signed\][^"]{0,120}' | head -1 | short)"
kubectl -n "$UNSIGNED_NS" scale deploy "$deploy" --replicas="$replicas" >/dev/null
