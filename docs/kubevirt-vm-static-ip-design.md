<!--
  Bundled copy of design/vm/kubevirt-vm-static-ip-design.md from the suanova/design
  repository, current as of 2026-09-28. Edit the canonical copy there; this one is a
  snapshot kept here so the repo is self-contained.

  The API group used in §5.4 and §5.6 (ipam.cubestack.io) matches the implementation, and
  §5, §7, §8 and §9 record where the implementation landed: R1 (live migration) is retired,
  spike 2 is dropped, and the shared underlay NAD is being retired. Spike 1 has still not
  run -- see the README for what that does and does not invalidate.
-->
# Static IP assignment for KubeVirt VMs — design proposal

**Status:** partly implemented. §5's API and the two controllers are built and tested in
`cubestack-ipam`; §9.4–§9.5 (tooling, rollout) are not. §1–§4 are analysis, §6–§8 are risk and
spike records. **Spike 1 has not run** — the load-bearing assumption that a single-address NAD
holds its address across a restart is still unverified, and §10 says which claims were checked
against a live cluster and which were not.
**Date:** 2026-09-23 (status line updated 2026-09-28)
**Scope:** user-side VM addressing on the `10.66.2.0/24` and `10.66.3.0/24` underlays.
**Related:** `kubevirt-vm-user-guide.md` §6.3/§7, `kubevirt-underlay-network-guide.md` §7C, `kubevirt-cluster-admin-guide.md` §2.

---

## TL;DR

VM underlay IPs are allocated by Whereabouts per **virt-launcher pod**, so they move when a VM
restarts or migrates. The upstream fix (`IPAMClaim` + KubeVirt's `PersistentIPs`) is not reachable
on this stack, and adopting Spiderpool would put the underlay dataplane (bridge binding on
`cnv-bridge`) into question.

**Proposal: build a thin claim layer on top of the Whereabouts already installed.**

Two CRDs:

- **`IPPool`** — admin-created, declares a bounded range of addresses.
- **`IPRequest`** — user-created, namespaced, owned by the VM. On creation a controller assigns it
  an address from the pool and materializes a **per-VM NetworkAttachmentDefinition** whose
  Whereabouts range is that single address (`range_start = range_end = <ip>`). The VM attaches to
  that NAD by a deterministic name; the guest stays on plain DHCP.

No custom CNI IPAM plugin. No new dataplane. The binding is enforced by the CNI itself, because a
single-address pool has no second address to hand out.

---

## 1. The problem

An underlay IP belongs to the *pod*, not to the VM:

```
$ kubectl -n kube-system get overlappingrangeipreservations.whereabouts.cni.cncf.io 10.66.3.205 -o yaml
spec:
  ifname: pod7f806d4ce1e
  podref: default/virt-launcher-cubestack6-m47r7      # ← pod name + random suffix
```

`podref` (and, where set, `containerid`) identifies the pod sandbox. A restart recreates that
sandbox with a new name and a new container ID, the binding no longer matches, and the address
returns to the pool. The next allocation may hand it to a different VM. Live migration has the
same shape plus worse: the target pod needs an address while the source pod still holds its own.

This matters more here than for ordinary workloads because a VM's address is not incidental — it
is baked into whatever runs inside the VM. A cubestack node's IP appears in its kubeadm
certificates, its etcd peer URLs and the installer's `NODES_MASTER` line, so a changed address is
not a config edit but a rebuild.

Note what is *already* stable, because it halves the problem: **kubemacpool** (installed via CNAO,
`spec.kubeMacPool: {}`) writes the assigned MAC into the **VM spec**, so it survives restart and
migration. Verified: `cubestack6` reports `spec_mac == live_mac == 02:17:8d:aa:80:61`. Only L3
drifts.

---

## 2. Verified environment (2026-09-23)

Read from the live cluster through `KUBECONFIG=~/.kube/kubeconfig.vm`.

| Item | Value |
|---|---|
| KubeVirt | `v1.8.4` |
| Feature gates | `["HostDevices","Snapshot","VMPool"]` — **no `PersistentIPs`** |
| `IPAMClaim` CRD | **absent** |
| Pod CNI | Calico |
| Underlay CNI | CNAO: Multus + linux bridge (`cnv-bridge`) |
| IPAM | Whereabouts, DaemonSet + reconciler in `kube-system`, image tag **`:latest`** |
| MAC allocation | kubemacpool, in `cluster-network-addons`, MAC persisted into the VM spec |
| Whereabouts CRDs | `ippools`, `overlappingrangeipreservations` only |

The `10.66.3.0/24` NAD carries the effective IPAM config — the `IPPool` CR is only the
allocation ledger:

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

Observed allocations cluster in `.200–.207`, consistent with `range_start` being honoured.

---

## 3. Requirements

| # | Requirement | Where it has to live |
|---|---|---|
| R1 | Bounded, discoverable address space | pool CRD + status |
| R2 | User chooses a specific address from it | **control plane (custom)** |
| R3 | Address survives VM restart / pod recreation | **identity model (custom)** |
| R4 | Address released automatically when the VM is deleted | ownership + GC |
| R5 | No duplicate allocation; conflict is loud | allocation primitive |
| R6 | Works on the existing Multus + bridge underlay | CNI / IPAM config |
| R7 | Self-service: user picks without an admin in the loop | **control plane (custom)** |

Only R6 touches the dataplane. The rest is a controller and a CRD — which is why a custom *IPAM
plugin* is not required (see §5).

---

## 4. Options considered

| Option | Verdict |
|---|---|
| **Guest static IP, outside the dynamic range** (`kubevirt-vm-user-guide.md` §7 C) | Works, zero platform change, but the guide itself concedes "靠人工记账": no conflict detection at all. Superseded by this proposal. |
| **`SpiderReservedIP` + guest static** | Coherent: the reservation *withholds* the address from dynamic allocation while the guest is configured statically. But the reservation is cluster-scoped (no `namespace` in its metadata) so a namespaced VM **cannot own it** — Kubernetes forbids a namespaced owner for a cluster-scoped dependent, so no `ownerReference` and no GC. Both the claim and the release stay manual, and deleting the reservation while the VM lives produces a silent duplicate. |
| **Spiderpool's native VM-scoped fixed IP** | Would solve R3 by construction (`SpiderEndpoint` inherits the **VM's** namespace and name rather than the pod's). Rejected for now because its KubeVirt docs pair with macvlan/ipvlan/OVS, and macvlan/ipvlan are documented as incompatible with KubeVirt **bridge** binding — i.e. it implies changing this cluster's underlay dataplane. Also picks addresses *randomly* from a specified pool, so R2 would still need per-VM pools. |
| **`IPAMClaim` / KubeVirt `PersistentIPs`** (upstream) | The right long-term answer, and the standard this proposal deliberately imitates. Not reachable: no CRD, no gate, and it is implemented by OVN-Kubernetes — Whereabouts was explicitly *not* the delegated-IPAM design. Would require moving the underlay to OVN-K UDN. |
| **`kubevirt-ip-helper`** | Off-the-shelf static DHCP reservations for KubeVirt bridge networks; pairs with the stable kubemacpool MAC to give restart- and migration-stable IPs with no guest-side config. Not chosen here (R2/R7 — no user-facing claim over a shared pool, the space is declared in its own config). **Still worth a spike as the cheapest path to R3+R6 if the self-service requirement is dropped.** |
| **Custom CNI IPAM plugin** | Feasible (the plugin surface is small — Whereabouts is itself just a binary plus CRD state) but the hard parts are GC, cross-node concurrency and migration, and it makes us own a dataplane component across KubeVirt/Multus/CNAO upgrades. Unnecessary once the binding is delegated to a single-address NAD. |

---

## 5. Chosen design

### 5.1 Shape

```
IPPool (cluster-scoped, admin)        IPRequest (namespaced, user)          NAD (per VM, minted)
  spec.range: 10.66.3.150-.189          spec.poolRef: cubestack-static-v4     name: cubestack6-static
  spec.nadTemplate:                     spec.nad:     cubestack6-static       ipam.range_start/end: <same ip>
    subnet, gateway, bridge, ...        status.assignedIP / phase             bridge: br0, gateway, dns, routes
  ← the single declaration of           status.conditions[]                   owned by the IPRequest
    both the band and the template      │                                        ▲
        │                               │  created by controller #2              │ minted by controller #1
        │  read by controller #1        └── ownerReference ← VM ────────────────┘
```

The fields divide along one line: `spec.range` is what the **allocator** needs, and
`spec.nadTemplate` is what the **CNI** needs, including the addressing that config carries.
`range` is therefore the only required field — a pool that mints nothing needs no subnet and no
gateway — and the template's fields are required exactly when the template is present, which
makes it all-or-nothing without any rule having to say so. (Updated 2026-09-28; the shape here
previously put `subnet` and `gateway` on the pool itself, alongside `range`.)

There is no ledger in `IPPool.status`: the set of `IPRequest`s is the only authoritative record
of who holds what, so a second list would be a second thing to keep in step (§R3).

Two controllers, deliberately split by what they decide rather than by object:

- **#1 owns `IPPool` and `IPRequest`.** It assigns the address and mints the NAD. Both, because
  the NAD's Whereabouts range *is* the assigned address — one fact, so one writer.
- **#2 owns `VirtualMachine`.** It turns an annotation into a claim and decides nothing else.
  It is create-only and never writes the VM.

Ownership is `VM → IPRequest → NAD`, all by `ownerReference`. There are **no finalizers**:
release is garbage collection walking that chain.

### 5.2 The flow

1. Admin creates an `IPPool`: the band, and — when the pool should mint networks — the NAD
   template cloned into each per-VM NAD (subnet, gateway, bridge, VLAN, MAC spoof checking, DNS,
   routes).
2. User creates a VM carrying `ipam.cubestack.io/pool: cubestack-static-v4` and naming its NAD in
   `multus.networkName` — a name it can choose before any address exists.
3. Controller #2 creates `IPRequest` `<vm-name>-ip` in the VM's namespace, with `spec.nad` set and
   an `ownerReference` to the VM.
4. Controller #1 takes the lowest free address in the pool, writes `status.assignedIP`, then mints
   the NAD from the pool's template with `range_start == range_end == <that ip>`, owned by the
   claim.
5. The guest stays on DHCP — the CNI hands the address to the pod and KubeVirt passes it into the
   guest, exactly as today. **No guest-side static configuration.**
6. Deleting the VM garbage-collects the `IPRequest`, which garbage-collects the NAD, which returns
   the address to where Whereabouts' ledger derives it from.

The VM necessarily exists before its NAD, so it shows `FailedCreatePodSandBox` for the few seconds
until step 4. That is the reverse of what §R2 originally prescribed, and it is the ordering the
trigger makes possible: annotation first, VM second.

An `IPRequest` can also be created by hand, with or without `spec.nad` — the claim layer does not
require a VM. Omitting `spec.nad` binds an address and mints nothing.

### 5.3 Why the binding must be a distinct NAD

**Corrected 2026-09-28 against the installed Whereabouts.** The original text here asserted that
"in Whereabouts the allocation scope is the network name, which is the NAD's name". That is not
what the installed version does. Verified live: the Whereabouts `IPPool` CR is named after the
**range** (`10.66.3.0-24`, in `kube-system`) and is shared by every NAD declaring that same
`range`; its schema is `spec.range` plus `spec.allocations` keyed by last octet. There is no
`spec.ranges[]` and no `ipPool:` field, so the underlay guide §7B's `vm-pool` form and open
question O4 do not apply to this installation.

The per-VM NAD still yields exclusivity, but by a different mechanism than claimed: `range_start ==
range_end` bounds *that network's candidate set* to one address, within a ledger **shared** with
every other NAD on the subnet. Exclusivity therefore holds twice over — the NAD cannot hand out a
second address, and the shared ledger refuses an address another network already holds. The second
is what makes a duplicate a **loud** failure: CNI `ADD` refuses, and the VM does not start, rather
than two guests silently sharing an address. It also means collisions are detected against
dynamically-allocated pods, not only against other claims.

That is why `ipam.range` is derived from `nadTemplate.subnet` rather than from the pool's
allocatable band: the range is what selects the ledger, so it has to be computed in one place, and
it is a different thing from the band the pool hands out (the band must lie inside it).

### 5.4 Draft CRDs

```yaml
apiVersion: ipam.cubestack.io/v1alpha1
kind: IPPool
metadata: {name: cubestack-static-v4}
spec:
  range: {start: 10.66.3.150, end: 10.66.3.189}   # the only required field
  nadTemplate:                     # optional; typed, and cloned into every minted NAD
    subnet: 10.66.3.0/24           # becomes ipam.range -- selects the Whereabouts ledger
    gateway: 10.66.3.254           # becomes ipam.gateway
    type: cnv-bridge               # defaults to cnv-bridge
    bridge: br0
    macspoofchk: false
    dns: {nameservers: [223.5.5.5, 8.8.8.8]}
    routes: [{dst: 0.0.0.0/0}]
```

The fields divide by what needs them: `range` is the allocator's, everything in `nadTemplate` is
the minted config's. So a pool that mints nothing is a band and nothing else — no subnet, no
gateway, no template:

```yaml
spec:
  range: {start: 10.66.3.150, end: 10.66.3.189}
```

`range_start`/`range_end` are still injected per claim, from the address assigned to it — those are
the one piece of addressing that is not the admin's to declare. `nadTemplate` is **typed** rather
than a JSON blob so that a missing `bridge`, a `vlan` of 9999 or an unknown plugin type is rejected
at admission, instead of surfacing later as a VM stuck in `FailedCreatePodSandBox`; the same
property now covers `subnet` and `gateway`, which is what lets a template be all-or-nothing. The
cost is on the other side: a CNI feature not modelled here cannot be expressed at all.

```yaml
apiVersion: ipam.cubestack.io/v1alpha1
kind: IPRequest
metadata:
  name: cubestack6-ip
  namespace: default
  ownerReferences:                   # set on create by controller #2
    - {apiVersion: kubevirt.io/v1, kind: VirtualMachine, name: cubestack6, uid: …}
spec:
  poolRef: cubestack-static-v4
  nad: cubestack6-static             # optional; the NAD to mint. Omit for a ledger-only claim
status:
  phase: Bound
  assignedIP: 10.66.3.150
  conditions: […]
```

There is no `spec.requestedIP` and no finalizer. The address is always controller-chosen — lowest
free in the pool — which makes allocation deterministic and removes the class of failure where a
requested address is already taken. Release is garbage collection; a finalizer would add a way for
an object to get stuck to do a job the garbage collector already does correctly.

### 5.5 Controller responsibilities

**Controller #1 — `IPPool` and `IPRequest`.**

- **Validate** — an `IPPool` is checked in Go for the invariants the CRD cannot express: band
  ordering, gateway outside the band, and gateway inside the template's subnet. The last of those
  was added 2026-09-28 with the split that moved `gateway` into `nadTemplate`: a gateway in another
  subnet is trivially outside the band, so the band check alone passed it, and the result is a VM
  whose default route points at an address its bridge cannot reach. The verdict is published as an
  `Available` condition. The remaining invariant, disjointness from other NADs on the subnet, is
  **not** checkable any more once the shared NAD is retired, and becomes an admin responsibility
  bounded by RBAC: regular users cannot create NADs.
- **Allocate** — on `IPRequest`, take the lowest free address in the pool. An address already in
  `status` is reused, never re-derived: a claim must not move because another claim appeared
  beneath it.
- **Mint** — render the pool's template with this claim's address and create the NAD, owned by the
  claim. Ordering is NAD first, `Bound` second, so that `Bound` implies the network exists.
- **Adopt** — the NAD is adopted only if it is already controller-owned by *this* claim. Any other
  owner fails the claim as `NADNameTaken`; an object this project did not create is never
  overwritten or taken over.
- **Release** — nothing. The chain is an `ownerReference` chain and garbage collection walks it.

**Controller #2 — `VirtualMachine`.**

- **Trigger** — `ipam.cubestack.io/pool` on the VM; `ipam.cubestack.io/nad-name` optionally
  overrides the default `<vm-name>-static`.
- **Create-only** — it creates `IPRequest` `<vm-name>-ip` and never patches the VM, and never
  patches a claim that already exists. The NAD name has to be known to the VM manifest before the
  address exists, so a controller that rewrote either side would race the object depending on it.
  Divergence between the annotation and the existing claim is reported as an event; the escape
  hatch is to delete the claim and let it be recreated.

**Audit — the part that must not be skipped.** A periodic sweep outside both reconcilers, because
every check here spans objects that no single reconcile sees:

- a `Bound` claim with `spec.nad` set and no NAD → the VM cannot start;
- a NAD carrying this project's label whose `IPRequest` is gone → orphaned address, still held in
  the Whereabouts ledger and invisible to the claim layer;
- two claims holding one address → the residue of the allocation race (below);
- claims referencing a pool that no longer exists → named once per pool, with the count, which is
  what an admin deciding whether to recreate it needs.

**A race that is documented rather than closed.** Two claims created in the same instant can both
be reconciled against a cache that does not yet show the other's assignment. The window is small
and the consequence is bounded and loud — the per-VM NADs share a ledger, so the second CNI `ADD`
is refused and its VM does not start, rather than the two silently sharing an address — and the
audit reports the collision afterwards. Closing it properly needs a compare-and-swap on something
address-shaped, which this design gave up when per-address claim objects were dropped.

### 5.6 Naming

The kinds `IPPool`/`IPRequest` live in a **distinct API group** (`ipam.cubestack.io`), because
`ippools` is already Whereabouts' plural in `kube-system`. Callers should expect to disambiguate
with `ippools.ipam.cubestack.io`.

---

## 6. What this buys, and what it costs

**Buys:** R1–R7, with the allocation itself enforceable by the CNI; the address is a real
allocation (not an invisible static one); no guest-side static config; lifecycle handled by
Kubernetes GC; no underlay dataplane change; a single authoritative ledger (the `IPRequest` set)
that is queryable for self-service.

**Costs:** one NAD and one `IPRequest` per VM; a controller to write, run and own; a dependency on
Whereabouts behaviour, currently shipped on a **mutable `:latest` tag** that should be pinned
before anything depends on it this hard.

---

## 7. Risks and open questions

**R1 — Live migration changes shape, and this is the biggest risk.** With
`range_start = range_end`, there is no fallback address: the target pod of a live migration asks
for the only address in the pool while the source pod still holds it, so the failure mode is
expected to be *migration stalls* rather than *IP changes*. The user guide already flags the
single-address NAD's migration caveat (§7 A, "Whereabouts 迁移通病"). On the `10.66.3.0/24` subnet
two nodes exist, so migration is otherwise available.

**Decision, 2026-09-28: live migration is out of scope for static-IP VMs.** Spike 2 is dropped
rather than run, and the risk is accepted as a documented limitation instead of an open question.
One consequence is easy to miss: if the cluster's `evictionStrategy` is `LiveMigrate`, a **node
drain will attempt exactly that migration**. Static-IP VMs should carry `evictionStrategy: None`
so a drain restarts them instead.

**R2 — Ordering.** The NAD must exist before the VM's virt-launcher pod is created, or Multus
cannot find it and the pod fails. As implemented the VM comes *first* — the annotation on it is
what creates the claim — so the VM sits in `FailedCreatePodSandBox` for the few seconds until the
NAD is minted. That is a visible, self-healing wait rather than a failure. What makes the VM
manifest writable at all is that the NAD's name comes from the VM's name (`<vm-name>-static`) and
not from the address.

**R3 — Two ledgers.** Whereabouts knows each per-VM pool but nothing about the parent `IPPool`, so
the `IPRequest` set is the only record of who holds what. A hand-made NAD carved from the parent
range would be invisible to it — hence the audit sweep in §5.5. With the shared underlay NAD
retired and regular users unable to create NADs, the residue is an admin creating one by hand.

**R4 — Deleting a NAD out from under a running VM** does not immediately disrupt it (no CNI `DEL`
until the pod goes), so the damage surfaces later. Ownership is what prevents it: the NAD is
controller-owned by its claim, so nothing removes it while the claim lives.

**R5 — Pool placement.** The `IPPool` range must be disjoint from any other NAD serving the same
subnet. `.150–.189` is the candidate on the `10.66.3.0/24` side. Two corrections to the original
text, both verified on 2026-09-28:

- **MetalLB is not installed on this cluster** — no CRDs, no namespace — so there is no MetalLB
  pool to avoid, and the half of this risk about `.221–.240` is unenforceable and moot. It has to
  be re-checked if MetalLB is ever installed on this subnet.
- The Whereabouts dynamic window (`.200–.220`) belongs to the shared `vm-underlay-10-66-3-0` NAD,
  which this design **retires**. Once it is gone the band is unconstrained by it. Keeping `.150–.189`
  preserves continuity with the earlier plan and avoids renumbering anything during the cutover.

The disjointness check itself retires as a controller check: with no live NAD to audit against,
it becomes an admin responsibility, bounded by RBAC — regular users cannot create NADs, so an
overlap can only be introduced deliberately.

**R6 — Documentation contradiction, still open.** `kubevirt-underlay-network-guide.md` §7C
recommends pinning an IP with the Multus `ips` annotation and claims Whereabouts reserves it;
`kubevirt-vm-user-guide.md` §7 opens by calling the same annotation an unreliable anti-pattern.
Both cannot be right. This design does not depend on the annotation, but the conflicting guidance
should be resolved (see spike 1).

**R7 — The cutover is a migration, not a deploy.** Deleting the shared `vm-underlay-10-66-3-0` NAD
cuts the network off from every VM and pod still attaching it (open question O7). Consumers have to
move onto claims *first*; the NAD can only be deleted once nothing references it. Out of scope for
the controller work, but it is the risk most likely to be underestimated.

**Open questions**

- **O1** — Does a single-address NAD actually hold its address across a VM restart, with the guest
  left on DHCP? (spike 1 — **still not run**, and still the load-bearing assumption)
- **O2** — What does live migration do? (spike 2 — closed: live migration is out of scope, see R1)
- **O3** — Does the address survive a **stop/start** (`runStrategy: Halted` → `Always`), not just a
  pod restart?
- **O4** — Can a Whereabouts `IPPool` CR be scoped to a workload, removing the per-VM NAD sprawl?
  (closed: the installed Whereabouts keys its pool on the range, not on a workload)
- **O5** — Should `IPPool` be cluster-scoped (as built) or namespaced for tenant isolation?
- **O6** — Should the controller also generate the VM manifest, or only the NAD? (decided: only the
  NAD. The VM is the trigger, not an output — see §5.5.)

---

## 8. Verification plan

Cheapest-first. Spike 1 still needs one scratch VM, and it is still decisive.

| # | Spike | Decides | Status |
|---|---|---|---|
| 1 | Create a single-address NAD, start a scratch VM on it, restart it several times, confirm the address holds and the guest needs no static config. | O1, R6 | not run |
| 2 | Live-migrate that VM between `10-66-3-46` and `10-66-3-47`. | O2, R1 | dropped — R1 retired 2026-09-28 |
| 3 | Stop/start the VM (`runStrategy` Halted → Always). | O3 | not run |
| 4 | Inspect whether an `IPPool` CR can carry workload affinity. | O4 | answered — it cannot |

The original gate here was that spike 2 had to pass before the controller was worth writing. That
gate is gone with R1. **Spike 1 has not run**, and the controller was written anyway, deliberately:
allocation, claiming and NAD minting do not depend on its answer. What depends on it is whether a
single-address NAD is a usable address binding at all — and if it is not, that invalidates the
approach rather than the code.

---

## 9. Implementation phases

1. **Spikes** above; record results in this document. **Still outstanding** for spike 1 and 3.
2. **CRDs + controllers** — `IPPool`, `IPRequest`, allocation, NAD minting, the `VirtualMachine`
   trigger, the audit sweep. **Done**, in `cubestack-ipam`; no finalizer, which is a change from
   what this phase originally said.
3. **Lifecycle** — ownership, garbage collection, the audit loop. **Done** as part of 2; there is
   no separate GC phase because there is nothing to release by hand.
4. **Tooling** — a small CLI or thin API for "what is free / claim this / release this", plus the
   audit report. **Not done.**
5. **Rollout** — pilot on cubestack VMs (whose IP is baked into cluster config and therefore the
   strongest motivation), then general use. **Not started**, and it is a migration, not a deploy:
   see R7.

---

## 10. Status of the claims in this document

- §1, §2 and the MAC-stability evidence are **verified** against the live cluster on 2026-09-23
  (`kubectl` output quoted inline).
- The Spiderpool rows in §4, the `IPAMClaim`/OVN-Kubernetes statement, and the `SpiderReservedIP`
  cluster-scope inference are **from upstream documentation via search results, not verified on a
  live Spiderpool** — this cluster does not run Spiderpool. That documentation was not directly
  fetchable from this environment, so treat those rows as leads to confirm before acting on them.
- Everything in §5–§9 is **proposal**, not observed behaviour, with two exceptions that were
  verified against the live cluster on 2026-09-28 and are recorded inline: the Whereabouts `IPPool`
  scoping correction in §5.3, and MetalLB's absence in §R5. The controller described in §5.5 is
  implemented in `cubestack-ipam` and its behaviour is covered by envtest, which tests the
  controller — not the dataplane, which is what spike 1 is for.
- The API shape in §5.1 and §5.4 is the implemented one, and it changed on 2026-09-28: `subnet` and
  `gateway` moved from `IPPool.spec` into `IPPool.spec.nadTemplate`, leaving `range` as the only
  required field. Read those sections as a record of the built API, not as the earlier sketch. The
  same edit added the gateway-inside-subnet check described in §5.5, which the earlier shape could
  not have stated because `gateway` and `subnet` were then independent top-level fields.

---

## Appendix A — commands used to establish §1 and §2

```bash
export KUBECONFIG=~/.kube/kubeconfig.vm

# the effective IPAM config lives in the NAD, not the IPPool CR
kubectl get nad -n default vm-underlay-10-66-3-0 -o jsonpath='{.spec.config}'

# the allocation binding that breaks on restart
kubectl -n kube-system get overlappingrangeipreservations.whereabouts.cni.cncf.io \
  10.66.3.205 -o yaml

# feature gates: no PersistentIPs
kubectl get kubevirt -n kubevirt -o jsonpath='{.items[0].spec.configuration.developerConfiguration}'

# no IPAMClaim CRD
kubectl get crd --no-headers | grep -i -E 'ipam|ipamclaim'

# MAC is persisted into the VM spec (kubemacpool), so it does NOT drift
kubectl -n default get vm cubestack6 -o jsonpath='{.spec.template.spec.domain.devices.interfaces[*].macAddress}'
kubectl -n default get vmi cubestack6 -o jsonpath='{.status.interfaces[*].mac}'
```

## Appendix B — references

- KubeVirt IPAM for secondary networks: <https://github.com/kubevirt/kubevirt/pull/11410>
- KubeVirt IPAM extension (IPAMClaim lifecycle): <https://github.com/kubevirt/ipam-extensions>
- Whereabouts `OverlappingRangeIPReservation` API reference:
  <https://docs.redhat.com/en/documentation/openshift_container_platform/4.14/html/network_apis/overlappingrangeipreservation-whereabouts-cni-cncf-io-v1alpha1>
- Spiderpool — Reserved IP: <https://spidernet-io.github.io/spiderpool/v0.6/usage/reserved-ip/>
- Spiderpool — KubeVirt fixed IP: <https://spidernet-io.github.io/spiderpool/v0.8/usage/kubevirt-zh_CN/>
- Spiderpool IPPool status / free-IP accounting: <https://spidernet-io.github.io/spiderpool/v0.7/usage/ippool-multi/>
- kubevirt-ip-helper: <https://github.com/joeyloman/kubevirt-ip-helper>
