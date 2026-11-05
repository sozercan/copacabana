# Validation

Everything here ran on kind (Kubernetes 1.37, three nodes) on a 16-core VM, with the real Copa, rootless BuildKit,
Trivy 0.76.1, Zot 2.1.23, Argo CD 3.6.0 and Kyverno 1.19.2. Scripts are in `hack/edge/`; the forged-patch tool
is `hack/forge/`. Earlier rounds (forged patches, rollback on readiness failure, mid-rollout changes, tag drift,
the missed-swap detector, SwapList abuse) are summarized in the README.

Default deploy for these runs: `REGISTRY=in-cluster`, two controller replicas with leader election.

## In-cluster registry (option 2)

Staging and patched images live in a Zot registry inside the cluster (`config/registry/zot.yaml`), served over
TLS from a generated CA that node containerd, BuildKit, the controller and Jobs trust. Source images stay wherever
teams keep them.

| Check | Result |
| --- | --- |
| Build, verify, promote, roll out (`demo-dev` automatic, `demo-prod` approved) | Pass. Nodes pull the patched image from the in-cluster registry |
| Per-account access (Zot access control) | Anonymous: read `patched/*` only. Builder: write `staging/*`, denied on `patched/*`. Verifier: write `patched/*`, read `staging/*`, denied writing `staging/*`. Other paths 403 |
| Referrers | Attestations and Notation signatures use the registry's referrers API; no fallback tags |
| TLS | Plain HTTP refused; full scan, build, verify and sign pipeline re-run over TLS |
| Registry down mid-build | Build fails at `build`, last good patch stays current, gap recorded |
| Registry down during a rollout | `ErrImagePull`, deadline, rolled back to the pinned source |
| Consumer cluster using this registry | Pass (see below) |

The registry is a runtime dependency once patches roll out: a pod rescheduled to a node without the image needs
it. It runs one replica on a ReadWriteOnce volume here.

## Results by area

| Area | Result | Bugs fixed |
| --- | --- | --- |
| Docker Hub short names, long forms, `tag@digest` | Pass | `tag@digest` was never swapped (only standard spellings were aliases) and recovery then failed; the missed-swap detector didn't notice. Aliases now include the spellings Deployments use; the detector compares normalized references, which also catches pods whose template copied a stale `copa.sh/swapped` annotation |
| Private source registry (htpasswd) | Skipped by design, with a `PrivateSource` warning event | Added: credentials for the controller and Jobs, and an `ImageNotResolved` warning event when an image can't be read (was only a log line). Patching a private source made its layers readable anonymously from `patched/**`, so sources that can't be read without credentials are no longer patched |
| Argo CD, automated sync + self-heal + prune | Pass. App stayed `Synced/Healthy`, the restart annotation wasn't reverted; a forced `Replace=true` sync recreated pods, which admission patched again | |
| Isolation | NetworkPolicies enforced; Pod Security as intended; Job service accounts have no permissions or tokens | Kubelets on node IPs were reachable from Job pods (added node exclusions). The controller's cluster-wide RBAC is now limited by two ValidatingAdmissionPolicies: opted-in namespaces only, pods only as dry runs, Deployments only the `kubectl.kubernetes.io/restartedAt` pod-template annotation |
| Multi-revision lifecycle | Pass. Back-out of `-2` restored `-1` (the previous patch, not the source) | Added `copa.sh/rebuild=force` |
| Failure injection | Registry down, Trivy down, BuildKit killed, Job deadline: build fails at the right stage, last patch kept. Leader killed mid-build: standby finished it | A cordoned builder node looked like a slow scan for 30 minutes; the PatchBuild now says the pod can't be scheduled |
| StatefulSets, DaemonSets, CronJobs, bare pods, HPA scaling, PDBs | Pass | Scaling mid-restart was treated as a team deploy (generation changed), skipping health checks; now compares the pod template. StatefulSet, DaemonSet and CronJob images weren't inventoried; rollouts now list StatefulSets and DaemonSets as not restarted |
| Copa distros | Debian, Ubuntu, Alpine 3.14 (EOL): promoted. UBI8: verification blocked it (57 new vulnerabilities from packages the update pulled in). Debian buster (EOL), Azure Linux 3, an old distroless digest: Copa failed, gap recorded | |
| Multi-platform (amd64 + arm64 under qemu) | Pass. arm64 image runs with updated packages | `arm64/v8` never matched `linux/arm64`, so arm64 was dropped silently. SourceImage platforms were frozen at creation. A platform change left an incomplete patch current and never rebuilt |
| Scale | 30 rollouts in 9m11s; controller 41 MiB | SwapList guard hit the CEL cost limit at 100–250 entries per namespace (now linear; 500 entries fine, ~800 is the object-size ceiling). The restart limit of 2 was exceeded under parallel reconciles (now 2) |
| Notation + Kyverno | Signed patch admitted; unsigned patch denied whether swapped or referenced directly; source images unaffected; rollout dry run passes through Kyverno | Needed TLS on the registry. Publishing is now atomic: tag written after the attestation and signature |
| Leader election, uninstall | Pass on a second cluster | Added leader election and `hack/uninstall.sh` |
| Consumer cluster | Pass. Second kind cluster in consumer mode used this cluster's patches; no builds, Jobs or signing key there | |

## Findings that need decisions

- **Turning on signature enforcement** blocks scale-ups of workloads whose selected patch predates signing.
  Force-rebuild everything selected first.
- **Docker Hub's anonymous rate limit** hit scan Jobs after a handful of images. Use `SOURCE_AUTH` with Docker Hub
  credentials or a pull-through mirror.
- **API server reachability** from Job pods: still reachable under kindnet even with its node address excluded.
  Job pods carry no token. Check on the target CNI.
- **Old revisions** aren't garbage-collected. Any retention policy must keep revisions a SwapList selects or
  records as previous.
- **Failed builds** wait for the next rescan interval before retrying.
- **StatefulSets and DaemonSets** aren't restarted by the controller; their pods are patched when recreated.
- **Copa failures** worth reporting upstream: Azure Linux 3 version check, distroless debian11 `busybox-static` 404.

## Not run

- 24-hour soak and daily rescans (time-based logic ran with short settings)
- Real arm64 nodes, and one platform failing while the other succeeds
- Flux, Notation with a real CA or Key Vault, CRD version conversion, envtest
