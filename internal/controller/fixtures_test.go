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
	claim := &ipamv1alpha1.IPRequest{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       ipamv1alpha1.IPRequestSpec{PoolRef: pool, NAD: nad},
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

// getClaim reads a claim, so assertions run against what the API server holds
// rather than against the in-memory object the reconciler mutated.
func getClaim(namespace, name string) *ipamv1alpha1.IPRequest {
	claim := &ipamv1alpha1.IPRequest{}
	ExpectWithOffset(1, k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, claim)).To(Succeed())
	return claim
}

// requestFor builds the reconcile request for a namespaced object.
func requestFor(namespace, name string) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}
