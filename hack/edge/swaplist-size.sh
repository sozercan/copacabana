#!/usr/bin/env bash
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
# Build SwapLists of N entries (5 aliases each) as the controller would, and time admission with them.
NS=t-scale-sw
kubectl create ns $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null; kubectl label ns $NS copa.sh/patching=enabled --overwrite >/dev/null
SA=system:serviceaccount:copa-system:copa-rollout-controller
P=registry.copa-registry.svc.cluster.local:5000/patched
for N in 100 250 500 1000; do
python3 - $N $P > /tmp/sw.json <<'PY'
import json,sys,hashlib
n=int(sys.argv[1]); P=sys.argv[2]
entries=[]; images={}
for i in range(n):
    d="sha256:"+hashlib.sha256(f"s{i}".encode()).hexdigest()
    pd="sha256:"+hashlib.sha256(f"p{i}".encode()).hexdigest()
    src=f"index.docker.io/library/app{i}:1.0.{i}"
    aliases=[src,f"docker.io/library/app{i}:1.0.{i}",f"app{i}:1.0.{i}",f"docker.io/app{i}:1.0.{i}",f"index.docker.io/app{i}:1.0.{i}"]
    ref=f"{P}/docker.io/library/app{i}@{pd}"
    entries.append({"source":src,"aliases":aliases,"sourceDigest":d,"fallback":f"index.docker.io/library/app{i}@{d}",
      "selected":{"ref":ref,"tag":f"1.0.{i}-1","digest":pd,"revision":1,"platforms":[{"platform":"linux/amd64","sourceDigest":d,"patchedDigest":pd}]}})
    for a in aliases: images[a]=ref
print(json.dumps({"apiVersion":"rollout.copa.sh/v1alpha1","kind":"SwapList","metadata":{"name":"copa","namespace":"t-scale-sw"},"spec":{"images":images,"entries":entries}}))
PY
kubectl -n $NS delete swaplist copa --ignore-not-found >/dev/null
size=$(wc -c </tmp/sw.json)
start=$(date +%s.%N); out=$(kubectl create -f /tmp/sw.json --as=$SA 2>&1); rc=$?; t=$(echo "$(date +%s.%N) - $start" | bc)
if [ $rc -eq 0 ]; then
  s2=$(date +%s.%N); kubectl -n $NS run probe --image=app7:1.0.7 --labels=copa.sh/rollout=enabled --dry-run=server -o jsonpath='{.spec.containers[0].image}' >/tmp/img 2>&1; t2=$(echo "$(date +%s.%N) - $s2" | bc)
  printf 'N=%-5s %7s bytes  write %.2fs  pod admission %.2fs  swapped=%s\n' $N $size $t $t2 "$(grep -c patched /tmp/img)"
else
  printf 'N=%-5s %7s bytes  write REJECTED: %s\n' $N $size "$(echo "$out" | grep -oE 'cost[^.]*|too large[^.]*|Request entity too large[^.]*|exceeded[^.]*' | head -1)"
fi
done
