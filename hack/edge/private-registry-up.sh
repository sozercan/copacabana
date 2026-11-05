#!/usr/bin/env bash
# Starts an htpasswd-protected source registry (private-registry:5000 on the kind network, localhost:5003 on the
# host), pushes a test image to it, points kind nodes at it, and writes a SOURCE_AUTH file for hack/deploy.sh:
#   SOURCE_AUTH=hack/.keys/privreg/source-auth.json EXTRA_INSECURE=private-registry:5000 make deploy
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
D=$ROOT/hack/.keys/privreg
CLUSTER=${CLUSTER:-copa}
mkdir -p "$D"
[ -f "$D/pw" ] || openssl rand -hex 16 | tr -d '\n' > "$D/pw"
docker run --rm --entrypoint htpasswd httpd:2.4-alpine -Bbn team "$(cat "$D/pw")" > "$D/htpasswd"
docker rm -f private-registry >/dev/null 2>&1 || true
docker run -d --name private-registry --network kind -p 127.0.0.1:5003:5000 -v "$D:/auth" \
  -e REGISTRY_AUTH=htpasswd -e REGISTRY_AUTH_HTPASSWD_REALM=private -e REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd registry:3 >/dev/null
python3 - "$D" <<'PY'
import base64, json, sys
d = sys.argv[1]
a = base64.b64encode(("team:" + open(d + "/pw").read()).encode()).decode()
json.dump({"auths": {"private-registry:5000": {"auth": a}, "localhost:5003": {"auth": a}}}, open(d + "/source-auth.json", "w"))
PY
mkdir -p "$D/docker" && cp "$D/source-auth.json" "$D/docker/config.json"
sleep 2
DOCKER_CONFIG=$D/docker crane copy --platform linux/amd64 localhost:5001/library/nginx:1.25.3-alpine localhost:5003/team/nginx:1.25.3-alpine
for node in $(kind get nodes --name "$CLUSTER"); do
  dir=/etc/containerd/certs.d/private-registry:5000
  docker exec "$node" mkdir -p "$dir"
  printf '[host."http://private-registry:5000"]\n' | docker exec -i "$node" cp /dev/stdin "$dir/hosts.toml"
done
echo "private registry ready; SOURCE_AUTH=$D/source-auth.json"
