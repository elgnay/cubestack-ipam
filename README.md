# cubestack-ipam

Static, user-claimable IP addresses for KubeVirt VMs on a Multus + Whereabouts underlay.

> **STATUS: SCAFFOLDING. THIS DOES NOT WORK YET.**
>
> The controllers are stubs — they log and take no action. **Nothing here allocates an
> address, mints a NetworkAttachmentDefinition, or releases anything.** The only logic
> implemented is `ipam.ValidatePoolRange` (pure validation) and the generated CRD schemas.
>
> This is deliberate. The design requires the spikes in [§8] to run *before* the
> controller is written, and the decisive one has not run. Do not deploy this expecting
> it to do anything.

[§8]: #spikes-to-run-before-writing-the-controller

## What this is

On this cluster a VM's underlay IP is allocated by Whereabouts to the **virt-launcher
pod**, not to the VM. A pod restart or recreation produces a new sandbox, the binding no
longer matches, and the address returns to the pool. That is survivable for a workload
whose address is incidental, and not survivable for a cubestack node whose address is
baked into its kubeadm certificates and etcd peer URLs.

The fix proposed in the design is a thin claim layer over the Whereabouts that is already
installed — no custom CNI IPAM plugin and no dataplane change:

- **`IPPool`** — cluster-scoped, created by an admin, declares a bounded band of addresses.
- **`IPRequest`** — namespaced, created by a user, owned by the VM. On creation a controller
  assigns it an address from the pool and materialises a **per-VM NetworkAttachmentDefinition**
  whose Whereabouts range is that single address (`range_start == range_end == <ip>`).
  The VM attaches to that NAD by a deterministic name and **the guest stays on plain DHCP**.

Because a single-address pool has no second address to hand out, the binding is enforced by
the CNI itself rather than by controller bookkeeping.

Full design: [`docs/kubevirt-vm-static-ip-design.md`](docs/kubevirt-vm-static-ip-design.md)
(a copy of `design/vm/kubevirt-vm-static-ip-design.md`; the copy is the one bundled here).

## The two CRDs

| CRD | Scope | Created by |
|---|---|---|
| `ippools.ipam.cubestack.io` | Cluster | admin |
| `iprequests.ipam.cubestack.io` | Namespaced | user |

> **Plural collision — read this before writing a `kubectl` command.** Whereabouts already
> owns the plural `ippools` (`ippools.whereabouts.cni.cncf.io`) in `kube-system`. This
> project's plural is `ippools.ipam.cubestack.io`, so listing pools **must** use the full
> resource name. There is deliberately no short name, because one would make the collision
> worse rather than better.

## Lifecycle

1. Admin creates an `IPPool` declaring the usable band for a subnet.
2. User creates an `IPRequest` (optionally naming a preferred address).
3. The controller picks a free address. **The create is the atomic claim** — the API server
   enforces object-name uniqueness, so a lost race is a `409`, not a silent double-book.
4. The controller templates the pool's `templateNAD` (bridge, gateway, routes, DNS) into a
   per-VM NAD whose Whereabouts range is exactly that one address.
5. The user creates the VM referencing that NAD via `multus.networkName`.
6. The guest stays on DHCP. No guest-side static configuration.
7. Deleting the VM garbage-collects the `IPRequest` via `ownerReference`; its finalizer
   (`ipam.cubestack.io/nad-cleanup`) removes the NAD and returns the address to the pool.

**Ordering matters:** the NAD must exist before the VM's virt-launcher pod is created.
Multus resolves it by name at pod-sandbox creation, and an absent NAD is not a clean error —
kubelet retries sandbox creation forever, so the pod sits in `FailedCreatePodSandBox` and
the VM never starts. That is why step 3-4 are synchronous on `IPRequest` create.

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

The `10.66.3.0/24` NAD carries the effective IPAM config; the `IPPool` CR is only the
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

A NAD on this underlay also needs the annotation
`k8s.v1.cni.cncf.io/resourceName: bridge.network.kubevirt.io/br0`.

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

Whatever band is chosen must also be disjoint from the **MetalLB pool** (`.221–.240`).

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

Note: the design document still lists R1 and spike 2 as open, since it is a record of the
analysis at the time.

## Spikes to run before writing the controller

Cheapest first. Each needs one scratch VM. Record results in the design document.

| # | Spike | Decides | Status |
|---|---|---|---|
| 1 | Create a single-address NAD, start a scratch VM on it, restart it several times. Does the address hold with the guest on DHCP? | O1, R6 | **not run — blocks everything** |
| 2 | ~~Live-migrate that VM between `10-66-3-46` and `10-66-3-47`~~ | O2, R1 | dropped, see above |
| 3 | Stop/start the VM (`runStrategy` Halted → Always) — not just a pod restart. | O3 | not run |
| 4 | Inspect whether a Whereabouts `IPPool` CR can carry workload affinity. | O4 | not run |

Spike 1 is the load-bearing one: the entire design rests on a single-address NAD holding its
address across a restart. If it does not, the design needs rework before any controller code
is worth writing.

## Open questions

- **O1** — Does a single-address NAD hold its address across a restart, with the guest on DHCP?
- **O3** — Does it survive a stop/start, not just a pod restart?
- **O4** — Can a Whereabouts `IPPool` CR be scoped to a workload, removing the per-VM NAD sprawl?
- **O5** — Should `IPPool` be cluster-scoped (as built) or namespaced for tenant isolation?
- **O6** — Should the controller also generate the VM manifest, or only the NAD?
- **O7** (new) — Does the rollout need to handle VMs already holding a dynamic address? The
  design does not say how an existing VM moves onto a claim.

## Development

```bash
make manifests generate   # regenerate CRDs and deepcopy — CI fails if this produces a diff
make build
make test                 # envtest; the CRD schemas are installed, which validates the CEL rules
```

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

Everything past validation. Specifically: address claiming, NAD minting, the finalizer's
cleanup path, `ownerReference` adoption, the three-direction audit loop (design §5.5), the
"what is free / claim this / release this" tooling, and any status conditions. `IPPool.status`
carries conditions only by design — the set of `IPRequest`s is the single authoritative
ledger, and a second list would drift (design §R3).
