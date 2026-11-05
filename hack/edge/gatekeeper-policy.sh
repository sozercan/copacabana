#!/usr/bin/env bash
# Enforces Notation signatures on patched images with Gatekeeper and Ratify, Gatekeeper's external data provider
# for signature verification (deploy with NOTATION=true first). Pods whose images come from the patched prefix
# must carry a signature from the test certificate; other images aren't sent to Ratify.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CERT=$ROOT/hack/.keys/notation/copa-test.crt
CA=$ROOT/hack/.keys/registry-tls/ca.crt
PREFIX=${PREFIX:-registry.copa-registry.svc.cluster.local:5000/patched/}
GATEKEEPER_VERSION=${GATEKEEPER_VERSION:-3.23.1}
RATIFY_CHART_VERSION=${RATIFY_CHART_VERSION:-1.15.9}
NS=gatekeeper-system

helm repo add gatekeeper https://open-policy-agent.github.io/gatekeeper/charts >/dev/null 2>&1 || true
helm repo add ratify https://notaryproject.github.io/ratify --force-update >/dev/null 2>&1
helm repo update gatekeeper ratify >/dev/null

# The validating webhook only sees Pods, and fails closed: like Kyverno's Enforce policy, if Gatekeeper is down,
# pods aren't admitted unverified. Gatekeeper's default rules match every resource; with Fail that includes
# Leases, so kube-controller-manager can't keep leadership, never marks Gatekeeper's endpoints ready, and the
# cluster deadlocks. A short external data cache so a newly signed patch is admitted soon after publishing.
helm upgrade --install gatekeeper gatekeeper/gatekeeper --version "$GATEKEEPER_VERSION" -n $NS --create-namespace --wait --timeout 10m \
  -f - >/dev/null <<'VALUES'
enableExternalData: true
validatingWebhookFailurePolicy: Fail
validatingWebhookTimeoutSeconds: 5
externaldataProviderResponseCacheTTL: 10s
controllerManager:
  exemptNamespaces: [kube-system]
validatingWebhookCustomRules:
- apiGroups: [""]
  apiVersions: ["v1"]
  operations: ["CREATE", "UPDATE"]
  resources: ["pods", "pods/ephemeralcontainers"]
VALUES
# Fail-closed, so kube-system pods (CoreDNS) mustn't depend on Gatekeeper; its own namespace is exempt already.
kubectl label ns kube-system admission.gatekeeper.sh/ignore=no-self-managing --overwrite >/dev/null

# Ratify:
# - provider.enableMutation=false: Ratify would otherwise resolve tags to digests in a mutating webhook, before
#   the swap policy sees the tag. Kyverno's policy had mutateDigest: false for the same reason.
# - cosign.enabled=false: only Notation signatures are checked.
# - RATIFY_CERT_ROTATION: Ratify generates the TLS certificate Gatekeeper uses to call it.
helm upgrade --install ratify ratify/ratify --version "$RATIFY_CHART_VERSION" -n $NS --wait --timeout 10m \
  --set featureFlags.RATIFY_CERT_ROTATION=true \
  --set provider.enableMutation=false \
  --set cosign.enabled=false \
  --set-file 'notationCerts[0]'="$CERT" >/dev/null

# The chart can't mount extra CAs, and the in-cluster registry's certificate comes from hack/.keys/registry-tls.
# Add it next to the system roots (Go reads every directory in SSL_CERT_DIR). Re-run this script after a
# helm upgrade of Ratify.
kubectl -n $NS create configmap copa-registry-ca --from-file=ca.crt="$CA" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n $NS patch deployment ratify --type=strategic -p '{"spec":{"template":{"spec":{
  "volumes":[{"name":"copa-registry-ca","configMap":{"name":"copa-registry-ca"}}],
  "containers":[{"name":"ratify",
    "env":[{"name":"SSL_CERT_DIR","value":"/etc/ssl/certs:/etc/copa-registry-ca"}],
    "volumeMounts":[{"name":"copa-registry-ca","mountPath":"/etc/copa-registry-ca","readOnly":true}]}]}}}}' >/dev/null
kubectl -n $NS rollout status deployment ratify --timeout=180s >/dev/null

# Only images under the patched prefix are verified. The template follows Ratify's library template; it skips
# the external data call when a pod has no patched images.
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: templates.gatekeeper.sh/v1
kind: ConstraintTemplate
metadata:
  name: copapatchedimagessigned
spec:
  crd:
    spec:
      names:
        kind: CopaPatchedImagesSigned
      validation:
        openAPIV3Schema:
          type: object
          properties:
            prefixes:
              type: array
              items: {type: string}
  targets:
  - target: admission.k8s.gatekeeper.sh
    rego: |
      package copapatchedimagessigned

      images := array.concat(
        array.concat([c.image | c := input.review.object.spec.containers[_]],
                     [c.image | c := input.review.object.spec.initContainers[_]]),
        [c.image | c := input.review.object.spec.ephemeralContainers[_]])

      patched := [img | img := images[_]; startswith(img, input.parameters.prefixes[_])]

      remote_data := external_data({"provider": "ratify-provider", "keys": patched})

      violation[{"msg": msg}] {
        count(patched) > 0
        err := remote_data.system_error
        err != ""
        msg := sprintf("can't verify patched images (Ratify): %s", [err])
      }

      violation[{"msg": msg}] {
        count(patched) > 0
        count(remote_data.errors) > 0
        msg := sprintf("can't verify patched images: %v", [remote_data.errors])
      }

      violation[{"msg": msg}] {
        count(patched) > 0
        result := remote_data.responses[_]
        result[1].isSuccess == false
        msg := sprintf("patched image %s has no valid Notation signature (trace-id %s)", [result[0], result[1].traceID])
      }
YAML
for i in $(seq 1 30); do
  kubectl get crd copapatchedimagessigned.constraints.gatekeeper.sh >/dev/null 2>&1 && break; sleep 2
done
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: constraints.gatekeeper.sh/v1beta1
kind: CopaPatchedImagesSigned
metadata:
  name: copa-patched-images-signed
spec:
  enforcementAction: deny
  match:
    kinds:
    - apiGroups: [""]
      kinds: ["Pod"]
  parameters:
    prefixes: ["${PREFIX}"]
YAML
for i in $(seq 1 30); do
  [ "$(kubectl get copapatchedimagessigned copa-patched-images-signed -o jsonpath='{.status.byPod[*].enforced}' 2>/dev/null)" != "" ] && break; sleep 2
done
echo "gatekeeper $GATEKEEPER_VERSION and ratify chart $RATIFY_CHART_VERSION ready; patched images under ${PREFIX} must be Notation-signed"
