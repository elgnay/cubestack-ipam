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

// Package ipam holds the pure address and config logic behind the IPPool/IPRequest
// claim layer.
//
// Nothing here touches a cluster. That is deliberate: the parts most likely to be
// wrong — address arithmetic, the shape of a generated NAD — are the parts that
// are hardest to observe from the outside and easiest to get subtly wrong, so they
// live where they can be tested directly. The Whereabouts CNI is the only thing
// that ultimately enforces an allocation, and a mistake here surfaces as a VM that
// will not start rather than as an error at the point of the mistake.
package ipam

import (
	"errors"
	"fmt"
	"net/netip"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

// ValidatePoolRange checks the IPPool invariants that cannot be expressed as
// schema rules.
//
// The Kubernetes CEL net library provides cidr(...).containsIP(ip(...)) but no
// ordering overload on IP values, so "start <= end", "gateway must not fall
// inside the range" and "gateway must sit inside the subnet" cannot be CEL
// validations however they are written. They are checked here instead.
//
// The subnet containment of the range IS also a schema rule; it is repeated here
// so this function is safe on objects that never went through the current schema
// (created before it existed, or admitted while the CRD was mid-update).
//
// spec.range is the only required field: a pool with no nadTemplate allocates
// addresses and mints no config, so it has no subnet or gateway to check. When
// the template IS present, its addressing is validated in full — the template's
// fields are required by the schema, but this function must still cope with a
// template that is present and incomplete, because a CRD update can admit one
// that the old schema would not have.
//
// This is a pure function: no cluster access, no allocation, no side effects.
func ValidatePoolRange(spec ipamv1alpha1.IPPoolSpec) error {
	// Parsed explicitly rather than via a helper so each error names its own field.
	start, err := netip.ParseAddr(spec.Range.Start)
	if err != nil {
		return fmt.Errorf("range.start %q is not a valid address: %w", spec.Range.Start, err)
	}
	end, err := netip.ParseAddr(spec.Range.End)
	if err != nil {
		return fmt.Errorf("range.end %q is not a valid address: %w", spec.Range.End, err)
	}

	// netip.Addr.Compare orders by address value, which is what makes this check
	// possible in Go and impossible in CEL.
	if start.Compare(end) > 0 {
		return fmt.Errorf("range.start %s must not be greater than range.end %s", start, end)
	}

	if spec.NADTemplate == nil {
		// Nothing mints from this pool, so there is no config to be incoherent.
		// The band still has to be a usable band, which is the two checks above.
		for _, a := range []netip.Addr{start, end} {
			if !a.Is4() {
				return fmt.Errorf("address %s is not IPv4; this claim layer is IPv4-only", a)
			}
		}
		return nil
	}

	tmpl := spec.NADTemplate
	if tmpl.Subnet == "" {
		return errors.New("nadTemplate.subnet is required when nadTemplate is set")
	}
	if tmpl.Gateway == "" {
		return errors.New("nadTemplate.gateway is required when nadTemplate is set")
	}

	subnet, err := netip.ParsePrefix(tmpl.Subnet)
	if err != nil {
		return fmt.Errorf("nadTemplate.subnet %q is not a valid CIDR: %w", tmpl.Subnet, err)
	}
	if !subnet.Addr().Is4() {
		return fmt.Errorf("nadTemplate.subnet %q is not IPv4; this claim layer is IPv4-only", tmpl.Subnet)
	}
	gateway, err := netip.ParseAddr(tmpl.Gateway)
	if err != nil {
		return fmt.Errorf("nadTemplate.gateway %q is not a valid address: %w", tmpl.Gateway, err)
	}

	for _, a := range []netip.Addr{start, end, gateway} {
		if !a.Is4() {
			return fmt.Errorf("address %s is not IPv4; this claim layer is IPv4-only", a)
		}
	}

	// Containment is checked before the gateway's own position: an address outside
	// the subnet is the more fundamental mistake, and reporting a gateway problem
	// for 10.66.4.10-10.66.3.189 would send the reader looking in the wrong place.
	if !subnet.Contains(start) || !subnet.Contains(end) {
		return fmt.Errorf("range %s-%s must lie entirely within subnet %s", start, end, subnet)
	}

	// The gateway must sit OUTSIDE the allocatable band: overlapping it means the
	// pool can hand a VM the gateway's own address, which fails in a way that looks
	// like a broken network rather than a bad pool.
	if gateway.Compare(start) >= 0 && gateway.Compare(end) <= 0 {
		return fmt.Errorf("gateway %s must not fall inside the allocatable range %s-%s", gateway, start, end)
	}

	// ...and INSIDE the subnet. A bridge binding needs the gateway on the same L2
	// as the VM, so an off-subnet gateway produces a default route that nothing can
	// resolve: the guest keeps its address, CNI ADD succeeds, and every off-link
	// flow black-holes. Checking only the "not in the band" half would admit exactly
	// that, since an address in another subnet is trivially outside the band.
	if !subnet.Contains(gateway) {
		return fmt.Errorf("gateway %s must lie within subnet %s", gateway, subnet)
	}

	return nil
}
