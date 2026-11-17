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
		// anything wrong, and the symptom depends on the pool's mode -- which the
		// message has to state, since that is the only thing a reader can act on.
		createPool("audit-dup", testPoolSpec()) // static
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

		// Static: nothing refuses the duplicate, so the finding must not promise a
		// failed start -- that would send a reader looking for a VM that never
		// started when in fact both are running.
		Expect(a[0].Message).To(ContainSubstring("intermittent connectivity"))
		Expect(a[0].Message).NotTo(ContainSubstring("will not start"))
	})

	It("reports a whereabouts duplicate as a failed start", func() {
		// The other half of the same check. The wording is not decoration: which
		// consequence applies is decided entirely by the mode, so a message that
		// ignored it would be wrong half the time and confidently so.
		createPool("audit-dup-wa", testWhereaboutsPoolSpec())
		for _, name := range []string{"audit-dup-wa-a", "audit-dup-wa-b"} {
			createClaim("default", name, "audit-dup-wa", "")
			claim := getClaim("default", name)
			claim.Status.AssignedIP = testFirstIP
			claim.Status.Phase = ipamv1alpha1.IPRequestPhaseBound
			Expect(k8sClient.Status().Update(ctx, claim)).To(Succeed())
		}

		a := withName(withReason(sweep(), "DuplicateAddress"), "audit-dup-wa-a")
		Expect(a).To(HaveLen(1))
		Expect(a[0].Message).To(ContainSubstring("will not start"))
		Expect(a[0].Message).NotTo(ContainSubstring("intermittent connectivity"))
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

	It("reports a claim sitting inside a delegated block", func() {
		// The collision nothing downstream refuses. On a static pool nothing refuses a
		// duplicate at all, and neither holder's own status shows anything wrong — the
		// claim holds an address the allocator gave it, the block holds the range it
		// was given — so both have to be told about the other.
		createPool("audit-inblock", testPoolSpec()) // static
		bindClaim("default", "audit-inblock-ip", "audit-inblock", testThirdIP)
		createRangeRequest("default", "audit-inblock-blk", "audit-inblock", 6)
		bindRange("default", "audit-inblock-blk", testFirstIP, testSixthIP) // .150-.155, covers .152

		findings := withReason(sweep(), "AddressInBlock")

		claimFinding := withName(findings, "audit-inblock-ip")
		Expect(claimFinding).To(HaveLen(1))
		Expect(claimFinding[0].GVK.Kind).To(Equal("IPRequest"))
		Expect(claimFinding[0].Message).To(ContainSubstring("audit-inblock-blk"))
		Expect(claimFinding[0].Message).To(ContainSubstring(testThirdIP))

		blockFinding := withName(findings, "audit-inblock-blk")
		Expect(blockFinding).To(HaveLen(1))
		Expect(blockFinding[0].GVK.Kind).To(Equal("IPRangeRequest"))
		Expect(blockFinding[0].Message).To(ContainSubstring("audit-inblock-ip"))

		// The block-side consequence must not be borrowed from the claim-side one.
		// That wording promises a refused CNI ADD, which cannot happen here: a block is
		// not CNI state, so there is no second ADD for anything to refuse.
		Expect(claimFinding[0].Message).To(ContainSubstring("intermittent connectivity"))
		Expect(claimFinding[0].Message).NotTo(ContainSubstring("will not start"))
		Expect(claimFinding[0].Message).NotTo(ContainSubstring("leases from Whereabouts"))
	})

	It("does not claim the CNI will catch a whereabouts block overlap", func() {
		// The other half of the same wording, and the one that matters most: Whereabouts
		// arbitrates the VM's own claim and has never heard of the block, so its
		// familiar consequence — the second ADD refused, the VM not starting — does not
		// apply. Saying it would send a reader looking for a VM that is running fine.
		createPool("audit-inblock-wa", testWhereaboutsPoolSpec())
		bindClaim("default", "audit-inblock-wa-ip", "audit-inblock-wa", testThirdIP)
		createRangeRequest("default", "audit-inblock-wa-blk", "audit-inblock-wa", 6)
		bindRange("default", "audit-inblock-wa-blk", testFirstIP, testSixthIP)

		matched := withName(withReason(sweep(), "AddressInBlock"), "audit-inblock-wa-ip")
		Expect(matched).To(HaveLen(1))
		Expect(matched[0].Message).To(ContainSubstring("does not know the block exists"))
		Expect(matched[0].Message).NotTo(ContainSubstring("will not start"))
		Expect(matched[0].Message).NotTo(ContainSubstring("intermittent connectivity"))
	})

	It("reports two blocks that overlap", func() {
		// The block-vs-block residue of the same allocation race. Two bounds state the
		// overlap exactly, because the intersection of two contiguous blocks is
		// contiguous.
		createPool("audit-overlap", testPoolSpec())
		createRangeRequest("default", "audit-overlap-a", "audit-overlap", 4)
		createRangeRequest("default", "audit-overlap-b", "audit-overlap", 4)
		bindRange("default", "audit-overlap-a", testFirstIP, testFourthIP) // .150-.153
		bindRange("default", "audit-overlap-b", testThirdIP, testSixthIP)  // .152-.155

		findings := withReason(sweep(), "RangeOverlap")

		a := withName(findings, "audit-overlap-a")
		Expect(a).To(HaveLen(1))
		Expect(a[0].Message).To(ContainSubstring("audit-overlap-b"))
		Expect(a[0].Message).To(ContainSubstring(testThirdIP + "-" + testFourthIP))

		b := withName(findings, "audit-overlap-b")
		Expect(b).To(HaveLen(1))
		Expect(b[0].GVK.Kind).To(Equal("IPRangeRequest"))
		Expect(b[0].Message).To(ContainSubstring("audit-overlap-a"))
	})

	It("is quiet about a healthy mixed cluster", func() {
		// A claim and a block sharing one pool without touching, which is the normal
		// state this whole feature has to leave alone.
		createPool("audit-mixed", testPoolSpec())
		bindClaim("default", "audit-mixed-ip", "audit-mixed", testFirstIP)
		createRangeRequest("default", "audit-mixed-blk", "audit-mixed", 3)
		bindRange("default", "audit-mixed-blk", testFifthIP, testBlockBandEnd) // .154-.157

		// Asserted first, so the negatives below cannot pass by the fixture having
		// quietly failed to exist.
		Expect(getRangeRequest("default", "audit-mixed-blk").Status.Start).To(Equal(testFifthIP))

		findings := sweep()
		Expect(withName(withReason(findings, "AddressInBlock"), "audit-mixed-ip")).To(BeEmpty())
		Expect(withName(withReason(findings, "AddressInBlock"), "audit-mixed-blk")).To(BeEmpty())
		Expect(withName(withReason(findings, "RangeOverlap"), "audit-mixed-blk")).To(BeEmpty())
	})

	It("says nothing about a block that records no addresses", func() {
		// A request that was never bound, or whose status does not parse, names no
		// addresses — so there is nothing for either block check to compare. Skipping
		// it is deliberate: its own reconciler already reports it as RangeInvalid, and
		// the audit's job here is not to say that twice.
		createPool("audit-unbound", testPoolSpec())
		createRangeRequest("default", "audit-unbound-block", "audit-unbound", 2)

		findings := sweep()
		Expect(withName(withReason(findings, "AddressInBlock"), "audit-unbound-block")).To(BeEmpty())
		Expect(withName(withReason(findings, "RangeOverlap"), "audit-unbound-block")).To(BeEmpty())
	})
})
