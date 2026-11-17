/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"os"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

// These specs cover the CRD schema itself, not the reconciler.
//
// The IPPool schema carries CEL rules for subnet containment. The Kubernetes CEL
// net library does NOT define ordering overloads on IP values, so a well-meaning
// "start <= end" rule is silently un-writable — it fails only when the CRD is
// installed, which surfaces as every spec in this package failing at BeforeSuite
// with a wall of CEL compiler output. That is a bad way to learn about it. These
// specs assert the rules that DO exist are present and actually enforced, so an
// invalid future rule fails here with a legible message.
//
// The ordering and gateway invariants that CEL cannot express are covered by
// ipam.ValidatePoolRange and its unit tests.
var _ = Describe("IPPool schema validation", func() {
	ctx := context.Background()

	newPool := func(mutate func(*ipamv1alpha1.IPPoolSpec)) *ipamv1alpha1.IPPool {
		spec := ipamv1alpha1.IPPoolSpec{
			Range: ipamv1alpha1.IPRange{Start: "10.66.3.150", End: "10.66.3.189"},
			NADTemplate: &ipamv1alpha1.NADTemplate{
				Subnet:  "10.66.3.0/24",
				Gateway: "10.66.3.254",
				Bridge:  "br0",
			},
		}
		mutate(&spec)
		return &ipamv1alpha1.IPPool{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-"},
			Spec:       spec,
		}
	}

	It("accepts the band the design proposes", func() {
		pool := newPool(func(*ipamv1alpha1.IPPoolSpec) {})
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })
	})

	It("rejects a range outside the subnet", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) {
			s.Range = ipamv1alpha1.IPRange{Start: "10.66.9.10", End: "10.66.9.20"}
		})
		err := k8sClient.Create(ctx, pool)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("must lie within the nadTemplate subnet"))
	})

	It("rejects a gateway that is not an address", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate.Gateway = "not-an-ip" })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	It("rejects an IPRequest with no poolRef", func() {
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec:       ipamv1alpha1.IPRequestSpec{},
		}
		Expect(k8sClient.Create(ctx, ipReq)).NotTo(Succeed())
	})

	// The minimal form: a band and nothing else. Nothing mints from this pool, so
	// it needs no subnet and no gateway, and the CRD must not demand them.
	It("accepts a pool with no nadTemplate", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate = nil })
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })
	})

	// A pool must declare a usable band; it is the one field with no default. (The
	// typed client always serialises range, so this exercises the bound patterns
	// rather than the required-ness of the field itself.)
	It("rejects a pool whose range bounds are empty", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.Range = ipamv1alpha1.IPRange{} })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	// The nadTemplate is typed precisely so that this class of mistake is caught
	// here instead of surfacing as a VM stuck in FailedCreatePodSandBox. Subnet and
	// gateway are required for the same reason: a template is all-or-nothing, which
	// is what removes the need for a cross-field rule saying so.
	It("rejects a nadTemplate with no bridge", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate.Bridge = "" })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	It("rejects a nadTemplate with no subnet", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate.Subnet = "" })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	It("rejects a nadTemplate with no gateway", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate.Gateway = "" })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	It("rejects a subnet that is not a CIDR", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate.Subnet = "10.66.3.0" })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	It("rejects a VLAN outside the 802.1Q range", func() {
		vlan := int32(9999)
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate.VLAN = &vlan })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	It("rejects an unknown CNI plugin type", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) { s.NADTemplate.Type = "sriov" })
		Expect(k8sClient.Create(ctx, pool)).NotTo(Succeed())
	})

	It("rejects an IPRequest whose nad is not a DNS subdomain", func() {
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec:       ipamv1alpha1.IPRequestSpec{PoolRef: "any", NAD: "Not_A_Name"},
		}
		Expect(k8sClient.Create(ctx, ipReq)).NotTo(Succeed())
	})

	It("rejects a change to an IPRequest's poolRef", func() {
		// Retrofitted when the block API made the same rule necessary there, and it
		// reinforces an invariant the ledger already rested on: a claim's address
		// belongs to the pool it was allocated from. Re-pointing the reference would
		// free that address in the old pool while the NAD minted for the claim still
		// carried it.
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec:       ipamv1alpha1.IPRequestSpec{PoolRef: "one-pool"},
		}
		Expect(k8sClient.Create(ctx, ipReq)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ipReq) })

		ipReq.Spec.PoolRef = "another-pool"
		err := k8sClient.Update(ctx, ipReq)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("poolRef is immutable"))
	})

	It("accepts a pool with an allocation policy", func() {
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) {
			s.Allocation = &ipamv1alpha1.PoolAllocation{
				Exclude: []ipamv1alpha1.IPRange{{Start: "10.66.3.150", End: "10.66.3.159"}},
			}
		})
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })
	})

	It("accepts a pool that forbids exact allocation", func() {
		allow := false
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) {
			s.Allocation = &ipamv1alpha1.PoolAllocation{AllowExact: &allow}
		})
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })
	})

	It("defaults allowExact to true", func() {
		// The default is the feature being available without every pool opting in.
		// Asserted through the API server so it is the CRD's default, not a Go
		// fallback that a hand-written object would not get.
		pool := newPool(func(s *ipamv1alpha1.IPPoolSpec) {
			s.Allocation = &ipamv1alpha1.PoolAllocation{}
		})
		Expect(k8sClient.Create(ctx, pool)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, pool) })

		stored := &ipamv1alpha1.IPPool{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), stored)).To(Succeed())
		Expect(stored.Spec.Allocation.AllowExact).NotTo(BeNil())
		Expect(*stored.Spec.Allocation.AllowExact).To(BeTrue())
	})

	It("accepts an IPRequest that asks for a specific address", func() {
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec: ipamv1alpha1.IPRequestSpec{
				PoolRef: "any-pool",
				Allocation: &ipamv1alpha1.AddressAllocation{
					Strategy: ipamv1alpha1.AllocationExact,
					Address:  "10.66.3.160",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ipReq)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ipReq) })
	})

	It("accepts an IPRequest with no allocation block", func() {
		// The backward-compatibility case: objects created before the field existed,
		// and hand-written claims that just want the lowest free address.
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec:       ipamv1alpha1.IPRequestSpec{PoolRef: "any-pool"},
		}
		Expect(k8sClient.Create(ctx, ipReq)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ipReq) })
	})

	It("rejects an Exact IPRequest with no address", func() {
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec: ipamv1alpha1.IPRequestSpec{
				PoolRef:    "any-pool",
				Allocation: &ipamv1alpha1.AddressAllocation{Strategy: ipamv1alpha1.AllocationExact},
			},
		}
		Expect(k8sClient.Create(ctx, ipReq)).NotTo(Succeed())
	})

	It("rejects an Auto IPRequest that carries an address", func() {
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec: ipamv1alpha1.IPRequestSpec{
				PoolRef: "any-pool",
				Allocation: &ipamv1alpha1.AddressAllocation{
					Strategy: ipamv1alpha1.AllocationAuto,
					Address:  "10.66.3.160",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ipReq)).NotTo(Succeed())
	})

	It("rejects a requested address that is not an IPv4 address", func() {
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec: ipamv1alpha1.IPRequestSpec{
				PoolRef: "any-pool",
				Allocation: &ipamv1alpha1.AddressAllocation{
					Strategy: ipamv1alpha1.AllocationExact,
					Address:  "not-an-ip",
				},
			},
		}
		Expect(k8sClient.Create(ctx, ipReq)).NotTo(Succeed())
	})

	It("rejects a change to an IPRequest's allocation", func() {
		ipReq := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec: ipamv1alpha1.IPRequestSpec{
				PoolRef: "any-pool",
				Allocation: &ipamv1alpha1.AddressAllocation{
					Strategy: ipamv1alpha1.AllocationAuto,
				},
			},
		}
		Expect(k8sClient.Create(ctx, ipReq)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, ipReq) })

		ipReq.Spec.Allocation = &ipamv1alpha1.AddressAllocation{
			Strategy: ipamv1alpha1.AllocationExact,
			Address:  "10.66.3.160",
		}
		err := k8sClient.Update(ctx, ipReq)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("allocation is immutable"))
	})
})

// The samples in config/samples are what a user copies first, so a broken one is
// the worst possible first impression. The design's verification step is
// `kubectl apply --dry-run=client -f config/samples/`, which cannot run without
// the CRDs installed anywhere; this asserts the same thing against envtest.
var _ = Describe("Shipped samples", func() {
	ctx := context.Background()

	for _, path := range []string{
		"../../config/samples/ipam_v1alpha1_ippool.yaml",
		"../../config/samples/ipam_v1alpha1_ippool_minimal.yaml",
		"../../config/samples/ipam_v1alpha1_iprequest.yaml",
		"../../config/samples/ipam_v1alpha1_iprangerequest.yaml",
		"../../config/samples/kubevirt_v1_virtualmachine.yaml",
	} {
		It("satisfies the schema: "+path, func() {
			raw, err := os.ReadFile(path)
			Expect(err).NotTo(HaveOccurred())

			var obj client.Object
			switch {
			case strings.Contains(string(raw), "kind: IPPool"):
				obj = &ipamv1alpha1.IPPool{}
			// Checked before IPRequest, whose substring it would otherwise be
			// mistaken for by a reader -- though not, as it happens, by this switch:
			// "kind: IPRequest" is not a substring of "kind: IPRangeRequest".
			case strings.Contains(string(raw), "kind: IPRangeRequest"):
				obj = &ipamv1alpha1.IPRangeRequest{}
			case strings.Contains(string(raw), "kind: IPRequest"):
				obj = &ipamv1alpha1.IPRequest{}
			case strings.Contains(string(raw), "kind: VirtualMachine"):
				// Unstructured because the VM is a third-party type this project
				// does not vendor — see testdata/virtualmachine-crd.yaml.
				vm := &unstructured.Unstructured{}
				vm.SetGroupVersionKind(virtualMachineGVK)
				obj = vm
			default:
				Fail("unrecognised sample kind in " + path)
			}

			Expect(yaml.Unmarshal(raw, obj)).To(Succeed())
			Expect(k8sClient.Create(ctx, obj)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(ctx, obj) })
		})
	}
})
