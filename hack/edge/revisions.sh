#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SI=nginx-3a2857b3b4
kubectl annotate sourceimage $SI copa.sh/rebuild=force --overwrite >/dev/null
echo "forced rebuild at $(date -u +%T)"
timeout 300 bash -c "until kubectl get sourceimage $SI -o jsonpath='{.status.current.tag}' | grep -q -- '-2$'; do sleep 5; done"
kubectl get patchbuilds -n copa-builder -l copa.sh/source-image=$SI --sort-by=.metadata.creationTimestamp -o custom-columns=NAME:.metadata.name,REASON:.spec.reason,PHASE:.status.phase,PATCH:.status.promoted.tag --no-headers | tail -2
RO=""; timeout 120 bash -c "until kubectl -n demo-dev get patchrollout --no-headers | grep -q 'alpine-2'; do sleep 3; done"
RO=$(kubectl -n demo-dev get patchrollout -o json | python3 -c 'import json,sys; print([i["metadata"]["name"] for i in json.load(sys.stdin)["items"] if i["spec"]["candidate"]["tag"].endswith("-2")][0])')
timeout 300 bash -c "until kubectl -n demo-dev get patchrollout $RO -o jsonpath='{.status.phase}' | grep -qE 'Soaking|Succeeded'; do sleep 5; done"
echo "demo-dev on -2: $(kubectl -n demo-dev get swaplist copa -o jsonpath='selected={.spec.entries[0].selected.tag} previous={.spec.entries[0].previous.tag}')"
echo "demo-prod: $(kubectl -n demo-prod get patchrollout --no-headers | awk '{print $3,$4}' | tr '\n' ' ')"
kubectl -n demo-dev annotate deploy web copa.sh/fail-rollout=$RO --overwrite >/dev/null; echo "team failed $RO at $(date -u +%T)"
timeout 300 bash -c "until kubectl -n demo-dev get patchrollout $RO -o jsonpath='{.status.phase}' | grep -qE 'RolledBack|RecoveryFailed'; do sleep 5; done"
kubectl -n demo-dev get patchrollout $RO -o jsonpath='{.status.phase}: restored={.status.restored} | {.status.message}{"\n"}'
echo "after: $(kubectl -n demo-dev get swaplist copa -o jsonpath='selected={.spec.entries[0].selected.tag} previous={.spec.entries[0].previous.tag}')"
echo "pods: $(kubectl -n demo-dev get pods -l app=web -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{" "}{end}' | grep -o '@sha256:.\{12\}' | sort | uniq -c | tr '\n' ' ')  (-1 is $(kubectl get sourceimage $SI -o jsonpath='{.status.current.digest}' >/dev/null; crane digest --insecure localhost:5001/library/nginx:1.25.3-alpine >/dev/null; echo 4c6beac546c0))"
kubectl -n demo-dev annotate deploy web copa.sh/fail-rollout- >/dev/null
