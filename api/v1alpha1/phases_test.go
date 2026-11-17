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

import "testing"

// TestPhasesAgree pins the two phase vocabularies together.
//
// IPRequestPhase and IPRangeRequestPhase are separate Go types with the same three
// values, which is a deliberate trade: a field named "IPRequestPhase" on a request
// that is not an IPRequest reads as a mistake, and one vocabulary across both kinds
// is worth more than either. The cost is that the two can drift, and a drift here
// would show up as two kinds whose statuses mean different things by the same word.
// This is what closes it.
//
// The CRD enum markers are not covered here — they are asserted by the schema
// specs in the controller suite, which create objects and see them accepted or
// refused.
func TestPhasesAgree(t *testing.T) {
	pairs := []struct {
		claim IPRequestPhase
		block IPRangeRequestPhase
	}{
		{IPRequestPhasePending, IPRangeRequestPhasePending},
		{IPRequestPhaseBound, IPRangeRequestPhaseBound},
		{IPRequestPhaseFailed, IPRangeRequestPhaseFailed},
	}

	for _, p := range pairs {
		if string(p.claim) != string(p.block) {
			t.Errorf("phase vocabularies have drifted: IPRequestPhase %q vs IPRangeRequestPhase %q", p.claim, p.block)
		}
	}

	// Three values, and the list above covers all three. A fourth added to one kind
	// and not the other is exactly the drift this test exists to catch, and it would
	// otherwise be invisible: the pairs above would still agree.
	if len(pairs) != 3 {
		t.Fatalf("the phase list has %d entries; there are three phases, and a new one must be added to both kinds", len(pairs))
	}
}
