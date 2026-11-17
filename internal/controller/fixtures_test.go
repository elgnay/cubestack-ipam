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
	"sort"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

// The band the design proposes for the 10.66.3.0/24 underlay (design §5.4): three
// addresses, which is enough to observe lowest-first allocation, the taken set and
// exhaustion without a large fixture.
const (
	testSubnet   = "10.66.3.0/24"
	testGateway  = "10.66.3.254"
	testFirstIP  = "10.66.3.150"
	testSecondIP = "10.66.3.151"
	testThirdIP  = "10.66.3.152"
)

// The band the block specs run on: an eight-address extension of the three above.
// Three addresses are exactly right for observing lowest-first allocation and
// exhaustion on single addresses, but a block needs room to be one — two requests
// and a claim have to fit, and a fragmentation case needs free addresses that do
// not line up.
const (
	testFourthIP     = "10.66.3.153"
	testFifthIP      = "10.66.3.154"
	testSixthIP      = "10.66.3.155"
	testBlockBandEnd = "10.66.3.157"
)

// testPoolSpec is a pool with a template, i.e. one that can mint NADs.
//
// It selects static explicitly rather than leaning on the field being absent, so
// that the ipam assertions in this suite are about the mode they name. The
// whereabouts form has its own pool below, and the default that an omitted field
// falls back to is pinned by its own test -- those are three different facts and a
// shared fixture would blur them.
func testPoolSpec() ipamv1alpha1.IPPoolSpec {
	return ipamv1alpha1.IPPoolSpec{
		Range: ipamv1alpha1.IPRange{Start: testFirstIP, End: testThirdIP},
		NADTemplate: &ipamv1alpha1.NADTemplate{
			Subnet:  testSubnet,
			Gateway: testGateway,
			IPAM:    "static",
			Bridge:  "br0",
			DNS:     &ipamv1alpha1.NADDNS{Nameservers: []string{testGateway}},
			Routes:  []ipamv1alpha1.NADRoute{{Dst: "0.0.0.0/0", GW: testGateway}},
		},
	}
}

// testWhereaboutsPoolSpec is testPoolSpec with the address leased from Whereabouts
// instead, exchanging migratability for exclusivity the CNI enforces.
func testWhereaboutsPoolSpec() ipamv1alpha1.IPPoolSpec {
	spec := testPoolSpec()
	spec.NADTemplate.IPAM = "whereabouts"
	return spec
}

// testLedgerOnlySpec is the minimal pool: a band and nothing else. It allocates
// addresses and mints no NAD, which is why it needs no subnet and no gateway.
func testLedgerOnlySpec() ipamv1alpha1.IPPoolSpec {
	return ipamv1alpha1.IPPoolSpec{
		Range: ipamv1alpha1.IPRange{Start: testFirstIP, End: testThirdIP},
	}
}

// testBlockPoolSpec is testPoolSpec over the eight-address band the block specs
// need, from testFirstIP through testBlockBandEnd.
func testBlockPoolSpec() ipamv1alpha1.IPPoolSpec {
	spec := testPoolSpec()
	spec.Range = ipamv1alpha1.IPRange{Start: testFirstIP, End: testBlockBandEnd}
	return spec
}

// createPool creates a cluster-scoped pool and removes it when the spec ends.
func createPool(name string, spec ipamv1alpha1.IPPoolSpec) {
	pool := &ipamv1alpha1.IPPool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, pool)).To(Succeed())
	DeferCleanup(func() {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, pool))).To(Succeed())
	})
}

// createClaim creates a claim in the given namespace and removes it when the spec
// ends. It returns the object as created, so callers can reconcile it.
func createClaim(namespace, name, pool, nad string) *ipamv1alpha1.IPRequest {
	return createClaimWithAllocation(namespace, name, pool, nad, &ipamv1alpha1.AddressAllocation{
		Strategy: ipamv1alpha1.AllocationAuto,
	})
}

// createExactClaim creates a claim that asks for one named address.
func createExactClaim(namespace, name, pool, address string) *ipamv1alpha1.IPRequest {
	return createClaimWithAllocation(namespace, name, pool, "", &ipamv1alpha1.AddressAllocation{
		Strategy: ipamv1alpha1.AllocationExact,
		Address:  address,
	})
}

// createClaimWithAllocation is the shared create, written explicitly rather than
// left to an omitted allocation: these specs are about a strategy being named, and
// a fixture that relied on the absent-field fallback would not notice the fallback
// changing.
func createClaimWithAllocation(namespace, name, pool, nad string, allocation *ipamv1alpha1.AddressAllocation) *ipamv1alpha1.IPRequest {
	claim := &ipamv1alpha1.IPRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       ipamv1alpha1.IPRequestSpec{PoolRef: pool, NAD: nad, Allocation: allocation},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	DeferCleanup(func() {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
	})
	return claim
}

// deleteNAD removes a NAD. Nothing else does: envtest runs no garbage collector,
// so an ownerReference does not collect anything here.
func deleteNAD(namespace, name string) {
	nad := newNAD(namespace, name)
	DeferCleanup(func() {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, nad))).To(Succeed())
	})
}

// getNAD reads a NAD and fails the spec if it is absent.
func getNAD(namespace, name string) *unstructured.Unstructured {
	nad := newNAD(namespace, name)
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKeyFromObject(nad), nad)).To(Succeed())
	return nad
}

// listNADs returns the namespace/name of every NetworkAttachmentDefinition in the
// cluster, sorted. For specs asserting that nothing was created — which have no
// name to look up, and for which a negative lookup would pass on a typo.
func listNADs() []string {
	var nads unstructured.UnstructuredList
	nads.SetGroupVersionKind(networkAttachmentDefinitionGVK.GroupVersion().WithKind("NetworkAttachmentDefinitionList"))
	ExpectWithOffset(1, k8sClient.List(ctx, &nads)).To(Succeed())

	names := make([]string, 0, len(nads.Items))
	for i := range nads.Items {
		names = append(names, nads.Items[i].GetNamespace()+"/"+nads.Items[i].GetName())
	}
	sort.Strings(names)
	return names
}

// getClaim reads a claim, so assertions run against what the API server holds
// rather than against the in-memory object the reconciler mutated.
func getClaim(namespace, name string) *ipamv1alpha1.IPRequest {
	claim := &ipamv1alpha1.IPRequest{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, claim)).To(Succeed())
	return claim
}

// bindClaim records an address in a claim's status directly, as the claim
// reconciler would have.
//
// The audit specs need a collision to exist, and the allocator is precisely what
// makes one impossible — so the fixture writes the state instead of driving the
// thing under test to produce it.
func bindClaim(namespace, name, pool, addr string) *ipamv1alpha1.IPRequest {
	claim := createClaim(namespace, name, pool, "")
	claim.Status.Phase = ipamv1alpha1.IPRequestPhaseBound
	claim.Status.AssignedIP = addr
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, claim)).To(Succeed())
	return claim
}

// createRangeRequest creates an Auto block request in the given namespace and
// removes it when the spec ends.
func createRangeRequest(namespace, name, pool string, count int32) *ipamv1alpha1.IPRangeRequest {
	return createRangeRequestWithAllocation(namespace, name, pool, ipamv1alpha1.RangeAllocation{
		Strategy: ipamv1alpha1.AllocationAuto,
		Count:    &count,
	})
}

// createExactRangeRequest creates a block request that asks for one named band.
func createExactRangeRequest(namespace, name, pool, start, end string) *ipamv1alpha1.IPRangeRequest {
	return createRangeRequestWithAllocation(namespace, name, pool, ipamv1alpha1.RangeAllocation{
		Strategy: ipamv1alpha1.AllocationExact,
		Range:    &ipamv1alpha1.IPRange{Start: start, End: end},
	})
}

func createRangeRequestWithAllocation(namespace, name, pool string, allocation ipamv1alpha1.RangeAllocation) *ipamv1alpha1.IPRangeRequest {
	block := &ipamv1alpha1.IPRangeRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       ipamv1alpha1.IPRangeRequestSpec{PoolRef: pool, Allocation: allocation},
	}
	ExpectWithOffset(1, k8sClient.Create(ctx, block)).To(Succeed())
	DeferCleanup(func() {
		ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, block))).To(Succeed())
	})
	return block
}

// getRangeRequest reads a range request, for the same reason getClaim does.
func getRangeRequest(namespace, name string) *ipamv1alpha1.IPRangeRequest {
	block := &ipamv1alpha1.IPRangeRequest{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, block)).To(Succeed())
	return block
}

// bindRange records a block in a range request's status directly. Same reasoning
// as bindClaim: the audit's subject is what the sweep makes of a state, not how
// the state arose.
func bindRange(namespace, name, start, end string) {
	block := getRangeRequest(namespace, name)
	block.Status.Phase = ipamv1alpha1.IPRangeRequestPhaseBound
	block.Status.Start = start
	block.Status.End = end
	ExpectWithOffset(1, k8sClient.Status().Update(ctx, block)).To(Succeed())
}

// reconcileRange drives the block reconciler once, as the manager would.
func reconcileRange(namespace, name string) {
	r := &IPRangeRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := r.Reconcile(ctx, requestFor(namespace, name))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

// requestFor builds the reconcile request for a namespaced object.
func requestFor(namespace, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}
