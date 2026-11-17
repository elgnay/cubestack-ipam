# cubestack-ipam user guide

How to give a KubeVirt VM a static underlay IP address that survives a pod restart.

This is the task-oriented guide. For why the project is built this way — the addressing modes, the
trade-offs, the risks — read [`../README.md`](../README.md) and the
[design document](kubevirt-vm-static-ip-design.md). This document assumes that work is done and
answers "what do I type".

---

## What you get

A VM that keeps the same address across a restart, with the guest on plain DHCP.

You never write the address down. You name a **pool** on your VM; a claim layer picks an address
from it and mints a **one-address network** for your VM to attach to. Because the address lives in
the network rather than in the pod's sandbox, recreating the pod does not lose it.

| | |
|---|---|
| Do | annotate the VM, name the NAD in its network list |
| Don't | configure a static IP inside the guest — it stays on DHCP |
| Result | address survives pod restart; migration depends on the pool's mode (below) |

## Who does what

| Role | Does | Frequency |
|---|---|---|
| Cluster admin | installs the controllers, creates `IPPool`s | once per subnet |
| VM owner | annotates a VM, names its network | once per VM |
| Anyone who needs a range | reserves a block of addresses from a pool | occasionally |

You need no special permission to *use* a pool — only to create one. Reserving a block is the same
kind of operation: it draws from a pool rather than defining one. It is not, however, granted by
default — see [Reserving a block of addresses](#reserving-a-block-of-addresses) for the permission
you need and why it is withheld.

---

## For VM owners

### 1. Find a pool

```bash
kubectl get ippools.ipam.cubestack.io
```

```
NAME          SUBNET         START         END           GATEWAY       AGE
dev-ip-pool   10.66.3.0/24   10.66.3.200   10.66.3.230   10.66.3.254   19h
```

> **Use the full resource name.** Whereabouts already owns the plural `ippools`
> (`ippools.whereabouts.cni.cncf.io`) in `kube-system`. A bare `kubectl get ippools` may show you
> the wrong objects, or nothing. There is deliberately no short name.

A pool's `START`/`END` is the band you might get an address from. By default the allocator
chooses — the lowest free one — but you can name one instead with
`ipam.cubestack.io/address` on a VM or `spec.allocation` on a claim; see below. The pool can
exclude some of its own band, and cannot be made to hand those out.

**Which mode is the pool in?** It matters for migration and it is not shown by default:

```bash
kubectl get ippools.ipam.cubestack.io dev-ip-pool -o jsonpath='{.spec.nadTemplate.ipam}{"\n"}'
```

`static` → your VM can live-migrate. `whereabouts` → it cannot, and a node drain may hang trying
(see [Live migrate](#live-migrate)).

### 2. Annotate the VM and name its network

Three annotations, and one line in the network list:

| Annotation | Required | Meaning |
|---|---|---|
| `ipam.cubestack.io/pool` | **yes** | The pool to claim from. Its presence is the trigger — a VM without it is left alone entirely |
| `ipam.cubestack.io/nad-name` | no | The NAD to mint. Defaults to `<vm-name>-static` |
| `ipam.cubestack.io/address` | no | A specific address to claim. Makes the claim `Exact`: it binds that address or fails, never substituting another. Without it the controller takes the lowest free one |

The VM's `networks[].multus.networkName` must name **the same NAD**, as `namespace/name`:

```yaml
metadata:
  annotations:
    ipam.cubestack.io/pool: dev-ip-pool
    ipam.cubestack.io/nad-name: myvm-static      # optional; this is the default anyway
    # ipam.cubestack.io/address: 10.66.3.205    # optional; Exact allocation
spec:
  template:
    spec:
      networks:
        - name: underlay
          multus:
            networkName: default/myvm-static     # must match, with the namespace
      domain:
        devices:
          interfaces:
            - name: underlay                     # local to this manifest
              bridge: {}
```

**A requested address is a demand, not a preference.** If it is outside the pool, excluded by the
pool, or already held, `<vm-name>-ip` goes `Failed` with `AddressOutOfRange`, `AddressExcluded` or
`AddressUnavailable`, and `status.blockers` names the holder. The VM then waits in
`FailedCreatePodSandBox` until the claim binds — a wrong address is a visible wait, never a VM
quietly running on a different one. A pool can refuse exact requests with
`allocation.allowExact: false` (`ExactNotAllowed`).

The NAD does not exist yet when you apply this. That is by design — the name is derived from the
VM's name, never from the address, so the manifest can be written before any address is assigned.

### 3. A complete working VM

Trimmed from [`../config/samples/kubevirt_v1_virtualmachine.yaml`](../config/samples/kubevirt_v1_virtualmachine.yaml),
which is the version to copy.

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: myvm
  namespace: default
  labels:
    owner: myname
  annotations:
    ipam.cubestack.io/pool: dev-ip-pool
spec:
  runStrategy: Always
  dataVolumeTemplates:
    - metadata:
        name: myvm-rootdisk
      spec:
        source:
          pvc:
            name: ubuntu-22.04-server-amd64-img
            namespace: default
        storage:
          # RWX + Block. RWX is what live migration needs: the target pod attaches
          # the same disk from another node while the source pod is still running.
          accessModes: [ReadWriteMany]
          storageClassName: ceph-rbd-kubevirt
          volumeMode: Block
          resources:
            requests:
              storage: 20Gi
  template:
    metadata:
      labels:
        kubevirt.io/domain: myvm
    spec:
      # No nodeSelector. Every node is on the same subnet, so the identity of the
      # node says nothing about the address -- the pool-minted NAD decides it.
      # (Older manifests pinned a subnet with a node label and a matching selector;
      #  that pairing is gone and the selector would now exclude nothing.)
      domain:
        cpu:
          cores: 4
        # Required. KubeVirt's webhook rejects a VMI with no memory declared.
        resources:
          requests:
            memory: 8Gi
        devices:
          disks:
            - disk: { bus: virtio }
              name: rootdisk
            - disk: { bus: virtio }
              name: cloudinitdisk
          interfaces:
            - name: underlay
              bridge: {}
      networks:
        - name: underlay
          multus:
            networkName: default/myvm-static
      volumes:
        - name: rootdisk
          dataVolume:
            name: myvm-rootdisk
        - name: cloudinitdisk
          cloudInitNoCloud:
            userData: |
              #cloud-config
              users:
                - name: ubuntu
                  sudo: ALL=(ALL) NOPASSWD:ALL
                  shell: /bin/bash
                  lock_passwd: false
                  passwd: "<crypt-hash>"      # openssl passwd -6 '<pw>'
              ssh_pwauth: true
              chpasswd:
                expire: false
            # The NAD's DNS block does NOT reach the guest. The interface is on DHCP,
            # which KubeVirt answers on the virt-launcher side, but nameservers have
            # to be declared here or the VM will have no DNS.
            networkData: |
              version: 2
              ethernets:
                all-en:
                  match: { name: "en*" }
                  dhcp4: true
                  nameservers:
                    addresses: [223.5.5.5, 8.8.8.8]
```

Three of those fields are required by KubeVirt rather than by this project, and each fails
loudly if missing: `domain.resources.requests.memory`, the cloud-init network data (for DNS), and
a root disk.

### 4. Check it

```bash
kubectl get vm myvm -n default            # Running
kubectl get iprequests.ipam.cubestack.io -n default <vm-name>-ip
```

```
NAME       POOL          ASSIGNED       NAD            PHASE   AGE
myvm-ip    dev-ip-pool   10.66.3.20x    myvm-static    Bound   30s
```

The address is whichever is lowest-free in the pool's band (`.200–.230` here), so read it off the
claim rather than expecting a particular one — see [Limits](#limits).

`Bound` with an address is success. The address is also on the VMI:

```bash
kubectl get vmi myvm -n default -o jsonpath='{.status.interfaces[*].ipAddress}{"\n"}'
```

The claim is named `<vm-name>-ip` and lives in the VM's namespace. You did not create it — the
controller did, and it owns it.

---

## Reserving a block of addresses

Everything above gives a VM **one** address. If what you need is a **run of contiguous addresses**
— to configure some other system's own IPAM with, say — use `IPRangeRequest` instead. It reserves N
consecutive addresses out of an existing pool and reports them; it creates no network and touches no
guest.

```yaml
apiVersion: ipam.cubestack.io/v1alpha1
kind: IPRangeRequest
metadata:
  name: blk-myapp
  namespace: default
spec:
  poolRef: dev-ip-pool     # the same pools a VM claims from; cluster-scoped, so a bare name
  allocation:
    strategy: Auto
    count: 8
```

```bash
kubectl apply -f blk-myapp.yaml
kubectl get iprr -n default <name>        # `iprr` is the short name
```

```
NAME        POOL          COUNT   START         END           PHASE   AGE
blk-myapp   dev-ip-pool   8       10.66.3.200   10.66.3.207   Bound   12s
```

`Bound` with a start and end is the whole answer — `START`..`END` inclusive, `count` addresses.

**The pool's allocator is the only one involved.** The block is taken through the same ledger a
VM's claim uses, so it cannot overlap a VM's address or another block. With `strategy: Auto` you do
not pick the range: the controller takes the **lowest free run** that fits, so a new request lands
at the bottom of what is left, and it will skip a free address that is too short to be part of a run
your size — that is `RangeFragmented` if it happens, which is not the same problem as the pool being
full (`RangeExhausted`).

If you need a **specific** band instead, name it:

```yaml
spec:
  poolRef: dev-ip-pool
  allocation:
    strategy: Exact
    range:
      start: 10.66.3.208
      end: 10.66.3.215
```

`Exact` is a demand, not a hint. If any address in the band is outside the pool, inside one of the
pool's `allocation.exclude` reservations, or already held, the request fails and **is not moved to
the nearest free run**. The condition's reason says which of those happened, and `status.blockers`
names the claims and blocks in the way:

```bash
kubectl get iprr -n default blk-myapp -o jsonpath='{.status.blockers}'
```

A pool can refuse `Exact` outright with `allocation.allowExact: false`, in which case the reason is
`ExactNotAllowed`.

Two things it is not:

- **Not a new pool.** If you want an admin-managed band of your own, that is an `IPPool` and it is
  admin scope — see [For admins](#for-admins). This kind only reserves addresses out of a band that
  already exists.
- **Not self-consuming.** Nothing configures whatever you hand these addresses to, and nothing
  reconfigures it when the request changes.

> ### Deleting it releases the addresses immediately
>
> There is no finalizer holding the block, and no downstream change. If whatever you configured
> with those addresses is still using them, it keeps using them — and the allocator will happily
> give them to the next VM. On a `static` pool nothing refuses the duplicate: you get two hosts on
> one address, which shows up as intermittent connectivity rather than an error.
>
> **Delete a range request only after its consumer has stopped using the addresses.** The delete
> emits a `RangeReleased` warning naming the pool and the range, and the audit sweep reports a
> later holder landing inside a block — but neither is a guard, only a report.

`poolRef` and the whole `allocation` block are **immutable**. Changing either would move addresses
out from under whatever was configured with them, so a different block means deleting this one and
creating a new one — which is also why the delete warning above matters.

**Permission.** Your cluster may not grant you write access to `IPRangeRequest` at all. The chart
ships a read-only role for it and deliberately no editor: reserving a block is a grant worth making
per team, against a named pool, rather than one binding away. If `kubectl apply` is refused, ask
your admin for:

```yaml
apiGroups: [ipam.cubestack.io]
resources: [iprangerequests]
verbs: [create, get, list, watch]
```

---

## For admins

### Create a pool

```bash
kubectl apply -f config/samples/ipam_v1alpha1_ippool.yaml
```

The minimum shape, with no `nadTemplate`, allocates addresses and mints nothing:

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

A pool that mints networks adds a `nadTemplate`, and then `subnet`, `gateway` and `bridge` are all
required — the template is all-or-nothing, so no cross-field rule is needed to say so.

| Field | Is |
|---|---|
| `spec.range` | The allocatable band. The **only** required field |
| `nadTemplate.subnet` | The CIDR minted addresses belong to. Supplies the prefix length and the containment check |
| `nadTemplate.gateway` | Must be inside `subnet` and outside `range` |
| `nadTemplate.ipam` | `whereabouts` (default) or `static` — see below |
| `nadTemplate.bridge`, `.type`, `.vlan`, `.macspoofchk`, `.dns`, `.routes` | Passed straight into every minted NAD |

`range` must lie inside `subnet`, and `range.start <= range.end`. The first is checked at
admission; the ordering and gateway rules are checked by the controller (Kubernetes CEL has no
ordering overload on IP values), so **a pool that violates them is accepted by the API server** and
the only place you see it is the condition:

```bash
kubectl get ippools.ipam.cubestack.io <pool> -o jsonpath='{.status.conditions[*].message}{"\n"}'
```

### Choose the addressing mode

This is the one real decision, and it is per pool. The two modes cannot be combined.

| | `whereabouts` (default) | `static` |
|---|---|---|
| Address lives in | the Whereabouts ledger, leased at CNI `ADD` | the NAD itself, written in |
| A second holder is | **refused**, and its VM does not start | **allowed**, silently |
| Live migration | **impossible** | works, guest keeps its address |
| Exclusivity rests on | the CNI | the allocator, the band split, the audit sweep |

**Choose `whereabouts`** when a collision must never pass unnoticed and VMs can tolerate a
stop/start to move. Its duplicates are loud: the second `ADD` is refused and the VM fails to start.

**Choose `static`** when VMs must migrate — a node drain or a patch should not need an outage. Its
duplicates are silent, which moves a burden onto you:

> **A `static` pool's band must be disjoint from every Whereabouts range on the subnet.** The
> ledger cannot see a static address, so it will hand a dynamic pod an address your claim already
> holds, and the first symptom is a broken flow rather than a failed `ADD`. This is load-bearing,
> not hygiene.

**On this cluster there is currently no dynamic window to avoid.** The only Whereabouts NAD on
`10.66.3.0/24` was the shared `vm-underlay-10-66-3-0` (`.200–.220`), and it was **deleted on
2026-09-29** once nothing referenced it. `dev-ip-pool` (`.200–.230`, `static`) is now the only thing
allocating on the subnet, so the disjointness risk is nil today. It returns the moment a second pool
is added or a Whereabouts NAD is reintroduced — so re-check before either:

```bash
# every NAD on the subnet and its IPAM config
kubectl get network-attachment-definitions.k8s.cni.cncf.io -n default \
  -o jsonpath='{range .items[*]}{.metadata.name}{"  "}{.spec.config}{"\n"}{end}'
```

Also check for a MetalLB pool on the same subnet if it was installed after this was written —
it was not installed on 2026-09-28.

### Changing a pool's mode later

```bash
kubectl patch ippools.ipam.cubestack.io <pool> --type=merge \
  -p '{"spec":{"nadTemplate":{"ipam":"static"}}}'
```

Every claim's NAD is re-rendered on the spot. A running VM is not disturbed — its pod was
configured at creation, so the new mode takes effect at the next pod creation. This is a real
trade, not a toggle: you gain migration and give up CNI-refused duplicates.

---

## Day-2

### Where is my address?

```bash
kubectl get iprequests.ipam.cubestack.io -n <namespace>          # all claims in a namespace
kubectl get iprequests.ipam.cubestack.io -A                       # every claim in the cluster
```

The `ASSIGNED` column is the address. `PHASE` is `Pending`, `Bound` or `Failed`.

Why a claim failed is in its condition, not in a log:

```bash
kubectl get iprequest -n <ns> <vm>-ip -o jsonpath='{.status.conditions[*].reason}{"  "}{.status.conditions[*].message}{"\n"}'
```

### What is free?

There is no tooling for this yet. List everything held in the pool and read the band:

```bash
kubectl get ippools.ipam.cubestack.io <pool>
kubectl get iprequests.ipam.cubestack.io -A | grep <pool>          # single addresses
kubectl get iprangerequests.ipam.cubestack.io -A | grep <pool>     # reserved blocks
```

**Both lists matter.** A block holds every address from its `START` to its `END`, and the allocator
will not place a claim inside one — so an address with no claim on it is not necessarily free.

With `strategy: Auto` the allocator takes the lowest free address (or the lowest free run, for a
block), so a fresh claim reliably lands at the bottom of what is left. With `strategy: Exact` you
name the address or band and the controller either binds it or says what is in the way — creating
the claim *is* the allocation either way; there is no reservation step to check first.

### Move a VM to a different pool or NAD name

**Editing the annotation on a VM that already has a claim does nothing.** The VM controller is
create-only: it will not patch the VM, and it will not patch a claim that exists. It emits an
event saying the change was ignored.

The escape hatch is to delete the claim and let it be recreated from the VM's current annotations:

```bash
kubectl -n <ns> delete iprequest <vm>-ip
```

This also deletes the NAD the claim owns. If the new NAD name differs, update the VM's
`networkName` to match **at the same time**, or the VM will be unable to start. If the recreated
claim comes back `Failed` with `NADNotOwned`, the old NAD had not been collected yet — see
[Troubleshooting](#troubleshooting).

### Live migrate

```bash
virtctl migrate <vm> -n <namespace>
```

`virtctl` is not installed in this environment — it ships with the KubeVirt release matching your
cluster (v1.8.4).

Requires a pool whose `ipam` is `static` — see [Choose the addressing mode](#choose-the-addressing-mode).
On a `whereabouts` pool the `VirtualMachineInstanceMigration` **hangs in `Scheduling` forever**
with no timeout:

```
Could not allocate IP in range: ip: 10.66.3.221 / - 10.66.3.221
```

If that happens, delete the migration object; there is nothing to wait for.

On a `whereabouts` pool, protect against it happening by accident. If the cluster-wide KubeVirt
config sets `evictionStrategy: LiveMigrate`, a **node drain will attempt that migration**, so such
VMs should opt out:

```yaml
spec:
  template:
    spec:
      evictionStrategy: None
```

Note the path: `spec.template.spec.evictionStrategy`, **not** `spec.evictionStrategy`. KubeVirt's
CRD is structural, so a wrong field name is silently pruned by the apiserver — no error, no
eviction strategy. Confirm it landed:

```bash
kubectl get vm <vm> -n <ns> -o jsonpath='{.spec.template.spec.evictionStrategy}{"\n"}'
```

### Release an address

The normal way is to **delete the VM**. The chain is `VM → IPRequest → NAD`, all by
`ownerReference`, so deleting the VM releases the address and the network by garbage collection.
No finalizers are involved.

To release an address while keeping the VM, you have to stop referencing the NAD as well:
remove the `ipam.cubestack.io/pool` annotation, drop the network from the VM's `networks` list,
then delete the `IPRequest`. Removing the annotation alone does nothing to an existing claim, and
deleting the NAD alone does nothing durable — the controller owns it and re-mints it.

---

## Troubleshooting

Match the symptom you actually see.

| Symptom | Cause | Fix |
|---|---|---|
| VM in `FailedCreatePodSandBox` for a few seconds at first start | The NAD is created after the VM, always | Wait. It is self-healing |
| Still there after a minute | The claim failed, so no NAD was minted | See the next four rows |
| Claim `Failed`, reason `PoolNotFound` | Pool name wrong, or you gave a namespace. `IPPool` is cluster-scoped | `kubectl get ippools.ipam.cubestack.io` (full resource name) |
| Claim `Failed`, reason `TemplateAbsent` | `spec.nad` is set but the pool has no `nadTemplate` | Use a pool that mints networks, or drop the NAD |
| Claim `Failed`, reason `NADNotOwned` (`NADNameTaken`) | An object of that name exists and this claim did not create it | Change `ipam.cubestack.io/nad-name`, or delete the other object |
| Claim `Failed`, reason `RangeExhausted` | The band is full | Admin widens `spec.range`, or delete an unused claim |
| Claim `Failed`, reason `RangeInvalid` | The *pool* is unusable — bad ordering or gateway | Fix the pool; check its `Available` condition |
| **Block** `Failed`, reason `RangeExhausted` | No free address at all left in the band | Admin widens `spec.range`, or release something |
| **Block** `Failed`, reason `RangeFragmented` | Addresses are free, but none form a run of `count` together | Read the message — it names the longest run available. Request a smaller block, or free up a neighbouring address |
| **Block** `Failed`, reason `PoolNotFound` / `RangeInvalid` | Same as for a claim — check the pool first | See the two claim rows above |
| **Block** keeps a range outside the pool's band | The pool was narrowed under it. Deliberate: it is never moved silently | Confirm whatever uses the addresses is finished with them, then delete the request and re-create it |
| Claim or block `Failed`, reason `AddressOutOfRange` / `RangeOutOfRange` | Your `Exact` address or band is not inside the pool's `spec.range` | Pick an address the pool actually covers, or ask an admin to widen the band |
| Claim or block `Failed`, reason `AddressExcluded` / `RangeExcluded` | The address or band is inside a reservation on the pool (`spec.allocation.exclude`) | Pick another one. The conditions name the band that is off-limits |
| Claim or block `Failed`, reason `AddressUnavailable` / `RangeUnavailable` | Something already holds it. `status.blockers` names each holder and its address | Delete a stale holder, or ask for a different address. This is not exhaustion — other addresses may be free |
| Claim or block `Failed`, reason `ExactNotAllowed` | The pool sets `allocation.allowExact: false` | Drop `strategy: Exact` (or the `address` annotation), or use another pool |
| Claim or block `Failed`, reason `AllocationInvalid` | The spec and the bound status disagree, or a required field is missing | Do not hand-edit `status`. A block whose `allocation` was written before 0.5.0 and never bound has to be re-created |
| A VM and something else answer on one address, intermittently, and one side is a block | `AddressInBlock` — the CNI does not know the block exists, so nothing refused it | Admin: check whether the block was deleted while its consumer still used it. The warning names both holders |
| Pool has no `Available=True` | Bad `range`/`gateway`/`subnet` combination | Read the condition message |
| VM starts but has no address | `networkName` does not match the minted NAD | Must be `namespace/name`, matching the annotation |
| VM has an address but no DNS | Expected: the NAD's `dns` block does not reach the guest | Set nameservers in cloud-init `networkData` |
| VM fails to start with a resource error | Minted NADs carry **no annotations** | See the note below |
| Migration hangs in `Scheduling` | The pool is `whereabouts` — its ledger cannot give one address to two pods | Use a `static` pool. Delete the stuck migration |
| Two VMs answer on one address, intermittently | `DuplicateAddress` — on a `static` pool nothing refuses it | Admin: check the pool's band against every other allocator on the subnet. The warning event names both claims |
| Editing the pool annotation did nothing | The VM controller is create-only | Delete the `<vm>-ip` `IPRequest` and let it be recreated |
| Recreated claim is `Failed` with `NADNotOwned` | The previous NAD had not been garbage-collected yet, so the name was still held by the old claim | Delete the leftover NAD — the claim is retried and succeeds |
| Deleting or hand-editing the NAD did not stick | The controller owns it and re-mints it | Nothing to fix — it is working as intended |

**On the resource error.** The hand-written `vm-underlay-10-66-3-0` carried the annotation
`k8s.v1.cni.cncf.io/resourceName: bridge.network.kubevirt.io/br0` (that NAD is deleted now, so you
cannot compare against it directly). A NAD minted here carries no
annotations at all, and the live VM starts without it on this cluster (measured 2026-09-28). So do
not add it speculatively — but if a VM ever fails with a resource error, that is the first
difference to check against a VM that works.

For anything that is not explained by a claim's condition, check for audit findings, which are
emitted as `Warning` events every 10 minutes:

```bash
kubectl get events -A --field-selector reason=DuplicateAddress
kubectl get events -A --field-selector reason=OrphanedNAD
kubectl get events -A --field-selector reason=PoolDeleted
kubectl get events -A --field-selector reason=AddressInBlock
kubectl get events -A --field-selector reason=RangeOverlap
```

`RangeReleased` is not a finding — it fires on the delete of a range request, naming the pool and
the range that just became available again. Filter for it after releasing a block to confirm what
you freed.

```bash
kubectl get events -A --field-selector reason=RangeReleased
```

---

## Command cheat sheet

```bash
# Pools (cluster-scoped). Full resource name always.
kubectl get ippools.ipam.cubestack.io
kubectl get ippools.ipam.cubestack.io <pool> -o yaml

# A pool's addressing mode -- not in the default columns
kubectl get ippools.ipam.cubestack.io <pool> -o jsonpath='{.spec.nadTemplate.ipam}{"\n"}'

# Claims
kubectl get iprequests.ipam.cubestack.io -A
kubectl get iprequests.ipam.cubestack.io -n <ns> <vm>-ip -o yaml

# Why a claim failed
kubectl get iprequest -n <ns> <vm>-ip \
  -o jsonpath='{.status.conditions[*].reason}{"  "}{.status.conditions[*].message}{"\n"}'

# The minted network and the address baked into it
kubectl get network-attachment-definitions.k8s.cni.cncf.io -n <ns> <nad> -o jsonpath='{.spec.config}{"\n"}'

# Release and re-claim
kubectl -n <ns> delete iprequest <vm>-ip

# Reserved blocks. Also a ledger: a claim cannot land inside one.
kubectl get iprangerequests.ipam.cubestack.io -A
kubectl get iprr -n <ns> <name> -o jsonpath='{.status.start}-{.status.end}{"\n"}'

# Why a block request failed
kubectl get iprr -n <ns> <name> \
  -o jsonpath='{.status.conditions[*].reason}{"  "}{.status.conditions[*].message}{"\n"}'
```

---

## Limits

- **A requested address or band is never moved.** `strategy: Exact` either binds exactly what you
  named or fails with the reason and the holders in `status.blockers`. It is not a preference the
  allocator may satisfy with something nearby: a typo has to fail, or the request would be
  unverifiable. Exact allocation is immutable once created, and a pool can refuse it with
  `allocation.allowExact: false`.
- **An excluded address is not handed out by Auto either.** `IPPool.spec.allocation.exclude`
  reserves bands from every strategy. An exclusion added after an address was already allocated
  does not move the existing holder; delete the claim first to reclaim the address.
- **A block request is a reservation, not a deployment.** Nothing configures its addresses for you,
  and deleting it releases them at once with nothing to stop the allocator reusing them. See
  [the deletion warning](#deleting-it-releases-the-addresses-immediately).
- **Migration is per pool, not per VM.** A VM can only migrate if its pool is `static`.
- **A claim never moves.** It keeps the address already in its status rather than re-deriving it,
  so a claim cannot be displaced by another appearing beneath it. It will also keep an address
  that has drifted out of the pool's band, and report that rather than silently moving your VM.
- **Two claims can collide.** Two created in the same instant can both be reconciled against a
  stale cache. On `static` this is silent and the audit sweep is the only detector — see the
  README's [audit sweep](../README.md#the-audit-sweep) section.
- **The shared `vm-underlay-10-66-3-0` NAD is gone.** It was deleted on 2026-09-29, after all five
  VMs on it had moved to pool claims and both Whereabouts ledgers were confirmed empty. `dev-ip-pool`
  is the only allocator on `10.66.3.0/24` now, and a pool claim is the only way to get an underlay
  address. If you find a manifest still naming the old NAD, it is stale — and it will fail as
  `FailedCreatePodSandBox`, which looks like "the NAD has not been minted yet" rather than "there is
  no such NAD".
- **Not installed / not built:** a "what is free, claim this, release this" CLI; migrating existing
  VMs; any guest-side configuration. This project never touches the guest.
