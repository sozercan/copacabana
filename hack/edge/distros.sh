#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NS=t-distro
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null; kubectl label ns $NS copa.sh/patching=enabled --overwrite >/dev/null
UBI=$(crane digest registry.access.redhat.com/ubi8/ubi-minimal:8.8-860)
declare -A IMGS=(
  [debian]="nginx:1.25.3"
  [ubuntu]="ubuntu:jammy-20230301"
  [ubi]="registry.access.redhat.com/ubi8/ubi-minimal@${UBI}"
  [azurelinux]="mcr.microsoft.com/azurelinux/base/core:3.0.20240727"
  [distroless]="gcr.io/distroless/base-debian11@sha256:010cc31504317d74a1c79993228b6a47090f64fe355ffad43211bb1c16042774"
  [eol-alpine]="alpine:3.14.1"
  [eol-buster]="debian:buster-20231218-slim"
)
for n in "${!IMGS[@]}"; do
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: $n, namespace: $NS}
spec:
  replicas: 0
  selector: {matchLabels: {app: $n}}
  template:
    metadata: {labels: {app: $n, copa.sh/rollout: enabled}}
    spec:
      containers: [{name: c, image: "${IMGS[$n]}", command: [sleep, infinity]}]
YAML
done
echo "created $(date -u +%T)"
