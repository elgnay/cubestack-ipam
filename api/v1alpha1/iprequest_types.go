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

// IPRequestPhase summarises where a claim is in its lifecycle.
//
// Only Pending, Bound and Failed are modelled. There is deliberately no
// "Released" phase: release is expressed by the object's deletion, and the
// address is returned by garbage collection — the claim owns the NAD, so neither
// needs a finalizer. A terminal phase that deletion also acts on would give two
// mechanisms for one transition.
//
// +kubebuilder:validation:Enum=Pending;Bound;Failed
type IPRequestPhase string

const (
	// IPRequestPhasePending means no address has been assigned yet.
	IPRequestPhasePending IPRequestPhase = "Pending"

	// IPRequestPhaseBound means an address is assigned. If spec.nad was set, its
	// NAD has been minted too; a claim with no spec.nad is bound without one.
	IPRequestPhaseBound IPRequestPhase = "Bound"

	// IPRequestPhaseFailed means the claim could not be satisfied. Claimants
	// should read status.conditions for the reason rather than retrying blindly.
	IPRequestPhaseFailed IPRequestPhase = "Failed"
)

// IPRequestSpec defines the desired state of IPRequest.
//
// An IPRequest is created by a user and is owned by the VM it serves. On creation
// the controller assigns an address from the referenced pool, and — when nad is
// set — mints a per-VM NetworkAttachmentDefinition carrying that address as a
// single static assignment (design §5.2 steps 3-4).
type IPRequestSpec struct {
	// poolRef names the IPPool to claim from. IPPool is cluster-scoped, so this is
	// a bare name with no namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	PoolRef string `json:"poolRef"`

	// nad optionally names the NetworkAttachmentDefinition to mint for this claim,
	// in the IPRequest's own namespace. The generated NAD takes this name verbatim,
	// which is what lets a VM manifest be written before the request is satisfied —
	// the VM references the NAD by a name it already knows.
	//
	// Omit it for a ledger-only claim: an address is bound and nothing is created.
	// The referenced pool must carry a nadTemplate, otherwise the claim fails with
	// a TemplateMissing condition.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	NAD string `json:"nad,omitempty"`
}

// IPRequestStatus defines the observed state of IPRequest.
type IPRequestStatus struct {
	// phase summarises the claim's lifecycle.
	// +optional
	Phase IPRequestPhase `json:"phase,omitempty"`

	// assignedIP is the address this claim holds, once bound.
	// +optional
	AssignedIP string `json:"assignedIP,omitempty"`

	// conditions represent the current state of the IPRequest resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Condition types set here:
	// - "Ready": True once an address is assigned and, if spec.nad was set, its NAD exists.
	//   The cause of a False goes in the condition's reason, e.g. "RangeExhausted" or
	//   "PoolNotFound" -- there is no separate "Degraded" type, which would be its exact
	//   complement and could drift out of step with it.
	// - "TemplateMissing": True when spec.nad is set but the pool carries no nadTemplate,
	//   so no NAD can be minted. Names a misconfiguration the user can fix.
	// - "NADNameTaken": True when the name in spec.nad is already held by an object this
	//   claim does not own. Such an object is never adopted or overwritten.
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolRef`
// +kubebuilder:printcolumn:name="Assigned",type=string,JSONPath=`.status.assignedIP`
// +kubebuilder:printcolumn:name="NAD",type=string,JSONPath=`.spec.nad`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPRequest is the Schema for the iprequests API
type IPRequest struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of IPRequest
	// +required
	Spec IPRequestSpec `json:"spec"`

	// status defines the observed state of IPRequest
	// +optional
	Status IPRequestStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IPRequestList contains a list of IPRequest
type IPRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []IPRequest `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &IPRequest{}, &IPRequestList{})
		return nil
	})
}
