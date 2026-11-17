//go:build mapspike

package mapspike

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

var (
	ctx     context.Context
	k8s     client.Client
	cfg     *rest.Config
	testEnv *envtest.Environment
)

// mapAPIVersion is the admissionregistration.k8s.io version the MAP objects are
// written at, and it is not a free choice -- it follows the apiserver:
//
//	1.32  alpha, v1alpha1
//	1.34+ beta,  v1beta1     (gate MutatingAdmissionPolicy, default OFF)
//	1.36+ GA,    v1          (gate locked on)
//
// So the same policy has to be submitted as v1beta1 on 1.35 and v1 on 1.36+, and
// "does it work on 1.35" is a question about v1beta1. Overridable so one binary can
// be pointed at either apiserver.
var mapAPIVersion = "v1"

var vmGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}

// One namespace per policy. A MutatingAdmissionPolicy is cluster-scoped and its
// binding is what narrows it, so scoping each policy to its own namespace via
// binding.matchResources.namespaceSelector is what keeps the guarded experiment
// from firing during the unguarded one. Creating every policy up front, rather
// than around each test, avoids racing the apiserver's informers.
const (
	nsOK        = "spike-ok"        // the guarded JSONPatch policy
	nsUnguarded = "spike-unguarded" // same patch, no has() guard, failurePolicy Fail
	nsApplyCfg  = "spike-applycfg"  // the ApplyConfiguration negative control
	nsOrder     = "spike-order"     // MAP against a stand-in mutating webhook
)

// hasNetworks and hasIfaces are the presence tests, written the long way, one level
// per has().
//
// The long form is defensive, not required. A nested test --
// `has(object.spec.template.spec.networks)` -- works: object.spec, .template and
// .spec are all present on any VirtualMachine KubeVirt accepts, and the probe shows
// the nested form evaluating correctly in a matchCondition. What a nested test does
// not survive is an object where an intermediate level is *absent*: CEL raises "no
// such key" rather than returning false, and under failurePolicy: Fail a match
// condition that errors rejects the request outright. This policy runs with Fail
// because a silently skipped injection is worse than a rejected create -- so the
// one-level form is cheaper insurance than it looks. See TestMatchConditionProbe.
//
// (An earlier round of this spike concluded that nested has() was simply broken.
// That was wrong, and the cause was the test fixture: vmBody was building the
// VirtualMachine with the *contents* of spec at the root of the object, so the CRD
// pruned spec entirely and `object.spec` really did not exist. TestWhatMAPSees
// exists to make that class of mistake visible -- it has the policy write its own
// view of the object back out, rather than trusting an assumption about what the
// apiserver handed it.)
const (
	hasNetworks = `has(object.spec) && has(object.spec.template) && has(object.spec.template.spec) && has(object.spec.template.spec.networks)`
	hasIfaces   = `has(object.spec) && has(object.spec.template) && has(object.spec.template.spec) && has(object.spec.template.spec.domain) && has(object.spec.template.spec.domain.devices) && has(object.spec.template.spec.domain.devices.interfaces)`
)

// injectExpr is the whole point of the exercise: append the underlay network and
// its interface, deriving the NAD name from the VM's own name.
//
// Four things are load-bearing, and none of them is obvious from the API docs.
//
//   - JSONPatch rather than ApplyConfiguration, because the latter may not modify
//     atomic arrays, and KubeVirt declares no merge key on networks or interfaces,
//     so both are atomic. (TestApplyConfigurationNegativeControl demonstrates the
//     failure rather than taking the documentation's word for it.)
//
//   - The hasNetworks/hasIfaces branches, because the interesting case is a pool
//     template that declares no networks at all. `add` with a `/-` path appends to
//     an existing array; it cannot create one. A single unguarded `/-` append --
//     the obvious first draft -- fails on exactly the case this feature exists for.
//     (TestWhatMAPSees is the blunt demonstration that `add` needs its parent to
//     exist: patching /metadata/annotations onto a VM with no annotations is
//     rejected, not helpfully created.)
//
//   - **Creating the array as an empty list, never as a list of initializers.**
//     The natural spelling of the create branch -- `value: [Object.spec...networks{...}]`
//     -- does not fail on 1.35. It PANICS the apiserver's request handler:
//
//     reflect.Set: value of type *structpb.Struct is not assignable to type *structpb.Value
//     cel-go/common/types.(*baseList).ConvertToNative  list.go:206
//     .../mutating/patch/json_patch.go:177  (*jsonPatcher).evaluatePatchExpression
//
//     json_patch.go:177 is `op.Val.ConvertToNative(reflect.TypeOf(&structpb.Value{}))` and
//     is byte-identical in apiserver 0.35.4 and 0.37.0; cel-go is not (0.26.0 at 0.35.x
//     and 0.36.x, 0.29.2 at 0.37.0). A list whose elements are object initializers
//     converts to a native list of *structpb.Struct, and the outer conversion to
//     *structpb.Value then fails a reflect.Set. A bare object value never goes through
//     that path, which is why the append branch works on 1.35 and the create branch did
//     not. TestCreateArrayAlternatives measures the six spellings.
//
//     So the array is created empty and the element is appended as a bare value, in two
//     patches. The cost is that `has()` has to be asked twice, once per patch, which is
//     why the whole thing is built by list concatenation rather than a ternary per pair.
//
//   - **Object.<path>{...} initializers rather than JSON-style map literals.**
//     `value: {"name": "underlay", "multus": {...}}` looks like JSON and is
//     rejected: Kubernetes CEL requires map literals to be homogeneous and infers
//     the value type from the first entry, so a string followed by a map is a
//     compile error whose message points at the nested map rather than the cause.
//     See TestExpressionCompilation.
//
// The cost of that last point is worth stating plainly, because it is the kind of
// thing that decides whether a mechanism is usable: an injected object cannot be
// written as data. It has to be written as a typed constructor whose type names
// follow a path convention, and (as the probe shows) the apiserver does not check
// those names against the CRD schema at compile time.
const injectExpr = `(` +
	hasNetworks + `
    ? []
    : [JSONPatch{op: "add", path: "/spec/template/spec/networks", value: []}]) + [` +
	JSONPatchNet + `] + (` +
	hasIfaces + `
    ? []
    : [JSONPatch{op: "add", path: "/spec/template/spec/domain/devices/interfaces", value: []}]) + [` +
	JSONPatchIface + `]`

// nadNameExpr is how the NAD name is derived, written once because two things need
// it: the mutation that writes the reference, and the guard that has to recognise it
// again on the second evaluation. If those two spelled it separately the policy could
// inject a reference it then failed to recognise as its own, and append a second copy.
//
// It mirrors the controller exactly -- internal/ipam/nad.go DefaultNADName() plus the
// ipam.cubestack.io/nad-name override that virtualmachine_controller.go applies first,
// including that file's `if nadName == ""` fallback, which is the `!= ”` test here.
// The one branch it cannot mirror is DefaultNADName's truncation of names over 246
// characters, which appends a SHA-256 digest that CEL has no way to compute.
const nadNameExpr = `(has(object.metadata.annotations) && 'ipam.cubestack.io/nad-name' in object.metadata.annotations && object.metadata.annotations['ipam.cubestack.io/nad-name'] != '' ? object.metadata.annotations['ipam.cubestack.io/nad-name'] : object.metadata.name + "-static")`

// underlayRef is the multus reference the policy injects: the derived name, qualified
// by the VM's *own* namespace.
//
// The namespace has to come from object.metadata.namespace, not a literal. The
// controller mints the NAD in the namespace the claim is in, and the claim is in the
// VM's namespace (internal/controller/iprequest_controller.go), so a reference of
// "default/<name>-static" is wrong for every VM outside `default` -- it names a NAD
// that will never be minted, and the VM waits forever. Every VM in this cluster
// happens to live in `default`, which is exactly why the literal survived as long as
// it did: the tests here use their own namespaces (spike-ok, spike-goc-N) and the
// assertion encoded the same literal, so it agreed with itself and proved nothing.
const underlayRef = `object.metadata.namespace + "/" + ` + nadNameExpr

// JSONPatchNet and JSONPatchIface are the two append operations, spelled once so the
// expression above reads as the sequence of patches it is: ensure the array exists,
// then append the element.
const (
	JSONPatchNet   = `JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: Object.spec.template.spec.networks{name: "underlay", multus: Object.spec.template.spec.networks.multus{networkName: ` + underlayRef + `}}}`
	JSONPatchIface = `JSONPatch{op: "add", path: "/spec/template/spec/domain/devices/interfaces/-", value: Object.spec.template.spec.domain.devices.interfaces{name: "underlay", bridge: Object.spec.template.spec.domain.devices.interfaces.bridge{}}}`
)

// unguardedExpr is the obvious first draft, kept as a specimen. It has no
// create-the-array branch, which is why it fails at evaluation on a VM that
// declares no networks -- and under failurePolicy: Fail that is not a quiet no-op.
const unguardedExpr = `[
  JSONPatch{op: "add", path: "/spec/template/spec/networks/-", value: Object.spec.template.spec.networks{name: "underlay", multus: Object.spec.template.spec.networks.multus{networkName: object.metadata.namespace + "/" + object.metadata.name + "-static"}}}
]`

func ipamCondition() admissionregistrationv1.MatchCondition {
	return admissionregistrationv1.MatchCondition{
		Name:       "has-ipam-pool-annotation",
		Expression: `has(object.metadata.annotations) && 'ipam.cubestack.io/pool' in object.metadata.annotations`,
	}
}

// alreadyInjected is the idempotency guard. Without it, reinvocationPolicy: IfNeeded
// means every mutation by a later mutator causes this policy to be re-run, and a
// second underlay network appears each time.
//
// It asks whether *this policy's own reference* is already present, and not -- as an
// earlier version of it did -- whether the VM has any multus network at all. Those two
// questions have the same answer only for a VM whose sole network is the underlay. A VM
// that passes a RoCE or SR-IOV VF through on its own NAD has a multus network already
// and still wants the L2 underlay added; the broader test would skip it silently, and
// the VM would come up without the network its pool claim is paying for.
//
// The && chain is load-bearing twice over: it is what makes each level safe to read
// on a partially-populated object, and CEL's error-absorbing && means an early false
// swallows the later terms rather than propagating an error.
func alreadyInjected() admissionregistrationv1.MatchCondition {
	return admissionregistrationv1.MatchCondition{
		Name:       "underlay-not-injected-yet",
		Expression: `!(` + hasNetworks + ` && object.spec.template.spec.networks.exists(n, has(n.multus) && has(n.multus.networkName) && n.multus.networkName == ` + underlayRef + `))`,
	}
}

func TestMain(m *testing.M) {
	ctx = context.Background()

	assets := assetsDir()
	if assets == "" {
		fmt.Fprintln(os.Stderr, "mapspike: no envtest assets found; set MAPSPIKE_ASSETS or run `make setup-envtest`")
		os.Exit(1)
	}

	if v := os.Getenv("MAPSPIKE_MAP_API"); v != "" {
		mapAPIVersion = v
	}

	testEnv = &envtest.Environment{
		// Its own CRD directory: a second virtualmachines.kubevirt.io in the
		// shared testdata/ root would collide with the controller suite's.
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "testdata", "mapspike")},
		BinaryAssetsDirectory: assets,
		ErrorIfCRDPathMissing: true,
		// envtest swallows the apiserver's stderr and reports only "timeout waiting
		// for process kube-apiserver to start successfully". When that happens the
		// apiserver's own complaint is the only useful evidence, so it is one
		// environment variable away rather than a recompile.
		AttachControlPlaneOutput: os.Getenv("MAPSPIKE_VERBOSE") != "",
	}

	// The feature gate is the whole question on 1.35, so it is a knob rather than a
	// constant:
	//
	//	unset            -> MutatingAdmissionPolicy=true   (the condition under test)
	//	MAPSPIKE_GATE=default -> no flag at all            (the control: beta, off)
	//	MAPSPIKE_GATE=false   -> explicitly off
	//
	// On 1.36+ the gate is GA and defaulted on, so passing =true there must be a
	// no-op that the apiserver accepts -- which the 1.37 run checks by doing it.
	switch g := os.Getenv("MAPSPIKE_GATE"); g {
	case "default":
		fmt.Printf("mapspike: feature gates left at their defaults\n")
	default:
		if g == "" {
			g = "true"
		}
		apiserver := testEnv.ControlPlane.GetAPIServer()
		apiserver.Configure().Set("feature-gates", "MutatingAdmissionPolicy="+g)
		fmt.Printf("mapspike: --feature-gates=MutatingAdmissionPolicy=%s\n", g)

		// The gate ALONE is not enough on 1.35, and the failure is not "the policy
		// is refused" -- it is the whole apiserver.
		//
		// The MAP admission plugin in k8s.io/apiserver@v0.35.4 lists its own
		// objects through hardwired v1beta1 informers (plugin.go:99-100, where
		// `type Policy = v1beta1.MutatingAdmissionPolicy`). In 1.35 that group
		// version is not served by default, so the informers 404, the generic
		// policy plugin's SetReadyFunc never reports synced, and
		// generic/plugin.go:210 then answers EVERY admission request with
		// `not yet ready to handle request`. Bootstrap requests are admission
		// requests too: kube-system never gets created, the
		// bootstrap-system-priority-classes PostStartHook fails, and the process
		// exits. Measured on the 1.35.0 binary: ready for ~10s, then dead, with
		// `Failed to watch ... *v1beta1.MutatingAdmissionPolicy` in between.
		//
		// Re-enabling the group version is what makes the gate usable. Measured
		// with it: v1beta1 served, mutatingadmissionpolicies served, readyz=ok,
		// zero informer failures. It is tied to the API version being exercised
		// rather than to the gate, because it is what makes v1beta1 exist at all --
		// on 1.37 the run does not pass it and does not need it.
		if g == "true" && mapAPIVersion != "v1" {
			apiserver.Configure().Set("runtime-config", "admissionregistration.k8s.io/v1beta1=true")
			fmt.Printf("mapspike: --runtime-config=admissionregistration.k8s.io/v1beta1=true (required to serve the v1beta1 API the 1.35 plugin uses)\n")
		}
	}

	fmt.Printf("mapspike: apiserver assets %s, MAP objects written as admissionregistration.k8s.io/%s\n",
		assets, mapAPIVersion)

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapspike: envtest start: %v\n", err)
		os.Exit(1)
	}

	k8s, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mapspike: client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	if err := waitForVMCRD(60 * time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "mapspike: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	if err := warmUpClient(); err != nil {
		fmt.Fprintf(os.Stderr, "mapspike: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "mapspike: setup: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

// assetsDir prefers an explicit MAPSPIKE_ASSETS path so a second apiserver version
// can be tested without disturbing bin/k8s -- note that adding a 1.35 directory
// under bin/k8s/ would silently move the whole controller suite, which picks the
// first directory there alphabetically.
func assetsDir() string {
	if p := os.Getenv("MAPSPIKE_ASSETS"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}

// waitForVMCRD blocks until the apiserver both reports the CRD Established and
// serves it over discovery.
//
// Without this the first few tests fail with "Resource kind kubevirt.io/v1,
// Kind=VirtualMachine not found" and the ones after them pass -- which looks like a
// behavioural difference between policies and is not. CRD installation in envtest is
// asynchronous, and the client's RESTMapper resolves lazily on first use.
func waitForVMCRD(timeout time.Duration) error {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return fmt.Errorf("discovery client: %w", err)
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		resources, err := dc.ServerResourcesForGroupVersion("kubevirt.io/v1")
		if err == nil {
			for _, r := range resources.APIResources {
				if r.Name == "virtualmachines" {
					return nil
				}
			}
		}
		last = err
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for virtualmachines.kubevirt.io to be served: %w", last)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// warmUpClient forces the client's RESTMapper to resolve the VirtualMachine kind
// once, before any test depends on it.
//
// waitForVMCRD is not enough on its own: discovery serving the resource and the
// mapper having resolved it are different states, and the mapper's first few calls
// after a CRD install can still miss. The symptom is ugly -- the first tests fail
// with "kind not found" and the later ones pass, which reads as a behavioural
// difference between policies and is purely a warm-up artefact.
//
// The throwaway VM goes in the default namespace, where no binding matches, so it
// is not itself a test of MAP.
func warmUpClient() error {
	u := vmObj("default", "mapspike-warmup", nil, nil, vmBody(nil, nil))
	deadline := time.Now().Add(60 * time.Second)
	for {
		err := k8s.Create(ctx, u)
		if err == nil {
			return k8s.Delete(ctx, u)
		}
		if !strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("warm-up create: %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("warm-up create never resolved the kind: %w", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// versioned converts a typed admissionregistration object to the version this
// apiserver serves. The JSON is identical across v1 and v1beta1 -- checked field by
// field against k8s.io/api -- so the typed v1 structs remain the single source of
// truth for what the policy says, and only the wire version changes.
func versioned(kind string, obj runtime.Object) (client.Object, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	u.SetAPIVersion("admissionregistration.k8s.io/" + mapAPIVersion)
	u.SetKind(kind)
	return u, nil
}

// setup creates the namespaces, policies and bindings the tests share, then waits
// once for the apiserver to observe them. Waiting once here rather than polling in
// each test keeps the tests about MAP's behaviour and not about informer lag.
func setup() error {
	for _, ns := range []string{nsOK, nsUnguarded, nsApplyCfg, nsOrder} {
		if err := k8s.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: map[string]string{"spike": ns}},
		}); err != nil {
			return fmt.Errorf("creating namespace %s: %w", ns, err)
		}
	}

	// Policy creation is deliberately non-fatal here. A policy the apiserver
	// rejects is one of the results this spike is looking for -- the
	// ApplyConfiguration control is expected to be one -- and aborting setup would
	// turn "this expression does not compile" into "the harness did not start".
	for _, p := range []struct {
		name   string
		ns     string
		expr   string
		use    string
		guards bool
	}{
		{"spike-inject", nsOK, injectExpr, "jsonpatch", true},
		// guards: false is the point of this one. It carries the ipam condition and
		// nothing else, so the only thing that can reject a request is the mutation
		// expression itself.
		{"spike-unguarded", nsUnguarded, unguardedExpr, "jsonpatch", false},
		{"spike-applycfg", nsApplyCfg, "", "applycfg", false},
		{"spike-order", nsOrder, injectExpr, "jsonpatch", true},
	} {
		conds := []admissionregistrationv1.MatchCondition{ipamCondition()}
		if p.guards {
			conds = append(conds, alreadyInjected())
		}

		var err error
		switch p.use {
		case "jsonpatch":
			err = createPolicy(p.name, p.ns, conds,
				[]admissionregistrationv1.Mutation{jsonPatchMutation(p.expr)})
		case "applycfg":
			err = createPolicy(p.name, p.ns, conds,
				[]admissionregistrationv1.Mutation{applyConfigMutation()})
		}
		if err != nil {
			fmt.Printf("mapspike: policy %s was NOT created: %v\n", p.name, err)
		}
	}

	time.Sleep(3 * time.Second)
	return nil
}

func createPolicy(name, ns string,
	conds []admissionregistrationv1.MatchCondition,
	mutations []admissionregistrationv1.Mutation,
) error {
	policy, err := policyObject(name, conds, mutations)
	if err != nil {
		return fmt.Errorf("building policy %s: %w", name, err)
	}
	if err := k8s.Create(ctx, policy); err != nil {
		return fmt.Errorf("creating policy %s: %w", name, err)
	}

	binding, err := bindingObject(name, ns)
	if err != nil {
		return fmt.Errorf("building binding %s: %w", name, err)
	}
	if err := k8s.Create(ctx, binding); err != nil {
		return fmt.Errorf("creating binding %s: %w", name, err)
	}
	return nil
}

// policyObject is separated from creation so the compile probe can submit a policy
// with no binding -- policy compilation happens at admission of the policy itself,
// so an expression can be tested without anything that would make it fire.
//
// Variables are variadic because only one probe candidate uses them.
func policyObject(name string,
	conds []admissionregistrationv1.MatchCondition,
	mutations []admissionregistrationv1.Mutation,
	vars ...admissionregistrationv1.Variable,
) (client.Object, error) {
	policy := &admissionregistrationv1.MutatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.MutatingAdmissionPolicySpec{
			FailurePolicy:      ptr.To(admissionregistrationv1.Fail),
			ReinvocationPolicy: admissionregistrationv1.IfNeededReinvocationPolicy,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{
							admissionregistrationv1.Create, admissionregistrationv1.Update,
						},
						Rule: admissionregistrationv1.Rule{
							APIGroups:   []string{"kubevirt.io"},
							APIVersions: []string{"v1"},
							Resources:   []string{"virtualmachines"},
						},
					},
				}},
			},
			Variables:       vars,
			MatchConditions: conds,
			Mutations:       mutations,
		},
	}
	return versioned("MutatingAdmissionPolicy", policy)
}

func bindingObject(name, ns string) (client.Object, error) {
	binding := &admissionregistrationv1.MutatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionregistrationv1.MutatingAdmissionPolicyBindingSpec{
			PolicyName: name,
			MatchResources: &admissionregistrationv1.MatchResources{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"spike": ns}},
			},
		},
	}
	return versioned("MutatingAdmissionPolicyBinding", binding)
}

func jsonPatchMutation(expr string) admissionregistrationv1.Mutation {
	return admissionregistrationv1.Mutation{
		PatchType: admissionregistrationv1.PatchTypeJSONPatch,
		JSONPatch: &admissionregistrationv1.JSONPatch{Expression: expr},
	}
}

func applyConfigMutation() admissionregistrationv1.Mutation {
	return admissionregistrationv1.Mutation{
		PatchType: admissionregistrationv1.PatchTypeApplyConfiguration,
		ApplyConfiguration: &admissionregistrationv1.ApplyConfiguration{
			Expression: `Object{spec: Object.spec{template: Object.spec.template{spec: Object.spec.template.spec{networks: [Object.spec.template.spec.networks{name: "underlay"}]}}}}`,
		},
	}
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

// vmBody builds a minimal VirtualMachine spec. A nil networks or interfaces leaves
// the field absent entirely, which is the case that matters: a pool template that
// declares no network at all.
func vmBody(networks, interfaces []any) map[string]any {
	devices := map[string]any{}
	if interfaces != nil {
		devices["interfaces"] = interfaces
	}
	tmplSpec := map[string]any{
		"domain": map[string]any{
			"resources": map[string]any{"requests": map[string]any{"memory": "1Gi"}},
			"devices":   devices,
		},
	}
	if networks != nil {
		tmplSpec["networks"] = networks
	}
	return map[string]any{
		"spec": map[string]any{
			"runStrategy": "Halted",
			"template":    map[string]any{"spec": tmplSpec},
		},
	}
}

func vmObj(ns, name string, annotations map[string]string, labels map[string]string, body map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: body}
	u.SetGroupVersionKind(vmGVK)
	u.SetNamespace(ns)
	u.SetName(name)
	for k, v := range annotations {
		u.SetAnnotations(merge(u.GetAnnotations(), k, v))
	}
	for k, v := range labels {
		u.SetLabels(merge(u.GetLabels(), k, v))
	}
	return u
}

func merge(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

func createVM(t *testing.T, ns, name string, annotations, labels map[string]string, body map[string]any) error {
	t.Helper()
	return k8s.Create(ctx, vmObj(ns, name, annotations, labels, body))
}

func mustCreateVM(t *testing.T, ns, name string, annotations, labels map[string]string, body map[string]any) {
	t.Helper()
	if err := createVM(t, ns, name, annotations, labels, body); err != nil {
		t.Fatalf("creating VM %s/%s: %v", ns, name, err)
	}
}

func getVM(t *testing.T, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(vmGVK)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, u); err != nil {
		t.Fatalf("getting VM %s/%s: %v", ns, name, err)
	}
	return u
}

func netsOf(t *testing.T, u *unstructured.Unstructured) []any {
	t.Helper()
	n, _, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "networks")
	if err != nil {
		t.Fatalf("reading networks: %v", err)
	}
	return n
}

func ifacesOf(t *testing.T, u *unstructured.Unstructured) []any {
	t.Helper()
	i, _, err := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
	if err != nil {
		t.Fatalf("reading interfaces: %v", err)
	}
	return i
}

func ipamAnn() map[string]string {
	return map[string]string{"ipam.cubestack.io/pool": "dev-ip-pool"}
}

// ---------------------------------------------------------------------------
// the matrix
// ---------------------------------------------------------------------------

// Rows 1 and 2: does the policy fire on a custom resource at all, and does the
// append work when the array is absent? This is the pool-template case -- a
// template that declares no network of its own.
func TestInjectOnAbsentArrays(t *testing.T) {
	name := "absent-arrays"
	mustCreateVM(t, nsOK, name, ipamAnn(), nil, vmBody(nil, nil))

	got := getVM(t, nsOK, name)

	nets := netsOf(t, got)
	if len(nets) != 1 {
		t.Fatalf("networks: want 1 entry, got %d: %#v", len(nets), nets)
	}
	multus, _, _ := unstructured.NestedMap(nets[0].(map[string]any), "multus")
	if want := nsOK + "/" + name + "-static"; multus["networkName"] != want {
		t.Errorf("networkName: want %q, got %#v", want, multus["networkName"])
	}
	t.Logf("PASS row 1 (MAP fires on kubevirt.io/v1 CR): networks=%#v", nets)

	ifaces := ifacesOf(t, got)
	if len(ifaces) != 1 {
		t.Fatalf("interfaces: want 1 entry, got %d: %#v", len(ifaces), ifaces)
	}
	if ifaces[0].(map[string]any)["name"] != "underlay" {
		t.Errorf("interface name: want underlay, got %#v", ifaces[0])
	}
	t.Logf("PASS row 2 (/- append, and array creation when absent): interfaces=%#v", ifaces)
}

// Row 3: an existing, unrelated network survives. The existing entry must be a
// non-multus one, or the policy's own guard correctly declines to fire -- which is
// the next test.
func TestInjectAppendsWithoutClobbering(t *testing.T) {
	name := "existing-net"
	body := vmBody(
		[]any{map[string]any{"name": "pod-net", "pod": map[string]any{}}},
		[]any{map[string]any{"name": "pod-net", "masquerade": map[string]any{}}},
	)
	mustCreateVM(t, nsOK, name, ipamAnn(), nil, body)

	nets := netsOf(t, getVM(t, nsOK, name))
	if len(nets) != 2 {
		t.Fatalf("networks: want 2 entries (existing + injected), got %d: %#v", len(nets), nets)
	}
	if nets[0].(map[string]any)["name"] != "pod-net" {
		t.Errorf("existing network was disturbed: %#v", nets[0])
	}
	if nets[1].(map[string]any)["name"] != "underlay" {
		t.Errorf("injected network is not last: %#v", nets[1])
	}
	t.Logf("PASS row 3 (existing entry preserved, ours appended): %#v", nets)
}

// Row 4: reinvocation is safe. An UPDATE re-runs the whole admission chain, and
// reinvocationPolicy: IfNeeded reruns this policy when anything else changes the
// object -- so a policy without the guard would grow a second network here.
func TestIdempotentAcrossUpdates(t *testing.T) {
	name := "idempotent"
	mustCreateVM(t, nsOK, name, ipamAnn(), nil, vmBody(nil, nil))

	// Three updates, each an admission request in its own right.
	for i := 0; i < 3; i++ {
		got := getVM(t, nsOK, name)
		got.SetLabels(merge(got.GetLabels(), "touch", fmt.Sprintf("%d", i)))
		if err := k8s.Update(ctx, got); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}

	nets := netsOf(t, getVM(t, nsOK, name))
	if len(nets) != 1 {
		t.Fatalf("networks: want exactly 1 after 3 updates, got %d: %#v", len(nets), nets)
	}
	t.Logf("PASS row 4 (idempotent across 3 updates): %#v", nets)
}

// Row 5: an annotated VM that brings its own multus NIC still gets the underlay.
//
// This is the RoCE/SR-IOV case, and it is the reason the guard tests for this
// policy's own reference rather than for "any multus network". A VM that passes a VF
// through on its own NAD has a multus network already; it still needs the L2 underlay,
// and with the broader test it would silently not get one -- while its pool claim went
// on holding an address.
func TestGuardAddsUnderlayBesideAnotherMultusNIC(t *testing.T) {
	name := "roce-and-underlay"
	body := vmBody(
		[]any{
			map[string]any{"name": "roce-net", "multus": map[string]any{"networkName": "default/roce-vf-nad"}},
			map[string]any{"name": "pod-net", "pod": map[string]any{}},
		},
		[]any{
			map[string]any{"name": "roce-net", "sriov": map[string]any{}},
			map[string]any{"name": "pod-net", "masquerade": map[string]any{}},
		},
	)
	mustCreateVM(t, nsOK, name, ipamAnn(), nil, body)

	nets := netsOf(t, getVM(t, nsOK, name))
	if len(nets) != 3 {
		t.Fatalf("networks: want 3 (roce, pod, injected underlay), got %d: %#v", len(nets), nets)
	}
	if got := nets[0].(map[string]any)["name"]; got != "roce-net" {
		t.Errorf("the VM's own multus NIC was disturbed: %#v", nets[0])
	}
	if got := nets[1].(map[string]any)["name"]; got != "pod-net" {
		t.Errorf("the pod network was disturbed: %#v", nets[1])
	}
	multus, _, _ := unstructured.NestedMap(nets[2].(map[string]any), "multus")
	if want := nsOK + "/" + name + "-static"; multus["networkName"] != want {
		t.Errorf("injected networkName: want %s, got %#v", want, multus["networkName"])
	}
	t.Logf("PASS row 5a (underlay added beside another multus NIC): %#v", nets)
}

// Row 5b: the guard's actual subject -- the policy's own reference. A VM that already
// names the NAD this policy would inject is left alone. Without this the reinvocation
// of row 4 would append a second copy.
func TestGuardLeavesAnAlreadyInjectedReferenceAlone(t *testing.T) {
	name := "already-injected"
	body := vmBody(
		[]any{map[string]any{"name": "underlay", "multus": map[string]any{"networkName": nsOK + "/" + name + "-static"}}},
		[]any{map[string]any{"name": "underlay", "bridge": map[string]any{}}},
	)
	mustCreateVM(t, nsOK, name, ipamAnn(), nil, body)

	nets := netsOf(t, getVM(t, nsOK, name))
	if len(nets) != 1 {
		t.Fatalf("networks: want the original 1 entry, got %d: %#v", len(nets), nets)
	}
	multus, _, _ := unstructured.NestedMap(nets[0].(map[string]any), "multus")
	if want := nsOK + "/" + name + "-static"; multus["networkName"] != want {
		t.Errorf("existing network was modified: want %s, got %#v", want, nets[0])
	}
	t.Logf("PASS row 5b (a VM already naming our NAD is not touched)")
}

// Row 5c: the override. ipam.cubestack.io/nad-name is honoured by the controller
// (virtualmachine_controller.go reads it before DefaultNADName), so the policy has to
// honour it too or it would inject a reference to a NAD the controller never mints.
func TestNadNameAnnotationOverridesTheDerivedName(t *testing.T) {
	name := "custom-nad"
	ann := ipamAnn()
	ann["ipam.cubestack.io/nad-name"] = "hand-picked-nad"
	mustCreateVM(t, nsOK, name, ann, nil, vmBody(nil, nil))

	nets := netsOf(t, getVM(t, nsOK, name))
	if len(nets) != 1 {
		t.Fatalf("networks: want 1, got %d: %#v", len(nets), nets)
	}
	multus, _, _ := unstructured.NestedMap(nets[0].(map[string]any), "multus")
	if want := nsOK + "/hand-picked-nad"; multus["networkName"] != want {
		t.Errorf("networkName: want %s, got %#v", want, multus["networkName"])
	}
	t.Logf("PASS row 5c (nad-name annotation honoured): %#v", nets)
}

// Row 5d: an empty override falls back, matching virtualmachine_controller.go's
// `if nadName == ""` rather than producing a reference to a NAD called "".
func TestEmptyNadNameAnnotationFallsBack(t *testing.T) {
	name := "empty-nad-ann"
	ann := ipamAnn()
	ann["ipam.cubestack.io/nad-name"] = ""
	mustCreateVM(t, nsOK, name, ann, nil, vmBody(nil, nil))

	nets := netsOf(t, getVM(t, nsOK, name))
	if len(nets) != 1 {
		t.Fatalf("networks: want 1, got %d: %#v", len(nets), nets)
	}
	multus, _, _ := unstructured.NestedMap(nets[0].(map[string]any), "multus")
	if want := nsOK + "/" + name + "-static"; multus["networkName"] != want {
		t.Errorf("networkName: want the derived fallback %s, got %#v", want, multus["networkName"])
	}
	t.Logf("PASS row 5d (empty annotation falls back to the derived name): %#v", nets)
}

// Row 5, the other half: a VM with no ipam annotation is not touched either.
func TestUnannotatedVMUntouched(t *testing.T) {
	name := "no-annotation"
	mustCreateVM(t, nsOK, name, nil, nil, vmBody(nil, nil))

	got := getVM(t, nsOK, name)
	if nets := netsOf(t, got); len(nets) != 0 {
		t.Fatalf("networks: want none, got %#v", nets)
	}
	t.Logf("PASS row 6 (annotation is a working trigger and a working scope)")
}

// Row 7: what a CEL error costs. This is the operational risk that matters most,
// because the expression is the part a human gets wrong.
func TestUnguardedExpressionRejectsTheRequest(t *testing.T) {
	err := createVM(t, nsUnguarded, "unguarded", ipamAnn(), nil, vmBody(nil, nil))
	if err == nil {
		t.Fatalf("expected the unguarded policy to reject a VM with no networks, but the create succeeded")
	}
	t.Logf("row 7 (failurePolicy: Fail, CEL runtime error) -- the create was rejected:")
	t.Logf("    %v", err)
	t.Log("    NOTE: this is the blast radius. A CEL bug in the expression does not")
	t.Log("    silently skip injection -- it blocks every VirtualMachine create in scope.")
}

// Row 9: generateName. The injected NAD name is derived from object.metadata.name,
// so if the name is generated after admission the policy cannot write the right one.
//
// It is worse than "cannot": the derivation does not degrade, it errors. So this is
// not a limitation that shows up as a wrong NAD name, it is one that shows up as
// every generateName create being rejected. Recorded as such.
func TestGenerateNameBehaviour(t *testing.T) {
	u := vmObj(nsOK, "", ipamAnn(), nil, vmBody(nil, nil))
	u.SetName("")
	u.SetGenerateName("generated-")

	err := k8s.Create(ctx, u)
	if err == nil {
		name := u.GetName()
		multus := ""
		if nets := netsOf(t, u); len(nets) > 0 {
			m, _, _ := unstructured.NestedMap(nets[0].(map[string]any), "multus")
			multus, _ = m["networkName"].(string)
		}
		t.Logf("row 9 (generateName): the create was allowed; name=%q networkName=%q", name, multus)
		if want := nsOK + "/" + name + "-static"; multus != want {
			t.Errorf("injected name %q does not match the generated name %q", multus, name)
		}
		return
	}

	t.Logf("row 9 (generateName): REJECTED -- %s", firstLine(err.Error()))
	t.Log("    The apiserver generates metadata.name after admission, so")
	t.Log("    object.metadata.name is absent when the policy runs, and the concatenation")
	t.Log("    errors rather than yielding an empty name. Under failurePolicy: Fail that")
	t.Log("    rejects the request: VMs in scope cannot be created with generateName at all.")
	t.Log("    A VirtualMachinePool creates members with explicit names, so this does not")
	t.Log("    affect the pool path -- but any other creator using generateName in a bound")
	t.Log("    namespace is blocked, which is a scoping hazard rather than a policy bug.")
	t.Log("    Hardening if this is ever adopted: gate the whole mutation on")
	t.Log("    has(object.metadata.name) and emit no patches when it is absent.")
}

// Row 8: the negative control. The API doc says an apply configuration "may not
// modify atomic structs, maps or arrays", and networks/interfaces are atomic
// because KubeVirt declares no merge key. Rather than take that on trust, try it.
func TestApplyConfigurationNegativeControl(t *testing.T) {
	var policy unstructured.Unstructured
	policy.SetAPIVersion("admissionregistration.k8s.io/" + mapAPIVersion)
	policy.SetKind("MutatingAdmissionPolicy")
	if err := k8s.Get(ctx, client.ObjectKey{Name: "spike-applycfg"}, &policy); err != nil {
		t.Logf("row 8 (ApplyConfiguration): the policy was never admitted -- the apiserver rejected it at creation.")
		t.Log("    That is a stronger result than the doc predicts: for a CRD there is no")
		t.Log("    registered Go type, so Object.spec... has no apply-configuration type to")
		t.Log("    construct at all. ApplyConfiguration is not merely unsuitable here, it is")
		t.Log("    unavailable. JSONPatch is forced.")
		return
	}

	err := createVM(t, nsApplyCfg, "applycfg", ipamAnn(), nil, vmBody(nil, nil))
	if err != nil {
		t.Logf("row 8 (ApplyConfiguration): rejected at evaluation -- %v", err)
		return
	}

	got := getVM(t, nsApplyCfg, "applycfg")
	nets := netsOf(t, got)
	ifaces := ifacesOf(t, got)
	t.Logf("row 8 (ApplyConfiguration): accepted. networks=%#v interfaces=%#v", nets, ifaces)
	t.Log("    If networks were replaced rather than merged, that is the atomic-array")
	t.Log("    hazard the doc warns about, demonstrated.")
}

// ---------------------------------------------------------------------------
// row 10: ordering against a mutating webhook
// ---------------------------------------------------------------------------

type observation struct {
	mode        string
	vm          string
	sawNetworks bool
	numNetworks int
	numIfaces   int
}

var (
	obsMu    sync.Mutex
	observed []observation
)

// TestOrderingAgainstMutatingWebhook measures something the API explicitly refuses
// to promise. The doc for reinvocationPolicy says mutations carry "no guarantee of
// order with respect to other admission plugins, admission webhooks, bindings of
// this policy and admission policies", and this cluster runs six mutating webhooks
// that touch VMs -- kubemacpool writes interfaces[], virt-api-mutator writes the VM.
//
// No amount of reading settles whether the injection survives them, so: stand a
// webhook up locally and look. Two variants, routed by a label the webhook sees.
//
//   - additive:    what kubemacpool actually does -- adds an annotation, and would
//     add a MAC to an interface. Harmless if it runs either way.
//   - destructive: replaces the whole interfaces array. This is the shape that
//     would drop the injection, and the one worth knowing about.
//
// The webhook also records whether it saw a networks[] array, which is how the
// implemented order becomes visible rather than inferred.
func TestOrderingAgainstMutatingWebhook(t *testing.T) {
	srv, closeSrv := startSpikeWebhook(t)
	defer closeSrv()

	none := admissionregistrationv1.SideEffectClassNone
	fail := admissionregistrationv1.Fail

	for _, mode := range []string{"additive", "destructive"} {
		cfg := &admissionregistrationv1.MutatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "spike-" + mode},
			Webhooks: []admissionregistrationv1.MutatingWebhook{{
				Name:                    "spike-" + mode + ".mapspike.example.com",
				AdmissionReviewVersions: []string{"v1"},
				SideEffects:             &none,
				FailurePolicy:           &fail,
				ReinvocationPolicy:      ptr.To(admissionregistrationv1.NeverReinvocationPolicy),
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					URL: ptr.To(srv.URL),
					// httptest hands back a DER certificate; the API wants PEM. Passing
					// Raw directly fails at call time with "unable to parse bytes as PEM
					// block", not at configuration time -- so the config is accepted and
					// every VM create then fails.
					CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}),
				},
				// Routed by label so both variants can exist at once and the server
				// can tell them apart without keeping state.
				ObjectSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"spike-mode": mode},
				},
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{
						admissionregistrationv1.Create, admissionregistrationv1.Update,
					},
					Rule: admissionregistrationv1.Rule{
						APIGroups:   []string{"kubevirt.io"},
						APIVersions: []string{"v1"},
						Resources:   []string{"virtualmachines"},
					},
				}},
			}},
		}
		if err := k8s.Create(ctx, cfg); err != nil {
			t.Fatalf("creating webhook config %s: %v", mode, err)
		}
	}

	// Webhook configurations are read from an informer; give it a moment rather
	// than debug a timing artefact as a behaviour difference.
	time.Sleep(3 * time.Second)

	for _, mode := range []string{"additive", "destructive"} {
		name := "order-" + mode
		labels := map[string]string{"spike-mode": mode}
		if err := createVM(t, nsOrder, name, ipamAnn(), labels, vmBody(nil, nil)); err != nil {
			t.Fatalf("[%s] creating VM: %v", mode, err)
		}

		got := getVM(t, nsOrder, name)
		nets := netsOf(t, got)
		ifaces := ifacesOf(t, got)

		t.Logf("--- %s webhook ---", mode)
		for _, o := range snapshot() {
			if o.vm == name {
				t.Logf("  webhook saw: networks present=%v (%d), interfaces=%d", o.sawNetworks, o.numNetworks, o.numIfaces)
			}
		}
		if len(nets) == 0 {
			// The webhook may have run before MAP; give reinvocation a beat.
			time.Sleep(1 * time.Second)
			got = getVM(t, nsOrder, name)
			nets = netsOf(t, got)
			ifaces = ifacesOf(t, got)
		}
		t.Logf("  final:      networks=%d %#v", len(nets), netNames(nets))
		t.Logf("  final:      interfaces=%d %#v", len(ifaces), ifaceNames(ifaces))

		// The verdict has to look at both halves of the injection, not just the
		// network. An earlier version of this test checked networks alone and
		// reported "survived" for the destructive webhook -- while the interface
		// that makes the network usable had been thrown away.
		netOK := containsName(nets, "underlay")
		ifaceOK := containsName(ifaces, "underlay")

		switch {
		case netOK && ifaceOK:
			t.Logf("  VERDICT (full):     both the network and its interface survived")
		case netOK && !ifaceOK:
			t.Logf("  VERDICT (partial):  the network survived but the interface did not.")
			t.Logf("      The object is left with a multus network and no matching interface.")
			t.Logf("      Whether KubeVirt's validating webhook rejects that shape is NOT")
			t.Logf("      settled here -- this harness installs a CRD with no validation -- but")
			t.Logf("      at best it is a network that cannot carry traffic. Worse, the")
			t.Logf("      no-multus-yet guard now sees a multus network and declines to repair")
			t.Logf("      it, so reinvocation does not recover. Whether this is reachable")
			t.Logf("      depends on whether any mutating webhook on the cluster replaces")
			t.Logf("      interfaces[] wholesale rather than adding to it; kubemacpool adds")
			t.Logf("      MACs, which is the additive shape.")
		case !netOK:
			t.Logf("  VERDICT (lost):     the injected network itself was dropped")
		}

		if !netOK {
			t.Errorf("%s webhook: the injected network did not survive", mode)
		}
	}
}

func containsName(items []any, name string) bool {
	for _, n := range netNames(items) {
		if n == name {
			return true
		}
	}
	return false
}

func netNames(nets []any) []string {
	var out []string
	for _, n := range nets {
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		nm, _ := m["name"].(string)
		out = append(out, nm)
	}
	return out
}

func ifaceNames(ifaces []any) []string {
	var out []string
	for _, i := range ifaces {
		m, ok := i.(map[string]any)
		if !ok {
			continue
		}
		nm, _ := m["name"].(string)
		out = append(out, nm)
	}
	return out
}

func snapshot() []observation {
	obsMu.Lock()
	defer obsMu.Unlock()
	return append([]observation(nil), observed...)
}

// startSpikeWebhook serves a mutating webhook over TLS. httptest provides both the
// server and a certificate whose raw bytes can be handed to the apiserver as the
// CA bundle, which is the whole reason this does not need envtest's webhook
// plumbing or a cert job.
func startSpikeWebhook(t *testing.T) (*httptest.Server, func()) {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read", http.StatusBadRequest)
			return
		}
		var review admissionv1.AdmissionReview
		if err := json.Unmarshal(body, &review); err != nil || review.Request == nil {
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}

		var obj unstructured.Unstructured
		_ = json.Unmarshal(review.Request.Object.Raw, &obj)

		nets, sawNets, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "networks")
		ifaces, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "domain", "devices", "interfaces")
		mode := obj.GetLabels()["spike-mode"]

		obsMu.Lock()
		observed = append(observed, observation{
			mode: mode, vm: obj.GetName(),
			sawNetworks: sawNets, numNetworks: len(nets), numIfaces: len(ifaces),
		})
		obsMu.Unlock()

		resp := admissionv1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"},
			Response: &admissionv1.AdmissionResponse{UID: review.Request.UID, Allowed: true},
		}
		if mode == "destructive" {
			pt := admissionv1.PatchTypeJSONPatch
			resp.Response.PatchType = &pt
			resp.Response.Patch = []byte(`[{"op":"add","path":"/spec/template/spec/domain/devices/interfaces","value":[{"name":"business-net","bridge":{}}]}]`)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	return srv, srv.Close
}
