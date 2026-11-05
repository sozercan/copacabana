#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NS=${NS:-t-work}
SRC=kind-registry:5000/library/nginx:1.25.3-alpine
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl label ns $NS copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
tmpl() { echo "metadata: {labels: {app: $1, copa.sh/rollout: enabled}}
    spec:
      containers: [{name: web, image: \"$2\", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}]"; }
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: dep, namespace: $NS}
spec:
  replicas: 2
  selector: {matchLabels: {app: dep}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    $(tmpl dep $SRC)
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: withpdb, namespace: $NS}
spec:
  replicas: 2
  selector: {matchLabels: {app: withpdb}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    $(tmpl withpdb $SRC)
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata: {name: withpdb, namespace: $NS}
spec: {minAvailable: 2, selector: {matchLabels: {app: withpdb}}}
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: sts, namespace: $NS}
spec:
  replicas: 1
  serviceName: sts
  selector: {matchLabels: {app: sts}}
  template:
    $(tmpl sts $SRC)
---
apiVersion: apps/v1
kind: DaemonSet
metadata: {name: ds, namespace: $NS}
spec:
  selector: {matchLabels: {app: ds}}
  template:
    $(tmpl ds $SRC)
---
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: sts-only, namespace: $NS}
spec:
  replicas: 1
  serviceName: sts-only
  selector: {matchLabels: {app: sts-only}}
  template:
    $(tmpl sts-only nginx:1.25.5-alpine)
YAML
RO=""; timeout 60 bash -c "until kubectl -n $NS get patchrollout -o name | grep -q .; do sleep 2; done"; RO=$(kubectl -n $NS get patchrollout -o jsonpath='{.items[0].metadata.name}')
# scale dep (as an HPA would) while it is restarting
timeout 200 bash -c "until kubectl -n $NS get patchrollout $RO -o jsonpath='{range .status.workloads[*]}{.name}={.phase} {end}' | grep -q 'dep=Restarting'; do sleep 1; done"
kubectl -n $NS scale deploy dep --replicas=3 >/dev/null; echo "scaled dep to 3 during restart at $(date -u +%T)"
# objects created after the SwapList exists
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: bare, namespace: $NS, labels: {app: bare, copa.sh/rollout: enabled}}
spec:
  containers: [{name: web, image: "$SRC"}]
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: cj, namespace: $NS}
spec:
  schedule: "* * * * *"
  jobTemplate:
    spec:
      template:
        metadata: {labels: {copa.sh/rollout: enabled}}
        spec:
          restartPolicy: Never
          containers: [{name: job, image: "$SRC", command: [sh, -c, "apk info -v | grep libcrypto3"]}]
YAML
timeout 400 bash -c "until kubectl -n $NS get patchrollout $RO -o jsonpath='{.status.phase}' | grep -qE 'Succeeded|RolledBack|RecoveryFailed'; do sleep 5; done"
kubectl -n $NS get patchrollout $RO -o jsonpath='{.status.phase}: {.status.message}{"\n"}{range .status.workloads[*]}{"   "}{.name}={.phase} ({.message}){"\n"}{end}'
timeout 90 bash -c "until kubectl -n $NS get pods -l job-name -o name | grep -q .; do sleep 5; done"
echo "--- what each pod runs"
kubectl -n $NS get pods -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[0].image' --no-headers | sed 's|registry.copa-registry.svc.cluster.local:5000/patched/[^@]*|PATCHED|; s|kind-registry:5000/library/||; s|@sha256:\(.\{12\}\)[0-9a-f]*|@\1|'
echo "--- sts-only image inventoried? $(kubectl get sourceimages -o name | grep -c 'nginx' ) nginx SourceImages; refs:"; kubectl get sourceimages -o jsonpath='{range .items[*]}{.status.references}{"\n"}{end}' | grep -c '1.25.5' 
