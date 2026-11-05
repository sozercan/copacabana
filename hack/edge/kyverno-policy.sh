#!/usr/bin/env bash
# Enforces Notation signatures on patched images with Kyverno (deploy with NOTATION=true first).
# Pods whose images come from the patched prefix must carry a signature from the test certificate.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CERT=$ROOT/hack/.keys/notation/notation/localkeys/copa-test.crt
PREFIX=${PREFIX:-registry.copa-registry.svc.cluster.local:5000/patched}
cat <<YAML | kubectl apply -f -
apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: copa-patched-images-signed
spec:
  validationFailureAction: Enforce
  webhookTimeoutSeconds: 30
  rules:
  - name: notation-signature
    match:
      any:
      - resources: {kinds: [Pod]}
    verifyImages:
    - type: Notary
      imageReferences: ["${PREFIX}/*"]
      mutateDigest: false
      verifyDigest: true
      required: true
      attestors:
      - count: 1
        entries:
        - certificates:
            cert: |-
$(sed 's/^/              /' "$CERT")
YAML
kubectl wait clusterpolicy copa-patched-images-signed --for=condition=Ready --timeout=120s
