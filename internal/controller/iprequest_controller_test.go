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
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

var _ = Describe("IPRequest Controller", func() {
	reconcileClaim := func(namespace, name string) {
		r := &IPRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// renderedConfig decodes a minted NAD's spec.config.
	renderedConfig := func(nad *unstructured.Unstructured) map[string]any {
		raw, found, err := unstructured.NestedString(nad.Object, "spec", "config")
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		ExpectWithOffset(1, found).To(BeTrue())

		var parsed map[string]any
		ExpectWithOffset(1, json.Unmarshal([]byte(raw), &parsed)).To(Succeed())
		return parsed
	}

	It("binds the lowest free address and mints the NAD", func() {
		createPool("claims-basic", testPoolSpec())
		createClaim("default", "claims-basic-ip", "claims-basic", "claims-basic-static")
		deleteNAD("default", "claims-basic-static")

		reconcileClaim("default", "claims-basic-ip")

		got := getClaim("default", "claims-basic-ip")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRequestPhaseBound))
		Expect(got.Status.AssignedIP).To(Equal(testFirstIP))
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ipamv1alpha1.ConditionReady)).To(BeTrue())
		Expect(got.UID).NotTo(BeEmpty())

		nad := getNAD("default", "claims-basic-static")
		Expect(metav1.IsControlledBy(nad, got)).To(BeTrue())
		Expect(nad.GetLabels()).To(HaveKeyWithValue(ipamv1alpha1.LabelRequest, "claims-basic-ip"))

		config := renderedConfig(nad)
		Expect(config).To(HaveKeyWithValue("name", "claims-basic-static"))
		Expect(config).To(HaveKeyWithValue("bridge", "br0"))

		ipam, ok := config["ipam"].(map[string]any)
		Expect(ok).To(BeTrue())
		// The claim's address is baked in as a single static assignment. The pool's
		// own fields remain the sole source of addressing -- the mask comes from
		// testSubnet, the gateway from the template -- but nothing arbitrates the
		// address at the CNI any more.
		Expect(ipam).To(HaveKeyWithValue("type", "static"))
		Expect(ipam).To(HaveKeyWithValue("addresses", []any{
			map[string]any{"address": testFirstIP + "/24", "gateway": testGateway},
		}))
	})

	It("gives the second claim the next address", func() {
		createPool("claims-two", testPoolSpec())
		createClaim("default", "claims-two-a", "claims-two", "")
		createClaim("default", "claims-two-b", "claims-two", "")

		reconcileClaim("default", "claims-two-a")
		reconcileClaim("default", "claims-two-b")

		Expect(getClaim("default", "claims-two-a").Status.AssignedIP).To(Equal(testFirstIP))
		Expect(getClaim("default", "claims-two-b").Status.AssignedIP).To(Equal(testSecondIP))
	})

	It("leases the address from Whereabouts when the pool asks for it", func() {
		createPool("claims-whereabouts", testWhereaboutsPoolSpec())
		createClaim("default", "claims-whereabouts-ip", "claims-whereabouts", "claims-whereabouts-static")
		deleteNAD("default", "claims-whereabouts-static")

		reconcileClaim("default", "claims-whereabouts-ip")

		ipam, ok := renderedConfig(getNAD("default", "claims-whereabouts-static"))["ipam"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(ipam).To(HaveKeyWithValue("type", "whereabouts"))
		// The ledger is keyed by the range's CIDR, so this must be the subnet and not
		// the pool's band: pointing it anywhere else puts this NAD on a different
		// ledger from the dynamic pods it has to collide against.
		Expect(ipam).To(HaveKeyWithValue("range", testSubnet))
		// Both bounds on the assigned address is what bounds the candidate set to one,
		// inside that shared ledger.
		Expect(ipam).To(HaveKeyWithValue("range_start", testFirstIP))
		Expect(ipam).To(HaveKeyWithValue("range_end", testFirstIP))
		// And the static form's key is absent: the two shapes share almost nothing, so
		// a leftover field here would be one mode bleeding into the other.
		Expect(ipam).NotTo(HaveKey("addresses"))
	})

	It("defaults an unset ipam to Whereabouts", func() {
		// The CRD's default, not just the renderer's. A pool created before the field
		// existed must come back with it set, or every such pool would quietly lose the
		// exclusivity the CNI was enforcing for it the first time it minted a NAD --
		// and the rendered config would look entirely plausible either way.
		spec := testPoolSpec()
		spec.NADTemplate.IPAM = ""
		createPool("claims-default-ipam", spec)

		pool := &ipamv1alpha1.IPPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "claims-default-ipam"}, pool)).To(Succeed())
		Expect(pool.Spec.NADTemplate.IPAM).To(Equal("whereabouts"))

		createClaim("default", "claims-default-ipam-ip", "claims-default-ipam", "claims-default-ipam-static")
		deleteNAD("default", "claims-default-ipam-static")
		reconcileClaim("default", "claims-default-ipam-ip")

		ipam, ok := renderedConfig(getNAD("default", "claims-default-ipam-static"))["ipam"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(ipam).To(HaveKeyWithValue("type", "whereabouts"))
	})

	It("binds a claim with no spec.nad without creating anything", func() {
		createPool("claims-ledger-only", testPoolSpec())
		createClaim("default", "claims-ledger-only-ip", "claims-ledger-only", "")

		reconcileClaim("default", "claims-ledger-only-ip")

		got := getClaim("default", "claims-ledger-only-ip")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRequestPhaseBound))
		Expect(got.Status.AssignedIP).To(Equal(testFirstIP))
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ipamv1alpha1.ConditionReady)).To(BeTrue())
	})

	It("keeps an address a claim already holds", func() {
		// The property that makes this layer worth having: a claim never moves. A
		// re-derivation here would let a lower free address displace a running VM.
		createPool("claims-sticky", testPoolSpec())
		claim := createClaim("default", "claims-sticky-ip", "claims-sticky", "")

		claim.Status.AssignedIP = testThirdIP
		Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())

		reconcileClaim("default", "claims-sticky-ip")

		got := getClaim("default", "claims-sticky-ip")
		Expect(got.Status.AssignedIP).To(Equal(testThirdIP))
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ipamv1alpha1.ConditionReady)).To(BeTrue())
	})

	It("reports exhaustion rather than over-allocating", func() {
		spec := testPoolSpec()
		// A band of exactly one address, so the second claim must fail.
		spec.Range = ipamv1alpha1.IPRange{Start: testFirstIP, End: testFirstIP}
		createPool("claims-exhausted", spec)
		createClaim("default", "claims-exhausted-a", "claims-exhausted", "")
		createClaim("default", "claims-exhausted-b", "claims-exhausted", "")

		reconcileClaim("default", "claims-exhausted-a")
		reconcileClaim("default", "claims-exhausted-b")

		Expect(getClaim("default", "claims-exhausted-a").Status.AssignedIP).To(Equal(testFirstIP))

		second := getClaim("default", "claims-exhausted-b")
		Expect(second.Status.Phase).To(Equal(ipamv1alpha1.IPRequestPhaseFailed))
		Expect(second.Status.AssignedIP).To(BeEmpty())
		ready := apimeta.FindStatusCondition(second.Status.Conditions, ipamv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(ipamv1alpha1.ReasonRangeExhausted))
	})

	It("fails a claim whose pool does not exist", func() {
		createClaim("default", "claims-nopool-ip", "claims-nopool", "")

		reconcileClaim("default", "claims-nopool-ip")

		got := getClaim("default", "claims-nopool-ip")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRequestPhaseFailed))
		ready := apimeta.FindStatusCondition(got.Status.Conditions, ipamv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(ipamv1alpha1.ReasonPoolNotFound))
	})

	It("fails a claim that asks for a NAD against a template-less pool", func() {
		spec := testPoolSpec()
		spec.NADTemplate = nil
		createPool("claims-notemplate", spec)
		createClaim("default", "claims-notemplate-ip", "claims-notemplate", "claims-notemplate-static")

		reconcileClaim("default", "claims-notemplate-ip")

		got := getClaim("default", "claims-notemplate-ip")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRequestPhaseFailed))

		// Named separately from Ready so the user can select on it directly: this is
		// a misconfiguration, not a broken controller, and it has to read that way.
		missing := apimeta.FindStatusCondition(got.Status.Conditions, ipamv1alpha1.ConditionTemplateMissing)
		Expect(missing).NotTo(BeNil())
		Expect(missing.Status).To(Equal(metav1.ConditionTrue))
		Expect(missing.Reason).To(Equal(ipamv1alpha1.ReasonTemplateAbsent))

		// No address is consumed: a claim that cannot be satisfied must not sit on
		// one of the pool's addresses.
		Expect(got.Status.AssignedIP).To(BeEmpty())
		absent := newNAD("default", "claims-notemplate-static")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(absent), absent))).To(BeTrue())
	})

	It("fails a claim whose nad name belongs to someone else", func() {
		createPool("claims-taken", testPoolSpec())
		createClaim("default", "claims-taken-ip", "claims-taken", "claims-taken-static")

		// A NAD this project did not create: no ownerReference, and not ours to
		// adopt or overwrite.
		foreign := newNAD("default", "claims-taken-static")
		Expect(unstructured.SetNestedField(foreign.Object, `{"name":"someone-elses"}`, "spec", "config")).To(Succeed())
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		deleteNAD("default", "claims-taken-static")

		reconcileClaim("default", "claims-taken-ip")

		got := getClaim("default", "claims-taken-ip")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRequestPhaseFailed))
		taken := apimeta.FindStatusCondition(got.Status.Conditions, ipamv1alpha1.ConditionNADNameTaken)
		Expect(taken).NotTo(BeNil())
		Expect(taken.Status).To(Equal(metav1.ConditionTrue))

		// And the foreign object is untouched.
		still := getNAD("default", "claims-taken-static")
		config, _, err := unstructured.NestedString(still.Object, "spec", "config")
		Expect(err).NotTo(HaveOccurred())
		Expect(config).To(Equal(`{"name":"someone-elses"}`))
	})

	It("is idempotent when the NAD already exists and is ours", func() {
		createPool("claims-idempotent", testPoolSpec())
		createClaim("default", "claims-idempotent-ip", "claims-idempotent", "claims-idempotent-static")
		deleteNAD("default", "claims-idempotent-static")

		reconcileClaim("default", "claims-idempotent-ip")
		first := getNAD("default", "claims-idempotent-static")

		reconcileClaim("default", "claims-idempotent-ip")
		second := getNAD("default", "claims-idempotent-static")

		Expect(second.GetResourceVersion()).To(Equal(first.GetResourceVersion()))
	})

	It("corrects a NAD whose config has drifted", func() {
		createPool("claims-drift", testPoolSpec())
		createClaim("default", "claims-drift-ip", "claims-drift", "claims-drift-static")
		deleteNAD("default", "claims-drift-static")

		reconcileClaim("default", "claims-drift-ip")

		nad := getNAD("default", "claims-drift-static")
		Expect(unstructured.SetNestedField(nad.Object, `{"name":"hand-edited"}`, "spec", "config")).To(Succeed())
		Expect(k8sClient.Update(ctx, nad)).To(Succeed())

		reconcileClaim("default", "claims-drift-ip")

		repair := getNAD("default", "claims-drift-static")
		ipam, ok := renderedConfig(repair)["ipam"].(map[string]any)
		Expect(ok).To(BeTrue())
		Expect(ipam).To(HaveKeyWithValue("addresses", []any{
			map[string]any{"address": testFirstIP + "/24", "gateway": testGateway},
		}))
	})

	It("clears the failure conditions once the pool is fixed", func() {
		spec := testPoolSpec()
		spec.NADTemplate = nil
		createPool("claims-recover", spec)
		createClaim("default", "claims-recover-ip", "claims-recover", "claims-recover-static")
		deleteNAD("default", "claims-recover-static")

		reconcileClaim("default", "claims-recover-ip")
		Expect(apimeta.FindStatusCondition(
			getClaim("default", "claims-recover-ip").Status.Conditions,
			ipamv1alpha1.ConditionTemplateMissing)).NotTo(BeNil())

		pool := &ipamv1alpha1.IPPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "claims-recover"}, pool)).To(Succeed())
		// A complete template: the CRD requires subnet and gateway alongside the
		// bridge, so a pool cannot be half-fixed into minting.
		pool.Spec.NADTemplate = testPoolSpec().NADTemplate
		Expect(k8sClient.Update(ctx, pool)).To(Succeed())

		reconcileClaim("default", "claims-recover-ip")

		got := getClaim("default", "claims-recover-ip")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRequestPhaseBound))
		// Removed rather than left as False: a problem that no longer exists should
		// not linger as a condition someone has to interpret.
		Expect(apimeta.FindStatusCondition(got.Status.Conditions, ipamv1alpha1.ConditionTemplateMissing)).To(BeNil())
		Expect(got.Status.AssignedIP).To(Equal(testFirstIP))
	})
})
