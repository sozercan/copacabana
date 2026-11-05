#!/usr/bin/env bash
# In-cluster patching demo (demo-magic). Assumes a fresh cluster from:
#   CLUSTER=copa-demo hack/kind-up.sh && KCTX=kind-copa-demo make deploy
# Run interactively (ENTER advances) or unattended with -n, e.g. under asciinema:
#   asciinema rec -i 3 -c "env TERM=xterm-256color demo/copa-incluster-demo.sh -n" demo/copa-incluster.cast
set -o pipefail
cd "$(dirname "$0")"
REPO=$(cd .. && pwd)

TYPE_SPEED=${TYPE_SPEED:-40}
. ./demo-magic.sh
DEMO_PROMPT="${GREEN}➜ ${COLOR_RESET}"
DEMO_COMMENT_COLOR=$CYAN

export KUBECONFIG=${KUBECONFIG_DEMO:-$HOME/.kube/copa-demo.kubeconfig}
export PATH=$REPO/hack:$PATH
SOURCE=localhost:5001/library/nginx:1.25.3-alpine           # the same image, as the host sees it
PATCHED=127.0.0.1:5443/patched/kind-registry-5000/library/nginx  # in-cluster registry, port-forwarded
PAUSE=${PAUSE:-3}

# ---- hidden setup -------------------------------------------------------------------------------------
docker pull -q "$SOURCE" >/dev/null
kubectl port-forward -n copa-registry svc/registry 5443:5000 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null' EXIT

# Comments are typed faster than commands.
say() { local s=$TYPE_SPEED; TYPE_SPEED=90; for line in "$@"; do NO_WAIT=true p "# $line"; done; TYPE_SPEED=$s; sleep "$PAUSE"; }
section() { echo; echo -e "${BOLD}${PURPLE}━━━ $1 ━━━${COLOR_RESET}"; echo; sleep 1; }
run() { pe "$1"; sleep "$PAUSE"; }

# Shows a real `kubectl get --watch` until a condition holds, dropping repeats that only differ in AGE.
watch_until() {
  local shown=$1 cond=$2 deadline=$((SECONDS + ${3:-900}))
  p "$shown"
  bash -c "$shown 2>/dev/null | awk '{k=\$1\" \"\$2; l=\$0; sub(/[ ]+[^ ]+\$/, \"\", l); if (l != seen[k]) {print; fflush()} seen[k]=l}'" &
  local pid=$!
  until eval "$cond" >/dev/null 2>&1 || [ $SECONDS -ge $deadline ]; do sleep 2; done
  sleep 3
  pkill -P $pid 2>/dev/null; kill $pid 2>/dev/null; builtin wait $pid 2>/dev/null
  sleep "$PAUSE"
}
phase() { kubectl get patchrollouts -n "$1" -o jsonpath='{.items[0].status.phase}' 2>/dev/null; }

clear

# ---------------------------------------------------------------------------------------------------------
section "The problem"
say "A container image ships a whole OS userland: alpine, debian, ubuntu, with their own packages." \
    "Those packages keep getting security fixes long after the image was built." \
    "The usual fix: wait for the image author, rebuild, redeploy. For every image, in every team." \
    "Copa patches the existing image instead: it adds one layer with the updated OS packages." \
    "This demo runs Copa inside a Kubernetes cluster, so patching happens without any team rebuilding."

# ---------------------------------------------------------------------------------------------------------
section "1. What runs in the cluster"
run "kubectl get pods -A | grep -E '^copa-'"
say "copa-rollout-controller  decides what to patch and when (two replicas, one active)" \
    "registry                 an in-cluster registry that stores the patched images" \
    "trivy-server             the vulnerability database that scans use"

# ---------------------------------------------------------------------------------------------------------
section "2. An app with an old image"
say "Two copies of the same app: shop-dev and shop-prod. Here is shop-dev:"
run "bat -pp app-dev.yaml"
say "Teams opt in with labels. Nothing else changes: the image is still nginx:1.25.3-alpine." \
    "(kind-registry:5000 is a local registry standing in for wherever your images live.)" \
    "shop-prod is identical, except it has no automatic approval."
say "How many fixable vulnerabilities does that image have?"
run "trivy image -q --pkg-types os --ignore-unfixed --insecure --table-mode summary $SOURCE"

# ---------------------------------------------------------------------------------------------------------
section "3. Patching, automatically"
say "Once the apps are deployed, the controller finds their image and runs three Jobs," \
    "each in a locked-down namespace:" \
    "  scan     Trivy lists the vulnerabilities that have a fix" \
    "  build    Copa updates those packages with BuildKit, on a dedicated builder node" \
    "  verify   a separate Job checks the patch and signs it. Only it holds the signing key"
run "kubectl apply -f app-dev.yaml -f app-prod.yaml"
watch_until "kubectl get patchbuilds -n copa-builder --watch" \
  "kubectl get patchbuilds -n copa-builder -o jsonpath='{.items[*].status.phase}' | grep -qE 'Promoted|Failed'"
say "The verify Job checked that the original layers are untouched, the fixes are really there," \
    "and that nothing new is vulnerable. Then it published the patch as revision 1:"
run "kubectl get sourceimages"
say "The patch lives in the in-cluster registry. Nothing was pushed outside the cluster." \
    "(127.0.0.1:5443 is a port-forward to that registry.) Attached to it:"
run "oras discover --insecure $PATCHED:1.25.3-alpine-1"
say "a signed attestation: which image was patched, into what, and which vulnerabilities were fixed." \
    "The controller only uses a patch whose signature checks out."

# ---------------------------------------------------------------------------------------------------------
section "4. Rolling it out"
until kubectl get patchrollouts -n shop-prod -o name 2>/dev/null | grep -q .; do sleep 2; done
say "Each namespace gets its own PatchRollout:"
run "kubectl get patchrollouts -A"
say "shop-dev approves automatically. shop-prod waits for a person." \
    "In shop-dev the controller restarts Deployments a few at a time, then waits:" \
    "pods must become healthy, stay healthy, and survive a soak period."
watch_until "kubectl get patchrollouts -n shop-dev --watch" \
  "[[ \$(phase shop-dev) =~ ^(Succeeded|RolledBack|RecoveryFailed)\$ ]]"

# ---------------------------------------------------------------------------------------------------------
section "5. What actually changed?"
say "The Deployment (and Git) still say the original image:"
run "kubectl get deploy web -n shop-dev -o custom-columns='DEPLOYMENT:.metadata.name,IMAGE:.spec.template.spec.containers[0].image'"
say "But the pods run the patched image:"
run "kubectl get pods -n shop-dev -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[0].image'"
say "Kubernetes admission swaps the image as each pod is created, using a per-namespace list:"
run "kubectl get swaplist copa -n shop-dev -o yaml | yq '.spec.entries[] | {\"source\": .source, \"use\": .selected.ref, \"fallback\": .fallback}'"
say "Package versions, original image vs. running pod:"
run "docker run --rm --entrypoint apk $SOURCE info -v 2>/dev/null | grep -E '^(curl|libssl3|busybox|libxml2)-[0-9]'"
run "kubectl exec -n shop-dev deploy/web -- apk info -v 2>/dev/null | grep -E '^(curl|libssl3|busybox|libxml2)-[0-9]'"
say "And the scan again, this time on the patched image:"
run "trivy image -q --pkg-types os --ignore-unfixed --insecure --table-mode summary $PATCHED:1.25.3-alpine-1"

# ---------------------------------------------------------------------------------------------------------
section "6. Production: a person approves"
run "kubectl get patchrollouts -n shop-prod"
run "kubectl copa approve -n shop-prod --all"
watch_until "kubectl get patchrollouts -n shop-prod --watch" \
  "[[ \$(phase shop-prod) =~ ^(Succeeded|RolledBack|RecoveryFailed)\$ ]]"

# ---------------------------------------------------------------------------------------------------------
section "7. Undo"
say "Something looks wrong in shop-dev? The team backs the patch out with one annotation." \
    "(The controller also does this by itself when pods fail health checks.)"
run "kubectl annotate deploy web -n shop-dev copa.sh/fail-rollout=true"
watch_until "kubectl get patchrollouts -n shop-dev --watch" \
  "[[ \$(phase shop-dev) =~ ^(RolledBack|RecoveryFailed)\$ ]]"
say "shop-dev is back on the original image, pinned to its digest. shop-prod keeps its patch:"
run "kubectl get pods -A -l app=web -o custom-columns='NAMESPACE:.metadata.namespace,IMAGE:.spec.containers[0].image'"

# ---------------------------------------------------------------------------------------------------------
section "Recap"
say "1. Teams opt in with labels; their manifests and Git keep the original image." \
    "2. The cluster scans, patches, verifies and signs the image, and stores it in its own registry." \
    "3. Each namespace approves on its own terms; pods get the patch as they are created." \
    "4. Health checks, soak time and a one-line undo keep it safe." \
    "Learn more about Copa: https://github.com/project-copacetic/copacetic"
sleep 2
