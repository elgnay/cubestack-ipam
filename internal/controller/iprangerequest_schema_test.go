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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

// The CRD schema for the block API, which is where several of its rules live
// rather than in the reconciler: the ceiling on an Auto count, the mutual
// exclusion of count and range, and the immutability of the whole allocation.
var _ = Describe("IPRangeRequest schema validation", func() {
	ctx := context.Background()

	count := func(n int32) *int32 { return &n }
	auto := func(n int32) ipamv1alpha1.RangeAllocation {
		return ipamv1alpha1.RangeAllocation{Strategy: ipamv1alpha1.AllocationAuto, Count: count(n)}
	}
	exact := func(start, end string) ipamv1alpha1.RangeAllocation {
		return ipamv1alpha1.RangeAllocation{
			Strategy: ipamv1alpha1.AllocationExact,
			Range:    &ipamv1alpha1.IPRange{Start: start, End: end},
		}
	}

	newRequest := func(mutate func(*ipamv1alpha1.IPRangeRequestSpec)) *ipamv1alpha1.IPRangeRequest {
		spec := ipamv1alpha1.IPRangeRequestSpec{PoolRef: "any-pool", Allocation: auto(4)}
		mutate(&spec)
		return &ipamv1alpha1.IPRangeRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec:       spec,
		}
	}

	// create makes a request and removes it when the spec ends.
	create := func(mutate func(*ipamv1alpha1.IPRangeRequestSpec)) *ipamv1alpha1.IPRangeRequest {
		req := newRequest(mutate)
		ExpectWithOffset(1, k8sClient.Create(ctx, req)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, req) })
		return req
	}

	It("accepts an Auto block request", func() {
		create(func(*ipamv1alpha1.IPRangeRequestSpec) {})
	})

	It("accepts a one-address block", func() {
		create(func(s *ipamv1alpha1.IPRangeRequestSpec) { s.Allocation = auto(1) })
	})

	It("accepts an Exact block request", func() {
		create(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation = exact("10.66.3.160", "10.66.3.163")
		})
	})

	It("accepts an Exact one-address block", func() {
		// start == end is the degenerate band the whole address model treats as
		// legal, so the Exact form has to accept it too.
		create(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation = exact("10.66.3.160", "10.66.3.160")
		})
	})

	// A count of zero is the only way the typed client can express an absent count:
	// the field is required when strategy is Auto, and this exercises Minimum rather
	// than the field's required-ness.
	It("rejects an Auto count of zero", func() {
		Expect(k8sClient.Create(ctx, newRequest(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation = auto(0)
		}))).NotTo(Succeed())
	})

	It("rejects a count above the range ceiling", func() {
		// The CRD half of the MaxRangeSize pair. ipam.MaxRangeSize is the Go half and
		// TestMaxRangeSize pins it; the two are necessarily duplicated, because a CEL
		// marker cannot reference a Go constant. A block cannot be larger than the
		// largest band ParseRange will materialize, so if one moves the other must.
		Expect(k8sClient.Create(ctx, newRequest(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation = auto(65537)
		}))).NotTo(Succeed())
	})

	// The boundary itself, since an off-by-one in either direction leaves the pair
	// looking consistent while disagreeing about where the ceiling is.
	It("accepts the range ceiling itself", func() {
		create(func(s *ipamv1alpha1.IPRangeRequestSpec) { s.Allocation = auto(65536) })
	})

	It("rejects Auto with a range", func() {
		Expect(k8sClient.Create(ctx, newRequest(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation.Range = &ipamv1alpha1.IPRange{Start: "10.66.3.160", End: "10.66.3.163"}
		}))).NotTo(Succeed())
	})

	It("rejects Auto with no count", func() {
		Expect(k8sClient.Create(ctx, newRequest(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation.Count = nil
		}))).NotTo(Succeed())
	})

	It("rejects Exact with a count", func() {
		Expect(k8sClient.Create(ctx, newRequest(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation = exact("10.66.3.160", "10.66.3.163")
			s.Allocation.Count = count(4)
		}))).NotTo(Succeed())
	})

	It("rejects Exact with no range", func() {
		Expect(k8sClient.Create(ctx, newRequest(func(s *ipamv1alpha1.IPRangeRequestSpec) {
			s.Allocation = ipamv1alpha1.RangeAllocation{Strategy: ipamv1alpha1.AllocationExact}
		}))).NotTo(Succeed())
	})

	It("rejects a missing allocation", func() {
		req := &ipamv1alpha1.IPRangeRequest{
			ObjectMeta: metav1.ObjectMeta{GenerateName: "schema-check-", Namespace: "default"},
			Spec:       ipamv1alpha1.IPRangeRequestSpec{PoolRef: "any-pool"},
		}
		Expect(k8sClient.Create(ctx, req)).NotTo(Succeed())
	})

	It("rejects a change to poolRef", func() {
		// The block belongs to the pool it was drawn from. Re-pointing the reference
		// would free the old addresses while whatever was configured with them kept
		// using them — the failure the whole project exists to prevent, arrived at by
		// editing one field.
		req := create(func(*ipamv1alpha1.IPRangeRequestSpec) {})

		req.Spec.PoolRef = "another-pool"
		err := k8sClient.Update(ctx, req)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("poolRef is immutable"))
	})

	It("rejects a change to count", func() {
		// Same reasoning: resizing a block moves addresses out from under a consumer
		// on both ends. A different size is a different request.
		req := create(func(*ipamv1alpha1.IPRangeRequestSpec) {})

		*req.Spec.Allocation.Count = 5
		err := k8sClient.Update(ctx, req)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("allocation is immutable"))
	})

	It("rejects a change from Auto to Exact", func() {
		// The strategy is part of the immutable allocation, not a separate knob: an
		// Auto block that became Exact would be asking to move addresses that a
		// consumer outside the cluster is already using.
		req := create(func(*ipamv1alpha1.IPRangeRequestSpec) {})

		req.Spec.Allocation = exact("10.66.3.160", "10.66.3.163")
		err := k8sClient.Update(ctx, req)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("allocation is immutable"))
	})

	It("still allows an ordinary edit", func() {
		// The immutability rules must be about the fields they name and nothing more.
		// A request whose labels cannot be changed is one users will resent, and a rule
		// that quietly blocks writes is worse than the mistake it prevents.
		req := create(func(*ipamv1alpha1.IPRangeRequestSpec) {})

		req.Labels = map[string]string{"owner": "someone"}
		Expect(k8sClient.Update(ctx, req)).To(Succeed())
	})
})
