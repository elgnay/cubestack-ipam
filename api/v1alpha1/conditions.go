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

// ConditionType aliases the plain string that metav1.Condition.Type is, so the
// constants below assign to it without a conversion. It is an alias, not a
// defined type, precisely so that meta.SetStatusCondition and friends accept them
// directly.
type ConditionType = string

// Condition types set on IPPool, IPRequest and IPRangeRequest.
//
// There is one primary condition per kind — Available for a pool, Ready for a
// claim and for a block — and the cause goes in its Reason rather than into a
// condition type of its own. A Degraded condition was considered and deliberately
// left out: on every kind it would be the exact complement of the primary one, so
// two entries would carry one fact and could disagree.
const (
	// ConditionAvailable is True when an IPPool is usable — its range parses and
	// its band and gateway are coherent.
	ConditionAvailable ConditionType = "Available"

	// ConditionReady is True when an IPRequest has been satisfied: an address is
	// assigned, and a NAD exists if one was asked for. It is also the primary
	// condition on an IPRangeRequest, True once a block is recorded in status.
	ConditionReady ConditionType = "Ready"

	// ConditionTemplateMissing is True when a claim asks for a NAD (spec.nad is
	// set) but its pool carries no nadTemplate, so none can be minted. It is set
	// alongside Ready=False because it names a misconfiguration the user can fix,
	// and because a condition is easier to select on than a reason string.
	ConditionTemplateMissing ConditionType = "TemplateMissing"

	// ConditionNADNameTaken is True when the name in spec.nad is already held by
	// an object this claim does not own. We never adopt or overwrite an object we
	// did not create, so this fails the claim instead.
	ConditionNADNameTaken ConditionType = "NADNameTaken"
)

// Reasons accompanying the conditions above. A Reason is required on every
// metav1.Condition, so these exist to keep them consistent between the code that
// writes a condition and the tests that assert on it.
const (
	ReasonPoolNotFound   = "PoolNotFound"
	ReasonRangeValid     = "RangeValid"
	ReasonRangeInvalid   = "RangeInvalid"
	ReasonRangeExhausted = "RangeExhausted"
	// ReasonRangeFragmented accompanies Ready=False on an IPRangeRequest whose pool
	// holds at least spec.allocation.count free addresses in total, but not that many together.
	// It is deliberately distinct from ReasonRangeExhausted because the two
	// prescribe different fixes: an exhausted band needs a bigger pool, a fragmented
	// one needs a smaller request or the fragmenting claims released. Reporting
	// "exhausted" when a run merely failed to line up would be false, and a false
	// diagnosis is worse than a vague one.
	ReasonRangeFragmented = "RangeFragmented"
	ReasonBound           = "Bound"
	ReasonTemplateAbsent  = "TemplateAbsent"
	ReasonNADNotOwned     = "NADNotOwned"
	ReasonNADMissing      = "NADMissing"
	ReasonQueryFailed     = "QueryFailed"

	// The reasons below belong to Exact allocation. Each one is a different
	// actionable fact: an out-of-range address means the request and the pool
	// disagree, an excluded one means an admin reserved it, and an unavailable one
	// means another holder has it. Collapsing them into one reason would leave the
	// user to work out which of three unrelated fixes applies.
	//
	// ReasonAddressOutOfRange and ReasonRangeOutOfRange mean the requested value is
	// not inside the pool's spec.range at all.
	ReasonAddressOutOfRange = "AddressOutOfRange"
	ReasonRangeOutOfRange   = "RangeOutOfRange"

	// ReasonAddressExcluded and ReasonRangeExcluded mean the requested value is
	// inside one of the pool's spec.allocation.exclude bands.
	ReasonAddressExcluded = "AddressExcluded"
	ReasonRangeExcluded   = "RangeExcluded"

	// ReasonAddressUnavailable and ReasonRangeUnavailable mean the requested value
	// is valid for the pool but already held. The holders are listed in
	// status.blockers.
	ReasonAddressUnavailable = "AddressUnavailable"
	ReasonRangeUnavailable   = "RangeUnavailable"

	// ReasonExactNotAllowed means the pool sets spec.allocation.allowExact to
	// false.
	ReasonExactNotAllowed = "ExactNotAllowed"

	// ReasonAllocationInvalid means the request itself is malformed — a status that
	// disagrees with spec, or an object predating the allocation field that never
	// recorded the size of its block. Deleting and recreating is the fix in every
	// case, which is what the message says.
	ReasonAllocationInvalid = "AllocationInvalid"
)

// Annotations read from a VirtualMachine to drive the claim layer. Keys are
// exported so they appear in godoc rather than only in the README.
const (
	// AnnotationPool names the cluster-scoped IPPool to claim an address from.
	// Its presence is the trigger: the VirtualMachine controller does nothing to a
	// VM that lacks it.
	AnnotationPool = "ipam.cubestack.io/pool"

	// AnnotationNADName overrides the name of the NAD minted for the VM. Optional;
	// it defaults to DefaultNADName(vm.Name), i.e. "<vm-name>-static".
	AnnotationNADName = "ipam.cubestack.io/nad-name"

	// AnnotationAddress asks for a specific address. Optional, and Exact when
	// present: the claim fails rather than being given a different address if this
	// one is out of range, excluded, or already held. Ignored when AnnotationPool
	// is absent, because without a pool there is no claim to allocate.
	AnnotationAddress = "ipam.cubestack.io/address"
)

// Labels written by the controllers.
const (
	// LabelRequest is set on a minted NAD and carries the name of the IPRequest
	// that owns it. The NAD is always created in its IPRequest's namespace, so the
	// name alone identifies the owner — the audit uses this to find NADs whose
	// IPRequest is gone.
	LabelRequest = "ipam.cubestack.io/request"
)
