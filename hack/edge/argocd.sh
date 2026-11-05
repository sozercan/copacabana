#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
set -euo pipefail
kubectl create ns t-git --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl create ns t-argo --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label ns t-argo copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
cat <<'YAML' | kubectl -n t-git apply -f - >/dev/null
apiVersion: v1
kind: ConfigMap
metadata: {name: app}
data:
  deployment.yaml: |
    apiVersion: apps/v1
    kind: Deployment
    metadata: {name: web, namespace: t-argo}
    spec:
      replicas: 2
      selector: {matchLabels: {app: web}}
      strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
      template:
        metadata: {labels: {app: web, copa.sh/rollout: enabled}}
        spec:
          containers:
          - {name: web, image: "kind-registry:5000/library/nginx:1.25.3-alpine", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: gitd}
spec:
  selector: {matchLabels: {app: gitd}}
  template:
    metadata: {labels: {app: gitd}}
    spec:
      containers:
      - name: gitd
        image: alpine:3.20
        command: [sh, -c]
        args:
        - |
          set -e; apk add -q --no-cache git git-daemon
          mkdir -p /tmp/w && cd /tmp/w && git init -q -b main && cp /app/deployment.yaml . &&
          git -c user.email=t@t -c user.name=t add . && git -c user.email=t@t -c user.name=t commit -qm init &&
          git clone -q --bare /tmp/w /srv/app.git && touch /srv/app.git/git-daemon-export-ok &&
          exec git daemon --reuseaddr --base-path=/srv --export-all --verbose /srv
        ports: [{containerPort: 9418}]
        volumeMounts: [{name: app, mountPath: /app}]
      volumes: [{name: app, configMap: {name: app}}]
---
apiVersion: v1
kind: Service
metadata: {name: gitd}
spec: {selector: {app: gitd}, ports: [{port: 9418}]}
YAML
kubectl -n t-git rollout status deploy/gitd --timeout=120s >/dev/null
cat <<'YAML' | kubectl apply -f - >/dev/null
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: web, namespace: argocd}
spec:
  project: default
  source: {repoURL: "git://gitd.t-git.svc.cluster.local/app.git", targetRevision: main, path: "."}
  destination: {server: https://kubernetes.default.svc, namespace: t-argo}
  syncPolicy: {automated: {prune: true, selfHeal: true}}
YAML
timeout 180 bash -c 'until [ "$(kubectl -n argocd get application web -o jsonpath="{.status.sync.status}/{.status.health.status}")" = "Synced/Healthy" ]; do sleep 5; done' || kubectl -n argocd get application web -o jsonpath='{.status.conditions}{"\n"}'
echo "argo app: $(kubectl -n argocd get application web -o jsonpath='{.status.sync.status}/{.status.health.status}') at $(date -u +%T)"
timeout 120 bash -c 'until kubectl -n t-argo get patchrollout -o name | grep -q .; do sleep 3; done'
RO=$(kubectl -n t-argo get patchrollout -o jsonpath='{.items[0].metadata.name}')
# watch Argo status and the rollout through restart and soak
for i in $(seq 1 36); do
  prev=${prev:-}; line="$(date -u +%T) rollout=$(kubectl -n t-argo get patchrollout $RO -o jsonpath='{.status.phase}') argo=$(kubectl -n argocd get application web -o jsonpath='{.status.sync.status}/{.status.health.status}') gen=$(kubectl -n t-argo get deploy web -o jsonpath='{.metadata.generation}') restartedAt=$(kubectl -n t-argo get deploy web -o jsonpath='{.spec.template.metadata.annotations.kubectl\.kubernetes\.io/restartedAt}')"
  [ "$line" != "${prev:-}" ] && echo "$line"; prev=$line
  kubectl -n t-argo get patchrollout $RO -o jsonpath='{.status.phase}' | grep -qE 'Succeeded|RolledBack|RecoveryFailed' && break
  sleep 10
done
kubectl -n t-argo get patchrollout $RO -o jsonpath='{.status.phase}: {.status.message}{"\n"}{range .status.workloads[*]}{"   "}{.name}={.phase} ({.message}){"\n"}{end}'
echo "argo history: $(kubectl -n argocd get application web -o jsonpath='{range .status.history[*]}{.id}:{.revision} {end}' | cut -c1-200)"
echo "pods: $(kubectl -n t-argo get pods -l app=web -o jsonpath='{range .items[*]}{.spec.containers[0].image}{" "}{end}' | sed 's|registry.copa-registry.svc.cluster.local:5000/patched/[^@]*|PATCHED|g; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|g')"
