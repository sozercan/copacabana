#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd $ROOT
echo "== missed swap: remove the mutating policy binding, create an opted-in pod"
kubectl delete mutatingadmissionpolicybinding copa-image-swap
timeout 20 tail -f /dev/null
kubectl -n demo-dev run missed --image=kind-registry:5000/library/nginx:1.25.3-alpine --labels=copa.sh/rollout=enabled --restart=Never >/dev/null
timeout 15 tail -f /dev/null
kubectl -n demo-dev get pod missed -o jsonpath='image={.spec.containers[0].image} swapped={.metadata.annotations.copa\.sh/swapped}{"\n"}'
kubectl -n demo-dev get events --field-selector involvedObject.name=missed -o custom-columns=TYPE:.type,REASON:.reason,MSG:.message | grep -iv 'pulled\|created\|started\|scheduled\|pulling'
POD=$(kubectl -n copa-system get pod -l app=copa-rollout-controller -o jsonpath='{.items[0].metadata.name}')
kubectl get --raw "/api/v1/namespaces/copa-system/pods/$POD:8080/proxy/metrics" | grep -E '^copa_(missed|rollouts_total|builds_total)'
kubectl apply -f config/admission/mutating-policy.yaml >/dev/null; kubectl -n demo-dev delete pod missed --wait=false >/dev/null
echo "== swaplist guard"
kubectl -n demo-dev patch swaplist copa --type=merge -p '{"spec":{"images":{"evil":"kind-registry:5000/patched/x@sha256:0000000000000000000000000000000000000000000000000000000000000000"}}}' --as=alice 2>&1 | tail -1
kubectl -n demo-dev patch swaplist copa --type=merge -p '{"spec":{"images":{"kind-registry:5000/library/nginx:1.25.3-alpine":"docker.io/evil/nginx:latest"}}}' --as=system:serviceaccount:copa-system:copa-rollout-controller 2>&1 | tail -1
kubectl -n demo-dev patch swaplist copa --type=merge -p '{"spec":{"images":{"kind-registry:5000/library/nginx:1.25.3-alpine":"kind-registry:5000/patched/evil:latest"}}}' --as=system:serviceaccount:copa-system:copa-rollout-controller 2>&1 | tail -1
kubectl -n demo-dev patch swaplist copa --type=merge -p '{"spec":{"images":{"x":"kind-registry:5000/patched/../evil@sha256:0000000000000000000000000000000000000000000000000000000000000000"}}}' --as=system:serviceaccount:copa-system:copa-rollout-controller 2>&1 | tail -1
kubectl -n demo-dev patch swaplist copa --type=merge -p '{"spec":{"images":{"x":"kind-registry:5000/patched/evil@sha256:0000000000000000000000000000000000000000000000000000000000000000"}}}' --as=alice --as-group=copa:platform-admins 2>&1 | tail -1
kubectl -n demo-dev get swaplist copa -o jsonpath='{.spec.images}{"\n"}'
