//go:build mapspike

package mapspike

import (
	"fmt"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
)

// TestExpressionCompilation is a compile probe, not an assertion.
//
// Policy compilation happens when the policy is admitted, so a candidate
// expression can be tested by creating a policy and reading the error -- no
// binding, no VM, no namespace needed.
//
// It exists because the first draft of the injection expression was rejected, and
// the reason was not obvious: `JSONPatch{value: {"name": "underlay", "multus":
// {"networkName": "..."}}}` looks like ordinary JSON, but Kubernetes CEL requires
// map literals to be homogeneous, and infers the value type from the first entry.
// "string" then "map" is a compile error.
func TestExpressionCompilation(t *testing.T) {
	candidates := []struct {
		name string
		expr string
	}{
		{"baseline: homogeneous map value",
			`[JSONPatch{op: "add", path: "/spec/template/spec/networks", value: [{"name": "underlay"}]}]`},

		{"nested map, singly-typed",
			`[JSONPatch{op: "add", path: "/x", value: {"a": {"b": "c"}}}]`},

		{"heterogeneous map value (the first draft)",
			`[JSONPatch{op: "add", path: "/x", value: {"name": "underlay", "multus": {"networkName": "n"}}}]`},

		{"heterogeneous, second entry a map",
			`[JSONPatch{op: "add", path: "/x", value: {"name": "underlay", "bridge": {}}}]`},

		{"typed object initializer",
			`[JSONPatch{op: "add", path: "/spec/template/spec/networks", value: [Object.spec.template.spec.networks{name: "underlay", multus: Object.spec.template.spec.networks.multus{networkName: "n"}}]}]`},

		{"typed initializer, multus named by field path only",
			`[JSONPatch{op: "add", path: "/x", value: Object.spec.template.spec.networks{name: "underlay", multus: Object.multus{networkName: "n"}}}]`},

		{"two patches: element then nested key by computed index",
			`[
			  JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: {"name": "underlay"}},
			  JSONPatch{op: "add", path: "/spec/template/spec/networks/0/multus", value: {"networkName": "n"}}
			]`},

		{"two patches: element then nested key at size()-1",
			`[
			  JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: {"name": "underlay"}},
			  JSONPatch{op: "add", path: "/spec/template/spec/networks/" + string(size(object.spec.template.spec.networks)) + "/multus", value: {"networkName": "n"}}
			]`},

		{"map value with a dyn-typed entry via string concat",
			`[JSONPatch{op: "add", path: "/x", value: {"name": "underlay", "multus": {"networkName": "default/" + object.metadata.name + "-static"}}}]`},

		{"named intermediate via variable",
			`[
			  JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: variables.net},
			  JSONPatch{op: "add", path: "/spec/template/spec/networks/" + string(size(object.spec.template.spec.networks)) + "/multus", value: variables.multus}
			]`},
	}

	for i, c := range candidates {
		name := fmt.Sprintf("spike-probe-%d", i)
		policy, err := policyObject(name, nil, []admissionregistrationv1.Mutation{jsonPatchMutation(c.expr)},
			admissionregistrationv1.Variable{Name: "net", Expression: `{"name": "underlay"}`},
			admissionregistrationv1.Variable{Name: "multus", Expression: `{"networkName": "default/" + object.metadata.name + "-static"}`},
		)
		if err != nil {
			t.Fatalf("building %s: %v", name, err)
		}

		err = k8s.Create(ctx, policy)
		switch {
		case err == nil:
			t.Logf("ACCEPTED  %-55s", c.name)
		default:
			t.Logf("REJECTED  %-55s", c.name)
			t.Logf("          %s", firstLine(err.Error()))
		}
	}
}

// TestWhatMAPSees dumps the shape of the `object` a MutatingAdmissionPolicy is
// actually given.
//
// This exists because the guard probe's results were not self-consistent: a
// condition that is plainly true for a VirtualMachine with no networks
// (`has(object.spec)`) appeared not to fire, while a condition that is also true
// (the optional-syntax ones) fired and then failed to patch. Two different
// explanations fit each result individually and none fit both, which means the
// assumption underneath them -- that `object` is the VirtualMachine as submitted --
// was never checked.
//
// Rather than reason further, have the policy write its own view of the object into
// an annotation. Annotations survive pruning, so what comes back is what the policy
// saw. The patch is a single string, which also settles whether string(bool) is
// available in Kubernetes CEL.
func TestWhatMAPSees(t *testing.T) {
	const ns = "spike-see"
	if err := k8s.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"spike": ns}},
	}); err != nil {
		t.Fatalf("namespace: %v", err)
	}

	expr := `[
	  JSONPatch{op: "add", path: "/metadata/annotations/mapspike-dbg", value:
	    "spec=" + string(has(object.spec))
	    + " tmpl=" + string(has(object.spec) && has(object.spec.template))
	    + " tmplspec=" + string(has(object.spec) && has(object.spec.template) && has(object.spec.template.spec))
	    + " nets=" + string(` + hasNetworks + `)
	    + " runStrategy=" + string(has(object.spec) && has(object.spec.runStrategy))
	    + " ifaces=" + string(string(` + hasIfaces + `))
	  }
	]`

	pol, err := policyObject("spike-see", nil, []admissionregistrationv1.Mutation{jsonPatchMutation(expr)})
	if err != nil {
		t.Fatalf("building policy: %v", err)
	}
	if err := k8s.Create(ctx, pol); err != nil {
		t.Fatalf("policy: %v", err)
	}
	bind, err := bindingObject("spike-see", ns)
	if err != nil {
		t.Fatalf("building binding: %v", err)
	}
	if err := k8s.Create(ctx, bind); err != nil {
		t.Fatalf("binding: %v", err)
	}
	time.Sleep(3 * time.Second)

	if err := createVM(t, ns, "see", ipamAnn(), nil, vmBody(nil, nil)); err != nil {
		t.Fatalf("creating VM (the policy may have rejected its own expression): %v", err)
	}

	got := getVM(t, ns, "see")
	ann := got.GetAnnotations()
	t.Logf("the policy reported: %s", ann["mapspike-dbg"])

	// And what the object actually looks like on the way out.
	raw, _ := got.MarshalJSON()
	t.Logf("stored object: %s", raw)
}

// TestServedAPIVersions is the gate's receipt.
//
// The claim under test on 1.35 is not that MAP "is available" but that a beta
// feature can be switched on, and the honest evidence for that is discovery: which
// admissionregistration.k8s.io versions the apiserver actually serves, and under
// which group version the MutatingAdmissionPolicy resource appears. Run this with
// and without the gate and the difference is the answer.
func TestServedAPIVersions(t *testing.T) {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("discovery client: %v", err)
	}
	groups, resources, err := dc.ServerGroupsAndResources()
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	_ = groups

	var saw []string
	for _, rl := range resources {
		if rl.GroupVersion != "admissionregistration.k8s.io/v1" && rl.GroupVersion != "admissionregistration.k8s.io/v1beta1" {
			continue
		}
		for _, r := range rl.APIResources {
			switch r.Name {
			case "mutatingadmissionpolicies", "mutatingadmissionpolicybindings":
				saw = append(saw, rl.GroupVersion+" "+r.Name)
			}
		}
	}
	if len(saw) == 0 {
		t.Logf("admissionregistration.k8s.io serves NO MutatingAdmissionPolicy: the gate is off")
		return
	}
	for _, s := range saw {
		t.Logf("served: %s", s)
	}
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}

// TestMatchConditionProbe asks a question the compile probe cannot.
//
// The guard expression compiles but fails at *evaluation* with "no such key: spec",
// which is not what `has()` is supposed to do -- has() exists precisely so that a
// missing field can be tested without an error. Since matchConditions run at
// admission and every VM create was being denied, the failure mode was total:
// nothing using this policy could be created at all.
//
// Nine ways of asking "does this VM already have a multus network" are tried
// against a VM that has no networks, which is the case the guard has to survive.
// Expected outcome for a correct guard is ALLOW (the condition is true, so the
// policy proceeds to inject).
func TestMatchConditionProbe(t *testing.T) {
	candidates := []struct {
		name string
		expr string
	}{
		{"one-level has(), present",
			`has(object.spec)`},
		{"nested has(), four levels",
			`has(object.spec.template.spec.networks)`},
		{"negated nested has()",
			`!has(object.spec.template.spec.networks)`},
		{"stepped has() at every level",
			`has(object.spec) && has(object.spec.template) && has(object.spec.template.spec) && has(object.spec.template.spec.networks)`},
		{"the guard as written",
			`!has(object.spec.template.spec.networks) || !object.spec.template.spec.networks.exists(n, has(n.multus))`},
		{"negated all() instead of exists()",
			`!has(object.spec.template.spec.networks) || object.spec.template.spec.networks.all(n, !has(n.multus))`},
		{"optional syntax with orValue",
			`(object.?spec.?template.?spec.?networks).orValue([]).all(n, !has(n.multus))`},
		{"optional syntax, hasValue",
			`!(object.?spec.?template.?spec.?networks).hasValue()`},
		{"no guard at all (control -- expect an error)",
			`object.spec.template.spec.networks.all(n, !has(n.multus))`},
	}

	for i, c := range candidates {
		ns := fmt.Sprintf("spike-mc-%d", i)
		if err := k8s.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"spike": ns}},
		}); err != nil {
			t.Fatalf("namespace %s: %v", ns, err)
		}
		if err := createPolicy("spike-mc-"+fmt.Sprint(i), ns,
			[]admissionregistrationv1.MatchCondition{ipamCondition(), {Name: "guard", Expression: c.expr}},
			[]admissionregistrationv1.Mutation{jsonPatchMutation(injectExpr)},
		); err != nil {
			t.Logf("POLICY REJECTED  %-45s %s", c.name, firstLine(err.Error()))
		}
	}

	time.Sleep(3 * time.Second)

	for i, c := range candidates {
		ns := fmt.Sprintf("spike-mc-%d", i)
		err := createVM(t, ns, "probe", ipamAnn(), nil, vmBody(nil, nil))
		switch {
		case err != nil && strings.Contains(err.Error(), "no such key"):
			t.Logf("ERROR    %-45s no such key in the guard", c.name)
			continue
		case err != nil:
			t.Logf("DENIED   %-45s %s", c.name, firstLine(err.Error()))
			continue
		}

		// "allowed" is not the interesting part -- what matters is whether the
		// mutation actually landed. Report it rather than assuming it, since a
		// condition that evaluates true and a mutation that injects are two
		// different claims.
		got := getVM(t, ns, "probe")
		nets := netsOf(t, got)
		injected := "not injected"
		if len(nets) == 1 {
			if m, _, _ := unstructured.NestedMap(nets[0].(map[string]any), "multus"); m["networkName"] != "" {
				injected = fmt.Sprintf("injected %v", m["networkName"])
			}
		}
		t.Logf("ALLOWED  %-45s %s", c.name, injected)
	}
}

// TestCreateArrayAlternatives probes how to create an absent array without
// panicking the apiserver.
//
// On the 1.35 assets the inject expression does not merely fail -- it takes the
// apiserver's request handler down:
//
//	E "Observed a panic" panic=<
//	  reflect.Set: value of type *structpb.Struct is not assignable to type *structpb.Value
//	  github.com/google/cel-go/common/types.(*baseList).ConvertToNative  list.go:206
//	  .../mutating/patch/json_patch.go:177  (*jsonPatcher).evaluatePatchExpression
//	E "apiserver panic'd" method="POST" URI="/apis/kubevirt.io/v1/namespaces/spike-ok/virtualmachines"
//	http2: panic serving 127.0.0.1:52562: reflect.Set: value of type *structpb.Struct ...
//
// json_patch.go:177 is `op.Val.ConvertToNative(reflect.TypeOf(&structpb.Value{}))`, and
// that line is IDENTICAL in apiserver 0.35.4 and 0.37.0. What differs is cel-go:
// 0.26.0 (pinned by 0.35.x AND 0.36.x) versus 0.29.2 (0.37.0). The value adapter in
// pkg/cel/mutation/dynamic/objects.go differs too -- 0.35.4 only handles an exact type
// match where 0.37.0 handles *structpb.Struct explicitly and wraps via structpb.NewValue.
//
// So the read is: a *list* whose elements are object initializers converts to a native
// list of *structpb.Struct, and the outer conversion to *structpb.Value then panics.
// A *bare* object value is fine -- which is why the append branch (`/-` with a single
// typed initializer) works on 1.35 while the create branch does not.
//
// If that read is right, the create branch can be split into "add an empty list, then
// append" and 1.35 falls back into the usable set. Each candidate is judged on three
// outcomes, and a panic is reported as such rather than as a denial, because the two
// have very different consequences: a denial stops one write, a panic is an
// INTERNAL_ERROR on every write the policy matches.
func TestCreateArrayAlternatives(t *testing.T) {
	net := `Object.spec.template.spec.networks{name: "underlay", multus: Object.spec.template.spec.networks.multus{networkName: "default/" + object.metadata.name + "-static"}}`

	candidates := []struct {
		name string
		expr string
	}{
		{"control: list of object initializers (the current create branch)",
			`[JSONPatch{op: "add", path: "/spec/template/spec/networks", value: [` + net + `]}]`},

		{"empty list only",
			`[JSONPatch{op: "add", path: "/spec/template/spec/networks", value: []}]`},

		{"empty list, then append the typed initializer",
			`[
			   JSONPatch{op: "add", path: "/spec/template/spec/networks", value: []},
			   JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: ` + net + `}
			 ]`},

		{"empty list, then append a bare map literal",
			`[
			   JSONPatch{op: "add", path: "/spec/template/spec/networks", value: []},
			   JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: {"name": "underlay"}}
			 ]`},

		{"list of plain map literals (no initializer)",
			`[JSONPatch{op: "add", path: "/spec/template/spec/networks", value: [{"name": "underlay"}]}]`},

		{"append only, array seeded by the VM (no create branch)",
			`[JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: ` + net + `}]`},
	}

	for i, c := range candidates {
		ns := fmt.Sprintf("spike-ca-%d", i)
		if err := k8s.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"spike": ns}},
		}); err != nil {
			t.Fatalf("namespace %s: %v", ns, err)
		}
		if err := createPolicy("spike-ca-"+fmt.Sprint(i), ns,
			[]admissionregistrationv1.MatchCondition{ipamCondition()},
			[]admissionregistrationv1.Mutation{jsonPatchMutation(c.expr)},
		); err != nil {
			t.Logf("POLICY REJECTED  %-58s %s", c.name, firstLine(err.Error()))
			continue
		}
	}

	time.Sleep(3 * time.Second)

	for i, c := range candidates {
		ns := fmt.Sprintf("spike-ca-%d", i)

		// The last candidate needs an array to append to; every other candidate is
		// asking what happens when there is none.
		var seed []any
		if strings.HasPrefix(c.name, "append only") {
			seed = []any{map[string]any{"name": "business-net", "multus": map[string]any{"networkName": "default/other"}}}
		}

		err := createVM(t, ns, "probe", ipamAnn(), nil, vmBody(seed, nil))
		switch {
		case err != nil && strings.Contains(err.Error(), "INTERNAL_ERROR"):
			t.Logf("PANIC     %-58s the apiserver's handler panicked; request aborted as INTERNAL_ERROR", c.name)
			continue
		case err != nil:
			t.Logf("DENIED    %-58s %s", c.name, firstLine(err.Error()))
			continue
		}

		got := getVM(t, ns, "probe")
		t.Logf("ALLOWED   %-58s networks now: %v", c.name, netNames(netsOf(t, got)))
	}
}

// TestGuardOnCreateWithExistingArray answers the one case TestCreateArrayAlternatives
// left open.
//
// In that probe the append-only candidate -- a VM created with an existing networks
// array, mutated by an unguarded single `add .../networks/-` -- came back holding
// THREE entries from one create: the seeded one plus two appends. No other candidate
// doubled, so it is not simply "MAP runs twice per create".
//
// The shape matters because a pool template can legitimately declare a network that is
// not multus (a pod/default network, say). Then the real policy's guard -- "this VM has
// no multus network yet" -- passes, the create branch is skipped because the array
// exists, and the append runs against a pre-existing array: exactly the candidate that
// doubled. If the guard does not bound it, one create produces two claims on one
// address, which is the failure the whole design exists to prevent.
func TestGuardOnCreateWithExistingArray(t *testing.T) {
	cases := []struct {
		name string
		seed []any
		want int
		// unguarded drops alreadyInjected, which is what the alternatives probe's
		// append-only candidate effectively was. It is here to say whether the
		// doubling it showed is a property of MAP being invoked more than once
		// (in which case the guard is what makes injection exactly-once) or
		// something about that probe in particular.
		unguarded bool
		// seedOurs builds the seed inside the loop instead, because the reference it
		// has to contain is namespace-qualified and the namespace is the loop's.
		seedOurs bool
	}{
		{"pod-only network (guard passes, array exists)", []any{
			map[string]any{"name": "default", "pod": map[string]any{}},
		}, 1, false, false},
		{"no networks at all (guard passes, array absent)", nil, 1, false, false},
		// Not "an unrelated multus network blocks the guard" -- that was the old
		// semantics and the reason a RoCE NIC would have cost the VM its underlay.
		// The guard's subject is the policy's own reference, so a VM carrying someone
		// else's multus network is a VM that still needs one appended.
		{"unrelated multus network (guard passes, ours appended)", []any{
			map[string]any{"name": "business-net", "multus": map[string]any{"networkName": "somewhere/other"}},
		}, 1, false, false},
		// The reference is namespaced from the VM's own namespace and spelled from the
		// VM's own name, so this seed cannot be written until the loop knows the
		// namespace it is about to create -- see seedOurs below.
		{"already carries our reference (guard blocks)", nil, 0, false, true},
		// want=2 is the measured behaviour, not a mistake: without the guard MAP
		// evaluates the policy twice for one create, so the append lands twice.
		// TestInvocationCount counts the evaluations and reaches the same number.
		{"pod-only network, no guard at all", []any{
			map[string]any{"name": "default", "pod": map[string]any{}},
		}, 2, true, false},
	}

	for i := range cases {
		ns := fmt.Sprintf("spike-goc-%d", i)
		if err := k8s.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"spike": ns}},
		}); err != nil {
			t.Fatalf("namespace %s: %v", ns, err)
		}
		conds := []admissionregistrationv1.MatchCondition{ipamCondition(), alreadyInjected()}
		if cases[i].unguarded {
			conds = []admissionregistrationv1.MatchCondition{ipamCondition()}
		}
		if err := createPolicy("spike-goc-"+fmt.Sprint(i), ns, conds,
			[]admissionregistrationv1.Mutation{jsonPatchMutation(injectExpr)},
		); err != nil {
			t.Fatalf("policy %d: %v", i, err)
		}
	}

	time.Sleep(3 * time.Second)

	for i, c := range cases {
		ns := fmt.Sprintf("spike-goc-%d", i)
		seed := c.seed
		if c.seedOurs {
			seed = []any{map[string]any{
				"name":   "underlay",
				"multus": map[string]any{"networkName": ns + "/probe-static"},
			}}
		}
		if err := createVM(t, ns, "probe", ipamAnn(), nil, vmBody(seed, nil)); err != nil {
			t.Fatalf("creating VM %d: %v", i, err)
		}
		got := getVM(t, ns, "probe")
		nets := netsOf(t, got)
		ifaces := ifacesOf(t, got)

		// Count interfaces, not networks. The injection always appends one of each,
		// but a seed can itself carry our exact network reference -- that is the whole
		// point of the "already carries our reference" case -- so counting networks
		// would score the seed as an injection and report a blocked guard as a firing
		// one. No seed here declares an interface, so an "underlay" interface can only
		// have come from the policy.
		injected := 0
		for _, i := range ifaces {
			if m, _, _ := unstructured.NestedMap(i.(map[string]any), "bridge"); m != nil {
				injected++
			}
		}

		verdict := "OK"
		if injected != c.want {
			verdict = "UNEXPECTED"
		}
		t.Logf("%-9s %-48s networks=%v interfaces=%v injected=%d want=%d",
			verdict, c.name, netNames(nets), netNames(ifaces), injected, c.want)
	}
}

// TestInvocationCount counts how many times a policy's mutations run for ONE create.
//
// The previous test showed an unguarded append landing twice while a guarded one
// landed once, which is consistent with two different stories -- MAP running twice
// per create (and the guard catching the second run), or something about the shape of
// the mutation itself. The stories predict different things about the guard's
// importance, so rather than pick one, count.
//
// The counter is a string of x's: each invocation reads the annotation it wrote last
// time and appends one more, so the stored value is the number of times the policy's
// mutations were evaluated against this object.
func TestInvocationCount(t *testing.T) {
	const counter = `[JSONPatch{op: "add", path: "/metadata/annotations/mapspike-runs", value:
	   (has(object.metadata.annotations) && 'mapspike-runs' in object.metadata.annotations
	      ? object.metadata.annotations['mapspike-runs'] : "") + "x"}]`

	cases := []struct {
		name string
		seed []any
		expr string
		// seedOurs builds the seed inside the loop: the reference the guard looks for is
		// namespace-qualified, and the namespace is the loop's.
		seedOurs bool
	}{
		{"guarded, array absent", nil, injectExpr, false},
		{"guarded, pod network present", []any{map[string]any{"name": "default", "pod": map[string]any{}}}, injectExpr, false},
		// "policy skips" here means the guard recognised its own reference -- which is
		// the only thing it skips on. An unrelated multus network does not stop it.
		{"guarded, ours already present (policy skips)", nil, injectExpr, true},
		{"guarded, unrelated multus present (policy runs)", []any{
			map[string]any{"name": "roce-net", "multus": map[string]any{"networkName": "default/roce-vf-nad"}},
		}, injectExpr, false},
		{"unguarded, array absent", nil, injectExpr, false},
		{"unguarded, pod network present", []any{map[string]any{"name": "default", "pod": map[string]any{}}}, injectExpr, false},
	}

	for i := range cases {
		ns := fmt.Sprintf("spike-inv-%d", i)
		if err := k8s.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"spike": ns}},
		}); err != nil {
			t.Fatalf("namespace %s: %v", ns, err)
		}
		conds := []admissionregistrationv1.MatchCondition{ipamCondition(), alreadyInjected()}
		if strings.HasPrefix(cases[i].name, "unguarded") {
			conds = []admissionregistrationv1.MatchCondition{ipamCondition()}
		}
		if err := createPolicy("spike-inv-"+fmt.Sprint(i), ns, conds,
			[]admissionregistrationv1.Mutation{
				jsonPatchMutation(cases[i].expr),
				jsonPatchMutation(counter),
			},
		); err != nil {
			t.Fatalf("policy %d: %v", i, err)
		}
	}

	time.Sleep(3 * time.Second)

	for i, c := range cases {
		ns := fmt.Sprintf("spike-inv-%d", i)
		seed := c.seed
		if c.seedOurs {
			seed = []any{map[string]any{
				"name":   "underlay",
				"multus": map[string]any{"networkName": ns + "/probe-static"},
			}}
		}
		if err := createVM(t, ns, "probe", ipamAnn(), nil, vmBody(seed, nil)); err != nil {
			t.Fatalf("creating VM %d: %v", i, err)
		}
		got := getVM(t, ns, "probe")
		runs := got.GetAnnotations()["mapspike-runs"]
		t.Logf("%-38s runs=%d networks=%v", c.name, len(runs), netNames(netsOf(t, got)))
	}
}
