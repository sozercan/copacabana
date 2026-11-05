#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
set -euo pipefail
NS=edge-drift
timeout 300 bash -c "until kubectl -n $NS get patchrollout -o jsonpath='{.items[*].status.phase}' | grep -qx Succeeded; do sleep 5; done"
echo "before: $(kubectl get sourceimages -o custom-columns=N:.metadata.name,D:.spec.digest,P:.status.current.tag --no-headers | grep app-)"
crane copy nginx:1.25.4-alpine localhost:5001/edge/app:1.0.0 2>/dev/null
echo "moved tag at $(date -u +%T) to $(crane digest --insecure localhost:5001/edge/app:1.0.0)"
# a pod created right after the move, before any new patch exists
timeout 20 tail -f /dev/null || true
kubectl -n $NS run probe --image=kind-registry:5000/edge/app:1.0.0 --labels=copa.sh/rollout=enabled --dry-run=server -o jsonpath='new pod right after the move gets: {.spec.containers[0].image}{"\n"}' | sed 's|kind-registry:5000/||'
timeout 420 bash -c "until [ \$(kubectl -n $NS get patchrollout --no-headers | grep -c Succeeded) -ge 2 ] || kubectl -n $NS get patchrollout --no-headers | grep -qE 'RolledBack|RecoveryFailed'; do sleep 5; done" || echo "timed out waiting"
kubectl get sourceimages -o custom-columns=NAME:.metadata.name,DIGEST:.spec.digest,FIX:.status.fixable,PATCH:.status.current.tag,USERS:.status.usedByCount,READY:.status.conditions[0].reason --no-headers | grep app- | cut -c1-170
kubectl get patchbuilds -n copa-builder --no-headers | grep app-
kubectl -n $NS get patchrollout -o custom-columns=NAME:.metadata.name,CAND:.spec.candidate.tag,SRCDIGEST:.spec.sourceDigest,PHASE:.status.phase --no-headers | cut -c1-140
kubectl -n $NS get swaplist copa -o jsonpath='{range .spec.entries[*]}entry {.source}: sourceDigest={.sourceDigest} selected={.selected.tag} previous={.previous.tag} fallback={.fallback}{"\n"}{end}' | sed 's|kind-registry:5000/||g'
echo "pods: $(kubectl -n $NS get pods -l app=app -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | sed 's|kind-registry:5000/||g')"
kubectl -n $NS exec deploy/app -- nginx -v 2>&1
