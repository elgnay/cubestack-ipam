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
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `metrics.enabled` | `true` | HTTPS on 8443 with token auth. Drops the Service, the flag *and* the auth RBAC together. |
| `metrics.serviceMonitor.enabled` | `false` | Requires the Prometheus Operator. |
| `resources`, `nodeSelector`, `tolerations`, `affinity`, `podAnnotations`, `podLabels` | kubebuilder defaults | |

## CRDs live in `crds/`, which has a consequence

Helm installs `crds/` on `helm install` and then **never upgrades or deletes them**.
That is normally the right behaviour for CRDs, but it bites here specifically: the
`IPPool` schema is still moving — `subnet` and `gateway` moved into `spec.nadTemplate`
on 2026-09-28 — so an upgrade that changes the schema will silently not apply.

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
- KubeVirt, for the VirtualMachine-triggered flow. The `IPRequest` API works without
  it; only controller #2 needs the CRD present.
- The manager is Linux-only; on a mixed-architecture cluster build with
  `make docker-buildx` rather than pinning the node pool.
