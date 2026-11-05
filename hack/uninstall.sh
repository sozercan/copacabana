#!/usr/bin/env bash
# Removes copa from a cluster without stranding workloads on images nobody serves anymore.
#
#   1. Unbind the image-swap policy: new pods get the image their template asks for (the source from Git).
#   2. RESTART=true restarts opted-in Deployments, StatefulSets and DaemonSets that still run patched images,
#      so they move back to the source. Without it, the script lists them and keeps the registry.
#   3. Remove the controller, policies, custom resources and CRDs.
#   4. Remove the in-cluster registry only when nothing runs a patched image (or FORCE_REGISTRY=true).
set -euo pipefail
KCTX=${KCTX:-kind-copa}
K="kubectl --context ${KCTX}"
PREFIX=${PREFIX:-registry.copa-registry.svc.cluster.local:5000/patched/}

$K delete mutatingadmissionpolicybinding copa-image-swap --ignore-not-found
$K delete mutatingadmissionpolicy copa-image-swap --ignore-not-found
# Stop the controller before touching workloads so it doesn't start new rollouts or recoveries.
$K -n copa-system scale deploy/copa-rollout-controller --replicas=0 2>/dev/null || true

# Pods still running a patched image, as namespace/owner-kind/owner-name.
patched_owners() {
  $K get pods -A -o json | python3 -c '
import json, sys
prefix = sys.argv[1]
seen = set()
for p in json.load(sys.stdin)["items"]:
    if p["metadata"].get("deletionTimestamp"):
        continue  # old pods still terminating after a restart
    imgs = [c["image"] for c in p["spec"].get("containers", []) + p["spec"].get("initContainers", [])]
    if not any(i.startswith(prefix) for i in imgs):
        continue
    ns = p["metadata"]["namespace"]
    refs = p["metadata"].get("ownerReferences", [])
    kind, name = (refs[0]["kind"], refs[0]["name"]) if refs else ("Pod", p["metadata"]["name"])
    if kind == "ReplicaSet":
        kind, name = "Deployment", name.rsplit("-", 1)[0]
    seen.add(f"{ns}/{kind}/{name}")
print("\n".join(sorted(seen)))' "$PREFIX"
}

owners=$(patched_owners)
if [ -n "$owners" ] && [ "${RESTART:-false}" = true ]; then
  while IFS=/ read -r ns kind name; do
    case $kind in
      Deployment|StatefulSet|DaemonSet) $K -n "$ns" rollout restart "${kind,,}/$name" ;;
      *) echo "left alone (restart it yourself): $ns/$kind/$name" ;;
    esac
  done <<<"$owners"
  while IFS=/ read -r ns kind name; do
    case $kind in Deployment|StatefulSet|DaemonSet) $K -n "$ns" rollout status "${kind,,}/$name" --timeout=10m ;; esac
  done <<<"$owners"
  owners=$(patched_owners)
fi

for p in copa-swaplist-guard copa-controller-scope copa-controller-deployments; do
  $K delete validatingadmissionpolicybinding "$p" --ignore-not-found
  $K delete validatingadmissionpolicy "$p" --ignore-not-found
done
for r in patchapprovals patchrollouts swaplists patchbuilds; do $K delete "$r.rollout.copa.sh" -A --all --ignore-not-found --wait=false 2>/dev/null || true; done
$K delete sourceimages.rollout.copa.sh --all --ignore-not-found --wait=false 2>/dev/null || true
$K delete crd -l app.kubernetes.io/part-of=copa --ignore-not-found 2>/dev/null || true
for crd in patchapprovals patchrollouts swaplists patchbuilds sourceimages; do $K delete crd "$crd.rollout.copa.sh" --ignore-not-found; done
$K delete clusterrole,clusterrolebinding -l app.kubernetes.io/part-of=copa --ignore-not-found 2>/dev/null || true
$K delete clusterrolebinding copa-rollout-controller --ignore-not-found
$K delete clusterrole copa-rollout-controller copa-approver --ignore-not-found
$K delete namespace copa-system copa-builder copa-verifier --ignore-not-found --wait=false

if [ -n "$owners" ] && [ "${FORCE_REGISTRY:-false}" != true ]; then
  echo "Kept the copa-registry namespace: these still run patched images and would fail to pull on a new node:"
  echo "$owners" | sed 's/^/  /'
  echo "Restart them (or rerun with RESTART=true), then delete namespace copa-registry."
  exit 0
fi
$K delete namespace copa-registry --ignore-not-found --wait=false
echo "copa removed"
