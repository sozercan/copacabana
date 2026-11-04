#!/usr/bin/env bash
# Creates a kind cluster wired to a local plain-HTTP registry reachable as kind-registry:5000
# from nodes, pods and BuildKit, and as localhost:5001 from the host.
set -euo pipefail
CLUSTER=${CLUSTER:-copa}
REG_NAME=kind-registry
REG_PORT=5001
NODE_IMAGE=${NODE_IMAGE:-kindest/node:v1.37.0}

if [ "$(docker inspect -f '{{.State.Running}}' "$REG_NAME" 2>/dev/null || true)" != 'true' ]; then
  docker run -d --restart=always -p "127.0.0.1:${REG_PORT}:5000" --network bridge --name "$REG_NAME" registry:3
fi

if ! kind get clusters | grep -qx "$CLUSTER"; then
cat <<KIND | kind create cluster --name "$CLUSTER" --image "$NODE_IMAGE" --config=-
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
containerdConfigPatches:
- |-
  [plugins."io.containerd.grpc.v1.cri".registry]
    config_path = "/etc/containerd/certs.d"
nodes:
- role: control-plane
- role: worker
  labels:
    copa.sh/pool: builder
- role: worker
KIND
fi

for node in $(kind get nodes --name "$CLUSTER"); do
  for host in "kind-registry:5000" "localhost:5001"; do
    dir="/etc/containerd/certs.d/${host}"
    docker exec "$node" mkdir -p "$dir"
    printf '[host."http://kind-registry:5000"]\n' | docker exec -i "$node" cp /dev/stdin "$dir/hosts.toml"
  done
done

if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "$REG_NAME")" = 'null' ]; then
  docker network connect kind "$REG_NAME"
fi

# Taint the builder node so only build Jobs land there.
builder=$(kubectl --context "kind-${CLUSTER}" get nodes -l copa.sh/pool=builder -o jsonpath="{.items[0].metadata.name}")
kubectl --context "kind-${CLUSTER}" taint nodes --overwrite "$builder" copa.sh/builder=true:NoSchedule
echo "cluster ${CLUSTER} ready; registry at localhost:${REG_PORT} (kind-registry:5000 in cluster)"
