#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SA=system:serviceaccount:copa-system:copa-rollout-controller
Z=0000000000000000000000000000000000000000000000000000000000000000
kubectl -n demo-dev patch swaplist copa --type=json -p '[{"op":"remove","path":"/spec/images/x"}]' --as=$SA | tail -1
echo "- stray key with valid-looking target:"
kubectl -n demo-dev patch swaplist copa --type=merge -p "{\"spec\":{\"images\":{\"x\":\"kind-registry:5000/patched/a/b@sha256:$Z\"}}}" --as=$SA 2>&1 | grep -o "denied request.*\|patched$"
echo "- alias pointed at a different patched digest:"
kubectl -n demo-dev patch swaplist copa --type=merge -p "{\"spec\":{\"images\":{\"kind-registry:5000/library/nginx:1.25.3-alpine\":\"kind-registry:5000/patched/kind-registry-5000/library/nginx@sha256:$Z\"}}}" --as=$SA 2>&1 | grep -o "denied request.*\|patched$"
echo "- entry selected ref with path traversal (images updated consistently):"
kubectl -n demo-dev get swaplist copa -o json | python3 -c "
import json,sys; s=json.load(sys.stdin); bad='kind-registry:5000/patched/../evil@sha256:$Z'
s['spec']['entries'][0]['selected']['ref']=bad
for k in list(s['spec']['images']): s['spec']['images'][k]=bad
print(json.dumps(s))" | kubectl replace -f - --as=$SA 2>&1 | grep -o "denied request.*\|replaced$"
echo "- fallback not pinned to the source digest:"
kubectl -n demo-dev get swaplist copa -o json | python3 -c "
import json,sys; s=json.load(sys.stdin); s['spec']['entries'][0]['fallback']='kind-registry:5000/library/nginx:1.25.3-alpine'
print(json.dumps(s))" | kubectl replace -f - --as=$SA 2>&1 | grep -o "denied request.*\|replaced$"
echo "- no-op rewrite of the current, valid object:"
kubectl -n demo-dev get swaplist copa -o json | kubectl replace -f - --as=$SA 2>&1 | tail -1
