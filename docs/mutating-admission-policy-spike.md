# Can a MutatingAdmissionPolicy inject the VM's layer 2 network?

**Status:** the experiment has run. §3–§5 come from `envtest` control planes started from
`hack/mapspike/`; §6 is a verification against the live cluster, where MAP has since been enabled by
its owner, using a temporary namespace that has been removed again. No cluster configuration was
changed in producing any of this, and no address was allocated.
**Date:** 2026-09-28
**Assets measured:** kube-apiserver **v1.35.0** (the 1.35 line's only published envtest asset),
v1.37.0, and the live cluster's **v1.35.4** for §6.
**Related:** `kubevirt-vm-static-ip-design.md` §5 (the pool design this would feed), `user-guide.md`.

---

## TL;DR

**Yes on 1.37, where MAP is GA. On 1.35 it can be enabled, but not with one flag and not with the
obvious expression.**

Two independent obstacles sit between "turn on the feature gate" and "a pool member gets its own
`<vm>-static` NAD":

1. **The gate alone bricks the apiserver on 1.35.** `--feature-gates=MutatingAdmissionPolicy=true`
   starts the MAP admission plugin, which then lists its own objects through hard-wired
   `admissionregistration.k8s.io/v1beta1` informers — a group version 1.35 does not serve by default.
   The informers 404, the plugin never reports ready, and from then on it answers **every** API
   request with `not yet ready to handle request`. That includes the requests the apiserver itself
   makes while bootstrapping, so `kube-system` is never created, the
   `bootstrap-system-priority-classes` PostStartHook fails, and the process exits. Measured: ready for
   ~10 seconds, then dead. **A second flag is required:
   `--runtime-config=admissionregistration.k8s.io/v1beta1=true`.**
2. **The natural way to spell the injection panics the apiserver on 1.35 and 1.36.** Creating the
   `networks` array as `value: [Object.spec...networks{...}]` — a list literal holding an object
   initializer — fails inside cel-go with `reflect.Set: value of type *structpb.Struct is not
   assignable to type *structpb.Value`. The request is aborted as `INTERNAL_ERROR`. Creating the
   array empty and appending the element as a *bare* value works on both releases.

With both obstacles handled, **the same policy expression passes every test on 1.35 (`v1beta1`) and
on 1.37 (`v1`)** — identical results in each run. The mechanism is viable on the cluster as it stands
today; §7 is what adoption would cost.

**And it has now been shown to do the thing it was wanted for.** On the live 1.35.4 cluster, with MAP
enabled, a `VirtualMachinePool` whose template declared **no networks at all** produced members that
each stored **their own** reference — `mapspike-pool-0` → `default/mapspike-pool-0-static`,
`mapspike-pool-1` → `default/mapspike-pool-1-static`, with distinct MACs and the same revision label —
and members created later, members rewritten by a template revision bump, and members whose network
was stripped by hand all ended up correct. §6 has the measurements; §8 has what that still leaves
open. The policy now ships in the chart, on by default and scoped by namespace — §7.1 — and chart
0.3.0 is deployed on the cluster, so it is live over `default`.

---

## 1. The question, and why it is worth this much

A `VirtualMachinePool` member takes its spec from `spec.virtualMachineTemplate`, and
`multus.networkName` there is **one literal string handed to every member**. Each member needs its
own NAD (`<vm>-static`), so at `replicas: 1` this works only because a human hard-codes the single
member's name — which is what `clq-cubestack-node` does today — and at `replicas: 2` member `-1`
points at member `0`'s NAD. On a `static` pool nothing refuses that duplicate, so both members come
up on one address and the symptom is intermittent connectivity. That is the exact failure `static`
cannot defend against, one character away on a pool that is live right now.

Injection would close it *before the VM is stored*: no window in which a member carries an
unexpanded value, no controller patching VMs after creation, no extra webhook to deploy. The
alternative (a `${vm-name}` token expanded by the IPAM controller) needs the controller to write
VMs and opens a window in which a member holds a literal token. So the question is whether MAP can
do it, on the cluster that exists, rather than on the cluster we would like to have.

## 2. Availability, per release

From the upstream gate tables (`k8s.io/apiserver`, `pkg/features/kube_features.go`):

| Release | `MutatingAdmissionPolicy` | API version |
|---|---|---|
| 1.32 | alpha, off by default | `v1alpha1` |
| 1.34 / **1.35** / 1.36 | beta, **off by default** at 1.34 and 1.35; **on by default (GA) at 1.36** | `v1beta1` |
| 1.37 | GA, gate still present in the table | `v1` |

The cluster's apiservers carry no `--feature-gates` flag at all, so MAP is not *disabled* there — it
is off by default, which is why `kubectl get mutatingadmissionpolicy` reports that the server has no
such resource type. Confirmed against a real 1.35.0 apiserver with the gate left at its default:
`admissionregistration.k8s.io` serves no `mutatingadmissionpolicies`/`mutatingadmissionpolicybindings`,
and every policy creation fails with `no matches for kind`.

Only **one** feature gate is involved; there is no separate gate for the API or for bindings.

## 3. Enabling it on 1.35 — two flags, not one

This is the finding that most changes the cost of the 1.35 route, and it was measured on the
apiserver binary directly rather than inferred from documentation.

With `--feature-gates=MutatingAdmissionPolicy=true` and nothing else, the plugin starts and then
cannot list its own resources:

```
E "Failed to watch" err="failed to list *v1beta1.MutatingAdmissionPolicy: the server could not find the requested resource"
E "Failed to watch" err="failed to list *v1beta1.MutatingAdmissionPolicyBinding: the server could not find the requested resource"
```

The reason is structural, and visible in the library that ships with 1.35:
`k8s.io/apiserver@v0.35.4/pkg/admission/plugin/policy/mutating/plugin.go:56` defines
`type Policy = v1beta1.MutatingAdmissionPolicy`, and lines 99–100 ask a client-go informer factory
for `f.Admissionregistration().V1beta1().MutatingAdmissionPolicies()`. That group version is not
served by default in 1.35 — discovery on the bare binary returns `admissionregistration.k8s.io/v1`
and nothing else.

The consequence is not a failure of the policy but of the control plane. The generic policy plugin
gates every request on its sources having synced
(`pkg/admission/plugin/policy/generic/plugin.go:210`):

```go
} else if !c.WaitForReady() {
    return admission.NewForbidden(a, fmt.Errorf("not yet ready to handle request"))
}
```

Bootstrap requests are admission requests. So `namespaces "kube-system" not found`,
`priorityclasses.scheduling.k8s.io "system-node-critical" is forbidden: not yet ready to handle request`,
`F PostStartHook "scheduling/bootstrap-system-priority-classes" failed`, and the process exits.
`envtest` reports only the unhelpful `timeout waiting for process kube-apiserver to start successfully`.

Adding the second flag fixes it completely. Measured with
`--feature-gates=MutatingAdmissionPolicy=true --runtime-config=admissionregistration.k8s.io/v1beta1=true`:

```
/apis/admissionregistration.k8s.io        -> v1, v1beta1
/apis/admissionregistration.k8s.io/v1beta1 -> mutatingadmissionpolicies mutatingadmissionpolicybindings
/readyz                                    -> ok
"Failed to watch ... MutatingAdmissionPolicy" -> 0 occurrences
```

**This is the single most important operational point in this document**: on 1.35 the feature gate
is not a safe flag to set on its own. Half of this pairing takes the control plane down; a cluster
that set the gate and rolled the apiservers one at a time would have a partially unavailable API,
not a missing feature.

## 4. What the injection expression has to look like

### 4.1 `patchType: JSONPatch` is forced

`ApplyConfiguration` documents that it "may not modify atomic structs, maps or arrays". KubeVirt
declares no `x-kubernetes-list-map-keys` on `networks` or `interfaces`, so both arrays are atomic,
and the measured failure is exactly that:

```
error applying patch: invalid ApplyConfiguration: may not mutate atomic arrays, maps or structs: .spec.template.spec.networks
```

### 4.2 The value cannot be written as JSON

`value: {"name": "underlay", "multus": {"networkName": "..."}}` looks like ordinary JSON and is
rejected at policy compile time: Kubernetes CEL requires map literals to be homogeneous and infers
the value type from the first entry. The injected object has to be written as a **typed constructor**
whose type names follow a path convention — `Object.spec.template.spec.networks{...}` — and the
apiserver does not check those names against the CRD schema, so a typo becomes a runtime error at
some later admission rather than a compile error.

### 4.3 A list of those constructors panics the apiserver (1.35 and 1.36 only)

The natural create-the-array branch,

```
JSONPatch{op: "add", path: "/spec/template/spec/networks",
          value: [Object.spec.template.spec.networks{name: "underlay", multus: ...}]}
```

does not fail the request. It takes the request handler down:

```
E "Observed a panic" panic=<
    reflect.Set: value of type *structpb.Struct is not assignable to type *structpb.Value
    github.com/google/cel-go/common/types.(*baseList).ConvertToNative   list.go:206
    .../mutating/patch/json_patch.go:177  (*jsonPatcher).evaluatePatchExpression
E "apiserver panic'd" method="POST" URI="/apis/kubevirt.io/v1/namespaces/spike-ok/virtualmachines"
http2: panic serving 127.0.0.1:52562: reflect.Set: value of type *structpb.Struct ...
```

`json_patch.go:177` is `op.Val.ConvertToNative(reflect.TypeOf(&structpb.Value{}))`, and that line is
**byte-identical in apiserver 0.35.4 and 0.37.0**. What differs is cel-go: **0.26.0** is pinned by
1.35.x *and* 1.36.x, while 1.37.0 pins 0.29.2. The value adapter differs too
(`pkg/cel/mutation/dynamic/objects.go`: 0.37.0 handles `*structpb.Struct` explicitly and wraps it via
`structpb.NewValue`, 0.35.4 only matches an exact type). A list whose elements are object
initializers converts to a native list of `*structpb.Struct`, and the outer conversion to
`*structpb.Value` then fails a `reflect.Set`. A **bare** object value never goes through that path,
which is why the append branch works on 1.35 and the create branch did not.

Because 1.35.4 pins the same cel-go 0.26.0, **this is not a 1.35.0-vs-1.35.4 issue** — the live
cluster would behave the same way. Six spellings were measured:

| create-branch spelling | 1.35.0 | 1.37.0 |
|---|---|---|
| `value: [Object...{...}]` (list of initializers) | **PANIC** | ok |
| `value: []` (empty list) | ok | ok |
| `value: []` then append a typed initializer at `/-` | ok | ok |
| `value: []` then append a map literal at `/-` | ok | ok |
| `value: [{"name": "underlay"}]` (list of plain maps) | ok | ok |
| append at `/-` only, array already present | ok | ok |

So the working shape is **create the array empty, then append the element as a bare value** — two
patches instead of one, at the cost of asking `has()` twice. The panic is specific to object
initializers inside a list; plain map literals inside a list are fine.

### 4.4 The guard must name *this policy's* reference, not "any multus network"

`matchConditions` carry a guard, and its role turns out to be larger than the design assumed. Counting
how many times a policy's mutations are evaluated for a **single** VM create:

| policy | mutation runs per one create | stored result |
|---|---|---|
| with the guard | **1** | one network, one interface |
| with the guard, the VM already names our NAD | 0 | untouched |
| **without the guard** | **2** | **two networks and two interfaces, same name** |

MAP evaluates the policy again after its own mutation changed the object; with the guard, the second
evaluation no longer matches and does nothing. Without it, an append is applied twice. On the real
cluster the doubled shape would be refused by KubeVirt's validation (duplicate network and interface
names), so the failure is loud rather than silent — but it is a failure, and the guard is what
prevents it.

**What the guard has to test is subtler than it looks, and getting it wrong costs a real VM its
network.** The first version of this guard asked whether the VM had *any* multus network:

```
!(hasNetworks && object.spec.template.spec.networks.exists(n, has(n.multus)))
```

For exactly-once that is sufficient — after injection the object has a multus network, so the second
evaluation skips. But it answers the wrong question. A VM that passes a RoCE or SR-IOV VF through on
its own NAD *already has a multus network* and still wants the L2 underlay added; the broad test skips
it silently, the VM comes up without the network its pool claim is holding an address for, and nothing
reports the discrepancy. The guard's subject has to be the reference this policy would write:

```
!(hasNetworks && object.spec.template.spec.networks.exists(n,
    has(n.multus) && has(n.multus.networkName) && n.multus.networkName == <the derived ref>))
```

This is also why the derived name is a single constant shared by the guard and the mutation
(`nadNameExpr`/`underlayRef` in the harness): if the two spelled it separately, the policy could inject
a reference it then failed to recognise as its own and append a second copy.

Measured both ways, on 1.35.0 and 1.37.0:

| VM already contains | runs | stored networks |
|---|---|---|
| nothing | 1 | `[underlay]` |
| the pod network | 1 | `[pod, underlay]` |
| an unrelated multus network (RoCE) | 1 | `[roce-net, underlay]` |
| our own reference | 0 | unchanged |

The guard also carries the `ipam.cubestack.io/nad-name` override, so a VM that renames its NAD gets a
reference to *that* name — matching `virtualmachine_controller.go`, which reads the annotation before
falling back to `DefaultNADName`. Without it the policy would inject `default/<vm>-static` while the
controller minted the override, and the VM would wait forever for a NAD that is never created. An
empty annotation falls back, mirroring that file's `if nadName == ""`.

## 5. The measured behaviour

All on 1.35.0 as `admissionregistration.k8s.io/v1beta1` **and** on 1.37.0 as
`admissionregistration.k8s.io/v1`.

| Claim | Result |
|---|---|
| MAP fires on a `kubevirt.io/v1` CR | yes — the VM comes back with the injected entry |
| An absent array is created, then appended | yes (see §4.3 for the only safe spelling) |
| An existing unrelated network survives | yes |
| Idempotent across updates | yes, with the guard |
| A VM already naming our NAD is left alone | yes |
| A VM carrying an unrelated multus NIC still gets the underlay | yes — `[roce-net, pod-net, underlay]`, the VM's own NIC untouched (§4.4) |
| A VM without the pool annotation is untouched | yes |
| `ipam.cubestack.io/nad-name` is honoured | yes, including the empty-string fallback (§4.4) |
| `failurePolicy: Fail` blast radius | an expression error **denies the write** with the policy's message, e.g. `add operation does not apply: doc is missing path: "/spec/template/spec/networks/-": missing value` |
| `ApplyConfiguration` cannot do it | confirmed negative control (see §4.1) |
| `generateName` | a VM in scope **cannot be created** with `generateName` at all: `object.metadata.name` is absent at admission, the guard errors, and under `failurePolicy: Fail` the create is rejected. Gate the whole mutation on `has(object.metadata.name)` if other creators use `generateName`. The pool path is unaffected — members are created with real names. |
| Order vs a mutating webhook | see below |

**Ordering.** A stand-in mutating webhook, registered to run on the same VM creates, was made to
record what it received and then to modify the object two ways.

* *Additive* (what `kubemacpool` actually does — it writes `interfaces[].macAddress`): the webhook
  saw the injected network and interface, and the final object kept both. **FULL.**
* *Destructive* (replaces the whole `interfaces` array): the webhook saw the injected network and
  interface, but the final object ended with the multus network and **no matching interface** —
  `interfaces=[business-net]` against `networks=[underlay]`. **PARTIAL.** Whether KubeVirt's
  validating webhook rejects that shape is **not settled here**; this harness has no KubeVirt.

The API is explicit that `reinvocationPolicy: IfNeeded` carries "no guarantee of order" with respect
to webhooks and other policies. That is a property of the mutating phase, not of MAP: a webhook of
our own would be exposed identically, and `kubemacpool-mutator` and `virt-api-mutator` already
coexist on these objects. The measured order is the reassurance; the non-guarantee is the residual
risk.

## 6. Verified on the live cluster (Kubernetes 1.35.4, MAP enabled)

§3–§5 were established on envtest control planes, which have no KubeVirt: no validating webhook, no
`virt-api-mutator`, no `kubemacpool`, no `VirtualMachinePool` controller. The cluster now runs with
MAP enabled — both flags of §3, so the API is served as `admissionregistration.k8s.io/v1beta1` — and
the same policy was applied there with only the `apiVersion` line changed. Everything below is a
read-back from `kubectl` in a throwaway namespace, `mapspike-live`, whose VMs are all
`runStrategy: Halted` and carry **no** `ipam.cubestack.io/pool` annotation: no address was allocated
and no NAD was minted at any point.

**Blast radius first.** The binding selects namespaces by label
(`namespaceSelector.matchLabels: {mapspike: live}`), and `kubectl get ns -l mapspike=live` returns
exactly one namespace. A `failurePolicy: Fail` policy is only as safe as its binding is narrow, and
this one is one namespace wide.

### 6.1 Standalone VMs

| Object | Before | Stored after |
|---|---|---|
| `mapspike-a` | no `networks` at all | `[{name: underlay, multus: {networkName: default/mapspike-a-static}}]`, with `interfaces: [{name: underlay, bridge: {}, macAddress: 02:17:8d:c8:12:d7}]` |
| `mapspike-b` | pod network only | `[default/pod, default/mapspike-b-static]` — the existing entry preserved, ours appended |
| `mapspike-c` | an existing multus network | unchanged |
| `mapspike-a` after `kubectl label vm mapspike-a touched=yes` | one network | still exactly one network, one interface |

Two caveats on this table, both about the policy that produced it rather than the results:

- It was run with the **§4.4 guard's earlier, broader form** ("no multus network at all"), and with a
  binding that matched the namespace only — no annotation condition. `mapspike-c` is therefore
  untouched for the broad guard's reason, and `mapspike-a`/`-b` were injected *without carrying the
  pool annotation*, which the corrected policy would not do. Every stored outcome above is identical
  under the corrected policy (all three VMs declare no NAD reference, so the reference-precise guard
  also passes for `-a`/`-b`, and `-c` is now excluded by the annotation condition instead). The
  corrected policy has been verified on envtest (§4.4) but **not re-run on the cluster**.
- `mapspike-a` and `-b` had no `ipam.cubestack.io/pool` annotation, so no claim was created and no NAD
  was ever minted: their stored references point at NADs that do not exist. They were `runStrategy:
  Halted`, so nothing tried to attach. That is the failure mode §4.4's guard correction and the
  annotation condition exist to prevent — a VM injected with a reference nothing will ever satisfy.
- **The namespace in every reference below is a literal, and that is the defect, not the feature.**
  The policy run here was written `"default/" + object.metadata.name + "-static"` — it did not read
  `object.metadata.namespace` — while the VMs were in `mapspike-live`. So §6.1's and §6.2's stored
  values are exactly what §4.4's namespace qualification was fixed to stop producing: references to
  NADs that would never be minted for a VM outside `default`. The tables are left as measured, but
  read `default/` in them as "the literal the old expression wrote", not as the correct answer for
  those objects. This went unnoticed live because the VMs were `Halted` and `default` is where every
  other VM on the cluster lives — which is the same reason the literal survived as long as it did.

`mapspike-a` is the row that answers §5's open question. The real KubeVirt CRD **accepted** the
injected shape, the real `virt-api-mutator` did not reject it, and the real `kubemacpool` assigned
the injected interface a MAC (its `kubemacpool.io/transaction-timestamp` annotation is on the
object). The stand-in webhook is no longer the only evidence that KubeVirt tolerates this.

### 6.2 Pool members — the actual requirement

This is the case the harness could not reach and the reason the investigation exists. A
`VirtualMachinePool` was created in the same namespace at `replicas: 2`, `runStrategy: Halted`, with
a containerDisk volume and — deliberately — a template that declares **no `networks` and no
`interfaces` at all**. If MAP does not fire for pool members, the members come up with no network.
It fired:

| Member | Stored `multus.networkName` | Stored MAC |
|---|---|---|
| `mapspike-pool-0` | `default/mapspike-pool-0-static` | `02:17:8d:c8:12:dc` |
| `mapspike-pool-1` | `default/mapspike-pool-1-static` | `02:17:8d:c8:12:db` |

Two things make this more than "it fired twice". First, **both members carry the same revision
label** `kubevirt.io/vm-pool-revision-name: mapspike-pool-1` — identical inputs from the template,
different stored values, so the divergence provably comes from the object (its own name), not from
anything the template could carry. Second, `kubemacpool` gave them **different** MACs, so there is no
collision on that axis either.

Three follow-on paths were then measured, because injection at create is worth little if the pool
controller's own writes undo it:

1. **Scale-up** — `replicas: 2` → `3`. The new member `mapspike-pool-2` stored
   `default/mapspike-pool-2-static`. Members created long after the policy was written are covered.
2. **Template revision bump** — the pool's template is truth at revision boundaries, so an edit
   rewrites *every* member. Changing the template memory `128Mi` → `160Mi` moved all three members
   from revision `1` to `3` and applied the new memory value; all three **kept their own** NAD
   reference. This was the case most likely to break the scheme, and it does not.
3. **Clobber** — removing `networks` and `interfaces` from a member by hand was **undone** by MAP on
   the resulting UPDATE: the member came back with its own `default/mapspike-pool-2-static` and a
   freshly assigned MAC (`…:dd`, the interface having genuinely been recreated). The injection is
   self-healing on UPDATE, not merely create-once — which is what makes it survive the pool
   controller's rewrite-instead-of-merge behaviour.

### 6.3 The defect this closes, visible in the live pool

The cluster's own pool makes the hole concrete rather than hypothetical. `clq-cubestack-node`'s
`spec.virtualMachineTemplate` still says:

    networks:
    - name: business-net
      multus:
        networkName: vm-underlay-10-66-3-0

— the **shared** NAD, written without a namespace prefix — while its only member,
`clq-cubestack-node-0`, carries `default/clq-cubestack-node-0-static` and the
`ipam.cubestack.io/pool: dev-ip-pool` annotation. The member was corrected by hand after the pool
created it. At `replicas: 1` that hand correction is the entire reason it works. The template still
holds the value that every *future* member inherits verbatim, which is exactly the `replicas: 2`
failure mode: two members on one address, on a `static` pool that refuses nothing.

> **Both halves of this are now history (2026-09-29).** The pool template was corrected — it no longer
> names any network at all, only the `ipam.cubestack.io/pool` annotation, and the admission policy
> injects each member's own `default/<member>-static`. The shared NAD the template pointed at has been
> deleted outright. The paragraph above is kept as the record of the defect the spike was written
> against — which is what the policy exists to prevent, and it is worth re-reading as such.

## 7. What adoption would cost on 1.35

Handed over as instructions rather than performed — nothing below has been run against the cluster.

1. **Add two flags to every apiserver**, not one:
   `--feature-gates=MutatingAdmissionPolicy=true` and
   `--runtime-config=admissionregistration.k8s.io/v1beta1=true`. In the Kubespray tree vendored at
   `cubestack-installer/deployments/kubespray/kubespray` those are `kube_feature_gates` (a list,
   default `[]`) and `kube_api_runtime_config` (a list, comma-joined into the `runtime-config`
   argument by `roles/kubernetes/control-plane/templates/kubeadm-config.v1beta4.yaml.j2:235`, and
   emitted **only if non-empty**). Verify on the first node before continuing:
   `kubectl get mutatingadmissionpolicies.admissionregistration.k8s.io` and
   `kubectl get --raw='/readyz?verbose'` — if the group version is missing, the second variable did
   not apply and the apiservers will fail to bootstrap.
2. **Write the policy as `admissionregistration.k8s.io/v1beta1`.** On 1.35 `v1` does not exist for
   MAP. The field set and the `patchType`/`jsonPatch` JSON tags are identical between `v1` and
   `v1beta1`, so the object transfers with the `apiVersion` line changed.
3. **Reconsider the flags at the next upgrade.** At 1.36 the gate goes GA and default-on, and the
   `v1beta1` API must be checked before assuming it is still served; at 1.37 the gate is still
   listed, so a leftover `=true` does not error there. This is the kind of thing to confirm against
   the target release rather than carry forward.
4. **Do not use the list-of-initializers spelling** anywhere, including on 1.36: apiserver 0.36.3
   pins the same cel-go 0.26.0 that panics.

### 7.1 It now ships in the chart, and it is live

The policy and its binding are in `charts/cubestack-ipam/templates/mutatingadmissionpolicy.yaml`,
gated on `map.enabled` (default `true`) and scoped by `map.namespaces` (default `[default]`, an
empty list refused rather than widened). **Chart 0.3.0 is deployed on the cluster** (release revision
3, 2026-09-28 22:30 +0800), so the policy below is live and covers `default`:

    kubectl get mutatingadmissionpolicy cubestack-ipam-vm-underlay
    # apiVersion: admissionregistration.k8s.io/v1beta1   failurePolicy: Fail
    # reinvocationPolicy: IfNeeded   operations: ["CREATE","UPDATE"]
    # binding matchExpressions: kubernetes.io/metadata.name In [default]

That makes the guard's precision load-bearing in a way it was not before — every VM write in
`default` is now evaluated against it. What that does and does not establish is in §8. What was
checked before the upgrade:

- `helm install --dry-run=server` against the live 1.35.4 cluster: the API server accepts the whole
  release, which means it **compiles the chart's rendered CEL**. It renders `v1beta1` there.
- The rendered guard and mutation are **character-identical, whitespace aside, to the harness
  constants** — the expressions §4.4 and §5 measured, not a retyped copy. The comparison was done
  by extracting both from the rendered manifest and from `hack/mapspike/mapspike_test.go` and
  diffing them normalised; re-run it if either side is edited.
- `make helm-lint` renders every branch and counts the policy: 1.35.4 → `v1beta1`, 1.36.0 → `v1`,
  discovery-reporting-but-gate-off → skipped, `map.enabled=false` → skipped, empty
  `map.namespaces` → refused. The branch that matters is "skipped": a detection bug there is a
  **silent** no-op, which looks exactly like a working policy.

The version detection is worth recording, because the obvious spelling is wrong twice over.
`admissionregistration.k8s.io/v1` has been served since 1.16 and says nothing about whether
`MutatingAdmissionPolicy` is in it, so the test has to name the Kind
(`…/v1beta1/MutatingAdmissionPolicy`) — and **that only works when Helm has contacted the
cluster**. Measured here with Helm 4.3.0:

| How Helm was invoked | `KubeVersion.Version` | API entries | Kind-level entries |
|---|---|---|---|
| `helm install --dry-run=server` (or a real install) | `v1.35.4` (real) | 259 | yes |
| `helm template`, `helm install --dry-run=client` | `v1.37.0` (fabricated) | 57 | **no** |

So a Kind-level test alone answers "no" under `helm template`, which is how GitOps renders — it
would quietly emit nothing. The helper therefore branches on the kube version first
(`>=1.36-0` → `v1`, `>=1.32-0` → `v1beta1`) and uses discovery, **when discovery has run**, only as
a veto. `apps/v1/Deployment` is the canary for which world we are in. Offline, pass
`--kube-version` to get the right branch; a wrong guess there fails loudly at apply with
`no matches for kind`, which names the problem.

Two notes on the 0.3.0 release itself, since they are easy to mistake for something else:

- **The manager binary did not change.** 0.3.0 is a chart-only release: the rebuilt image has the
  same image ID and the same `/manager` sha256 as 0.2.0. The tag exists so the release's image and
  `appVersion` agree.
- **The CRD shortnames (`ipp`, `ipr`) were applied with `kubectl apply -f charts/cubestack-ipam/crds/`
  rather than by Helm.** Helm installs `crds/` on install and never upgrades them, so a CRD change
  is always a separate apply — same trap the chart's README describes for `nadTemplate.ipam`.

## 8. What this record does not establish

- **The RPC/`ApplyConfiguration` rows beyond what §5 measured**, and the ordering guarantee itself.
  §6.1 settles that KubeVirt's webhooks tolerate the injected shape in the additive direction, but
  the API's refusal to promise an order relative to other mutating webhooks (§5) is unchanged by
  anything measured live — the live cluster's mutating webhooks happen to run additively, and a
  future webhook that rewrote `interfaces[]` wholesale would still be the risk.
- **1.36.** No assets were fetched. The gate table says the API is GA and on by default there; the
  cel-go pin says the panic shape is still unsafe there.
- **The IPAM controller.** §6 ran with no `ipam.cubestack.io/pool` annotation anywhere, deliberately,
  so nothing here exercises `cubestack-ipam` minting a NAD for a MAP-injected reference. The two
  mechanisms are still only observed separately: MAP writes a name; the controller must mint the NAD
  that name refers to. That join is the next thing to test, and it consumes real addresses.
- **The corrected policy, on the cluster — half closed.** §4.4's reference-precise guard and the
  `nad-name` override were measured on envtest only, and §6 ran the earlier, broader guard with a
  namespace-only binding. As of 0.3.0 the corrected policy **is live**, scoped to `default`, so its
  expressions are now evaluated on every VM write there — a CEL error would reject the write. What is
  still unmeasured is the **injection** branch on the cluster: every VM in `default` already names its
  own reference, so the guard skips all five and nothing has exercised the append path live. That is
  by construction (§8's last-but-one bullet) and it is also why the risk of the live policy is low —
  but "the branch has not run" is a fact, not an assurance.
- **KubeVirt with two multus NICs — half of it is now answered by the cluster, the other half is
  not.** `default/vm-roce-gpu` is exactly §4.4's shape and is live: annotated with
  `ipam.cubestack.io/pool: dev-ip-pool`, carrying `default/vm-roce-gpu-static` as a bridge `underlay`
  **and** `default/vm-roce-gpu-roce` as `sriov`, with `kubemacpool` MACs on **both** interfaces
  (`…d5` and `…d6`). So kubemacpool does not leave an SR-IOV interface alone, and the broad guard
  would have skipped this VM — which is the reason the guard was rewritten. What is still unmeasured
  is the injection itself against a VM like that: no live VM has had the underlay *appended beside*
  an existing SR-IOV NIC by the policy, only the envtest row. Note that the corrected guard would
  leave `vm-roce-gpu` alone too — correctly, because it already names its own reference — so a
  binding over `default` is a no-op for every VM there today.
- **Failure modes on a running member.** Every VM in §6 was `runStrategy: Halted`. What a
  KubeVirt-side restart does when a MAP-injected reference points at a NAD that does not exist yet is
  the `FailedCreatePodSandBox` behaviour the pool path already has today, but it was not re-measured
  here.

## 9. Reproducing

```
cd cubestack-ipam

# 1.35 (gate + runtime-config, v1beta1)
CGO_ENABLED=0 MAPSPIKE_ASSETS=$PWD/bin/k8s-135/controller-tools/envtest MAPSPIKE_MAP_API=v1beta1 \
  go test -tags mapspike ./hack/mapspike/... -v

# 1.37 (v1; no runtime-config needed)
CGO_ENABLED=0 MAPSPIKE_ASSETS=$PWD/bin/k8s/1.37.0-darwin-arm64 MAPSPIKE_MAP_API=v1 \
  go test -tags mapspike ./hack/mapspike/... -v

# the control: gate left at its default, which is what the cluster does today
CGO_ENABLED=0 MAPSPIKE_GATE=default MAPSPIKE_ASSETS=... MAPSPIKE_MAP_API=v1beta1 \
  go test -tags mapspike ./hack/mapspike/... -v -run TestServedAPIVersions
```

`MAPSPIKE_VERBOSE=1` attaches the apiserver's own output, which is the only way to see §3 and §4.3 —
`envtest` otherwise reports both as `timeout waiting for process kube-apiserver to start
successfully` and `stream error ... INTERNAL_ERROR` respectively.

Two 1.35 assets live outside `bin/k8s/` on purpose: `getFirstFoundEnvTestBinaryDir()` in
`internal/controller/suite_test.go` takes the first directory there **alphabetically**, so a
`1.35.0-darwin-arm64` sibling would silently move the whole existing controller suite onto 1.35.

§6 was produced against the live cluster rather than envtest, so it is reproduced by applying the
policy and binding to a namespace of your own whose label the binding's `namespaceSelector` matches,
and reading the objects back. The policy in §6 was the expression of §4.3 verbatim, with
`apiVersion: admissionregistration.k8s.io/v1beta1`. Note that the harness's `MAPSPIKE_MAP_API` and
the cluster's served version must agree — on 1.35 that is `v1beta1` on both.

## 10. Status of the claims

- §2's gate table and per-release API versions are read from the upstream sources in the module
  cache, not from recollection.
- §3, §4.3, §4.4 and §5 are **measured** on envtest control planes started from the binaries named
  above; the quoted log lines are verbatim.
- §3 is additionally confirmed by §6: the live 1.35.4 cluster serves
  `admissionregistration.k8s.io/v1beta1`, which is the observable consequence of both flags being set
  and of nothing else.
- §6 is **measured** on the live cluster, by read-back, in a temporary namespace; the objects it
  created were removed afterwards. It is the only section whose evidence is KubeVirt's own CRD and
  webhooks rather than a stand-in.
- §7 is **instruction**, not action: the control-plane flags were set by the cluster's owner, and
  step 4's list-of-initializers warning is untouched by §6, which used the safe spelling throughout.
  §7.1 records what was checked about the chart, and that chart 0.3.0 — including the policy — is
  **deployed** on the cluster as of 2026-09-28 22:30 +0800.
- §8 lists what is inferred rather than measured. Its largest item is the IPAM join — MAP's output is
  a *reference*; whether `cubestack-ipam` mints the NAD it names, on a pool member, is still untested.
