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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// IPRangeRequestPhase summarises where a block request is in its lifecycle.
//
// The values are deliberately identical to IPRequestPhase, and the words mean the
// same thing. Two Go types rather than one shared type, because a field named
// "IPRequestPhase" on a request that is not an IPRequest reads as a mistake; the
// drift risk that opens up is closed by a test asserting the two enums agree
// (TestPhasesAgree), which is cheaper than the confusion would have been.
//
// As on a claim, there is no "Released" phase: release is the object's deletion,
// and nothing outlives it to record the fact.
//
// +kubebuilder:validation:Enum=Pending;Bound;Failed
type IPRangeRequestPhase string

const (
	// IPRangeRequestPhasePending means no block has been assigned yet.
	IPRangeRequestPhasePending IPRangeRequestPhase = "Pending"

	// IPRangeRequestPhaseBound means a block is recorded in status.start and
	// status.end.
	IPRangeRequestPhaseBound IPRangeRequestPhase = "Bound"

	// IPRangeRequestPhaseFailed means no block could be assigned. Read
	// status.conditions for the reason rather than retrying blindly.
	IPRangeRequestPhaseFailed IPRangeRequestPhase = "Failed"
)

// IPRangeRequestSpec defines the desired state of IPRangeRequest.
//
// An IPRangeRequest claims a block of contiguous addresses from an existing
// IPPool, for a consumer that needs a range rather than a single address — a
// nested platform being provisioned onto VMs and configuring its own IPAM, say.
// The addresses are recorded in status and nothing else is created: no NAD is
// minted and no IPPool is declared, so this is a user-scope operation exactly
// like claiming one address.
//
// spec.allocation chooses the block. Auto carries count and asks for the lowest
// free run of that size. Exact carries an inclusive range and is never moved: if
// any address in it is out of range, excluded, or held, the request fails with the
// holders in status.blockers.
//
// The pool stays the single allocator. A block is drawn through the same ledger a
// single-address claim uses, so a block cannot overlap a VM's address or another
// block — by construction, rather than by two admins picking disjoint bands and
// hoping.
//
// NOTHING CONSUMES THE BLOCK ON YOUR BEHALF. The addresses are yours to hand to
// whatever needs them outside this cluster; nothing here reconfigures that
// consumer when the request changes or goes away. Deleting an IPRangeRequest
// frees the addresses immediately and the next claim or block will be given them,
// so delete one only after the consumer has stopped using it. That is the failure
// this whole project exists to prevent, which is why the audit watches for a
// later holder landing inside a block — see the README's audit table.
type IPRangeRequestSpec struct {
	// poolRef names the IPPool to draw the block from. IPPool is cluster-scoped, so
	// this is a bare name with no namespace.
	//
	// Immutable. The block belongs to the pool it was drawn from, and re-pointing
	// the reference would silently free the old addresses while whatever was
	// configured with them kept using them. Delete the request and create a new one
	// against the other pool.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="poolRef is immutable: a block belongs to the pool it was drawn from, and re-pointing it would free addresses a consumer may still be using. Delete this request and create a new one against the other pool."
	// +required
	PoolRef string `json:"poolRef"`

	// allocation chooses the block: Auto with a count, or Exact with an inclusive
	// range.
	//
	// Required. There is no sensible default between "give me any three free
	// addresses" and "give me exactly these three", and picking one silently would
	// make the other a surprise.
	//
	// Immutable. Changing it would move addresses out from under whatever was
	// configured with the block; a different block is a different request.
	//
	// +required
	Allocation RangeAllocation `json:"allocation"`
}

// IPRangeRequestStatus defines the observed state of IPRangeRequest.
type IPRangeRequestStatus struct {
	// phase summarises the request's lifecycle.
	// +optional
	Phase IPRangeRequestPhase `json:"phase,omitempty"`

	// start is the first address of the assigned block, once bound.
	// +optional
	Start string `json:"start,omitempty"`

	// end is the last address of the assigned block, once bound. Inclusive, so a
	// request for one address has start == end.
	//
	// Together with start this is the whole record of the block: the ledger that
	// stops a later claim from landing inside it is built by expanding these two
	// fields, so a bound request with no start/end holds nothing.
	// +optional
	End string `json:"end,omitempty"`

	// blockers names the holders standing in the way of an Exact request.
	//
	// Empty for Auto requests. On a failed Exact request this is the actionable
	// half of the condition: each entry is a claim whose address falls in the
	// requested band or a block it overlaps, and can be looked up directly. Cleared
	// when the request binds.
	//
	// +optional
	// +listType=atomic
	Blockers []Blocker `json:"blockers,omitempty"`

	// conditions represent the current state of the IPRangeRequest resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Condition types set here:
	// - "Ready": True once a block is recorded in status.start and status.end. The
	//   cause of a False goes in the condition's reason, e.g. "PoolNotFound",
	//   "RangeInvalid", "RangeExhausted" or "RangeFragmented"; an Exact request that
	//   cannot have the band it named adds "RangeOutOfRange", "RangeExcluded",
	//   "RangeUnavailable", "ExactNotAllowed" or "AllocationInvalid", with the holders
	//   in status.blockers -- there is no separate "Degraded" type, which would be its
	//   exact complement and could drift out of step with it.
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=iprr
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef`
// +kubebuilder:printcolumn:name="Count",type=integer,JSONPath=`.spec.allocation.count`
// +kubebuilder:printcolumn:name="Requested",type=string,JSONPath=`.spec.allocation.range.start`
// +kubebuilder:printcolumn:name="Start",type=string,JSONPath=`.status.start`
// +kubebuilder:printcolumn:name="End",type=string,JSONPath=`.status.end`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPRangeRequest is the Schema for the iprangerequests API
type IPRangeRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of IPRangeRequest
	// +required
	Spec IPRangeRequestSpec `json:"spec"`

	// status defines the observed state of IPRangeRequest
	// +optional
	Status IPRangeRequestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IPRangeRequestList contains a list of IPRangeRequest
type IPRangeRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []IPRangeRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &IPRangeRequest{}, &IPRangeRequestList{})
		return nil
	})
}
