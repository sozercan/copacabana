#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
NODE_IP=$(kubectl get node copa-worker2 -o jsonpath='{.status.addresses[0].address}')
WEB_IP=$(kubectl -n demo-dev get pod -l app=web -o jsonpath='{.items[0].status.podIP}')
TRIVY=$(kubectl -n copa-builder get svc trivy-server -o jsonpath='{.spec.clusterIP}')
probe_pod() {
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: netprobe, namespace: $1, labels: {copa.sh/stage: scan}}
spec:
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: p
    image: busybox:1.36
    command: [sleep, "3600"]
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
YAML
kubectl -n $1 wait pod/netprobe --for=condition=Ready --timeout=60s >/dev/null
}
check() { # ns label host port
  if kubectl -n $1 exec netprobe -- nc -z -w 3 $3 $4 >/dev/null 2>&1; then r=REACHABLE; else r=blocked; fi
  printf '  %-9s %-34s %s\n' "$r" "$2" "$3:$4"
}
for ns in copa-builder copa-verifier; do
  echo "$ns job pod:"
  probe_pod $ns
  check $ns "kube-apiserver (ClusterIP)" 10.96.0.1 443
  check $ns "kube-apiserver (control plane)" $(kubectl get node copa-control-plane -o jsonpath='{.status.addresses[0].address}') 6443
  check $ns "kubelet on a node" $NODE_IP 10250
  check $ns "team pod (demo-dev web)" $WEB_IP 80
  check $ns "in-cluster registry" 10.96.200.200 5000
  check $ns "trivy-server" $TRIVY 4954
  check $ns "internet (ghcr.io)" ghcr.io 443
  kubectl -n $ns delete pod netprobe --wait=false >/dev/null
done
echo "control: same probe from an unrestricted namespace (demo-dev) to the apiserver ClusterIP:"
kubectl -n demo-dev run netprobe --image=busybox:1.36 --restart=Never --command -- sleep 3600 >/dev/null; kubectl -n demo-dev wait pod/netprobe --for=condition=Ready --timeout=60s >/dev/null
check demo-dev "kube-apiserver (ClusterIP)" 10.96.0.1 443; kubectl -n demo-dev delete pod netprobe --wait=false >/dev/null
echo "--- pod security"
for ns in copa-verifier copa-registry copa-system copa-builder; do
  if kubectl -n $ns run privtest --image=busybox:1.36 --restart=Never --privileged --dry-run=server -o name >/dev/null 2>/tmp/pe; then echo "  $ns: privileged pod ALLOWED ($(kubectl get ns $ns -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}'))"; else echo "  $ns: privileged pod rejected ($(grep -o 'violates PodSecurity "[a-z]*:[a-z]*"' /tmp/pe))"; fi
done
echo "--- RBAC"
for sa in copa-builder:copa-scan copa-builder:copa-build copa-verifier:copa-verify; do
  n=$(kubectl auth can-i --list --as=system:serviceaccount:${sa%%:*}:${sa#*:} 2>/dev/null | grep -vE 'selfsubject|^Resources|\[/(api|apis|healthz|livez|readyz|version|openapi)' | grep -c .)
  echo "  $sa: $n non-discovery permissions"
done
C=system:serviceaccount:copa-system:copa-rollout-controller
for check in "get secrets -A" "list secrets -A" "create pods -n demo-dev" "patch deployments -n kube-system" "delete deployments -n demo-dev" "create jobs -n demo-dev" "create jobs -n copa-builder" "update swaplists -n demo-dev" "create mutatingadmissionpolicies"; do
  printf '  controller %-38s %s\n' "$check" "$(kubectl auth can-i $check --as=$C)"
done
