# cubestack-ipam Helm chart

Deploys the two controllers. It does **not** create an `IPPool` — a pool is a
per-subnet decision an admin makes, not something a chart default should guess at.
See the repository README for the API.

## Install

```bash
helm install cubestack-ipam charts/cubestack-ipam \
  --namespace cubestack-system --create-namespace \
  --set image.repository=<registry>/<repo> \
  --set image.tag=<tag> \
  --set image.pullPolicy=Always
```

From a clone of the repo, `make helm-install IMG=<repo>/<img>:<tag>` does the same after
checking `crds/` has not drifted from `config/crd/bases`.

There is **no published image**. The defaults (`controller:latest`) match what
`make docker-build` produces, so a local loop works without overrides:

```bash
make docker-build
kind load docker-image controller:latest    # or push to your own registry
helm install cubestack-ipam charts/cubestack-ipam -n cubestack-system --create-namespace
```

## Values

| Key | Default | Notes |
|---|---|---|
| `image.repository` / `.tag` / `.pullPolicy` | `controller` / `latest` / `IfNotPresent` | Matches the Makefile. Override for anything real. |
| `replicaCount` | `1` | >1 is supported: both controllers and the audit sweep are leader-gated. |
| `leaderElection.enabled` | `true` | Leave on. Without a lease every replica runs the audit sweep and emits duplicate events. |
| `namespace.create` | `true` | Set `false` if the namespace exists and holds other things. |
| `rbac.create` | `true` | The manager ClusterRole + bindings. |
| `rbac.helperRoles` | `true` | The scaffolded Admin/Editor/Viewer ClusterRoles. Read the caveat below. |
| `map.enabled` | `true` | The per-member L2 injection policy. See the section below — read it before upgrading an existing release. |
| `map.namespaces` | `[default]` | The namespaces the injection applies to. This list *is* the blast radius of a `failurePolicy: Fail` policy. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `metrics.enabled` | `true` | HTTPS on 8443 with token auth. Drops the Service, the flag *and* the auth RBAC together. |
| `metrics.serviceMonitor.enabled` | `false` | Requires the Prometheus Operator. |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels` | kubebuilder defaults | |

## The L2 injection policy (`map.*`)

`map.enabled` installs a cluster-scoped `MutatingAdmissionPolicy` and its binding. It
exists for one failure:

> A `VirtualMachinePool` member takes its spec from `spec.virtualMachineTemplate`, and
> `multus.networkName` there is **one literal string handed to every member**. At
> `replicas: 1` that works only because a human hard-codes the single member's name; at
> `replicas: 2` member `-1` points at member `0`'s NAD, and on a `static` pool nothing
> refuses the duplicate — both members come up on one address, and the symptom is
> intermittent connectivity.

The policy rewrites the reference per member at admission, deriving
`<namespace>/<name>-static` from the object being created, so the member is correct
before it is stored and nothing has to write it afterwards. Controller #2 stays
create-only. The annotation contract is unchanged: the trigger is
`ipam.cubestack.io/pool`, and `ipam.cubestack.io/nad-name` is honoured as an override,
both mirroring `virtualmachine_controller.go`.

Four things to know before turning it on:

- **It fails closed.** `failurePolicy: Fail` means a VM in a listed namespace whose
  expressions cannot be evaluated is *rejected*, not admitted unmodified. That is
  deliberate — admitting it unmodified is the silent duplicate-address outcome the
  policy exists to prevent — but it makes `map.namespaces` a real blast radius. An
  empty list is refused rather than widened to the whole cluster.
- **It needs Kubernetes 1.34+ with the feature on.** The chart picks
  `admissionregistration.k8s.io/v1` on 1.36+ (where the feature is GA and locked on)
  or `v1beta1` on 1.34–1.35, and detects from the API server whether the gate is
  actually on. Where it is off, nothing is rendered and the install notes say so —
  loudly, because a silent skip looks exactly like a policy that is running. The flags
  are in that note. Details: `docs/mutating-admission-policy-spike.md`.
- **`helm template` cannot see the cluster.** Everything above is decided from
  discovery where discovery is available (`helm install`, `helm upgrade`,
  `--dry-run=server`); a pure `helm template` has a hardcoded capability list with no
  Kind-level entries and a fabricated `KubeVersion`, so pass `--kube-version` in CI to
  get the right branch.
- **Upgrading an existing release.** `map.enabled` defaults to `true`, so the first
  upgrade onto this chart installs the policy. On a cluster that does not serve it the
  install **fails** rather than skipping — set `map.enabled=false` if you are not
  ready. On a cluster that does, the policy is scoped to `map.namespaces` (`default`
  unless overridden), and it only touches VMs carrying the pool annotation, so a
  namespace full of VMs on the legacy shared NAD is unaffected by the object merely
  existing.

## CRDs live in `crds/`, which has a consequence

Helm installs `crds/` on `helm install` and then **never upgrades or deletes them**.
That is normally the right behaviour for CRDs, but it bites here specifically: the
schemas are still moving — `subnet` and `gateway` moved into `spec.nadTemplate`
on 2026-09-28 — so an upgrade that changes the schema will silently not apply.

It is worse when a release adds a *kind*. **0.4.0 adds `IPRangeRequest`**, and if you
skip the CRD apply the kind simply does not exist: the manager watches it, its cache
can never sync that informer, leadership is never acquired, and **nothing reconciles at
all** — not blocks, not claims, not pools. The same release makes
`IPRequest.spec.poolRef` CEL-immutable, which only the new schema enforces.

**0.5.0** is a breaking schema change and needs the same treatment. `IPRangeRequest`'s
top-level `count` moved to `spec.allocation.count`, and `spec.allocation` is now
required. A bound block is retained and keeps reconciling, because a held block is
never re-derived from its spec; an unbound one reports `AllocationInvalid`, and the new
schema rejects any edit to it (the old `count` is pruned and `allocation` is required),
so it has to be deleted and re-created. `IPRequest` and `IPPool` only gain optional
fields, so they are unaffected unless you use them.

When you change a CRD, apply it explicitly first:

```bash
kubectl apply -f charts/cubestack-ipam/crds/
helm upgrade cubestack-ipam charts/cubestack-ipam -n cubestack-system
```

`crds/` is a copy of `config/crd/bases`, which `make manifests` generates. If the two
disagree, the generated one is right — re-copy rather than hand-editing:

```bash
make helm-sync-crds     # copy the generated bases in
make helm-crds-check    # fail if they have drifted (helm-lint depends on this)
```

## `rbac.helperRoles` grants more than a tenant needs

The Admin/Editor/Viewer roles are ClusterRoles, so binding **Editor** to a user lets
them create an `IPRequest` in *any* namespace, and `IPPool` is cluster-scoped — so
`ippool-editor` is effectively cluster-admin over the address space.

**`IPRangeRequest` ships a Viewer and nothing else, on purpose.** A block is a resource
reservation with no guard behind it: deleting the object frees the addresses at once
while whatever was configured with them keeps using them, and only the audit sweep would
notice. That is a grant worth making per team, against a named pool, not a one-line
binding — so the chart does not provide one. Read the shipped viewer as "you can see what
is reserved and by whom", and write the write-grant yourself:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: iprangerequest-reserver
  namespace: <tenant-ns>
rules:
  - apiGroups: [ipam.cubestack.io]
    resources: [iprangerequests]
    verbs: [create, get, list, watch]
```

Add `delete` only if the same team should be able to release its own blocks — the
release is immediate and unguarded, which is why it is not in the list above.

A tenant using the VM flow needs none of that. The VirtualMachine controller creates
the `IPRequest` on their behalf, so they only need to be able to annotate their own
VirtualMachine. If you want them to hand-write claims (or delete one to re-mint its
NAD — the documented escape hatch), bind a **namespaced** Role instead:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: iprequest-claimer
  namespace: <tenant-ns>
rules:
  - apiGroups: [ipam.cubestack.io]
    resources: [iprequests]
    verbs: [create, get, list, watch, delete]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: iprequest-claimer
  namespace: <tenant-ns>
subjects:
  - kind: User
    name: <user>
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: iprequest-claimer
```

## Migrating from the kustomize install

The Deployment selector and every object name differ, so this is a replace, not an
in-place upgrade. Delete the kustomize install first or you will have two managers
reconciling the same CRDs — they would contend for the fixed leader-election lease
only if they share a namespace, but they would still fight over NADs.

```bash
make undeploy                      # from the kustomize tree
helm install cubestack-ipam charts/cubestack-ipam -n <ns> --create-namespace
```

Deleting the controller does **not** delete the CRDs, the `IPPool`s or the claims, so
existing allocations survive the swap. Reconcile resumes where it left off.

## Requirements

- Multus and Whereabouts installed. Whereabouts owns the `ippools` plural in
  `kube-system`, so always use the full `ippools.ipam.cubestack.io`.
- KubeVirt, for the VirtualMachine-triggered flow. The `IPRequest` and `IPRangeRequest`
  APIs work without it — neither needs a NAD or a VM; only controller #2 needs the CRD
  present.
- The manager is Linux-only; on a mixed-architecture cluster build with
  `make docker-buildx` rather than pinning the node pool.
