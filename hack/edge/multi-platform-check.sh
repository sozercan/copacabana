SI=nginx-dce83ce539
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
kubectl get patchbuilds -n copa-builder -l copa.sh/source-image=$SI --sort-by=.metadata.creationTimestamp -o jsonpath='{range .items[*]}{.metadata.name} {.spec.reason} {.status.phase} {.metadata.creationTimestamp} {.status.completionTime}{"\n"}{end}'
kubectl get sourceimage $SI -o jsonpath='current={.status.current.tag} platforms={.status.current.platforms[*].platform}{"\n"}'
REF=$(kubectl get sourceimage $SI -o jsonpath='{.status.current.ref}' | sed 's|registry.copa-registry.svc.cluster.local:5000|localhost:5002|')
kubectl -n copa-registry port-forward svc/registry 5002:5000 >/dev/null 2>&1 & PF=$!; sleep 2
crane manifest --insecure $REF | python3 -c 'import json,sys; m=json.load(sys.stdin); print("index:", [x["platform"]["architecture"]+"/"+x["platform"].get("variant","") for x in m["manifests"]], "annotations:", {k.split("/")[-1]:v[:20] for k,v in m.get("annotations",{}).items()})'
for arch in amd64 arm64; do echo "$arch config arch: $(crane config --insecure --platform linux/$arch $REF | python3 -c 'import json,sys; c=json.load(sys.stdin); print(c["architecture"], c.get("variant",""))')"; done
kill $PF
timeout 400 bash -c 'until kubectl -n t-mp get patchrollout --no-headers | grep -- "-2 " | grep -qE "Succeeded|RolledBack|Recovery"; do sleep 10; done'
kubectl -n t-mp get patchrollout --no-headers | awk '{print $3,$4}'
echo "pods imageID: $(kubectl -n t-mp get pods -l app=web -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{" "}{end}' | grep -o '@sha256:.\{12\}' | sort | uniq -c | tr '\n' ' ')"
