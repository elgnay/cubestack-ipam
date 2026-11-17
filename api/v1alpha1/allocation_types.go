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

// AllocationStrategy says who chooses the addresses a request receives.
//
// Auto is the original behaviour: the controller takes the lowest free address
// (or the lowest free contiguous run) from the pool. Exact turns the request into
// a demand for a named address or band, and the controller refuses it rather than
// substituting a different one — a typo has to fail, not land somewhere plausible.
//
// +kubebuilder:validation:Enum=Auto;Exact
type AllocationStrategy string

const (
	// AllocationAuto lets the controller choose the address or block.
	AllocationAuto AllocationStrategy = "Auto"

	// AllocationExact requests the address or block named in the same object.
	AllocationExact AllocationStrategy = "Exact"
)

// PoolAllocation is the admin's allocation policy for a pool. It is optional, and
// omitting it changes nothing: exact requests are allowed and no address is
// reserved.
type PoolAllocation struct {
	// exclude is a list of inclusive bands that the pool will never allocate,
	// whatever the strategy. It is how an admin reserves addresses for things that
	// are not claims: a gateway on a wider band, a cluster-service address, or a
	// block handed to another system.
	//
	// Exclusions are enforced by the allocator, not by the CNI. On a static pool
	// that is the only enforcement there is; on a whereabouts pool the CNI would
	// refuse a duplicate of a pod's address, but it has never heard of this band.
	// Each band must lie within spec.range, and the bounds are checked in Go because
	// CEL has no ordering overload on IP values.
	//
	// An exclusion added after an address was already allocated does NOT move the
	// holder. Addresses are sticky, as everywhere in this API: release the claim
	// first if the address has to be reclaimed.
	//
	// +optional
	// +listType=atomic
	Exclude []IPRange `json:"exclude,omitempty"`

	// allowExact controls whether callers may use strategy Exact against this pool.
	// It defaults to true; set it to false to keep a pool Auto-only.
	//
	// This is a coarse control, not a security boundary. Anyone who can create an
	// IPRequest can already consume the pool's addresses with Auto requests; exact
	// only changes which ones they land on. Use exclude for the addresses that
	// actually matter.
	//
	// +kubebuilder:default=true
	// +optional
	AllowExact *bool `json:"allowExact,omitempty"`
}

// AddressAllocation is the IPRequest's side of the allocation policy.
//
// The two strategies are mutually exclusive and the schema enforces it: Auto must
// carry no address, Exact must carry one. There is no fallback between them — an
// Exact request that cannot be satisfied fails with a condition and a structured
// list of blockers, rather than being quietly given a different address.
//
// +kubebuilder:validation:XValidation:rule="self.strategy == 'Exact' ? has(self.address) : !has(self.address)",message="allocation.address must be set when strategy is Exact and must be absent when strategy is Auto"
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="allocation is immutable: an address belongs to the claim it was bound to, and changing it would move an address out from under a running VM. Delete this claim and create a new one if you need a different allocation."
type AddressAllocation struct {
	// strategy selects who chooses the address. Omitted means Auto.
	//
	// +kubebuilder:default=Auto
	// +required
	Strategy AllocationStrategy `json:"strategy"`

	// address is the IPv4 address the caller requires. It is only meaningful with
	// strategy Exact, and the schema requires it there and forbids it otherwise.
	//
	// The address must lie inside the pool's spec.range, outside its exclusions, and
	// not be held by another claim or block. Anything else fails the request; it is
	// never replaced with a nearby free address.
	//
	// +kubebuilder:validation:Pattern=`^((25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])$`
	// +optional
	Address string `json:"address,omitempty"`
}

// RangeAllocation is the IPRangeRequest's side of the allocation policy.
//
// Auto carries count and asks for the lowest free run of that size. Exact carries
// range and asks for that specific band. The two are mutually exclusive and the
// schema enforces it. As on a claim, there is no fallback: an Exact band that is
// unavailable fails rather than being moved elsewhere.
//
// +kubebuilder:validation:XValidation:rule="self.strategy == 'Exact' ? (has(self.range) && !has(self.count)) : (has(self.count) && !has(self.range))",message="strategy Exact requires allocation.range and forbids allocation.count; strategy Auto requires allocation.count and forbids allocation.range"
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="allocation is immutable: a block belongs to whatever was configured with it, and resizing or moving it would free addresses still in use. Delete this request and create a new one of the size and place you need."
type RangeAllocation struct {
	// strategy selects who chooses the block. Omitted means Auto.
	//
	// +kubebuilder:default=Auto
	// +required
	Strategy AllocationStrategy `json:"strategy"`

	// count is how many contiguous addresses an Auto request claims.
	//
	// The upper bound mirrors ipam.MaxRangeSize, the ceiling ParseRange applies to
	// a pool band — a block cannot be larger than the band it is drawn from. The
	// two are necessarily duplicated, since a CEL marker cannot reference a Go
	// constant; TestMaxRangeSize pins this side's counterpart and the controller
	// suite's "rejects a count above the range ceiling" spec pins the schema.
	//
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65536
	// +optional
	Count *int32 `json:"count,omitempty"`

	// range is the inclusive band an Exact request requires. Both bounds are
	// inside the pool's spec.range and outside its exclusions, and no address in
	// the band may be held by another claim or block.
	//
	// +optional
	Range *IPRange `json:"range,omitempty"`
}

// Blocker names one object that stands between an Exact allocation and success.
//
// It is deliberately structured rather than only a sentence: the usual next step
// is to look at the holder, delete it if it is stale, or choose another address,
// and a name to look up is what makes that possible. Reason strings stay on the
// condition; this list is the detail behind "AddressUnavailable" or
// "RangeUnavailable".
type Blocker struct {
	// kind is the holder's kind: IPRequest or IPRangeRequest.
	// +required
	Kind string `json:"kind"`

	// namespace is the holder's namespace. Empty for cluster-scoped kinds.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// name is the holder's name.
	// +required
	Name string `json:"name"`

	// address is the single address the holder owns, when the blocker is an
	// IPRequest.
	// +optional
	Address string `json:"address,omitempty"`

	// range is the block the holder owns, when the blocker is an IPRangeRequest,
	// or the pool exclusion that covers the request.
	// +optional
	Range *IPRange `json:"range,omitempty"`
}

// AllowsExact reports whether a pool accepts strategy Exact. A pool with no
// allocation block, or one that leaves allowExact unset, accepts it — the feature
// is available by default and an admin opts out.
func (s *IPPoolSpec) AllowsExact() bool {
	return s.Allocation == nil || s.Allocation.AllowExact == nil || *s.Allocation.AllowExact
}

// Exclusions returns the pool's excluded bands, or nil when it declares none.
func (s *IPPoolSpec) Exclusions() []IPRange {
	if s.Allocation == nil {
		return nil
	}
	return s.Allocation.Exclude
}

// StrategyOf returns a claim's allocation strategy, treating an absent allocation
// as Auto. The absence case is what makes this API backward compatible with claims
// created before the field existed.
func (a *AddressAllocation) StrategyOf() AllocationStrategy {
	if a == nil || a.Strategy == "" {
		return AllocationAuto
	}
	return a.Strategy
}

// StrategyOf returns a block request's allocation strategy, treating an empty
// strategy as Auto for the same reason as a claim — with the difference that an
// empty strategy on a new object is rejected by the schema, so this only matters
// for objects created before the field existed.
func (a *RangeAllocation) StrategyOf() AllocationStrategy {
	if a == nil || a.Strategy == "" {
		return AllocationAuto
	}
	return a.Strategy
}
