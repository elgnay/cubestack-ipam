# cubestack-ipam

Static, user-claimable IP addresses for KubeVirt VMs on a Multus + Whereabouts underlay.

> **Just want to use it?** Read the [user guide](docs/user-guide.md) instead — it is task-shaped
> and answers "what do I type". This README is the why: the design, the trade-offs, the risks.

> **STATUS: DEPLOYED AND RUNNING; NOT YET A ROLLOUT.**
>
> Address allocation, claim lifecycle and per-VM NetworkAttachmentDefinition minting all work
> and are covered by envtest. The mechanism the design gates on — a single-address NAD holding
> its address across a restart, with the guest on plain DHCP — **has now been confirmed on real
> hardware** (2026-09-28, see [Spikes](#spikes-to-run-before-trusting-this)). So has live
> migration, under the addressing mode that permits it.
>
> The controllers are deployed (`cubestack-system`, chart 0.3.0) and the live pool `dev-ip-pool` runs
> `ipam: static`, so VMs claiming from it are migratable. Two items that were open in the previous
> revision of this file have since closed. The re-rendered static NAD **has** now been exercised by a
> real pod creation — measured 2026-09-28,
> a VM created with no `networks[]` at all came up on the minted NAD and the CNI logged
> `Add pod… [10.66.3.205/24] from default/vm-mapspike-static`. And the shared
> `vm-underlay-10-66-3-0` NAD **has been deleted** (2026-09-29), once nothing referenced it — see
> [Retiring the shared underlay NAD](#retiring-the-shared-underlay-nad), now a record of how it was
> done rather than a plan.

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
  mints a **per-VM NetworkAttachmentDefinition** serving that one address. The VM attaches to
  that NAD by a name it chose in advance, and **the guest stays on plain DHCP**. The address is
  normally the lowest free one; `spec.allocation` can demand a specific address instead — see
  [Asking for a specific address](#asking-for-a-specific-address).
- **`IPRangeRequest`** — namespaced, created by a user. Claims **N contiguous addresses** from an
  existing pool and records them in `status.start`/`status.end`. No NAD is minted and no pool is
  created — it is a reservation, for a consumer that needs a range rather than one address (a
  nested platform being provisioned onto VMs and configuring its own IPAM, say). The run is
  normally the lowest free one that fits; it can also be named exactly. See
  [Reserving a block of addresses](#reserving-a-block-of-addresses).

Every minted NAD serves exactly the one address its claim holds, and that is what makes the
address survive a pod restart: the binding is a property of the network the VM attaches to,
not of the sandbox that happened to come up.

**How that one address is expressed is a per-pool choice**, and it is the one real trade-off in
this project (`nadTemplate.ipam`):

| | `whereabouts` (default) | `static` |
|---|---|---|
| Address lives in | the Whereabouts ledger, leased at CNI `ADD` | the NAD itself, written in |
| A second holder is | **refused**, and its VM does not start | **allowed**, silently |
| Live migration | **impossible** | works, and the guest keeps its address |
| Exclusivity rests on | the CNI | the allocator, the band split, the audit |

The two cannot be combined on one pool, and that is a property of the mechanism rather than a
gap in the code. Live migration deliberately runs the target pod *while the source pod is still
serving*, so migration needs two pods to hold one address at once. Whereabouts stores one
allocation slot per address, so it cannot grant that — the second `ADD` is refused and the
`VirtualMachineInstanceMigration` sits in `Scheduling` forever, with no timeout.

So: a pool whose VMs must never collide silently, and which can tolerate a stop/start to move,
wants `whereabouts`. A pool whose VMs have to migrate wants `static`, and accepts that a
collision becomes a silent duplicate rather than a loud failure.

Full design: [`docs/kubevirt-vm-static-ip-design.md`](docs/kubevirt-vm-static-ip-design.md)
(a copy of `design/vm/kubevirt-vm-static-ip-design.md`; the copy is the one bundled here).

## The three CRDs

| CRD | Scope | Created by |
|---|---|---|
| `ippools.ipam.cubestack.io` | Cluster | admin |
| `iprequests.ipam.cubestack.io` | Namespaced | user, or the VirtualMachine controller |
| `iprangerequests.ipam.cubestack.io` | Namespaced | user (short name `iprr`) |

`IPPool` divides along one line:

- **`spec.range`** — what the *allocator* needs. The only required field.
- **`spec.nadTemplate`** — what the *CNI* needs, including the addressing that config carries (`subnet`, `gateway`, `ipam`, `bridge`, …). Present means the pool mints networks; its fields are required exactly when it is present.

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
| `ipam` is one of `whereabouts` / `static` | CRD schema | admission error |
| `range` inside `nadTemplate.subnet` | CRD schema (CEL) | admission error |
| `range.start <= range.end` | controller | `Available=False/ReasonRangeInvalid` |
| `gateway` outside `range` | controller | `Available=False/ReasonRangeInvalid` |
| `gateway` inside `subnet` | controller | `Available=False/ReasonRangeInvalid` |
| each `allocation.exclude` band inside `range` | controller | `Available=False/ReasonRangeInvalid` |
| each `allocation.exclude` band ordered | controller | `Available=False/ReasonRangeInvalid` |

The last five are the controller's because Kubernetes CEL has no ordering overload on IP values, so they cannot be written as schema rules however they are phrased. A pool that fails them is **accepted by the API server** — the condition is the only place you see it. They are enforced in Go, in `ipam.ValidatePoolRange`.

The `gateway` row is worth knowing about: a gateway in a different subnet from `subnet` is trivially outside `range`, so the band check alone would wave it through, and the result is a VM whose default route points at an address its bridge cannot reach — the guest keeps its address, CNI `ADD` succeeds, and every off-link flow black-holes.

## Installing the controllers

A Helm chart lives in `charts/cubestack-ipam/`. The `config/` kustomize tree still works and remains what the tests and `make deploy` use; the chart is for handing to someone who does not have the repo.

```bash
make helm-lint                        # lint + render, and check crds/ has not drifted
make helm-install IMG=<repo>/<img>:<tag>
```

There is **no published image**, so the chart defaults to `controller:latest` — what `make docker-build` produces — and expects you to have loaded or pushed it. See `charts/cubestack-ipam/README.md` for the values, the tenant-RBAC caveat, and the migration note if you are replacing a kustomize install.

One caveat is worth repeating here because it will bite this project specifically: Helm installs `crds/` on `helm install` and then **never upgrades or deletes them**. The schemas are still moving, so after `make manifests` you need `make helm-sync-crds` *and* `kubectl apply -f charts/cubestack-ipam/crds/` — a `helm upgrade` alone will silently leave the old schema in place.

That is not advisory on the upgrade to **0.4.0**, which adds the `IPRangeRequest` kind. Skip the CRD apply and the kind does not exist at all — the manager watches it, so its cache cannot sync that informer, leadership is never acquired, and **nothing reconciles**: not blocks, not claims, not pools. The same release also makes `IPRequest.spec.poolRef` CEL-immutable, which only the new schema enforces. And unlike 0.3.0, 0.4.0 changes the **manager binary**, so the image has to be rebuilt and pushed rather than just the chart re-installed.

**0.5.0 is a breaking API change, and the CRDs must go on first.** `IPRangeRequest.spec.count` moved to `spec.allocation.count` and `spec.allocation` is now required. A **bound** block survives: the allocator never re-derives a held block, so it is kept and continues to reconcile. An **unbound** one reports `AllocationInvalid` (there is no allocation to read), and the new schema will not let it be edited — the old top-level `count` is pruned on write and `allocation` is required — so the only conversion is to delete it and create it again. Everything else is additive: `IPRequest.spec.allocation` absent still means `Auto`, and `IPPool.spec.allocation` absent still means no exclusions with exact requests allowed. The image changes too, as always for a controller change.

## Using it

```bash
kubectl apply -f config/samples/ipam_v1alpha1_ippool.yaml     # admin, once per subnet
kubectl apply -f config/samples/kubevirt_v1_virtualmachine.yaml   # user, per VM
```

Or claim an address without a VM, with `config/samples/ipam_v1alpha1_iprequest.yaml`.
**Apply one or the other, not both**: they claim the same NAD name, and the second would fail
as `NADNameTaken`.

`config/samples/ipam_v1alpha1_iprangerequest.yaml` reserves a block instead — see below. It is
**not** in the samples kustomization, deliberately: every sample there shares `dev-ip-pool`, and a
block takes the lowest free addresses in it, which are the ones the single-address sample and the
VM sample expect to land on. Apply it *instead of* those, not alongside.

`config/samples/ipam_v1alpha1_ippool_minimal.yaml` is the same pool without a `nadTemplate` — the ledger-only form above. Use it *instead of* the full pool, not alongside: both carry the same band, and each allocates against its own `PoolRef`, so two pools over one band would happily hand the same address to two claims.

On the `VirtualMachine`:

| Annotation | Required | Meaning |
|---|---|---|
| `ipam.cubestack.io/pool` | yes | The cluster-scoped `IPPool` to claim from. Its presence is the trigger — without it the VM is left alone entirely |
| `ipam.cubestack.io/nad-name` | no | The NAD to mint. Defaults to `<vm-name>-static` |
| `ipam.cubestack.io/address` | no | A specific address to claim. The claim becomes `Exact`: it binds that address or fails, naming what is in the way |

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

## Asking for a specific address

By default the allocator chooses: an `IPRequest` takes the lowest free address, an
`IPRangeRequest` the lowest free run of its count. `spec.allocation` makes that choice the
caller's instead.

```yaml
# one address, named
spec:
  poolRef: dev-ip-pool
  allocation:
    strategy: Exact
    address: 10.66.3.205

# a run, named
spec:
  poolRef: dev-ip-pool
  allocation:
    strategy: Exact
    range:
      start: 10.66.3.208
      end: 10.66.3.215
```

Omitting `allocation` (or writing `strategy: Auto`) is exactly the old behaviour, and an
`IPRequest` with no `allocation` at all is still valid — existing objects do not change meaning.
The other shape — `Auto` with `count` for a block, `Auto` with no `count` for an address — is what
you get if you do not write the block.

**Exact is a demand, not a hint.** If the address is not available, the request fails; it is never
given a different one. Each refusal is a distinct reason, and `status.blockers` names the objects
in the way, so a conflict can be resolved without guessing:

| Reason | Means | Blockers name |
|---|---|---|
| `AddressOutOfRange` / `RangeOutOfRange` | outside `spec.range` | nothing — it is the pool's band that rules it out |
| `AddressExcluded` / `RangeExcluded` | inside an `allocation.exclude` band on the pool | the `IPPool`, and the band |
| `AddressUnavailable` / `RangeUnavailable` | already held, by a claim or by a block | each holder, with its address or range |
| `ExactNotAllowed` | the pool sets `allocation.allowExact: false` | the `IPPool` |
| `AllocationInvalid` | the spec and the held status disagree | nothing — a hand edit, not a shortage |

An `Auto` allocation never consults these: exhaustion is `Exhausted` (or `RangeExhausted` /
`RangeFragmented`), which means "the pool is full", not "somebody has your address".

**Exclusions** are how an address is kept out of the allocator's hands altogether — for another
system's fixed address, say — rather than by hoping no claim lands on it:

```yaml
spec:
  range:
    start: 10.66.3.200
    end: 10.66.3.230
  allocation:
    allowExact: true          # default; false refuses Exact requests on this pool
    exclude:
      - start: 10.66.3.200    # must lie inside range
        end: 10.66.3.200
```

`Auto` skips excluded addresses, and `Exact` refuses them with the reason above, so an exclusion
removes an address from the pool under either strategy. It is enforced by this allocator, not by
the CNI: on a `static` pool that is the only enforcement there is.

**Exact does not mean pre-reserved.** The address is arbitrated at the moment the claim binds,
through the same ledger an `Auto` claim uses, so two `Exact` requests for one address cannot both
bind — the second fails `AddressUnavailable` and says who holds it. The allocation race documented
[above](#the-audit-sweep) applies here too, and is easier to hit because both callers named the
same address: for an instant neither claim's status shows the other's assignment. The sweep's
`DuplicateAddress` finding is still the detector for the residue, with the same mode-dependent
consequences.

**Nothing is moved once it binds.** A held address stays held even if the pool later narrows under
it or an exclusion is added over it; the claim reports the disagreement instead. That is the same
rule as everywhere else on this API: allocation never moves an address out from under a running
VM. Delete the claim to reclaim it.

**`allocation` is immutable** — the whole block, for both kinds — because the address is written
into the NAD (or into `status` for a block) and changing it would move an address under a running
VM. A different address or band is a different request. That is enforced by CEL, so it is an
admission error rather than a condition.

From a `VirtualMachine`, the annotation is the whole API: `ipam.cubestack.io/address` sets
`strategy: Exact` and that address on the claim the controller creates. With no annotation the
claim is `Auto`. The controller still makes no decision about availability and never retries with
another address — the claim's own status is where a conflict shows, and the VM waits in
`FailedCreatePodSandBox` until it clears.

## Reserving a block of addresses

Some consumers need a **range**, not one address — a nested platform being provisioned onto VMs
that configures its own IPAM itself, for instance. An `IPRangeRequest` asks for a count of
contiguous addresses from an existing pool:

```yaml
apiVersion: ipam.cubestack.io/v1alpha1
kind: IPRangeRequest
metadata:
  name: blk-cubestack
  namespace: default
spec:
  poolRef: dev-ip-pool          # cluster-scoped, so a bare name
  allocation:
    strategy: Auto
    count: 8                    # poolRef and allocation are immutable
```

```bash
kubectl apply -f config/samples/ipam_v1alpha1_iprangerequest.yaml
kubectl get iprr -n default
```

```
NAME           POOL          COUNT   START         END           PHASE   AGE
blk-cubestack  dev-ip-pool   8       10.66.3.200   10.66.3.207   Bound   12s
```

`Bound` with a start and end is the whole result. **The pool remains the single allocator**: the
block is drawn through the same ledger a VM's claim uses, so it cannot overlap a VM's address or
another block — by construction, rather than by two admins picking disjoint bands and hoping. Under
`strategy: Auto` the allocator takes the **lowest free run** that fits, unaligned; a free address
that cannot be part of a long-enough run is skipped rather than handed out, and if no such run
exists the request fails with `RangeFragmented` naming the longest run available, which is a
different fix from `RangeExhausted`. Under `strategy: Exact` the run is the one named in
`allocation.range` or nothing — see
[Asking for a specific address](#asking-for-a-specific-address).

Two things it deliberately is not:

- **Not an `IPPool`.** Nothing here creates one. If you want the block to be independently
  allocatable, an admin still declares a pool for it; this kind only reserves addresses out of a
  band that already exists.
- **Not self-consuming.** Nothing reconfigures whatever you hand these addresses to. There is no
  NAD minted and no guest touched.

> **Deleting one frees the addresses immediately.** No finalizer holds them and nothing downstream
> is un-configured, so a component still using them keeps using them while the allocator hands
> them to the next VM — and on a `static` pool nothing refuses the duplicate. Delete a range
> request only once its consumer has stopped. The delete emits a `RangeReleased` warning naming the
> pool and the released range, and the [audit sweep](#the-audit-sweep) reports a later holder
> landing inside a block, but neither is a guard. This is the one operation on this API that can
> cause an outage.

`poolRef` and the whole `allocation` block are **immutable**. Changing either would move addresses
out from under whatever was configured with them, so a different block is a different request:
delete it and create a new one.

## Lifecycle

1. Admin creates an `IPPool`: the band, and — if the pool should mint networks — the NAD template carrying its subnet, gateway, bridge and an `ipam` mode.
2. User creates a VM carrying `ipam.cubestack.io/pool` and naming its NAD.
3. Controller #2 creates `IPRequest` `<vm-name>-ip` in the VM's namespace, owned by the VM.
4. Controller #1 takes the **lowest free address** in the pool — or, if the claim named one, that
   address and nothing else — then mints the NAD from the pool's template, carrying that one
   address in the mode the template selected, owned by the claim.
5. The guest stays on DHCP. No guest-side static configuration.
6. Deleting the VM garbage-collects the claim, which garbage-collects the NAD.

**Ordering:** the VM necessarily exists before its NAD, so it shows `FailedCreatePodSandBox` for
the few seconds until step 4. That is a visible, self-healing wait, not a failure to investigate.

A range request is the same path with the NAD half removed:

1. User creates an `IPRangeRequest` naming a pool and, in `allocation`, either a count or an exact band.
2. Controller #3 takes the **lowest free run of that count** — or the named band, if it can have it
   — and records it in `status.start`/`status.end`.
3. Deleting the object is the release. Nothing is garbage-collected, because nothing was created.

**No finalizers anywhere.** The chain for a VM is `VM → IPRequest → NAD`, all by `ownerReference`,
and release is garbage collection walking it. A range request is a leaf: it owns nothing, so
deleting it *is* the release. A claim keeps the address it already holds — it is never re-derived —
so a claim cannot move because another one appeared beneath it, and a bound block is never moved
either (a block whose pool narrows under it is kept and reported `RangeInvalid` rather than
silently relocated).

**Three controllers, split by what they decide:**

| | Owns | Does |
|---|---|---|
| #1 | `IPPool`, `IPRequest` | Validates pools, allocates addresses, mints NADs |
| #2 | `VirtualMachine` | Turns the annotation into a claim, and nothing else |
| #3 | `IPRangeRequest` | Allocates contiguous blocks, and nothing else |

The split is deliberate: whichever mode a pool uses, the NAD's addressing *is* the assigned
address, so both have to be written by one controller or the two can disagree.

**#1 and #3 share one ledger reader** (`internal/controller/ledger.go`), which lists both request
kinds and expands a block's `status.start`/`status.end` into the addresses it holds. That is what
makes a claim unable to land inside a delegated block, and a block unable to swallow an address a
claim already holds. Two independent readers would have made that a convention rather than a
property. Each also watches the other's kind, so deleting whichever one was holding a pool at
capacity re-triggers the other.

**Controller #2 is create-only.** It never patches the VM, and never patches a claim that already
exists. So **editing `ipam.cubestack.io/pool` or `ipam.cubestack.io/address` on a VM that already
has a claim does nothing** apart from an event saying so. To move a VM to another pool, NAD name or
address, delete the `IPRequest` and let it be recreated:

```bash
kubectl -n default delete iprequest <vm-name>-ip
```

**NAD name reuse:** if the name in `spec.nad` already exists and is controller-owned by *this*
claim, it is left alone. Any other owner fails the claim with a `NADNameTaken` condition — an
object this project did not create is never adopted or overwritten.

## The audit sweep

Every 10 minutes a sweep outside the controllers looks for what no single reconcile can see from
the object it is working on, and reports each finding as a `Warning` event (and a log line). It
writes no status, so it cannot race the controllers it is checking.

| Finding | Means |
|---|---|
| `OrphanedNAD` | A NAD of ours whose claim is gone. Its address is held by no claim, so the allocator can hand it to a new one while the orphan's VM may still be running |
| `DuplicateAddress` | Two claims holding one address. Names both, because neither claim's own status shows anything wrong |
| `AddressInBlock` | A claim's address falls inside a delegated block. Names both, same reason — neither object's own status shows anything wrong |
| `RangeOverlap` | Two blocks in one pool intersect. The block-vs-block residue of the same allocation race |
| `PoolDeleted` | Claims left behind by a deleted pool, reported once with a count |

`AddressInBlock` and `RangeOverlap` are what the block API adds to this sweep, and they exist for
the same reason `DuplicateAddress` does: a block is held by one object instead of one object per
address, so without them a collision would be *quieter* than before, not louder.

Their wording is deliberately not the same as `DuplicateAddress`'s, because a block is not CNI
state. On a `whereabouts` pool a duplicate claim is refused at `ADD`, but Whereabouts does not know
the block exists — nothing refuses the CNI — so the finding says so rather than promising a
failure that will not happen. On a `static` pool the consequence is the same intermittent
connectivity a duplicate produces.

**What the sweep cannot see:** a block deleted while a component outside the cluster still uses
it. The object is gone and nothing recorded its former contents, so there is no evidence left to
find. That case is only ever visible as its consequence — which is these findings catching a *new*
holder colliding with a stale user.

`DuplicateAddress` is the residue of a race that is documented rather than closed: two claims
created in the same instant can both be reconciled against a cache that does not yet show the
other's assignment. The window is small. **How bad the outcome is depends on the pool's mode, and
that is the sharpest practical difference between them:**

- On a `whereabouts` pool it fails *loudly* — the per-VM NADs share a ledger, so the second CNI
  `ADD` is refused and the duplicate's VM does not start. This sweep is a backstop that explains
  what already went wrong.
- On a `static` pool nothing refuses it. Both VMs come up, both configure the same address, and
  the symptom is intermittent connectivity rather than a failed start. **This sweep is the only
  detector**, which is why it reports the collision on *both* claims rather than one.

The same asymmetry applies to the band: on a `whereabouts` pool an address a dynamic pod already
holds is refused at `ADD`, so overlap is survivable. On a `static` pool it is not — an address
handed out of a band that reaches into the Whereabouts dynamic window collides silently. **A
static pool's band being disjoint from every Whereabouts range on the subnet is load-bearing, not
hygiene.**

## Deliberate omissions

- **No IPPool deletion guard.** Deleting a pool leaves existing claims working; the audit reports
  the dangling references afterwards.
- **No deletion guard on a delegated block.** Deleting an `IPRangeRequest` releases its addresses
  at once, and nothing holds it while whatever was configured with them stops. A finalizer would be
  this project's first, and it would make release a two-step operation for a consumer this cluster
  cannot see or verify — so the trade was made the other way: a delete-time `RangeReleased` warning,
  the `AddressInBlock` finding above, and
  [a loud section in the README](#reserving-a-block-of-addresses). **This is the one operation on
  this API that can cause an outage.**
- **No cluster-wide NAD overlap sweep.** A pool's band must be disjoint from any other NAD on the
  same subnet, but with the shared NAD retired there is no longer a live object to audit against.
  This is an admin responsibility, bounded by RBAC: **regular users cannot create NADs**, so an
  overlap can only be introduced deliberately. It matters most for a `static` pool, where nothing
  else would catch it — see above.
- **MetalLB is not installed** on this cluster (no CRDs, no namespace, verified 2026-09-28), so the
  MetalLB half of design R5 is unenforceable and moot. If it is ever installed on this subnet, the
  band has to be re-checked against it.
- **No reservation ahead of binding.** `strategy: Exact` names the address a claim must get, but it
  does not set it aside: until the claim binds, the address is free for an `Auto` claim to take,
  and the `Exact` one then fails `AddressUnavailable`. Nothing in this API holds an address for a
  caller that has not asked for it yet. An exclusion on the pool is the tool for keeping an
  address out of every claim.
- **No enforcement of an exclusion over a holder.** Excluding a band, like narrowing `range`,
  does not move an address that is already bound; the holder keeps it and the pool validates
  against the new exclusion for everyone else. That is the sticky-allocation rule, not a gap.
- **No automatic choice of `ipam` mode.** It is per pool, made at creation, and not revisited.

## Live migration

Live migration is supported, but only on a pool whose `ipam` is `static` — see
[What this is](#what-this-is) for why the two modes are mutually exclusive. The design's biggest
risk (§R1) and spike 2 were both about this; both are now answered, the second one affirmatively
and only for `static`.

Measured on this cluster 2026-09-28, with the NAD hand-switched to `static` while the controller
was scaled down: live migration moved the VM from `10-66-3-47` to `10-66-3-46` in about 20
seconds, `completed: True, failed: None`, and the **guest kept `10.66.3.221`**. At a sample taken
10 seconds in, the source pod (`Running`) and the target pod (`Pending`) were both up holding that
same address — which is exactly the state Whereabouts refuses and the whole reason the mode exists.

The one thing to get right on a `whereabouts` pool is `evictionStrategy`. If it is `LiveMigrate`
(cluster-wide on the KubeVirt CR, or per-VM), then a **node drain will attempt that migration**,
and on a `whereabouts` pool the attempt hangs rather than failing fast. Such VMs should carry
`spec.template.spec.evictionStrategy: None` so a drain restarts them instead. Static pools need no
such override — the sample VM deliberately leaves it at the default. Check which applies:

```bash
kubectl get kubevirt -n kubevirt -o jsonpath='{.items[0].spec.configuration.evictionStrategy}'
```

Note the path: `spec.template.spec.evictionStrategy`, **not** `spec.evictionStrategy`. KubeVirt's
CRD is structural, so the apiserver silently *prunes* a wrong field name rather than rejecting it —
you get no error and no eviction strategy. Confirm it landed with `kubectl get vm -o jsonpath`.

## Retiring the shared underlay NAD

**Done — `vm-underlay-10-66-3-0` was deleted on 2026-09-29.** This section is kept as the record of
*how*, because the order is the part that would be got wrong on a second cluster.

The NAD could not simply be deleted: doing so cuts the network off from every VM and pod still
attaching it (design O7). The order was:

1. Deploy cubestack-ipam and create the `IPPool`. ✅
2. Move consumers onto claims, one at a time, verifying each VM's address. ✅ — all five VMs
   (`clq-cubestack-node-0`, `vm-roce-gpu`, `vm1/2/3-weina`) ended up on their own minted NADs.
3. Only once nothing references the shared NAD, delete it. ✅

Step 3 was a sweep, not an assumption: no `VirtualMachine`, `VirtualMachineInstance`,
`VirtualMachinePool`, `Pod` (including terminated ones), `ConfigMap` or CNAO config referenced it,
both Whereabouts ledgers were empty, and the NAD had no finalizers or owner references. The only
hit was the stale `last-applied-configuration` annotation on `VirtualMachinePool/clq-cubestack-node`
— kubectl bookkeeping, not a live reference.

Deletion was a no-op for every workload: five VMs stayed `Running`, five claims stayed `Bound` on
the same addresses. It also closed the band-overlap hazard, because that NAD was the **only**
Whereabouts NAD left on `10.66.3.0/24` — `dev-ip-pool` is now the sole allocator on the subnet, so
there is nothing left for a `static` band to collide with.

**Second-order effect, worth knowing before editing a pool.** Removing `networks[]`/`interfaces[]`
from a `VirtualMachinePool`'s template so the injection policy supplies them **restarts every
member** — measured 2026-09-28: the pool controller rewrote the member and KubeVirt gracefully shut
its VMI down one second later. The address survives (the minted NAD pins it), but plan for the
restart rather than expecting a mere `RestartRequired` condition.

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

That matters twice. First, it is why `ipam.range` in a `whereabouts` minted NAD comes from
`nadTemplate.subnet` rather than from the pool's allocatable band: the range selects the ledger,
so a pool's band must sit inside it and the two cannot be the same field. Second, it is why
per-VM NADs in that mode are safe: `range_start == range_end` bounds each network's candidate set
to one address *within a shared ledger*, so exclusivity holds both against other claims and
against dynamically-allocated pods — and a duplicate is refused by the CNI rather than silently
shared.

That last property is exactly what makes the mode unmigratable, so it is the one thing `static`
gives up. A `static` pool's addresses never touch this ledger at all, which means the ledger
cannot see them: it will happily hand a dynamic pod an address a static claim is already using,
and the first symptom is a broken flow rather than a failed `ADD`. Splitting the bands is what
keeps that from happening — see [The audit sweep](#the-audit-sweep).

The `10.66.3.0/24` NAD that established this — and the config the sample's `nadTemplate` was copied
from. **It no longer exists** (deleted 2026-09-29); the config is kept here because it is the
provenance of the template, not because anything reads it:

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

**A minted NAD needs no annotations at all.** An earlier version of this file claimed every underlay
NAD must carry `k8s.v1.cni.cncf.io/resourceName: bridge.network.kubevirt.io/br0`, and that minted NADs
were broken without it. **That is false** — the hand-written NAD carried the annotation, so it was
mistaken for a requirement. Measured 2026-09-28: VMs started normally on minted NADs with no
annotations. If a VM on one fails to start, look at the claim's condition, not at the NAD's metadata.

## Address facts — settled, and the one disagreement that outlived the NADs

**This section used to be a list of contradictions between the design and the guides, resolved by
reading the shared NADs off the cluster. Both of those NADs are now deleted**, so there is nothing
left to read, and the contradictions it recorded are moot: the band is no longer inherited from a
hand-written NAD at all, it is whatever the pool declares. Take the facts from the pool:

```bash
export KUBECONFIG=/Users/elgnay/workspaces/suanova/design/vm/kubeconfig
kubectl get ippools.ipam.cubestack.io dev-ip-pool -o jsonpath='{.spec}{"\n"}'
kubectl get nad -n default dev-ip-pool -o jsonpath='{.spec.config}{"\n"}'   # a member's NAD, if any exist
```

For the record, the two facts that every document already agreed on, and which the pool keeps:
gateway `10.66.3.254`, DNS `223.5.5.5` / `8.8.8.8`. The `10.66.2.0/24` window is not a fact about
anything — that subnet has no nodes and no NAD.

**The one disagreement still worth knowing about** is not about an address:

| Fact | Position |
|---|---|
| Whether the Multus `ips` annotation works | the underlay guide §7C titles it 推荐 and claims Whereabouts enforces it **vs** the user guide §7 which calls the same annotation 反模式 |

It is unresolved here, and it does not need resolving: **do not use it.** The pool path is what
`ips` was reaching for — an address that is stable and exclusive — and it does so without pinning the
guest to a hand-written address. See §"Never assign an underlay IP yourself" in the user guide.

Any band a pool declares must be disjoint from every other allocator on the same subnet — other pools,
any remaining Whereabouts NAD, and the **MetalLB pool** if one is ever installed (the design mentions
`.221–.240`; it is not installed today). With the shared NAD deleted there is currently no dynamic
window on `10.66.3.0/24` to avoid, which makes this easier than it was — but it is a property of the
present, not a guarantee.

## Spikes to run before trusting this

Cheapest first. Each needs one scratch VM. Record results in the design document.

| # | Spike | Decides | Status |
|---|---|---|---|
| 1 | Create a single-address NAD, start a scratch VM on it, restart it several times. Does the address hold with the guest on DHCP? | O1, R6 | **run 2026-09-28 — PASSES.** `kubectl delete vmi` brought the VM back `Running` on the same `10.66.3.221`, new pod name, ledger entry re-pointed |
| 2 | Live-migrate that VM between `10-66-3-46` and `10-66-3-47` | O2, R1 | **run 2026-09-28 — fails on `whereabouts`, PASSES on `static`** (moved in ~20s, guest kept `.221`). Resolved by making the mode a per-pool choice |
| 3 | Stop/start the VM (`runStrategy` Halted → Always) — not just a pod restart. | O3 | not run |
| 4 | Inspect whether a Whereabouts `IPPool` CR can carry workload affinity. | O4 | answered: it cannot |

The controller was written before spike 1 ran, deliberately. Allocation, claiming and NAD minting
do not depend on its answer. What depended on it was whether a single-address NAD is a usable
address binding at all — and it is.

Both spike 1 results share a mechanism worth stating, because it is easy to get wrong: the address
survives because the **NAD** serves it, not because the ledger remembers the pod. In `whereabouts`
mode the claim's NAD pins `range_start == range_end`; in `static` mode it writes the address in.
Either way, if the `IPRequest` is deleted while the VM is down, the address returns to the free
pool and any claim can take it.

## Open questions

- **O1** — Does a single-address NAD hold its address across a restart, with the guest on DHCP?
  **Yes** (spike 1, 2026-09-28).
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
