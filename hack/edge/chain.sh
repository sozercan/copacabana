#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
set -euo pipefail
NS=edge-chain
timeout 400 bash -c "until [ \"\$(kubectl -n $NS get patchrollout -o jsonpath='{.items[*].status.phase}')\" = 'Succeeded Succeeded' ]; do sleep 5; done"
TAGRO=$(kubectl -n $NS get patchrollout -o json | python3 -c 'import json,sys; print([i["metadata"]["name"] for i in json.load(sys.stdin)["items"] if "@" not in i["spec"]["source"]][0])')
kubectl -n edge-manual get deploy web -o json | python3 -c '
import json,sys; d=json.load(sys.stdin)
for k in ["uid","resourceVersion","creationTimestamp","generation","managedFields","annotations"]: d["metadata"].pop(k,None)
d.pop("status",None); d["metadata"]["name"]="late"; d["metadata"]["namespace"]="edge-chain"
d["spec"]["selector"]["matchLabels"]["app"]="late"; d["spec"]["template"]["metadata"]["labels"]["app"]="late"
d["spec"]["template"]["metadata"].pop("annotations",None)
print(json.dumps(d))' | kubectl apply -f - >/dev/null
kubectl -n $NS rollout status deploy/late --timeout=90s >/dev/null
kubectl -n $NS annotate deploy late copa.sh/fail-rollout=$TAGRO >/dev/null
echo "failed $TAGRO via untracked deploy late at $(date -u +%T)"
timeout 400 bash -c "until kubectl -n $NS get patchrollout $TAGRO -o jsonpath='{.status.phase}' | grep -qE 'RolledBack|RecoveryFailed'; do sleep 5; done"
kubectl -n $NS get patchrollout -o jsonpath='{range .items[*]}{.spec.source}{"\n  "}{.status.phase}: {.status.message}{"\n"}{range .status.workloads[*]}{"    "}{.name}={.phase} ({.message}){"\n"}{end}{end}' | sed 's|kind-registry:5000/||g'
for d in pinned tagged late; do echo "$d: $(kubectl -n $NS get pods -l app=$d -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | grep -o 'patched[^ ]*@sha256:.\{12\}\|library/nginx[^ ]*' | sed 's|@sha256:\(.\{12\}\).*|@\1|' | sort | uniq -c | tr '\n' ' ')"; done
