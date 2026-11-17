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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/recorder"
)

// RBAC for every event this package emits, declared once here rather than beside
// each emitting controller.
//
// THE GROUP IS NOT OPTIONAL AND NOT THE OBVIOUS ONE. controller-runtime's
// Manager.GetEventRecorder returns a recorder.EventRecorder, which writes
// events.k8s.io/v1 -- the newer events API, not the core v1 one. A marker granting
// only groups="" therefore looks right, generates a role that looks right, and
// fails at runtime: the API server rejects each event with
//
//	events.events.k8s.io is forbidden: ... cannot create resource "events"
//	in API group "events.k8s.io"
//
// and the broadcaster logs "Server rejected event (will not retry!)" and drops it.
// Nothing else fails -- the reconcile succeeds, the object is correct, and only the
// human-visible half of the contract is silently missing. That is exactly how this
// shipped broken: the events had never once been emitted on the live cluster.
//
// So: events.k8s.io, NOT "". There is no core-group marker to go with it, because
// nothing here uses the legacy k8s.io/client-go/tools/record recorder. If something
// ever does, it needs BOTH groups.

// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// recordEvent emits a Kubernetes event, or does nothing when no recorder is
// configured.
//
// The recorder is optional so that a test constructing a reconciler directly does
// not have to stand up an event broadcaster. In the binary it is always set, so a
// nil recorder means "not wired", never "events are off in production".
//
// Events are how the VM-side contract is made legible. Controller #2 is
// create-only, so the common mistake — editing the pool annotation on a VM that
// already has a claim — produces no object change at all; without an event it
// would look like the controller had simply ignored the edit.
//
// A nil-recorder guard is not enough to notice a missing grant, which is why the
// group is called out above: rec is non-nil in the binary, so every call succeeds
// locally and the rejection only appears in the broadcaster's log.
func recordEvent(rec recorder.EventRecorder, obj runtime.Object, eventtype, reason, action, note string, args ...any) {
	if rec == nil {
		return
	}
	rec.Eventf(obj, nil, eventtype, reason, action, note, args...)
}
