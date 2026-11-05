#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
N=${N:-30}
for i in $(seq 1 $N); do ns=t-s$i
kubectl create ns $ns --dry-run=client -o yaml | kubectl apply -f - >/dev/null; kubectl label ns $ns copa.sh/patching=enabled copa.sh/approval=automatic --overwrite >/dev/null
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: $ns}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  strategy: {type: RollingUpdate, rollingUpdate: {maxUnavailable: 0, maxSurge: 1}}
  template:
    metadata: {labels: {app: web, copa.sh/rollout: enabled}}
    spec:
      containers: [{name: web, image: "kind-registry:5000/library/nginx:1.25.3-alpine", readinessProbe: {httpGet: {path: /, port: 80}, periodSeconds: 3}}]
YAML
done
start=$(date +%s); echo "created $N namespaces at $(date -u +%T)"
maxr=0; first=""
while :; do
  json=$(kubectl get patchrollouts -A -o json)
  read r s t <<<$(echo "$json" | python3 -c '
import json,sys
items=json.load(sys.stdin)["items"]
restarting=sum(1 for i in items for w in i.get("status",{}).get("workloads",[]) if w.get("phase")=="Restarting")
mine=[i for i in items if i["metadata"]["namespace"].startswith("t-s")]
done=sum(1 for i in mine if i.get("status",{}).get("phase") in ("Succeeded","RolledBack","RecoveryFailed"))
print(restarting, done, len(mine))')
  [ $r -gt $maxr ] && maxr=$r
  [ -z "$first" ] && [ $s -gt 0 ] && first=$(( $(date +%s) - start ))
  [ $(( $(date +%s) % 60 )) -lt 6 ] && echo "  $(date -u +%T) restarting-now=$r done=$s/$t"
  [ "$s" = "$N" ] && break
  [ $(( $(date +%s) - start )) -gt 1800 ] && echo TIMEOUT && break
  sleep 5
done
echo "all $N done in $(( $(date +%s) - start ))s; first done at ${first}s; max concurrent restarting observed=$maxr (limit 2)"
kubectl get patchrollouts -A --no-headers | grep '^t-s' | awk '{print $5}' | sort | uniq -c
for n in $(kind get nodes --name copa); do docker exec $n crictl stats --label io.kubernetes.container.name=controller -o json 2>/dev/null | python3 -c '
import json,sys
for s in json.load(sys.stdin).get("stats",[]):
    print("  controller container", s["attributes"]["labels"]["io.kubernetes.pod.name"], "mem", int(s["memory"]["workingSetBytes"]["value"])//1048576, "MiB")' 2>/dev/null; done
