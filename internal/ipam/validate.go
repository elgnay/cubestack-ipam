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

// Package ipam holds address arithmetic for the IPPool/IPRequest claim layer.
//
// It deliberately contains no allocation logic yet: the design requires the §8
// spikes to run before an address can be claimed. What lives here is the
// validation the CRD schema is unable to express.
package ipam

import (
	"fmt"
	"net/netip"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

// ValidatePoolRange checks the IPPool invariants that cannot be expressed as
// schema rules.
//
// The Kubernetes CEL net library provides cidr(...).containsIP(ip(...)) but no
// ordering overload on IP values, so "start <= end" and "gateway must not fall
// inside the range" cannot be CEL validations however they are written. They are
// checked here instead.
//
// Subnet containment IS also a schema rule; it is repeated here so this function
// is safe on objects that never went through the current schema (created before
// it existed, or admitted while the CRD was mid-update).
//
// This is a pure function: no cluster access, no allocation, no side effects.
func ValidatePoolRange(spec ipamv1alpha1.IPPoolSpec) error {
	subnet, err := netip.ParsePrefix(spec.Subnet)
	if err != nil {
		return fmt.Errorf("subnet %q is not a valid CIDR: %w", spec.Subnet, err)
	}
	if !subnet.Addr().Is4() {
		return fmt.Errorf("subnet %q is not IPv4; this claim layer is IPv4-only", spec.Subnet)
	}

	// Parsed explicitly rather than via a helper so each error names its own field.
	start, err := netip.ParseAddr(spec.Range.Start)
	if err != nil {
		return fmt.Errorf("range.start %q is not a valid address: %w", spec.Range.Start, err)
	}
	end, err := netip.ParseAddr(spec.Range.End)
	if err != nil {
		return fmt.Errorf("range.end %q is not a valid address: %w", spec.Range.End, err)
	}
	gateway, err := netip.ParseAddr(spec.Gateway)
	if err != nil {
		return fmt.Errorf("gateway %q is not a valid address: %w", spec.Gateway, err)
	}

	for _, a := range []netip.Addr{start, end, gateway} {
		if !a.Is4() {
			return fmt.Errorf("address %s is not IPv4; this claim layer is IPv4-only", a)
		}
	}

	// Containment is checked before ordering: an address outside the subnet is a
	// more fundamental mistake, and reporting "start > end" for 10.66.4.10-10.66.3.189
	// would send the reader looking in the wrong place.
	if !subnet.Contains(start) || !subnet.Contains(end) {
		return fmt.Errorf("range %s-%s must lie entirely within subnet %s", start, end, subnet)
	}

	// netip.Addr.Compare orders by address value, which is what makes this check
	// possible in Go and impossible in CEL.
	if start.Compare(end) > 0 {
		return fmt.Errorf("range.start %s must not be greater than range.end %s", start, end)
	}

	// The gateway must sit outside the allocatable band. Overlapping it means the
	// pool can hand a VM the gateway's own address, which fails in a way that looks
	// like a broken network rather than a bad pool.
	if gateway.Compare(start) >= 0 && gateway.Compare(end) <= 0 {
		return fmt.Errorf("gateway %s must not fall inside the allocatable range %s-%s", gateway, start, end)
	}

	return nil
}
