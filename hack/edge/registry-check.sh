#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd $ROOT
PATH=$PWD/hack:$PATH kubectl copa approve -n demo-prod --all | tail -1
timeout 400 bash -c 'until [ "$(kubectl get patchrollouts -A -o jsonpath="{.items[*].status.phase}")" = "Succeeded Succeeded" ]; do sleep 10; done'
kubectl get patchrollouts -A --no-headers | awk '{print $1, $4, $5}'
echo "pods: $(kubectl -n demo-prod get pods -l app=web -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{" "}{end}')"
echo "--- permissions (via port-forward)"
kubectl -n copa-registry port-forward svc/registry 5002:5000 >/dev/null 2>&1 & PF=$!
timeout 10 bash -c 'until curl -s localhost:5002/v2/ >/dev/null; do sleep 0.5; done'
R=localhost:5002
P=$R/patched/kind-registry-5000/library/nginx
B=$(cat hack/.keys/registry/builder); V=$(cat hack/.keys/registry/verifier)
try() { local label=$1; shift; if out=$("$@" 2>&1); then echo "ALLOWED  $label"; else echo "denied   $label ($(echo "$out" | grep -oE 'UNAUTHORIZED|DENIED|401|403|unauthorized|denied' | head -1))"; fi; }
cfg() { d=$(mktemp -d); printf '{"auths":{"%s":{"auth":"%s"}}}' "$R" "$(printf '%s:%s' "$1" "$2" | base64 -w0)" > $d/config.json; echo $d; }
BD=$(cfg builder $B); VD=$(cfg verifier $V); AD=$(mktemp -d); echo '{}' > $AD/config.json
try "anonymous list patched tags"      env DOCKER_CONFIG=$AD crane ls --insecure $P
try "anonymous pull patched"           env DOCKER_CONFIG=$AD crane digest --insecure $P:1.25.3-alpine-1
try "anonymous list staging"           env DOCKER_CONFIG=$AD crane ls --insecure $R/staging/kind-registry-5000/library/nginx
try "anonymous push patched"           env DOCKER_CONFIG=$AD crane tag --insecure $P:1.25.3-alpine-1 anon-evil
try "builder push patched"             env DOCKER_CONFIG=$BD crane tag --insecure $P:1.25.3-alpine-1 builder-evil
try "builder push staging"             env DOCKER_CONFIG=$BD crane copy --insecure $P:1.25.3-alpine-1 $R/staging/test/builder-ok:1
try "verifier push staging"            env DOCKER_CONFIG=$VD crane copy --insecure $P:1.25.3-alpine-1 $R/staging/test/verifier-evil:1
try "verifier read staging"            env DOCKER_CONFIG=$VD crane ls --insecure $R/staging/test/builder-ok
try "builder push outside both"        env DOCKER_CONFIG=$BD crane copy --insecure $P:1.25.3-alpine-1 $R/other/evil:1
echo "--- referrers API (native, no fallback tag)"
D=$(crane digest --insecure $P:1.25.3-alpine-1)
curl -s "$R/v2/patched/kind-registry-5000/library/nginx/referrers/$D" | python3 -c 'import json,sys; m=json.load(sys.stdin)["manifests"]; print(len(m), "referrer(s):", [x.get("artifactType") for x in m])'
echo "fallback tags: $(crane ls --insecure $P | grep -c '^sha256-')"
kill $PF
