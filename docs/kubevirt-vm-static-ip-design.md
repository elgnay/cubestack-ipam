<!--
  Bundled copy of design/vm/kubevirt-vm-static-ip-design.md from the suanova/design
  repository, current as of 2026-09-28. Edit the canonical copy there; this one is a
  snapshot kept here so the repo is self-contained.

  The API group used in §5.4 and §5.6 (ipam.cubestack.io) matches the implementation.
  §7 and §8 still list live migration (R1, spike 2) as open -- R1 is retired and spike 2
  dropped as of 2026-09-28; see the README. The document is a record of the analysis at
  the time and is left as written.
-->
# Static IP assignment for KubeVirt VMs — design proposal

**Status:** proposal, not implemented. Nothing in this document has been built.
**Date:** 2026-09-23
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
IPPool (cluster-scoped, admin)        IPRequest (namespaced, user)          NAD (per VM, generated)
  spec.range: 10.66.3.150-10.66.3.189   spec.poolRef:  cubestack-static-v4    name: vm-static-10-66-3-105
                                        spec.requestedIP: 10.66.3.105         ipam.range_start/end: <same ip>
  status.allocated[]  ← single source   status.assignedIP / nadName / phase   bridge: br0, gateway, dns, routes
       of truth (the IPRequest set)              │                                    │
                                                 └── ownerReference ← VM ─────────────┘ (by deterministic name)
```

### 5.2 The flow

1. Admin creates an `IPPool` declaring the usable range for a subnet.
2. User creates an `IPRequest` (optionally naming a preferred address).
3. Controller picks a free address — **the create is the atomic claim**, since the API server
   enforces object-name uniqueness; a lost race is a 409, not a silent double-book.
4. Controller templates the shared NAD's config (bridge, gateway, routes, DNS) into a per-VM NAD
   whose Whereabouts range is exactly that one address, and records its name in `status`.
5. User creates the VM referencing that NAD via `multus.networkName` (deterministic if the address
   was requested; otherwise read from `status.nadName` after the request is satisfied).
6. The guest stays on DHCP — the CNI hands the address to the pod and KubeVirt passes it into the
   guest, exactly as today. **No guest-side static configuration.**
7. Deleting the VM garbage-collects the `IPRequest` (ownerReference), whose finalizer removes the
   NAD and thereby returns the address to the pool.

### 5.3 Why the binding must be a distinct NAD

In Whereabouts the allocation scope is the **network name**, which is the NAD's name. A per-VM
`IPPool` CR sharing a network name with the shared NAD would *merge* its range into that network's
set — every VM could then be handed every per-VM address, so no binding. A distinct NAD is what
makes the single address exclusive. (Open question O4 below: if a Whereabouts `IPPool` CR can be
scoped to a workload, this sprawl disappears and one shared NAD suffices.)

### 5.4 Draft CRDs

```yaml
apiVersion: ipam.cubestack.io/v1alpha1
kind: IPPool
metadata: {name: cubestack-static-v4}
spec:
  subnet: 10.66.3.0/24
  range: 10.66.3.150-10.66.3.189     # disjoint from the dynamic window (.200-.220)
  gateway: 10.66.3.254               # and from the MetalLB pool (.221-.240)
  templateNAD: vm-underlay-10-66-3-0 # bridge/dns/routes are cloned from here
```

```yaml
apiVersion: ipam.cubestack.io/v1alpha1
kind: IPRequest
metadata:
  name: cubestack6-ip
  namespace: default
  ownerReferences:                   # set by the controller once the VM exists
    - {apiVersion: kubevirt.io/v1, kind: VirtualMachine, name: cubestack6, uid: …}
spec:
  poolRef: cubestack-static-v4
  requestedIP: 10.66.3.105           # optional; omit to let the controller pick
status:
  phase: Bound
  assignedIP: 10.66.3.105
  nadName: vm-static-10-66-3-105
  conditions: […]
```

Finalizer `ipam.cubestack.io/nad-cleanup` removes the generated NAD before the `IPRequest` goes.

### 5.5 Controller responsibilities

- **Allocate** — on `IPRequest` create, atomically claim an address (object name = address), create
  the NAD, set `status`. Retry with another address on collision.
- **Adopt** — once the referenced VM exists, set the `ownerReference` so GC works.
- **Release** — finalizer deletes the NAD.
- **Audit** (the part that must not be skipped):
  - `IPRequest`s with no NAD → the VM cannot start, or was started without its own address;
  - NADs with no `IPRequest` → orphaned address, invisible to the pool ledger;
  - VMs whose address is no longer covered by a claim → the duplicate direction.

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
two nodes exist, so migration is otherwise available. **Decide after spike 2**, with three possible
outcomes: accept no live migration for static VMs; cold-migrate instead; or fall back to a shared
pool for those VMs and accept address churn.

**R2 — Ordering.** The NAD must exist before the VM's virt-launcher pod is created, or Multus
cannot find it and the pod fails. Hence the controller creates it synchronously on `IPRequest`, and
the VM must be created *after*. Deterministic NAD naming (`vm-static-<ip-dashed>`) is what makes
the VM manifest writable in advance; in pick-mode the caller reads `status.nadName` first.

**R3 — Two ledgers.** Whereabouts knows each per-VM pool but nothing about the parent `IPPool`, so
the `IPRequest` set is the only record of who holds what. A hand-made NAD carved from the parent
range would be invisible to it — hence the audit loop in §5.5.

**R4 — Deleting a NAD out from under a running VM** does not immediately disrupt it (no CNI `DEL`
until the pod goes), so the damage surfaces later. Release ordering matters.

**R5 — Pool placement.** The `IPPool` range must be disjoint from both the Whereabouts dynamic
window (`.200–.220`) and the MetalLB pool (`.221–.240`). `.150–.189` is a candidate on the
`10.66.3.0/24` side; the `10.66.2.0/24` side needs its own band (dynamic window `.200–.220`).

**R6 — Documentation contradiction, still open.** `kubevirt-underlay-network-guide.md` §7C
recommends pinning an IP with the Multus `ips` annotation and claims Whereabouts reserves it;
`kubevirt-vm-user-guide.md` §7 opens by calling the same annotation an unreliable anti-pattern.
Both cannot be right. This design does not depend on the annotation, but the conflicting guidance
should be resolved (see spike 1).

**Open questions**

- **O1** — Does a single-address NAD actually hold its address across a VM restart, with the guest
  left on DHCP? (spike 1)
- **O2** — What does live migration do? (spike 2 — decides R1)
- **O3** — Does the address survive a **stop/start** (`runStrategy: Halted` → `Always`), not just a
  pod restart?
- **O4** — Can a Whereabouts `IPPool` CR be scoped to a workload, removing the per-VM NAD sprawl?
- **O5** — Should `IPPool` be cluster-scoped (as drawn) or namespaced for tenant isolation?
- **O6** — Should the controller also generate the VM manifest, or only the NAD?

---

## 8. Verification plan

Cheapest-first; the first two need one scratch VM each and are decisive.

| # | Spike | Decides |
|---|---|---|
| 1 | Create a single-address NAD, start a scratch VM on it, restart it several times, confirm the address holds and the guest needs no static config. | O1, R6 |
| 2 | Live-migrate that VM between `10-66-3-46` and `10-66-3-47`. | O2, R1 — go/no-go for the whole design |
| 3 | Stop/start the VM (`runStrategy` Halted → Always). | O3 |
| 4 | Inspect whether an `IPPool` CR can carry workload affinity. | O4 |

Only after spike 2 passes is it worth writing the controller.

---

## 9. Implementation phases

1. **Spikes** above; record results in this document.
2. **CRDs + controller skeleton** — `IPPool`, `IPRequest`, allocation with atomic create, NAD
   templating, finalizer.
3. **Lifecycle** — ownerReference adoption, GC, the audit loop.
4. **Tooling** — a small CLI or thin API for "what is free / claim this / release this", plus the
   audit report.
5. **Rollout** — pilot on cubestack VMs (whose IP is baked into cluster config and therefore the
   strongest motivation), then general use.

---

## 10. Status of the claims in this document

- §1, §2 and the MAC-stability evidence are **verified** against the live cluster on 2026-09-23
  (`kubectl` output quoted inline).
- The Spiderpool rows in §4, the `IPAMClaim`/OVN-Kubernetes statement, and the `SpiderReservedIP`
  cluster-scope inference are **from upstream documentation via search results, not verified on a
  live Spiderpool** — this cluster does not run Spiderpool. That documentation was not directly
  fetchable from this environment, so treat those rows as leads to confirm before acting on them.
- Everything in §5–§9 is **proposal**, not observed behaviour.

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
