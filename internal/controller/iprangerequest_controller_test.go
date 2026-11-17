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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

var _ = Describe("IPRangeRequest Controller", func() {
	// readyReason is the reason on the primary condition, which is where every
	// failure mode this controller has is reported.
	readyReason := func(block *ipamv1alpha1.IPRangeRequest) (string, string) {
		ready := apimeta.FindStatusCondition(block.Status.Conditions, ipamv1alpha1.ConditionReady)
		ExpectWithOffset(1, ready).NotTo(BeNil())
		return ready.Reason, ready.Message
	}

	It("binds the lowest free run", func() {
		createPool("blocks-basic", testBlockPoolSpec())
		createRangeRequest("default", "blocks-basic", "blocks-basic", 3)

		reconcileRange("default", "blocks-basic")

		got := getRangeRequest("default", "blocks-basic")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseBound))
		Expect(got.Status.Start).To(Equal(testFirstIP))
		Expect(got.Status.End).To(Equal(testThirdIP))
		Expect(apimeta.IsStatusConditionTrue(got.Status.Conditions, ipamv1alpha1.ConditionReady)).To(BeTrue())
		Expect(got.UID).NotTo(BeEmpty())
	})

	It("records a one-address block as start == end", func() {
		// The degenerate case is worth pinning because the rest of the code reads a
		// block as a band, and a band of one has to survive that unchanged.
		createPool("blocks-single", testBlockPoolSpec())
		createRangeRequest("default", "blocks-single", "blocks-single", 1)

		reconcileRange("default", "blocks-single")

		got := getRangeRequest("default", "blocks-single")
		Expect(got.Status.Start).To(Equal(testFirstIP))
		Expect(got.Status.End).To(Equal(testFirstIP))
	})

	It("gives the second request the next run, with no overlap", func() {
		createPool("blocks-two", testBlockPoolSpec())
		createRangeRequest("default", "blocks-two-a", "blocks-two", 3)
		createRangeRequest("default", "blocks-two-b", "blocks-two", 3)

		reconcileRange("default", "blocks-two-a")
		reconcileRange("default", "blocks-two-b")

		Expect(getRangeRequest("default", "blocks-two-a").Status.Start).To(Equal(testFirstIP))
		Expect(getRangeRequest("default", "blocks-two-a").Status.End).To(Equal(testThirdIP))
		// The second starts where the first ended plus one. Contiguity is the whole
		// feature, so a gap or an overlap here is the same bug.
		Expect(getRangeRequest("default", "blocks-two-b").Status.Start).To(Equal(testFourthIP))
		Expect(getRangeRequest("default", "blocks-two-b").Status.End).To(Equal(testSixthIP))
	})

	It("keeps a block the request already holds", func() {
		// The property that makes this worth having: a block never moves. A
		// re-derivation would displace addresses that something outside the cluster
		// has already been configured with, and unlike a VM's address nothing here
		// would notice.
		createPool("blocks-sticky", testBlockPoolSpec())
		createRangeRequest("default", "blocks-sticky", "blocks-sticky", 2)
		bindRange("default", "blocks-sticky", testSixthIP, testBlockBandEnd)

		reconcileRange("default", "blocks-sticky")

		got := getRangeRequest("default", "blocks-sticky")
		Expect(got.Status.Start).To(Equal(testSixthIP))
		Expect(got.Status.End).To(Equal(testBlockBandEnd))
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseBound))
	})

	It("reports exhaustion when the band is full", func() {
		createPool("blocks-full", testBlockPoolSpec())
		createRangeRequest("default", "blocks-full-a", "blocks-full", 8) // the whole band
		createRangeRequest("default", "blocks-full-b", "blocks-full", 1)

		reconcileRange("default", "blocks-full-a")
		reconcileRange("default", "blocks-full-b")

		Expect(getRangeRequest("default", "blocks-full-a").Status.Start).To(Equal(testFirstIP))
		Expect(getRangeRequest("default", "blocks-full-a").Status.End).To(Equal(testBlockBandEnd))

		second := getRangeRequest("default", "blocks-full-b")
		Expect(second.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseFailed))
		Expect(second.Status.Start).To(BeEmpty())
		reason, message := readyReason(second)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeExhausted))
		Expect(message).To(ContainSubstring("0 free addresses"))
	})

	It("reports exhaustion when the band could never hold the request", func() {
		// A distinct case from a full band: nothing is claimed, and the request is
		// simply larger than the pool has ever been. The same reason covers both, but
		// the message has to be true of both.
		createPool("blocks-toobig", testBlockPoolSpec())
		createRangeRequest("default", "blocks-toobig", "blocks-toobig", 9)

		reconcileRange("default", "blocks-toobig")

		got := getRangeRequest("default", "blocks-toobig")
		reason, message := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeExhausted))
		Expect(message).To(ContainSubstring("has 8 free addresses and 9 were requested"))
	})

	It("reports fragmentation rather than exhaustion", func() {
		// The distinction the RangeFragmented reason exists for. Six addresses are
		// free and four were asked for, so "exhausted" would be false — and the two
		// prescribe different fixes, so the message has to name the longest run.
		createPool("blocks-fragmented", testBlockPoolSpec())
		// Two claims parked in the middle split the band into 150,151 | 153 | 155-157.
		bindClaim("default", "blocks-frag-a", "blocks-fragmented", testThirdIP)
		bindClaim("default", "blocks-frag-b", "blocks-fragmented", testFifthIP)
		createRangeRequest("default", "blocks-frag", "blocks-fragmented", 4)

		reconcileRange("default", "blocks-frag")

		got := getRangeRequest("default", "blocks-frag")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseFailed))
		reason, message := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeFragmented))
		Expect(message).To(ContainSubstring("has 6 free addresses but no run of 4 together"))
		Expect(message).To(ContainSubstring("longest contiguous run is 3"))
	})

	It("fails a request whose pool does not exist", func() {
		createRangeRequest("default", "blocks-nopool", "blocks-nopool", 2)

		reconcileRange("default", "blocks-nopool")

		got := getRangeRequest("default", "blocks-nopool")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseFailed))
		reason, _ := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonPoolNotFound))
	})

	It("retains a block that has drifted out of the band", func() {
		createPool("blocks-drift", testBlockPoolSpec())
		createRangeRequest("default", "blocks-drift", "blocks-drift", 2)
		reconcileRange("default", "blocks-drift")
		Expect(getRangeRequest("default", "blocks-drift").Status.Start).To(Equal(testFirstIP))

		// Narrowed so the held block's first address falls outside. The block is kept
		// rather than moved: whatever is outside the cluster is still using it, and an
		// address that moves under a live consumer is worse than one that has drifted.
		pool := &ipamv1alpha1.IPPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "blocks-drift"}, pool)).To(Succeed())
		pool.Spec.Range = ipamv1alpha1.IPRange{Start: testSecondIP, End: testBlockBandEnd}
		Expect(k8sClient.Update(ctx, pool)).To(Succeed())

		reconcileRange("default", "blocks-drift")

		got := getRangeRequest("default", "blocks-drift")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseFailed))
		reason, message := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeInvalid))
		Expect(message).To(ContainSubstring("no longer within pool"))
		Expect(message).To(ContainSubstring("retained"))
		Expect(got.Status.Start).To(Equal(testFirstIP))
		Expect(got.Status.End).To(Equal(testSecondIP))
	})

	It("frees the block for the next request when it is deleted", func() {
		// Deleting a range request is the release, with no finalizer and no guard. The
		// addresses have to be genuinely available again, or the feature would leak the
		// band one request at a time.
		createPool("blocks-release", testBlockPoolSpec())
		createRangeRequest("default", "blocks-release-a", "blocks-release", 3)
		reconcileRange("default", "blocks-release-a")
		Expect(getRangeRequest("default", "blocks-release-a").Status.Start).To(Equal(testFirstIP))

		Expect(k8sClient.Delete(ctx, getRangeRequest("default", "blocks-release-a"))).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Namespace: "default", Name: "blocks-release-a"},
				&ipamv1alpha1.IPRangeRequest{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue(), "a range request has no finalizer, so deletion is immediate")

		createRangeRequest("default", "blocks-release-b", "blocks-release", 3)
		reconcileRange("default", "blocks-release-b")

		// The freed run is handed straight back out, which is the point — and is also
		// why IPRangeRequestSpec says deleting one is safe only once its consumer has
		// stopped using the addresses.
		Expect(getRangeRequest("default", "blocks-release-b").Status.Start).To(Equal(testFirstIP))
	})

	It("mints nothing", func() {
		// Stated as a spec rather than left to the type having no field for it,
		// because "a block creates no network" is the thing a reader is most likely to
		// assume is wrong. Asserted on the count of NADs before and after, since a
		// block has no name to look one up by.
		createPool("blocks-nonad", testBlockPoolSpec())

		before := listNADs()
		createRangeRequest("default", "blocks-nonad", "blocks-nonad", 3)
		reconcileRange("default", "blocks-nonad")
		Expect(listNADs()).To(Equal(before))

		Expect(getRangeRequest("default", "blocks-nonad").Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseBound))
	})

	// --- Exact allocation ---------------------------------------------------

	It("binds the band the request named, not the lowest free one", func() {
		createPool("blocks-exact", testBlockPoolSpec())
		createExactRangeRequest("default", "blocks-exact", "blocks-exact", testFourthIP, testSixthIP)

		reconcileRange("default", "blocks-exact")

		got := getRangeRequest("default", "blocks-exact")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseBound))
		Expect(got.Status.Start).To(Equal(testFourthIP))
		Expect(got.Status.End).To(Equal(testSixthIP))
		Expect(got.Status.Blockers).To(BeEmpty())
	})

	It("binds an Exact band in the gap a claim leaves", func() {
		// The useful shape: an address in the middle of the pool is taken, and the
		// caller asks for a band below or above it rather than the lowest run.
		createPool("blocks-exact-gap", testBlockPoolSpec())
		bindClaim("default", "blocks-exact-gap-ip", "blocks-exact-gap", testFourthIP)
		createExactRangeRequest("default", "blocks-exact-gap", "blocks-exact-gap", testFirstIP, testThirdIP)

		reconcileRange("default", "blocks-exact-gap")

		got := getRangeRequest("default", "blocks-exact-gap")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseBound))
		Expect(got.Status.Start).To(Equal(testFirstIP))
		Expect(got.Status.End).To(Equal(testThirdIP))
	})

	It("fails rather than moving a requested band that overlaps a claim", func() {
		createPool("blocks-exact-taken", testBlockPoolSpec())
		bindClaim("default", "blocks-exact-taken-ip", "blocks-exact-taken", testThirdIP)
		createExactRangeRequest("default", "blocks-exact-taken-blk", "blocks-exact-taken", testSecondIP, testFourthIP)

		reconcileRange("default", "blocks-exact-taken-blk")

		got := getRangeRequest("default", "blocks-exact-taken-blk")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseFailed))
		Expect(got.Status.Start).To(BeEmpty())
		reason, _ := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeUnavailable))

		Expect(got.Status.Blockers).To(HaveLen(1))
		Expect(got.Status.Blockers[0].Kind).To(Equal("IPRequest"))
		Expect(got.Status.Blockers[0].Name).To(Equal("blocks-exact-taken-ip"))
		Expect(got.Status.Blockers[0].Address).To(Equal(testThirdIP))
	})

	It("names a block that overlaps a requested band", func() {
		createPool("blocks-exact-overlap", testBlockPoolSpec())
		createRangeRequest("default", "blocks-exact-overlap-a", "blocks-exact-overlap", 3)
		reconcileRange("default", "blocks-exact-overlap-a")
		Expect(getRangeRequest("default", "blocks-exact-overlap-a").Status.Start).To(Equal(testFirstIP))

		createExactRangeRequest("default", "blocks-exact-overlap-b", "blocks-exact-overlap", testThirdIP, testFifthIP)
		reconcileRange("default", "blocks-exact-overlap-b")

		got := getRangeRequest("default", "blocks-exact-overlap-b")
		reason, _ := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeUnavailable))
		Expect(got.Status.Blockers).To(HaveLen(1))
		Expect(got.Status.Blockers[0].Kind).To(Equal("IPRangeRequest"))
		Expect(got.Status.Blockers[0].Name).To(Equal("blocks-exact-overlap-a"))
		Expect(got.Status.Blockers[0].Range).NotTo(BeNil())
		Expect(got.Status.Blockers[0].Range.End).To(Equal(testThirdIP))
	})

	It("reports a requested band that is not entirely in the pool", func() {
		createPool("blocks-exact-outside", testBlockPoolSpec())
		createExactRangeRequest("default", "blocks-exact-outside", "blocks-exact-outside", testSixthIP, "10.66.3.159")

		reconcileRange("default", "blocks-exact-outside")

		got := getRangeRequest("default", "blocks-exact-outside")
		reason, _ := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeOutOfRange))
		Expect(got.Status.Start).To(BeEmpty())
	})

	It("reports a requested band that intersects a pool exclusion", func() {
		spec := testBlockPoolSpec()
		spec.Allocation = &ipamv1alpha1.PoolAllocation{
			Exclude: []ipamv1alpha1.IPRange{{Start: testFourthIP, End: testFifthIP}},
		}
		createPool("blocks-exact-excluded", spec)
		createExactRangeRequest("default", "blocks-exact-excluded", "blocks-exact-excluded", testThirdIP, testFourthIP)

		reconcileRange("default", "blocks-exact-excluded")

		got := getRangeRequest("default", "blocks-exact-excluded")
		reason, _ := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeExcluded))
		Expect(got.Status.Blockers).To(HaveLen(1))
		Expect(got.Status.Blockers[0].Kind).To(Equal("IPPool"))
		Expect(got.Status.Blockers[0].Range).NotTo(BeNil())
		Expect(got.Status.Blockers[0].Range.Start).To(Equal(testFourthIP))
	})

	It("reports an Exact block against a pool that forbids it", func() {
		allow := false
		spec := testBlockPoolSpec()
		spec.Allocation = &ipamv1alpha1.PoolAllocation{AllowExact: &allow}
		createPool("blocks-exact-denied", spec)
		createExactRangeRequest("default", "blocks-exact-denied", "blocks-exact-denied", testFirstIP, testThirdIP)

		reconcileRange("default", "blocks-exact-denied")

		reason, _ := readyReason(getRangeRequest("default", "blocks-exact-denied"))
		Expect(reason).To(Equal(ipamv1alpha1.ReasonExactNotAllowed))
	})

	It("makes an Auto block skip an excluded band", func() {
		spec := testBlockPoolSpec()
		spec.Allocation = &ipamv1alpha1.PoolAllocation{
			Exclude: []ipamv1alpha1.IPRange{{Start: testFirstIP, End: testFirstIP}},
		}
		createPool("blocks-auto-excluded", spec)
		createRangeRequest("default", "blocks-auto-excluded", "blocks-auto-excluded", 3)

		reconcileRange("default", "blocks-auto-excluded")

		got := getRangeRequest("default", "blocks-auto-excluded")
		Expect(got.Status.Phase).To(Equal(ipamv1alpha1.IPRangeRequestPhaseBound))
		Expect(got.Status.Start).To(Equal(testSecondIP))
		Expect(got.Status.End).To(Equal(testFourthIP))
	})

	It("retains an Exact block that has drifted out of the pool", func() {
		createPool("blocks-exact-drift", testBlockPoolSpec())
		createExactRangeRequest("default", "blocks-exact-drift", "blocks-exact-drift", testFirstIP, testThirdIP)
		reconcileRange("default", "blocks-exact-drift")

		pool := &ipamv1alpha1.IPPool{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "blocks-exact-drift"}, pool)).To(Succeed())
		pool.Spec.Range = ipamv1alpha1.IPRange{Start: testSecondIP, End: testBlockBandEnd}
		Expect(k8sClient.Update(ctx, pool)).To(Succeed())

		reconcileRange("default", "blocks-exact-drift")

		got := getRangeRequest("default", "blocks-exact-drift")
		reason, message := readyReason(got)
		Expect(reason).To(Equal(ipamv1alpha1.ReasonRangeInvalid))
		Expect(message).To(ContainSubstring("retained"))
		Expect(got.Status.Start).To(Equal(testFirstIP))
		Expect(got.Status.End).To(Equal(testThirdIP))
	})
})
