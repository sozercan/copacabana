#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
set -euo pipefail
NS=edge-manual2
RO=$(kubectl -n $NS get patchrollout -o jsonpath='{.items[0].metadata.name}')
PATH=$ROOT/hack:$PATH kubectl copa approve -n $NS --all
kubectl -n $NS delete deploy web-delete --wait=false >/dev/null
kubectl -n $NS patch deploy web-optout --type=json -p '[{"op":"remove","path":"/spec/template/metadata/labels/copa.sh~1rollout"}]' >/dev/null
timeout 300 bash -c "until kubectl -n $NS get patchrollout $RO -o jsonpath='{range .status.workloads[*]}{.name}={.phase} {end}' | grep -q 'web-team=Restarting'; do sleep 1; done"
kubectl -n $NS set env deploy/web-team TEAM_DEPLOY=1 >/dev/null
echo "team deployed web-team at $(date -u +%T)"
kubectl -n copa-system delete pod -l app=copa-rollout-controller --wait=false >/dev/null
echo "killed controller at $(date -u +%T); rollout was: $(kubectl -n $NS get patchrollout $RO -o jsonpath='{.status.phase} | {range .status.workloads[*]}{.name}={.phase} {end}')"
timeout 480 bash -c "until kubectl -n $NS get patchrollout $RO -o jsonpath='{.status.phase}' | grep -qE 'Succeeded|RolledBack|RecoveryFailed'; do sleep 5; done"
kubectl -n $NS get patchrollout $RO -o jsonpath='{.status.phase}: {.status.message}{"\n"}{range .status.workloads[*]}{"   "}{.name}={.phase} ({.message}){"\n"}{end}'
for d in web web-team web-optout web-late; do echo "$d: $(kubectl -n $NS get pods -l app=$d -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | grep -o 'patched[^ ]*@sha256:.\{12\}\|library/nginx[^ ]*' | sort | uniq -c | tr '\n' ' ')"; done
