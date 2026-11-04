In-cluster Copa OS patching: implementation sketch

Nov 3, 2026 · @Sertac · Companion to [design.md](design.md)

This sketch shows what the design would look like as code and manifests. Names, APIs and YAML are illustrative. Items marked **verify** depend on Kubernetes or Azure behavior we haven't tested yet.

# Shape of the project
One new repository, separate from Copa, built with controller-runtime (kubebuilder layout). It ships two binaries and one Helm chart.

| **Binary** | **Runs as** | **Does** |
| --- | --- | --- |
| `copa-rollout-controller` | Deployment in `copa-system` | Inventory, scan and build scheduling, candidates, rollouts, recovery, reports, metrics |
| `copa-worker` | Image used by scan, build and verify Jobs | `copa-worker scan`, `copa-worker build`, `copa-worker verify`. Wraps Trivy and Copa, talks to the registry with go-containerregistry, signs with notation-go |

The controller runs in one of three modes, set per cluster:

| **Mode** | **Clusters** | **Turns on** |
| --- | --- | --- |
| `report-only` | Pilot phase 0, any cluster | Inventory, scans, report. No builds, no swaps |
| `builder` | Non-production builder cluster | Everything: inventory, scans, builds, verify, promotion, rollout |
| `consumer` | Production clusters | Inventory, catalog lookup in `patched/*`, rollout. No Jobs |

```
copa-rollout/
  api/v1alpha1/
    patchpolicy_types.go      # cluster policy: approval, timings, concurrency, registries
    sourceimage_types.go      # one per source digest in use
    patchbuild_types.go       # one per build attempt (builder mode only)
    swaplist_types.go         # one per opted-in namespace, read by admission
    patchrollout_types.go     # one per candidate per rollout group
    patchapproval_types.go    # created by approvers
  cmd/
    controller/main.go
    worker/main.go            # scan | build | verify subcommands
  internal/
    inventory/                # Deployment watch, image extraction, normalization, tag resolution
    registry/                 # ACR auth, catalog lookup in patched/*, referrers
    scan/                     # scan Job templates, report parsing (Trivy JSON)
    build/                    # PatchBuild reconciler, build Job templates
    verify/                   # layer check, index assembly, attestation, signing, promotion
    rollout/                  # PatchRollout state machine, restart limiter, health checks
    swap/                     # SwapList writer, missed-swap detection
    report/                   # adoption and gap report, Prometheus metrics
  config/
    crd/  rbac/  manager/
    admission/
      mutating-policy.yaml
      mutating-binding.yaml
      validating-swaplist.yaml
    builder/                  # namespace, node pool taints, NetworkPolicy, SAs
    verifier/
  charts/copa-rollout/
  test/
    envtest/                  # reconciler tests
    e2e/                      # kind with Kubernetes 1.36 and a local registry
```

# Custom resources
All in API group `rollout.copa.sh/v1alpha1`.

## PatchPolicy (cluster-scoped)
What the platform and teams agree up front. One `default` policy, with more later if groups need different timings.

```yaml
apiVersion: rollout.copa.sh/v1alpha1
kind: PatchPolicy
metadata:
  name: default
spec:
  namespaceSelector:
    matchLabels: {copa.sh/patching: enabled}
  platforms: [linux/amd64]          # every platform must be patched before a candidate is eligible
  registry:
    patched: myacr.azurecr.io/patched
    staging: myacr.azurecr.io/staging
    reports: myacr.azurecr.io/staging/reports
    sourceCache: myacr.azurecr.io/cache   # ACR artifact cache for docker.io and others
  scan:
    rescanInterval: 24h
    trivyDBRepository: myacr.azurecr.io/mirror/trivy-db
  build:
    maxConcurrentJobs: 4
    jobTimeout: 30m
  rollout:
    approval: Manual                # Manual | Automatic
    rolloutDeadline: 15m
    healthyFor: 10m
    soak: 24h
    maxConcurrentRestarts: 3        # per cluster
    maintenanceWindow: {days: [Mon, Tue, Wed, Thu], start: "09:00", end: "16:00", timeZone: UTC}
  eligibility:
    rejectFloatingTags: true        # latest, 1, 1.25 and similar
```

## SourceImage (cluster-scoped)
One per source digest any opted-in template uses. The name is derived from the digest, so it's stable and deduplicates across namespaces.

```go
type SourceImageSpec struct {
    Repository string            `json:"repository"`          // docker.io/library/nginx
    Digest     string            `json:"digest"`              // index or manifest digest
    Platforms  []PlatformDigest  `json:"platforms"`           // linux/amd64 -> sha256:...
}

type SourceImageStatus struct {
    References []string           `json:"references"`         // image strings seen in templates, e.g. nginx:1.25.3
    UsedBy     []WorkloadRef      `json:"usedBy"`
    LastScan   *ScanResult        `json:"lastScan,omitempty"`  // DB version, time, fixable count, report digest
    Current    *PatchRef          `json:"current,omitempty"`   // latest promoted patch for this digest
    Gap        *Gap               `json:"gap,omitempty"`       // EOL, unsupported, no fix yet, build failed
    Conditions []metav1.Condition `json:"conditions"`
}
```

## PatchBuild (namespaced, `copa-builder`, builder mode only)
One per build attempt. Owns its Jobs.

```go
type PatchBuildSpec struct {
    SourceImage string   `json:"sourceImage"`
    Platforms   []string `json:"platforms"`
    Reason      string   `json:"reason"`   // NewSource | NewFixes | Manual
}

type PatchBuildStatus struct {
    Phase     BuildPhase      `json:"phase"` // Scanning, Building, Verifying, Promoted, NothingToFix, Failed
    Platforms []PlatformBuild `json:"platforms"` // per platform: report digest, job names, staged digest
    Promoted  *PatchRef       `json:"promoted,omitempty"` // patched/nginx:1.25.3-2@sha256:...
    Failure   *Failure        `json:"failure,omitempty"`  // stage, reason, retryable
}
```

## SwapList (namespaced, one named `copa` per opted-in namespace)
Admission reads `spec.images`, a flat map. The controller writes every normalized alias of the source as a key, so admission only needs an exact lookup. CEL doesn't normalize image references.

```yaml
apiVersion: rollout.copa.sh/v1alpha1
kind: SwapList
metadata:
  name: copa
  namespace: payments-dev
spec:
  images:                                   # what admission reads
    nginx:1.25.3: myacr.azurecr.io/patched/nginx@sha256:9b1c...
    docker.io/nginx:1.25.3: myacr.azurecr.io/patched/nginx@sha256:9b1c...
    docker.io/library/nginx:1.25.3: myacr.azurecr.io/patched/nginx@sha256:9b1c...
  entries:                                  # what people and the controller read
  - source: docker.io/library/nginx:1.25.3
    sourceDigest: sha256:4f7a...
    selected:  {ref: myacr.azurecr.io/patched/nginx:1.25.3-2, digest: sha256:9b1c...}
    previous:  {ref: myacr.azurecr.io/patched/nginx:1.25.3-1, digest: sha256:77e0...}
    candidate: null
    rollout: nginx-4f7a-r2
status:
  workloads:
  - {name: checkout, uid: 0c9e..., adopted: true}
```

## PatchRollout and PatchApproval (namespaced)
`PatchRollout` is created by the controller for one candidate in one rollout group. Only the controller writes it. Approvers create a `PatchApproval`, which is the only thing they need RBAC for. That keeps approvals auditable and stops an approver from editing what they approved.

```yaml
apiVersion: rollout.copa.sh/v1alpha1
kind: PatchApproval
metadata:
  name: nginx-4f7a-r2-approval
  namespace: payments-prod
spec:
  rollout: nginx-4f7a-r2
  candidateDigest: sha256:9b1c...      # must match, or the approval is ignored
```

`PatchRollout.status.phase` moves through:

```
Pending -> AwaitingApproval -> Selecting -> Restarting -> Soaking -> Succeeded
                                    \            \           \
                                     +------------+-----------+--> RollingBack -> RolledBack
                                                                              \-> RecoveryFailed
```

# Admission
Assumes Kubernetes 1.36 with MutatingAdmissionPolicy in `admissionregistration.k8s.io/v1`. On 1.34 and 1.35 it's `v1beta1` behind a feature gate. **Verify** what AKS ships.

The binding scopes the policy to opted-in namespaces and labelled pods. The param is the namespace's `SwapList`, and a missing one means there's nothing to swap.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingAdmissionPolicyBinding
metadata:
  name: copa-image-swap
spec:
  policyName: copa-image-swap
  paramRef:
    name: copa                           # SwapList name; namespace defaults to the pod's
    parameterNotFoundAction: Allow
  matchResources:
    namespaceSelector:
      matchLabels: {copa.sh/patching: enabled}
    objectSelector:
      matchLabels: {copa.sh/rollout: enabled}
```

The policy only matches pod creates. It annotates the pod with what it swapped, and uses optional fields so it never writes an empty container list. Once swapped, the images no longer match any key, so reinvocation is a no-op.

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingAdmissionPolicy
metadata:
  name: copa-image-swap
spec:
  paramKind:
    apiVersion: rollout.copa.sh/v1alpha1
    kind: SwapList
  matchConstraints:
    resourceRules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      operations: ["CREATE"]
      resources: ["pods"]
  failurePolicy: Ignore
  reinvocationPolicy: IfNeeded
  variables:
  - name: swaps
    expression: "has(params.spec.images) ? params.spec.images : {}"
  - name: containers
    expression: "object.spec.containers.filter(c, c.image in variables.swaps)"
  - name: initContainers
    expression: >
      has(object.spec.initContainers)
        ? object.spec.initContainers.filter(c, c.image in variables.swaps)
        : []
  mutations:
  - patchType: ApplyConfiguration
    applyConfiguration:
      expression: >
        variables.containers.size() + variables.initContainers.size() == 0 ? Object{} :
        Object{
          metadata: Object.metadata{
            annotations: {
              "copa.sh/swapped": (variables.initContainers + variables.containers)
                  .map(c, c.name + "=" + c.image).join(","),
              "copa.sh/swaplist-generation": string(params.metadata.generation)
            }
          },
          spec: Object.spec{
            ?containers: variables.containers.size() == 0 ? optional.none() :
              optional.of(variables.containers.map(c,
                Object.spec.containers{name: c.name, image: variables.swaps[c.image]})),
            ?initContainers: variables.initContainers.size() == 0 ? optional.none() :
              optional.of(variables.initContainers.map(c,
                Object.spec.initContainers{name: c.name, image: variables.swaps[c.image]}))
          }
        }
```

**Verify:** that MutatingAdmissionPolicy accepts optional field syntax in `ApplyConfiguration`, that `containers` merges by name, and that `params.metadata.generation` is readable. If optional fields aren't allowed, split this into separate mutations and test how variables are re-evaluated between them.

A ValidatingAdmissionPolicy on `SwapList` is a second lock behind RBAC. Every value must be a digest in `patched/*`, or the recorded source digest for a first-rollout fallback, and only the controller may write it:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: copa-swaplist-guard
spec:
  matchConstraints:
    resourceRules:
    - apiGroups: ["rollout.copa.sh"]
      apiVersions: ["v1alpha1"]
      operations: ["CREATE", "UPDATE"]
      resources: ["swaplists"]
  validations:
  - expression: >
      request.userInfo.username == "system:serviceaccount:copa-system:copa-rollout-controller"
    message: "only the copa controller may write swap lists"
  - expression: >
      !has(object.spec.images) || object.spec.images.all(k,
        (object.spec.images[k].startsWith("myacr.azurecr.io/patched/")
          && object.spec.images[k].contains("@sha256:"))
        || (has(object.spec.entries) && object.spec.entries.exists(e,
              object.spec.images[k] == e.source.split(":")[0] + "@" + e.sourceDigest)))
    message: "swap targets must be digests in patched/* or the recorded source digest"
```

The source-digest check above is simplified. A registry host with a port breaks the `split(":")`. The real version stores the fallback reference explicitly in each entry.

Signature checks stay where they are today, in the cluster's existing image integrity policy (for example Ratify or AKS Image Integrity), extended to require the verify identity's signature on anything from `patched/*`.

# Controller loops

| **Reconciler** | **Watches** | **Does** |
| --- | --- | --- |
| Inventory | Deployments in opted-in namespaces with the opt-in label | Extract images from the pod template, normalize, reject floating tags, resolve to digests, upsert `SourceImage`, notice tag drift |
| Scan scheduler (builder) | `SourceImage`, a timer, Trivy DB version | Create scan Jobs when a source is new, the DB moved and the interval passed, or an operator asked |
| Build (builder) | `PatchBuild`, Jobs | Create build Jobs per platform, then the verify Job. Record results. Enforce `maxConcurrentJobs` |
| Catalog (consumer) | Timer | Find promoted patches in `patched/*` for each `SourceImage`, check signature and attestation |
| Candidate | `SourceImage.status.current` | For each opted-in namespace using the source, create a `PatchRollout` if the promoted patch is newer than the selected one |
| Rollout | `PatchRollout`, `PatchApproval`, Deployments, Pods | Run the state machine. Write `SwapList`, server-side dry-run, restart with the limiter, check images and health, soak, roll back |
| Missed swaps | Pods with the opt-in label | Flag pods whose image is a `SwapList` key but which have no `copa.sh/swapped` annotation |

The rollout reconciler records each Deployment's UID and pod template hash before restarting it. If either changes during the rollout, a team deployed. The controller re-checks that workload and never reverts it.

## Controller RBAC
| **Resource** | **Verbs** | **Scope** |
| --- | --- | --- |
| deployments | get, list, watch, patch | Opted-in namespaces, through a RoleBinding the namespace opt-in creates |
| pods | get, list, watch, create (dry-run only) | Opted-in namespaces. Kubernetes RBAC can't limit `create` to dry-run, so this is a real create permission. The design flags it for security review |
| swaplists, patchrollouts | all | Opted-in namespaces |
| patchapprovals | get, list, watch | Opted-in namespaces |
| sourceimages, patchpolicies | all / get | Cluster |
| jobs | create, delete, get, list, watch | `copa-builder`, `copa-verifier` only |

# Jobs
Every Job uses the `copa-worker` image and `ttlSecondsAfterFinished`, and runs on its own tainted node pool. Each has its own service account and Azure workload identity.

| **Job** | **Namespace** | **Identity can** |
| --- | --- | --- |
| scan | `copa-builder` | Pull sources and the Trivy DB mirror. Push to `staging/reports` |
| build | `copa-builder` | Pull sources and reports. Push to `staging/*` |
| verify | `copa-verifier` | Pull `staging/*`. Push to `patched/*`. Sign with the Key Vault key |

## Build Job
One per platform, scheduled on a node of that platform. BuildKit runs as a native sidecar (an init container with `restartPolicy: Always`) and gets no identity. A credential init container swaps the workload identity token for a short-lived ACR token and writes a Docker config that only the worker container mounts. Copa sends the credentials to BuildKit through its session for the push. Package scripts run inside BuildKit's sandbox and never see them.

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: build-nginx-4f7a-r2-linux-amd64
  namespace: copa-builder
  labels: {copa.sh/patchbuild: nginx-4f7a-r2}
spec:
  backoffLimit: 0                       # the controller decides about retries
  activeDeadlineSeconds: 1800
  ttlSecondsAfterFinished: 3600
  template:
    metadata:
      labels: {azure.workload.identity/use: "true"}
      annotations:
        azure.workload.identity/skip-containers: buildkitd   # verify on the AKS version in use
    spec:
      serviceAccountName: copa-build
      automountServiceAccountToken: false
      restartPolicy: Never
      runtimeClassName: kata-vm-isolation                    # Pod Sandboxing, if approved
      nodeSelector: {copa.sh/pool: builder, kubernetes.io/arch: amd64}
      tolerations: [{key: copa.sh/builder, operator: Exists, effect: NoSchedule}]
      initContainers:
      - name: acr-login
        image: myacr.azurecr.io/copa/copa-worker:v0.1.0
        args: [acr-login, --registry, myacr.azurecr.io, --out, /creds/config.json]
        volumeMounts: [{name: creds, mountPath: /creds}]
      - name: buildkitd
        image: moby/buildkit:v0.x-rootless
        restartPolicy: Always
        args: [--addr, unix:///run/buildkit/buildkitd.sock, --oci-worker-no-process-sandbox]
        securityContext:
          runAsUser: 1000
          runAsGroup: 1000
          seccompProfile: {type: Unconfined}                 # per BuildKit rootless docs; verify
          appArmorProfile: {type: Unconfined}
        volumeMounts: [{name: buildkit-run, mountPath: /run/buildkit}]
      containers:
      - name: worker
        image: myacr.azurecr.io/copa/copa-worker:v0.1.0
        env: [{name: DOCKER_CONFIG, value: /creds}]
        args:
        - build
        - --source=myacr.azurecr.io/cache/library/nginx@sha256:<platform digest>
        - --platform=linux/amd64
        - --report=myacr.azurecr.io/staging/reports/nginx@sha256:<report digest>
        - --target=myacr.azurecr.io/staging/nginx:1.25.3-r2-linux-amd64
        - --buildkit=unix:///run/buildkit/buildkitd.sock
        volumeMounts:
        - {name: creds, mountPath: /creds, readOnly: true}
        - {name: buildkit-run, mountPath: /run/buildkit}
        - {name: work, mountPath: /work}
      volumes:
      - {name: creds, emptyDir: {medium: Memory}}
      - {name: buildkit-run, emptyDir: {}}
      - {name: work, emptyDir: {}}
```

`copa-worker build` pulls the report, then runs:

```
copa patch -i <source> -r /work/report.json -t <target> \
  -a unix:///run/buildkit/buildkitd.sock --push -f openvex -o /work/vex.json
```

It pushes `vex.json` to staging as a referrer of the staged image, and writes a short summary (staged digest, number of updates) to the termination message for the controller to read.

## Scan Job
Same pod shape without BuildKit. `copa-worker scan` runs:

```
trivy image --pkg-types os --format json --db-repository <mirror> -o /work/report.json <platform digest>
```

It pushes the report to `staging/reports` as an OCI artifact and writes the report digest, DB version and fixable count to the termination message. To rescan a promoted patch, it runs the same thing against the patched digest.

## Verify Job
No BuildKit, and it never runs the image. `copa-worker verify --build nginx-4f7a-r2`:

1. Reads the source and staged manifest for each platform. Fails unless the staged image's layers start with the source's layers unchanged, and the config differs only in fields Copa is expected to change.
2. Re-scans each staged image with the same Trivy DB version the build used. Fails if any vulnerability the report listed as fixable is still there, or if a new one appears.
3. Builds an index of the staged platform images, with annotations `copa.sh/source-digest` and `copa.sh/revision`.
4. Works out the next revision by listing `patched/nginx` tags `1.25.3-*`. Only the builder cluster promotes, so there's one writer.
5. Pushes the index to `patched/nginx:1.25.3-2`. Attaches the attestation and the per-platform VEX as referrers. Signs the index and the attestation with Notation and the Key Vault key.

The attestation is an in-toto statement with a custom predicate:

```json
{
  "_type": "https://in-toto.io/Statement/v1",
  "subject": [{"name": "myacr.azurecr.io/patched/nginx", "digest": {"sha256": "9b1c..."}}],
  "predicateType": "https://copa.sh/attestation/patch/v1",
  "predicate": {
    "source": {"name": "docker.io/library/nginx:1.25.3", "digest": "sha256:4f7a..."},
    "platforms": [
      {
        "platform": "linux/amd64",
        "sourceDigest": "sha256:a1...",
        "patchedDigest": "sha256:b2...",
        "report": {"digest": "sha256:c3...", "trivyDB": "2026-11-03T06:00:00Z"},
        "vex": "sha256:d4...",
        "updates": [{"name": "libssl3", "installed": "3.0.11-1", "fixed": "3.0.15-1"}]
      }
    ],
    "builder": {"copa": "v0.x", "buildkit": "v0.x", "worker": "v0.1.0", "patchBuild": "nginx-4f7a-r2"},
    "verification": {"layerCheck": "passed", "postPatchScan": "passed"}
  }
}
```

# Observability
Prometheus metrics from the controller:

| **Metric** | **Meaning** |
| --- | --- |
| `copa_fix_to_running_seconds` | From the scan that found a fix to all opted-in pods of a workload running the patch |
| `copa_missed_swaps_total` | Opted-in pods created with a swappable image but no swap |
| `copa_build_total{result,reason}` | Builds by outcome: promoted, nothing to fix, failed by stage |
| `copa_recovery_seconds` | From rollback start to recovery checks passing |
| `copa_gaps{reason}` | Source images that can't be patched, by reason |

The adoption report is `kubectl get sourceimages` and `kubectl get swaplists -A` with printer columns, plus an exported JSON report that dashboards can read instead of Deployment specs.

# Changes in the Copa repository
Separate PRs here, none of which block the pilot:

1. Fix per-platform VEX in multi-platform runs (#1732).
2. Add `--oci-dir` support for single-platform output. The build Job could then hand an OCI layout to the verify Job and wouldn't need any push identity.
3. Add an opt-in patch attestation output carrying the fields in the predicate above. The verify Job would compare it with what it observed instead of building it from scratch.

# Milestones
| **Milestone** | **Mode** | **Delivers** | **Exit check** |
| --- | --- | --- | --- |
| M0 | report-only | CRDs, inventory, scan Jobs, gap and adoption report | Inventory matches what runs. Scan results look right for a sample of images |
| M1 | builder, no rollout | Build and verify Jobs, promotion, attestation, signing | Builds pass verification. Build identity can't write `patched/*`. Egress policy holds |
| M2 | builder, rollout in non-prod | SwapList, admission policies, PatchRollout with manual approval, restarts, health checks | Swaps survive redeploys and scale to zero. Group boundaries hold. No missed swaps |
| M3 | builder and consumer | Rollback, team back-out, PatchApproval flow, metrics, consumer mode in one production cluster | Failed patches roll back. Teams back out on their own. Production runs only patches soaked in non-prod |

# Testing
- **envtest** for every reconciler, with a fake registry client: tag drift, team deploys mid-rollout, controller restart mid-rollout, approval for a stale candidate digest.
- **kind on Kubernetes 1.36 with a local registry** for admission end to end: swap on create only, aliases match, reinvocation is a no-op, missing `SwapList` allows the pod, `SwapList` guard rejects non-controller writes and unpatched targets.
- **Builder end to end** on a real AKS node pool: rootless BuildKit settings, the workload identity skip-containers annotation, egress policy, and a build that tries to read its own credentials and fails.
- **Image check** against each container runtime in scope, comparing `imageID` with the swapped digest for single-platform and index references.
