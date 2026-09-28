# cubestack-ipam

Static, user-claimable IP addresses for KubeVirt VMs on a Multus + Whereabouts underlay.

> **STATUS: IMPLEMENTED, NOT YET ROLLED OUT.**
>
> Address allocation, claim lifecycle and per-VM NetworkAttachmentDefinition minting all work
> and are covered by envtest. What has **not** happened is the thing the design gates on:
> **spike 1 has not run**, so nobody has yet confirmed on a real VM that a single-address NAD
> holds its address across a restart with the guest on plain DHCP. Everything here assumes it
> does. If it does not, the approach is wrong rather than the code — see
> [Spikes](#spikes-to-run-before-trusting-this).
>
> The shared `vm-underlay-10-66-3-0` NAD has **not** been deleted. Doing so is a migration, not
> a deploy — see [Retiring the shared underlay NAD](#retiring-the-shared-underlay-nad).

## What this is

On this cluster a VM's underlay IP is allocated by Whereabouts to the **virt-launcher
pod**, not to the VM. A pod restart or recreation produces a new sandbox, the binding no
longer matches, and the address returns to the pool. That is survivable for a workload
whose address is incidental, and not survivable for a cubestack node whose address is
baked into its kubeadm certificates and etcd peer URLs.

The fix is a thin claim layer over the Whereabouts that is already installed — no custom CNI
IPAM plugin and no dataplane change:

- **`IPPool`** — cluster-scoped, created by an admin. Declares a bounded band of addresses *and*
  the NAD template that every claim against it will be minted from. It is the single declaration
  of where static VM addresses come from.
- **`IPRequest`** — namespaced, created by a user or by the VirtualMachine controller, owned by
  the VM. The controller assigns it an address from the pool and — when the claim names a NAD —
  mints a **per-VM NetworkAttachmentDefinition** whose Whereabouts range is that single address
  (`range_start == range_end == <ip>`). The VM attaches to that NAD by a name it chose in advance,
  and **the guest stays on plain DHCP**.

Because a single-address network has no second address to hand out, the binding is enforced by
the CNI itself rather than by controller bookkeeping.

Full design: [`docs/kubevirt-vm-static-ip-design.md`](docs/kubevirt-vm-static-ip-design.md)
(a copy of `design/vm/kubevirt-vm-static-ip-design.md`; the copy is the one bundled here).

## The two CRDs

| CRD | Scope | Created by |
|---|---|---|
| `ippools.ipam.cubestack.io` | Cluster | admin |
| `iprequests.ipam.cubestack.io` | Namespaced | user, or the VirtualMachine controller |

`IPPool` divides along one line:

- **`spec.range`** — what the *allocator* needs. The only required field.
- **`spec.nadTemplate`** — what the *CNI* needs, including the addressing that config carries (`subnet`, `gateway`, `bridge`, …). Present means the pool mints networks; its fields are required exactly when it is present.

So a pool that mints nothing is a band and nothing else:

```yaml
apiVersion: ipam.cubestack.io/v1alpha1
kind: IPPool
metadata:
  name: cubestack-ledger-only
spec:
  range:
    start: 10.66.3.150
    end: 10.66.3.189
```

`subnet` and `gateway` live in the template rather than on the pool because they are addressing *of the generated config* — meaningless without one. Keeping them here means a pool with no `nadTemplate` doesn't carry two fields nothing reads, and it makes a template all-or-nothing, so no cross-field rule is needed to say so.

> **Plural collision — read this before writing a `kubectl` command.** Whereabouts already
> owns the plural `ippools` (`ippools.whereabouts.cni.cncf.io`) in `kube-system`. This
> project's plural is `ippools.ipam.cubestack.io`, so listing pools **must** use the full
> resource name. There is deliberately no short name, because one would make the collision
> worse rather than better.

### What a pool must satisfy

| Rule | Enforced by | Reported as |
|---|---|---|
| `range` present, both bounds IPv4 | CRD schema | admission error |
| `subnet`, `gateway`, `bridge` present when `nadTemplate` is | CRD schema | admission error |
| `range` inside `nadTemplate.subnet` | CRD schema (CEL) | admission error |
| `range.start <= range.end` | controller | `Available=False/ReasonRangeInvalid` |
| `gateway` outside `range` | controller | `Available=False/ReasonRangeInvalid` |
| `gateway` inside `subnet` | controller | `Available=False/ReasonRangeInvalid` |

The last three are the controller's because Kubernetes CEL has no ordering overload on IP values, so they cannot be written as schema rules however they are phrased. A pool that fails them is **accepted by the API server** — the condition is the only place you see it. They are enforced in Go, in `ipam.ValidatePoolRange`.

The `gateway` row is worth knowing about: a gateway in a different subnet from `subnet` is trivially outside `range`, so the band check alone would wave it through, and the result is a VM whose default route points at an address its bridge cannot reach — the guest keeps its address, CNI `ADD` succeeds, and every off-link flow black-holes.

## Installing the controllers

A Helm chart lives in `charts/cubestack-ipam/`. The `config/` kustomize tree still works and remains what the tests and `make deploy` use; the chart is for handing to someone who does not have the repo.

```bash
make helm-lint                        # lint + render, and check crds/ has not drifted
make helm-install IMG=<repo>/<img>:<tag>
```

There is **no published image**, so the chart defaults to `controller:latest` — what `make docker-build` produces — and expects you to have loaded or pushed it. See `charts/cubestack-ipam/README.md` for the values, the tenant-RBAC caveat, and the migration note if you are replacing a kustomize install.

One caveat is worth repeating here because it will bite this project specifically: Helm installs `crds/` on `helm install` and then **never upgrades or deletes them**. The `IPPool` schema is still moving, so after `make manifests` you need `make helm-sync-crds` *and* `kubectl apply -f charts/cubestack-ipam/crds/` — a `helm upgrade` alone will silently leave the old schema in place.

## Using it

```bash
kubectl apply -f config/samples/ipam_v1alpha1_ippool.yaml     # admin, once per subnet
kubectl apply -f config/samples/kubevirt_v1_virtualmachine.yaml   # user, per VM
```

Or claim an address without a VM, with `config/samples/ipam_v1alpha1_iprequest.yaml`.
**Apply one or the other, not both**: they claim the same NAD name, and the second would fail
as `NADNameTaken`.

`config/samples/ipam_v1alpha1_ippool_minimal.yaml` is the same pool without a `nadTemplate` — the ledger-only form above. Use it *instead of* the full pool, not alongside: both carry the same band, and each allocates against its own `PoolRef`, so two pools over one band would happily hand the same address to two claims.

On the `VirtualMachine`:

| Annotation | Required | Meaning |
|---|---|---|
| `ipam.cubestack.io/pool` | yes | The cluster-scoped `IPPool` to claim from. Its presence is the trigger — without it the VM is left alone entirely |
| `ipam.cubestack.io/nad-name` | no | The NAD to mint. Defaults to `<vm-name>-static` |

and the VM's network list names the same NAD:

```yaml
spec:
  template:
    spec:
      networks:
        - name: underlay
          multus:
            networkName: default/cubestack6-static
```

The name is derived from the VM's own name, never from the address — that is what lets the VM
manifest be written before any address exists.

## Lifecycle

1. Admin creates an `IPPool`: the band, and — if the pool should mint networks — the NAD template carrying its subnet, gateway and bridge.
2. User creates a VM carrying `ipam.cubestack.io/pool` and naming its NAD.
3. Controller #2 creates `IPRequest` `<vm-name>-ip` in the VM's namespace, owned by the VM.
4. Controller #1 takes the **lowest free address** in the pool, then mints the NAD from the pool's
   template with `range_start == range_end == <that address>`, owned by the claim.
5. The guest stays on DHCP. No guest-side static configuration.
6. Deleting the VM garbage-collects the claim, which garbage-collects the NAD.

**Ordering:** the VM necessarily exists before its NAD, so it shows `FailedCreatePodSandBox` for
the few seconds until step 4. That is a visible, self-healing wait, not a failure to investigate.

**No finalizers anywhere.** The chain is `VM → IPRequest → NAD`, all by `ownerReference`, and
release is garbage collection walking it. A claim keeps the address it already holds — it is never
re-derived — so a claim cannot move because another one appeared beneath it.

**Two controllers, split by what they decide:**

| | Owns | Does |
|---|---|---|
| #1 | `IPPool`, `IPRequest` | Validates pools, allocates addresses, mints NADs |
| #2 | `VirtualMachine` | Turns the annotation into a claim, and nothing else |

The split is deliberate: the NAD's Whereabouts range *is* the assigned address, so both have to be
written by one controller or the two can disagree.

**Controller #2 is create-only.** It never patches the VM, and never patches a claim that already
exists. So **editing `ipam.cubestack.io/pool` on a VM that already has a claim does nothing** apart
from an event saying so. To move a VM to another pool or NAD name, delete the `IPRequest` and let
it be recreated:

```bash
kubectl -n default delete iprequest <vm-name>-ip
```

**NAD name reuse:** if the name in `spec.nad` already exists and is controller-owned by *this*
claim, it is left alone. Any other owner fails the claim with a `NADNameTaken` condition — an
object this project did not create is never adopted or overwritten.

## The audit sweep

Every 10 minutes a sweep outside both controllers looks for what no single reconcile can see from
the object it is working on, and reports each finding as a `Warning` event (and a log line). It
writes no status, so it cannot race the controllers it is checking.

| Finding | Means |
|---|---|
| `OrphanedNAD` | A NAD of ours whose claim is gone. Its address is still held in the Whereabouts ledger and invisible to the claim layer |
| `DuplicateAddress` | Two claims holding one address. Only one can pass CNI `ADD`, so the other's VM will not start |
| `PoolDeleted` | Claims left behind by a deleted pool, reported once with a count |

`DuplicateAddress` is the residue of a race that is documented rather than closed: two claims
created in the same instant can both be reconciled against a cache that does not yet show the
other's assignment. The window is small, and it fails loudly — the per-VM NADs share a Whereabouts
ledger, so the second CNI `ADD` is refused instead of the two silently sharing an address.

## Deliberate omissions

- **No IPPool deletion guard.** Deleting a pool leaves existing claims working; the audit reports
  the dangling references afterwards.
- **No cluster-wide NAD overlap sweep.** A pool's band must be disjoint from any other NAD on the
  same subnet, but with the shared NAD retired there is no longer a live object to audit against.
  This is an admin responsibility, bounded by RBAC: **regular users cannot create NADs**, so an
  overlap can only be introduced deliberately.
- **MetalLB is not installed** on this cluster (no CRDs, no namespace, verified 2026-09-28), so the
  MetalLB half of design R5 is unenforceable and moot. If it is ever installed on this subnet, the
  band has to be re-checked against it.
- **No live migration** for static-IP VMs. See below.

## Live migration is out of scope (decided 2026-09-28)

The design's biggest risk (§R1) and spike 2 were both about live migration: with
`range_start == range_end` there is no fallback address, so a migrating VM's target pod asks
for an address the source pod still holds. **This project does not support live migration
for static-IP VMs**, which retires R1 and drops spike 2.

One consequence is easy to miss and worth acting on: if the cluster's `evictionStrategy` is
`LiveMigrate` (set cluster-wide on the KubeVirt CR, or per-VM), then a **node drain will
attempt exactly that migration**. Static-IP VMs should carry `evictionStrategy: None` so a
drain restarts them instead. Verify which applies:

```bash
kubectl get kubevirt -n kubevirt -o jsonpath='{.items[0].spec.configuration.evictionStrategy}'
```

## Retiring the shared underlay NAD

`vm-underlay-10-66-3-0` is **still live**. This project does not delete it, and deleting it is not
a deployment step — it cuts the network off from every VM and pod still attaching it (design O7).
The order has to be:

1. Deploy cubestack-ipam and create the `IPPool`.
2. Move consumers onto claims, one at a time, verifying each VM's address.
3. Only once nothing references the shared NAD, delete it.

Step 2 and 3 are out of scope here.

## Verified environment

Read from the live cluster on 2026-09-23 (design §2). **Re-verify before relying on any of it** —
in particular the Whereabouts image tag.

| Item | Value |
|---|---|
| KubeVirt | `v1.8.4` |
| Feature gates | `["HostDevices","Snapshot","VMPool"]` — no `PersistentIPs` |
| `IPAMClaim` CRD | absent |
| Pod CNI | Calico |
| Underlay CNI | CNAO: Multus + linux bridge (`cnv-bridge`) |
| IPAM | Whereabouts, DaemonSet + reconciler in `kube-system`, image tag **`:latest`** |
| MAC allocation | kubemacpool, in `cluster-network-addons`; MAC persisted into the VM spec |
| Whereabouts CRDs | `ippools`, `overlappingrangeipreservations` only |

MAC stability is already solved by kubemacpool (`spec_mac == live_mac` verified on
`cubestack6`, `02:17:8d:aa:80:61`). **Only L3 drifts** — that is the entire problem this
project addresses.

### How Whereabouts actually scopes its ledger

Verified live on 2026-09-28, and it is not what design §5.3 originally said. The Whereabouts
`IPPool` CR is named after the **range** (`10.66.3.0-24`, in `kube-system`) and is shared by every
NAD declaring that same `range`. Its schema is `spec.range` plus `spec.allocations` keyed by last
octet; there is no `spec.ranges[]` and no `ipPool:` field.

That matters twice. First, it is why `ipam.range` in a minted NAD comes from `nadTemplate.subnet`
rather than from the pool's allocatable band: the range selects the ledger, so a pool's band must
sit inside it and the two cannot be the same field. Second, it is why per-VM NADs are safe:
`range_start == range_end` bounds each network's candidate set to one address *within a shared
ledger*, so exclusivity holds both against other claims and against dynamically-allocated pods —
and a duplicate is refused by the CNI rather than silently shared.

The `10.66.3.0/24` NAD that established this (and carries the config the sample's `nadTemplate`
was copied from):

```json
{
  "name": "vm-underlay-10-66-3-0",
  "type": "cnv-bridge",  "bridge": "br0",  "macspoofchk": false,
  "ipam": {
    "type": "whereabouts",
    "range": "10.66.3.0/24",
    "range_start": "10.66.3.200",
    "range_end": "10.66.3.220",
    "gateway": "10.66.3.254",
    "routes": [{"dst": "0.0.0.0/0"}]
  },
  "dns": {"nameservers": ["223.5.5.5", "8.8.8.8"]}
}
```

A NAD on this underlay also needs the annotation
`k8s.v1.cni.cncf.io/resourceName: bridge.network.kubevirt.io/br0`. Minted NADs do **not** carry it
— if a VM on one fails to start with a resource error, that is the first thing to check.

## Address facts the source documents disagree about

Do **not** bake defaults from any single document. Each of these was confirmed by comparing
the design against the guides; pin them against the live cluster first:

```bash
export KUBECONFIG=~/.kube/kubeconfig.vm
kubectl get nad -n default vm-underlay-10-66-2-0 vm-underlay-10-66-3-0 -o jsonpath='{.spec.config}'
```

| Fact | Conflicting sources |
|---|---|
| `10.66.2.0/24` dynamic window | `10.66.2.130–.139` (user guide §1.3, cluster-admin guide §2.1) **vs** `.200–.220` (design §R5, RDMA proposal) |
| `10.66.3.0/24` static band | `.150–.189` (design §5.4, and the sample here) **vs** `.181–.199` (user guide §7C) |
| Whether the Multus `ips` annotation works | underlay guide §7C titles it 推荐 and claims Whereabouts enforces it **vs** user guide §7 which opens by calling the same annotation 反模式 |

The `10.66.3.0/24` dynamic window (`.200–.220`), gateway (`10.66.3.254`) and DNS are
consistent across every document. The `.2` window and the `.3` static band are not.

Whatever band is chosen must be disjoint from any other NAD serving the same subnet — and from
the **MetalLB pool** if one is ever installed (the design mentions `.221–.240`; it is not
installed today).

## Spikes to run before trusting this

Cheapest first. Each needs one scratch VM. Record results in the design document.

| # | Spike | Decides | Status |
|---|---|---|---|
| 1 | Create a single-address NAD, start a scratch VM on it, restart it several times. Does the address hold with the guest on DHCP? | O1, R6 | **not run — the load-bearing assumption** |
| 2 | ~~Live-migrate that VM between `10-66-3-46` and `10-66-3-47`~~ | O2, R1 | dropped, see above |
| 3 | Stop/start the VM (`runStrategy` Halted → Always) — not just a pod restart. | O3 | not run |
| 4 | Inspect whether a Whereabouts `IPPool` CR can carry workload affinity. | O4 | answered: it cannot |

The controller was written before spike 1 ran, deliberately. Allocation, claiming and NAD minting
do not depend on its answer. What depends on it is whether a single-address NAD is a usable address
binding at all — and if it is not, that invalidates the approach, not the code.

## Open questions

- **O1** — Does a single-address NAD hold its address across a restart, with the guest on DHCP?
- **O3** — Does it survive a stop/start, not just a pod restart?
- **O5** — Should `IPPool` be cluster-scoped (as built) or namespaced for tenant isolation?
- **O7** — How does an existing VM holding a dynamic address move onto a claim? The design
  does not say, and the cutover depends on it.

## Development

```bash
make manifests generate   # regenerate CRDs and deepcopy — CI fails if this produces a diff
make build
make test                 # envtest; the CRD schemas are installed, which validates the CEL rules
```

Tests need the two third-party CRDs this project watches. Those live in `testdata/` as minimal
hand-written copies — Multus's and KubeVirt's real CRDs belong to those projects and are far too
large to vendor. `suite_test.go` installs them alongside the generated ones.

Go 1.26+, `controller-runtime` v0.25.0, `k8s.io/*` v0.37.0 — matching what kubebuilder
v4.16.0 generates.

**On a cluster running an older Kubernetes:** `k8s.io/*` v0.37.0 is newer than the
`v1.35.4` server this targets. That is a 2-minor client skew, outside the officially
supported ±1. It is not known to cause problems for a controller doing CRD and NAD CRUD,
and the org's `cubestack-operator` runs a comparable skew in the other direction
(v0.33.2 against the same server). If you want to pin to the server instead, pin the whole
`k8s.io/*` family together — pinning `client-go` alone silently resolves back up via
`apiextensions-apiserver`, and the v0.33 line predates the `k8s.io/streaming` module.

**If `make test` fails at the link step** with `error: unknown architecture … arm64e.x1-macos`
from `/Library/Developer/CommandLineTools/SDKs/MacOSX*.sdk`, that is a local toolchain/SDK
mismatch, not a code problem — it affects any Go program that links cgo. Work around it with:

```bash
CGO_ENABLED=0 go test ./internal/... ./api/...
```

## Not implemented

- The `"what is free / claim this / release this"` tooling (design §9 phase 4).
- Migrating existing VMs onto claims, and deleting the shared NAD (design §9 phase 5, O7).
- Guest-side configuration of any kind. This project never touches the guest.
