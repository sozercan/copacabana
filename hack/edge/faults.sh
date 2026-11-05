#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
# usage: fi.sh <name> ; force-rebuilds SourceImage and runs the named fault
SI=nginx-3a2857b3b4
latest_pb() { kubectl get patchbuilds -n copa-builder -l copa.sh/source-image=$SI --sort-by=.metadata.creationTimestamp -o name | tail -1; }
before=$(latest_pb)
kubectl annotate sourceimage $SI copa.sh/rebuild=force --overwrite >/dev/null
timeout 90 bash -c "until [ \"\$(kubectl get patchbuilds -n copa-builder -l copa.sh/source-image=$SI --sort-by=.metadata.creationTimestamp -o name | tail -1)\" != '$before' ]; do sleep 1; done"
PB=$(latest_pb); echo "[$1] $PB"
wait_stage() { timeout 120 bash -c "until kubectl get -n copa-builder $PB -o jsonpath='{.status.phase}' | grep -qx '$1'; do sleep 1; done"; }
job_pod() { kubectl get pods -n $1 -l copa.sh/patchbuild=${PB#*/},copa.sh/stage=$2 -o name | head -1; }
case $1 in
  registry-down)
    wait_stage Building; sleep 3
    kubectl -n copa-registry scale deploy/registry --replicas=0 >/dev/null; echo "  registry scaled to 0 during Building"
    timeout 600 bash -c "until kubectl get -n copa-builder $PB -o jsonpath='{.status.phase}' | grep -qE 'Failed|Promoted'; do sleep 3; done"
    kubectl -n copa-registry scale deploy/registry --replicas=1 >/dev/null; kubectl -n copa-registry rollout status deploy/registry --timeout=120s >/dev/null ;;
  trivy-down)
    kubectl -n copa-builder scale deploy/trivy-server --replicas=0 >/dev/null; echo "  trivy-server scaled to 0"
    timeout 600 bash -c "until kubectl get -n copa-builder $PB -o jsonpath='{.status.phase}' | grep -qE 'Failed|Promoted|NothingToFix'; do sleep 3; done"
    kubectl -n copa-builder scale deploy/trivy-server --replicas=1 >/dev/null; kubectl -n copa-builder rollout status deploy/trivy-server --timeout=300s >/dev/null ;;
  buildkit-crash)
    wait_stage Building
    timeout 60 bash -c "until kubectl get -n copa-builder \$(kubectl get pods -n copa-builder -l copa.sh/patchbuild=${PB#*/},copa.sh/stage=build -o name | head -1) -o jsonpath='{.status.initContainerStatuses[0].ready}' 2>/dev/null | grep -q true; do sleep 1; done"; sleep 4
    P=$(job_pod copa-builder build); kubectl -n copa-builder exec $P -c buildkitd -- sh -c 'kill -9 $(pidof buildkitd) || kill -9 1' 2>&1 | tail -1; echo "  killed buildkitd in $P"
    timeout 600 bash -c "until kubectl get -n copa-builder $PB -o jsonpath='{.status.phase}' | grep -qE 'Failed|Promoted'; do sleep 3; done" ;;
  job-timeout)
    wait_stage Building; J=$(kubectl get jobs -n copa-builder -l copa.sh/patchbuild=${PB#*/},copa.sh/stage=build -o name | head -1)
    kubectl -n copa-builder patch $J -p '{"spec":{"activeDeadlineSeconds":3}}' >/dev/null; echo "  set $J activeDeadlineSeconds=3"
    timeout 300 bash -c "until kubectl get -n copa-builder $PB -o jsonpath='{.status.phase}' | grep -qE 'Failed|Promoted'; do sleep 3; done" ;;
  controller-kill)
    wait_stage Building
    L=$(kubectl -n copa-system get lease copa-rollout-controller.copa.sh -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1)
    kubectl -n copa-system delete pod $L --wait=false >/dev/null; echo "  killed leader $L during Building at $(date -u +%T)"
    timeout 600 bash -c "until kubectl get -n copa-builder $PB -o jsonpath='{.status.phase}' | grep -qE 'Failed|Promoted'; do sleep 3; done"
    echo "  new leader: $(kubectl -n copa-system get lease copa-rollout-controller.copa.sh -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1) (transitions $(kubectl -n copa-system get lease copa-rollout-controller.copa.sh -o jsonpath='{.spec.leaseTransitions}'))" ;;
  builder-cordoned)
    : ;;
esac
kubectl get -n copa-builder $PB -o jsonpath='  result: {.status.phase} stage={.status.failedStage} | {.status.message}{"\n"}' | cut -c1-260
kubectl get sourceimage $SI -o jsonpath='  sourceimage: current={.status.current.tag} gap={.status.gap}{"\n"}' | cut -c1-200
