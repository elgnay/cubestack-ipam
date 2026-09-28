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

// NOTE ON PLURALS: Whereabouts already owns the `ippools` plural
// (ippools.whereabouts.cni.cncf.io) in kube-system. This type's plural is
// `ippools.ipam.cubestack.io`, so callers listing pools MUST disambiguate with
// the full resource name. There is no short name on purpose — one would make the
// collision worse rather than better.

// IPPoolSpec defines the desired state of IPPool.
//
// An IPPool is created by a cluster admin and declares a bounded, discoverable
// band of addresses that users may claim via IPRequest (design §5.1). Whereabouts
// remains the actual allocation primitive; this type is the operator-facing
// declaration of intent, not a second IPAM implementation.
//
// The range MUST be disjoint from the Whereabouts dynamic window and from the
// MetalLB pool on the same subnet. That invariant cannot be checked from inside
// this object — the reconciler is expected to audit it against the referenced
// NAD (design §5.5).
//
// Only the two containment rules below are CEL: the Kubernetes CEL net library
// exposes cidr(...).containsIP(ip(...)) but defines NO ordering overload on IP
// values, so "start <= end" and "gateway is outside the range" are not
// expressible as schema rules however they are written. They are enforced in Go
// instead — see ipam.ValidatePoolRange, which the reconciler calls.
//
// +kubebuilder:validation:XValidation:rule="cidr(self.subnet).containsIP(ip(self.range.start))",message="range.start must lie within subnet"
// +kubebuilder:validation:XValidation:rule="cidr(self.subnet).containsIP(ip(self.range.end))",message="range.end must lie within subnet"
type IPPoolSpec struct {
	// subnet is the CIDR this pool's addresses are drawn from, e.g. "10.66.3.0/24".
	// +kubebuilder:validation:Pattern=`^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])/(3[0-2]|[12]?[0-9])$`
	// +required
	Subnet string `json:"subnet"`

	// range is the inclusive band of addresses available to IPRequest claims.
	// It must be disjoint from the Whereabouts dynamic window (.200-.220 on
	// 10.66.3.0/24) and from the MetalLB pool (.221-.240) — see the README.
	// +required
	Range IPRange `json:"range"`

	// gateway is the default gateway handed to VMs whose address comes from this
	// pool. It must sit outside the allocatable range.
	// +kubebuilder:validation:Pattern=`^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$`
	// +required
	Gateway string `json:"gateway"`

	// templateNAD is the NetworkAttachmentDefinition whose bridge, dns and routes
	// are cloned into each generated per-VM NAD that this pool mints (design §5.4).
	// Written as "namespace/name" or bare "name" for the default namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	TemplateNAD string `json:"templateNAD"`
}

// IPRange is an inclusive address band. Both bounds are required. A band holding
// a single address is expressed as start == end — that is the shape the
// controller generates per-VM NADs from, so it is deliberately representable.
type IPRange struct {
	// start is the first address in the band (inclusive).
	// +kubebuilder:validation:Pattern=`^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$`
	// +required
	Start string `json:"start"`

	// end is the last address in the band (inclusive).
	// +kubebuilder:validation:Pattern=`^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$`
	// +required
	End string `json:"end"`
}

// IPPoolStatus defines the observed state of IPPool.
//
// DELIBERATE OMISSION: design §5.1 sketches a `status.allocated[]` ledger here,
// but §R3 and §5.5 both state the set of IPRequests is the ONLY authoritative
// record of who holds what. Modelling a second list would create exactly the
// two-ledger drift §R3 warns about, so this status carries conditions only and
// the IPRequest set is the ledger.
type IPPoolStatus struct {
	// conditions represent the current state of the IPPool resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Subnet",type=string,JSONPath=`.spec.subnet`
// +kubebuilder:printcolumn:name="Start",type=string,JSONPath=`.spec.range.start`
// +kubebuilder:printcolumn:name="End",type=string,JSONPath=`.spec.range.end`
// +kubebuilder:printcolumn:name="Gateway",type=string,JSONPath=`.spec.gateway`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// IPPool is the Schema for the ippools API
type IPPool struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of IPPool
	// +required
	Spec IPPoolSpec `json:"spec"`

	// status defines the observed state of IPPool
	// +optional
	Status IPPoolStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// IPPoolList contains a list of IPPool
type IPPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []IPPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &IPPool{}, &IPPoolList{})
		return nil
	})
}
