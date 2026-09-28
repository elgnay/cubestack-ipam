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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

var _ = Describe("Auditor", func() {
	// sweep runs every check. The suite shares one cluster, so other specs'
	// leftovers are expected findings; each spec filters down to what it caused.
	sweep := func() []Finding {
		findings, err := (&Auditor{Client: k8sClient}).RunOnce(ctx)
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return findings
	}

	withReason := func(findings []Finding, reason string) []Finding {
		var matched []Finding
		for _, f := range findings {
			if f.Reason == reason {
				matched = append(matched, f)
			}
		}
		return matched
	}

	withName := func(findings []Finding, name string) []Finding {
		var matched []Finding
		for _, f := range findings {
			if f.Name == name {
				matched = append(matched, f)
			}
		}
		return matched
	}

	It("reports a NAD whose claim is gone", func() {
		// The chain this project relies on is an ownerReference, so an orphan means
		// that chain was broken. The address in the NAD's ledger is still held while
		// the pool reports it as free, which is why this is worth saying out loud.
		orphan := newNAD("default", "audit-orphan-static")
		orphan.SetLabels(map[string]string{ipamv1alpha1.LabelRequest: "audit-orphan-ip"})
		Expect(unstructured.SetNestedField(orphan.Object, `{"name":"audit-orphan-static"}`, "spec", "config")).To(Succeed())
		Expect(k8sClient.Create(ctx, orphan)).To(Succeed())
		deleteNAD("default", "audit-orphan-static")

		matched := withName(withReason(sweep(), "OrphanedNAD"), "audit-orphan-static")
		Expect(matched).To(HaveLen(1))
		Expect(matched[0].Message).To(ContainSubstring("audit-orphan-ip"))
	})

	It("does not report a live NAD", func() {
		createPool("audit-live", testPoolSpec())
		createClaim("default", "audit-live-ip", "audit-live", "audit-live-static")
		deleteNAD("default", "audit-live-static")

		r := &IPRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, requestFor("default", "audit-live-ip"))
		Expect(err).NotTo(HaveOccurred())

		Expect(withName(withReason(sweep(), "OrphanedNAD"), "audit-live-static")).To(BeEmpty())
	})

	It("reports two claims holding one address", func() {
		// The residue of the allocation race: neither claim's own status shows
		// anything wrong, and the symptom is a VM that will not start.
		createPool("audit-dup", testPoolSpec())
		for _, name := range []string{"audit-dup-a", "audit-dup-b"} {
			createClaim("default", name, "audit-dup", "")
			claim := getClaim("default", name)
			claim.Status.AssignedIP = testFirstIP
			claim.Status.Phase = ipamv1alpha1.IPRequestPhaseBound
			Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())
		}

		duplicates := withReason(sweep(), "DuplicateAddress")

		// Reported on both, since whichever one a reader is looking at should say
		// what is wrong with it — and should name the other.
		a := withName(duplicates, "audit-dup-a")
		Expect(a).To(HaveLen(1))
		Expect(a[0].Message).To(ContainSubstring("audit-dup-b"))

		b := withName(duplicates, "audit-dup-b")
		Expect(b).To(HaveLen(1))
		Expect(b[0].Message).To(ContainSubstring("audit-dup-a"))
	})

	It("does not report distinct addresses", func() {
		createPool("audit-distinct", testPoolSpec())
		for name, addr := range map[string]string{"audit-distinct-a": testFirstIP, "audit-distinct-b": testSecondIP} {
			createClaim("default", name, "audit-distinct", "")
			claim := getClaim("default", name)
			claim.Status.AssignedIP = addr
			claim.Status.Phase = ipamv1alpha1.IPRequestPhaseBound
			Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())
		}

		duplicates := withReason(sweep(), "DuplicateAddress")
		Expect(withName(duplicates, "audit-distinct-a")).To(BeEmpty())
		Expect(withName(duplicates, "audit-distinct-b")).To(BeEmpty())
	})

	It("reports claims left behind by a deleted pool", func() {
		// No deletion guard is deliberate: a pool can go, and the claims it served
		// are then reported rather than silently invalid. What no single reconcile
		// can say is how many were stranded, which is what this adds.
		for _, name := range []string{"audit-dangling-a", "audit-dangling-b"} {
			createClaim("default", name, "audit-gone-pool", "")
		}

		var matched []Finding
		for _, f := range withReason(sweep(), "PoolDeleted") {
			if strings.Contains(f.Message, "audit-gone-pool") {
				matched = append(matched, f)
			}
		}
		Expect(matched).To(HaveLen(1))
		Expect(matched[0].Message).To(ContainSubstring("audit-dangling-a"))
		Expect(matched[0].Message).To(ContainSubstring("audit-dangling-b"))
		Expect(matched[0].Message).To(ContainSubstring("2 claim(s)"))
	})

	It("is quiet about a healthy claim", func() {
		createPool("audit-healthy", testPoolSpec())
		createClaim("default", "audit-healthy-ip", "audit-healthy", "audit-healthy-static")
		deleteNAD("default", "audit-healthy-static")

		r := &IPRequestReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, requestFor("default", "audit-healthy-ip"))
		Expect(err).NotTo(HaveOccurred())

		// Asserted first, so the negative assertions below cannot pass by the
		// fixture having quietly failed to exist.
		Expect(getClaim("default", "audit-healthy-ip").Status.AssignedIP).To(Equal(testFirstIP))
		Expect(getNAD("default", "audit-healthy-static")).NotTo(BeNil())

		findings := sweep()
		Expect(withName(withReason(findings, "OrphanedNAD"), "audit-healthy-static")).To(BeEmpty())
		Expect(withName(withReason(findings, "DuplicateAddress"), "audit-healthy-ip")).To(BeEmpty())
		for _, f := range withReason(findings, "PoolDeleted") {
			Expect(f.Message).NotTo(ContainSubstring("audit-healthy"))
		}
	})
})
