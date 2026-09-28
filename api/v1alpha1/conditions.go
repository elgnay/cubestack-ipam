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

// Condition types set on IPPool and IPRequest.
//
// There is one primary condition per kind — Available for a pool, Ready for a
// claim — and the cause goes in its Reason rather than into a condition type of
// its own. A Degraded condition was considered and deliberately left out: on both
// kinds it would be the exact complement of the primary one, so two entries would
// carry one fact and could disagree.
const (
	// ConditionAvailable is True when an IPPool is usable — its range parses and
	// its band and gateway are coherent.
	ConditionAvailable ConditionType = "Available"

	// ConditionReady is True when an IPRequest has been satisfied: an address is
	// assigned, and a NAD exists if one was asked for.
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
	ReasonBound          = "Bound"
	ReasonTemplateAbsent = "TemplateAbsent"
	ReasonNADNotOwned    = "NADNotOwned"
	ReasonNADMissing     = "NADMissing"
	ReasonQueryFailed    = "QueryFailed"
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
)

// Labels written by the controllers.
const (
	// LabelRequest is set on a minted NAD and carries the name of the IPRequest
	// that owns it. The NAD is always created in its IPRequest's namespace, so the
	// name alone identifies the owner — the audit uses this to find NADs whose
	// IPRequest is gone.
	LabelRequest = "ipam.cubestack.io/request"
)
