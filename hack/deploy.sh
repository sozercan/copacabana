#!/usr/bin/env bash
# Builds the images, pushes them to the local registry, and installs everything into the kind cluster.
#
#   REGISTRY=in-cluster (default)  staging and patched images live in a Zot registry inside the cluster, with
#                                   separate push accounts for builders and the verifier
#   REGISTRY=external               staging and patched images go to the host's kind-registry (no auth)
#   REGISTRY_REMOTE=ip:port         with REGISTRY=in-cluster, don't run Zot here; reach another cluster's registry
#                                   (its NodePort) under the same name. For consumer clusters.
#   SOURCE_AUTH=path/config.json   Docker config with logins for private source registries (and Docker Hub, to avoid
#                                   its anonymous rate limit). Merged into the controller's and Jobs' credentials.
#   EXTRA_INSECURE=host:port,...    more plain-HTTP source registries
#   NOTATION=true                   also sign patched images with Notation (test certificate in hack/.keys/notation)
#                                   for an image integrity policy such as hack/edge/kyverno-policy.sh
#   MODE=builder|consumer|report-only, PLATFORMS=linux/amd64[,linux/arm64], REPLICAS=2
#   IMAGE_REGISTRY=localhost:5001, IN_CLUSTER_REGISTRY=kind-registry:5000, TAG=dev
#                                   where the controller and worker images are pushed and pulled from
set -euo pipefail
cd "$(dirname "$0")/.."
KCTX=${KCTX:-kind-copa}
CLUSTER=${KCTX#kind-}
MODE=${MODE:-builder}
REGISTRY=${REGISTRY:-in-cluster}
PLATFORMS=${PLATFORMS:-linux/amd64}
ALLOW_EMULATION=${ALLOW_EMULATION:-false}
REPLICAS=${REPLICAS:-2}
ZOT_IMAGE=${ZOT_IMAGE:-ghcr.io/project-zot/zot-linux-amd64:v2.1.23}
REGISTRY_IP=${REGISTRY_IP:-10.96.200.200}
K="kubectl --context ${KCTX}"

case "$REGISTRY" in
  in-cluster)
    REGISTRY_HOST=registry.copa-registry.svc.cluster.local:5000
    INSECURE="kind-registry:5000" ;;   # the in-cluster registry serves TLS from hack/.keys/registry-tls
  external)
    REGISTRY_HOST=kind-registry:5000
    INSECURE=kind-registry:5000 ;;
  *) echo "REGISTRY must be in-cluster or external" >&2; exit 2 ;;
esac
INSECURE="${INSECURE}${EXTRA_INSECURE:+,${EXTRA_INSECURE}}"   # more plain-HTTP source registries

# Controller and worker images: pushed to IMAGE_REGISTRY, pulled from IN_CLUSTER_REGISTRY (the same registry as
# nodes reach it), pinned by digest so Jobs never run a stale worker.
IMAGE_REGISTRY=${IMAGE_REGISTRY:-localhost:5001}
IN_CLUSTER_REGISTRY=${IN_CLUSTER_REGISTRY:-kind-registry:5000}
TAG=${TAG:-dev}
if [ -z "${SKIP_IMAGES:-}" ]; then make images IMAGE_REGISTRY="$IMAGE_REGISTRY" TAG="$TAG"; fi
ctrl_digest=$(crane digest --insecure "${IMAGE_REGISTRY}/copa/copa-rollout-controller:${TAG}")
worker_digest=$(crane digest --insecure "${IMAGE_REGISTRY}/copa/copa-worker:${TAG}")
CONTROLLER_IMAGE="${IN_CLUSTER_REGISTRY}/copa/copa-rollout-controller@${ctrl_digest}"
WORKER_IMAGE="${IN_CLUSTER_REGISTRY}/copa/copa-worker@${worker_digest}"

# Signing key for the verify Job, public key for the controller. Generated once.
mkdir -p hack/.keys
if [ ! -f hack/.keys/signing.key ]; then
  go run ./cmd/worker keygen --out hack/.keys
fi

# Job NetworkPolicies allow the internet but not nodes: ClusterIP traffic to the API server is translated to a node
# address before policy applies, so excluding the service range alone doesn't block it.
NODE_CIDRS=$($K get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"/32 "}{end}' | xargs | sed 's/ /, /g')

# registry_secret NS [HOST USER PASSWORD]: the namespace's copa-registry-credentials, holding one copa registry
# account (if given) plus every login in SOURCE_AUTH.
registry_secret() {
  local cfg
  cfg=$(python3 - "${SOURCE_AUTH:-}" "${2:-}" "${3:-}" "${4:-}" <<'PY'
import base64, json, sys
src, host, user, pw = sys.argv[1:5]
auths = json.load(open(src)).get("auths", {}) if src else {}
auths = {k: v for k, v in auths.items() if v.get("auth")}
if host:
    auths[host] = {"auth": base64.b64encode(f"{user}:{pw}".encode()).decode()}
print(json.dumps({"auths": auths}))
PY
)
  $K -n "$1" create secret generic copa-registry-credentials --type=kubernetes.io/dockerconfigjson \
    --from-literal=.dockerconfigjson="$cfg" --dry-run=client -o yaml | $K apply -f -
}

render() {
  sed -e "s|CONTROLLER_IMAGE|${CONTROLLER_IMAGE}|" -e "s|WORKER_IMAGE|${WORKER_IMAGE}|" -e "s|MODE|${MODE}|" \
    -e "s|INSECURE_REGISTRIES|${INSECURE}|" -e "s|REGISTRY_HOST|${REGISTRY_HOST}|g" -e "s|PLATFORMS|${PLATFORMS}|" -e "s|ALLOW_EMULATION|${ALLOW_EMULATION}|" \
    -e "s|REPLICAS|${REPLICAS}|" -e "s|ZOT_IMAGE|${ZOT_IMAGE}|" -e "s|REGISTRY_CLUSTER_IP|${REGISTRY_IP}|" \
    -e "s|NODE_CIDRS|${NODE_CIDRS}|" "$@"
}

$K apply -f config/crd
$K apply -f config/manager/namespaces.yaml
if [ "$MODE" != consumer ]; then   # consumers never sign, so they don't get the key
  notation_files=()
  if [ "${NOTATION:-false}" = true ]; then
    nk=hack/.keys/notation/notation/localkeys
    [ -f "$nk/copa-test.key" ] || XDG_CONFIG_HOME=$PWD/hack/.keys/notation notation cert generate-test copa-test >/dev/null
    notation_files=(--from-file=notation.key="$nk/copa-test.key" --from-file=notation.crt="$nk/copa-test.crt")
  fi
  $K -n copa-verifier create secret generic copa-signing-key --from-file=signing.key=hack/.keys/signing.key \
    "${notation_files[@]}" --dry-run=client -o yaml | $K apply -f -
fi
$K -n copa-system create secret generic copa-attestation-pubkey --from-file=signing.pub=hack/.keys/signing.pub \
  --dry-run=client -o yaml | $K apply -f -

TLS_DIR=hack/.keys/registry-tls
if [ "$REGISTRY" = in-cluster ] && [ ! -f "$TLS_DIR/tls.crt" ]; then
  # A private CA and a serving certificate for the registry's name and fixed ClusterIP. Generated once.
  mkdir -p "$TLS_DIR"
  openssl req -x509 -newkey rsa:3072 -nodes -days 825 -subj "/CN=copa registry CA" \
    -keyout "$TLS_DIR/ca.key" -out "$TLS_DIR/ca.crt" 2>/dev/null
  openssl req -newkey rsa:3072 -nodes -subj "/CN=${REGISTRY_HOST%:*}" -keyout "$TLS_DIR/tls.key" -out "$TLS_DIR/tls.csr" 2>/dev/null
  printf 'subjectAltName=DNS:%s,DNS:registry.copa-registry.svc,DNS:registry.copa-registry,IP:%s\nextendedKeyUsage=serverAuth\n' \
    "${REGISTRY_HOST%:*}" "$REGISTRY_IP" > "$TLS_DIR/ext.cnf"
  openssl x509 -req -in "$TLS_DIR/tls.csr" -CA "$TLS_DIR/ca.crt" -CAkey "$TLS_DIR/ca.key" -CAcreateserial -days 825 \
    -extfile "$TLS_DIR/ext.cnf" -out "$TLS_DIR/tls.crt" 2>/dev/null
fi
# System roots plus the registry CA, for the controller and Jobs (Go, Trivy, Notation read SSL_CERT_FILE).
bundle=$(mktemp)
cat /etc/ssl/certs/ca-certificates.crt > "$bundle"
[ "$REGISTRY" = in-cluster ] && cat "$TLS_DIR/ca.crt" >> "$bundle"
for ns in copa-system copa-builder copa-verifier; do
  $K -n "$ns" create configmap copa-ca-bundle --from-file=ca-bundle.crt="$bundle" --dry-run=client -o yaml | $K apply --server-side --force-conflicts -f -
done
rm -f "$bundle"

if [ "$REGISTRY" = in-cluster ] && [ -n "${REGISTRY_REMOTE:-}" ]; then
  # Same registry name as the builder cluster, backed by its NodePort. Read-only: no accounts here.
  $K create namespace copa-registry --dry-run=client -o yaml | $K apply -f -
  cat <<YAML | $K apply -f -
apiVersion: v1
kind: Service
metadata: {name: registry, namespace: copa-registry}
spec:
  clusterIP: ${REGISTRY_IP}
  ports: [{name: http, port: 5000}]
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata: {name: registry-remote, namespace: copa-registry, labels: {kubernetes.io/service-name: registry}}
addressType: IPv4
ports: [{name: http, port: ${REGISTRY_REMOTE#*:}}]
endpoints: [{addresses: ["${REGISTRY_REMOTE%:*}"]}]
YAML
fi
if [ "$REGISTRY" = in-cluster ]; then
  # One account per writer. Passwords are generated once and kept out of git with the signing key.
  if [ -z "${REGISTRY_REMOTE:-}" ]; then
  mkdir -p hack/.keys/registry
  htpasswd=""
  for user in builder verifier admin; do
    [ -f "hack/.keys/registry/${user}" ] || openssl rand -hex 24 > "hack/.keys/registry/${user}"
    htpasswd+=$(docker run --rm --entrypoint htpasswd httpd:2.4-alpine -Bbn "$user" "$(cat "hack/.keys/registry/${user}")")$'\n'
  done
  $K apply -f <(render config/registry/zot.yaml | sed -n '1,/^---/p')   # namespace first
  printf '%s' "$htpasswd" | $K -n copa-registry create secret generic zot-htpasswd --from-file=htpasswd=/dev/stdin \
    --dry-run=client -o yaml | $K apply -f -
  $K -n copa-registry create secret tls zot-tls --cert="$TLS_DIR/tls.crt" --key="$TLS_DIR/tls.key" \
    --dry-run=client -o yaml | $K apply -f -
  render config/registry/zot.yaml | $K apply -f -
  $K -n copa-registry rollout restart deploy/registry >/dev/null   # pick up htpasswd or config changes
  $K -n copa-registry rollout status deploy/registry --timeout=3m
  fi

  # kind nodes don't use cluster DNS, so point their containerd at the registry's fixed ClusterIP.
  for node in $(kind get nodes --name "$CLUSTER"); do
    dir="/etc/containerd/certs.d/${REGISTRY_HOST}"
    docker exec "$node" mkdir -p "$dir"
    docker exec -i "$node" cp /dev/stdin "$dir/ca.crt" < "$TLS_DIR/ca.crt"
    printf 'server = "https://%s"\n\n[host."https://%s:5000"]\n  capabilities = ["pull", "resolve"]\n  ca = "%s/ca.crt"\n' \
      "$REGISTRY_HOST" "$REGISTRY_IP" "$dir" | docker exec -i "$node" cp /dev/stdin "$dir/hosts.toml"
  done

  for pair in ${REGISTRY_REMOTE:+none}copa-builder:builder ${REGISTRY_REMOTE:+none}copa-verifier:verifier; do
    [ "${pair#none}" = "$pair" ] || continue
    ns=${pair%%:*}; user=${pair#*:}
    registry_secret "$ns" "$REGISTRY_HOST" "$user" "$(cat "hack/.keys/registry/${user}")"
  done
else
  for ns in copa-builder copa-verifier; do
    if [ -n "${SOURCE_AUTH:-}" ]; then registry_secret "$ns"; else $K -n "$ns" delete secret copa-registry-credentials --ignore-not-found; fi
  done
fi
# The controller reads sources (to resolve tags) but never writes, so it only gets source logins.
if [ -n "${SOURCE_AUTH:-}" ]; then registry_secret copa-system; else $K -n copa-system delete secret copa-registry-credentials --ignore-not-found; fi

# BuildKit reaches every plain-HTTP registry it pulls sources from, and pushes staging images over TLS to the
# in-cluster registry, trusting its CA.
toml=""
IFS=, read -ra hosts <<<"$INSECURE"
for h in "${hosts[@]}"; do toml+="[registry.\"${h}\"]"$'\n'"  http = true"$'\n'"  insecure = true"$'\n'; done
bk_files=()
if [ "$REGISTRY" = in-cluster ]; then
  toml+="[registry.\"${REGISTRY_HOST}\"]"$'\n'"  ca = [\"/etc/buildkit/ca.crt\"]"$'\n'
  bk_files=(--from-file=ca.crt="$TLS_DIR/ca.crt")
fi
$K -n copa-builder create configmap buildkitd-config --from-literal=buildkitd.toml="$toml" "${bk_files[@]}" \
  --dry-run=client -o yaml | $K apply -f -

$K apply -f config/rbac
render config/builder/builder.yaml | $K apply -f -
for f in config/admission/*.yaml; do render "$f" | $K apply -f -; done
render config/manager/controller.yaml | $K apply -f -

$K -n copa-builder rollout status deploy/trivy-server --timeout=10m
$K -n copa-system rollout status deploy/copa-rollout-controller --timeout=3m
echo "deployed in ${MODE} mode, registry ${REGISTRY} (${REGISTRY_HOST}), platforms ${PLATFORMS}, ${REPLICAS} replicas"
