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
	"fmt"
	"net/netip"
	"strings"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
	"github.com/suanova/cubestack-ipam/internal/ipam"
)

// The helpers here are the address arithmetic shared by both allocators. They are
// pure, so the reconciles stay testable without a cluster, and they all work on
// netip.Addr rather than strings — ordering is the operation this API needs most
// and the one Kubernetes CEL cannot express.

// requestedAddress returns the address an Exact claim asks for, or an invalid
// Addr for an Auto claim.
//
// The schema already requires the address on Exact and forbids it elsewhere, so
// this mostly re-parses for the controller's own use. It still validates rather
// than trusting admission: an object admitted before the rule existed, or written
// through a CRD that had not been applied yet, reaches this code too.
func requestedAddress(claim *ipamv1alpha1.IPRequest) (netip.Addr, error) {
	alloc := claim.Spec.Allocation
	if alloc.StrategyOf() != ipamv1alpha1.AllocationExact {
		return netip.Addr{}, nil
	}

	raw := strings.TrimSpace(alloc.Address)
	if raw == "" {
		return netip.Addr{}, &allocError{
			reason:  ipamv1alpha1.ReasonAllocationInvalid,
			message: "spec.allocation.address is required when strategy is Exact",
		}
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil || !addr.Is4() {
		return netip.Addr{}, &allocError{
			reason:  ipamv1alpha1.ReasonAllocationInvalid,
			message: fmt.Sprintf("spec.allocation.address %q is not a valid IPv4 address", alloc.Address),
		}
	}
	return addr, nil
}

// requestedBand returns the band an Exact block request asks for, or false for an
// Auto request.
//
// Auto's count is checked here too, so a malformed request fails with one clear
// condition rather than an exhaustion message derived from a zero.
func requestedBand(block *ipamv1alpha1.IPRangeRequest) (heldRange, bool, error) {
	alloc := &block.Spec.Allocation
	switch alloc.StrategyOf() {
	case ipamv1alpha1.AllocationExact:
		if alloc.Range == nil {
			return heldRange{}, false, &allocError{
				reason:  ipamv1alpha1.ReasonAllocationInvalid,
				message: "spec.allocation.range is required when strategy is Exact",
			}
		}
		// Count and range are mutually exclusive at the schema; violating that here
		// means the object predates the rule or the CRD had not been updated.
		if alloc.Count != nil {
			return heldRange{}, false, &allocError{
				reason:  ipamv1alpha1.ReasonAllocationInvalid,
				message: "spec.allocation.count must be absent when strategy is Exact",
			}
		}
		start, end, ok := bandBounds(*alloc.Range)
		if !ok {
			return heldRange{}, false, &allocError{
				reason:  ipamv1alpha1.ReasonAllocationInvalid,
				message: fmt.Sprintf("spec.allocation.range %s-%s is not a usable band", alloc.Range.Start, alloc.Range.End),
			}
		}
		if start.Compare(end) > 0 {
			return heldRange{}, false, &allocError{
				reason:  ipamv1alpha1.ReasonAllocationInvalid,
				message: fmt.Sprintf("spec.allocation.range start %s is greater than its end %s", start, end),
			}
		}
		// ParseRange also applies MaxRangeSize, so an Exact request cannot materialize
		// a larger set than an Auto one can. The result is discarded; this is a
		// validation call.
		if _, err := ipam.ParseRange(start.String(), end.String()); err != nil {
			return heldRange{}, false, &allocError{reason: ipamv1alpha1.ReasonAllocationInvalid, message: err.Error()}
		}
		return heldRange{start: start, end: end}, true, nil

	default: // Auto
		if alloc.Count == nil || *alloc.Count <= 0 {
			return heldRange{}, false, &allocError{
				reason:  ipamv1alpha1.ReasonAllocationInvalid,
				message: "spec.allocation.count is required and must be positive when strategy is Auto",
			}
		}
		return heldRange{}, false, nil
	}
}

// bandBounds parses both ends of a band.
func bandBounds(band ipamv1alpha1.IPRange) (start, end netip.Addr, ok bool) {
	s, err := netip.ParseAddr(strings.TrimSpace(band.Start))
	if err != nil {
		return netip.Addr{}, netip.Addr{}, false
	}
	e, err := netip.ParseAddr(strings.TrimSpace(band.End))
	if err != nil {
		return netip.Addr{}, netip.Addr{}, false
	}
	return s, e, true
}

// bandContains reports whether addr lies inside band, inclusive.
func bandContains(band ipamv1alpha1.IPRange, addr netip.Addr) bool {
	start, end, ok := bandBounds(band)
	if !ok {
		return false
	}
	return start.Compare(addr) <= 0 && addr.Compare(end) <= 0
}

// bandCovers reports whether inner lies entirely inside outer.
//
// Both bands are expected to have been validated already; an unparseable bound is
// treated as not covered, which fails the request safely rather than admitting an
// address whose position cannot be established.
func bandCovers(outer, inner ipamv1alpha1.IPRange) bool {
	os, oe, ok := bandBounds(outer)
	if !ok {
		return false
	}
	is, ie, ok := bandBounds(inner)
	if !ok {
		return false
	}
	return os.Compare(is) <= 0 && ie.Compare(oe) <= 0
}

// bandIntersects reports whether two inclusive bands share an address.
func bandIntersects(a, b ipamv1alpha1.IPRange) bool {
	as, ae, ok := bandBounds(a)
	if !ok {
		return false
	}
	bs, be, ok := bandBounds(b)
	if !ok {
		return false
	}
	return as.Compare(be) <= 0 && bs.Compare(ae) <= 0
}

// exclusionBlocker renders a pool exclusion as a status blocker. Its kind is
// IPPool because that is the object an admin edits to remove the reservation.
func exclusionBlocker(poolName string, band ipamv1alpha1.IPRange) ipamv1alpha1.Blocker {
	return ipamv1alpha1.Blocker{
		Kind:  "IPPool",
		Name:  poolName,
		Range: &ipamv1alpha1.IPRange{Start: band.Start, End: band.End},
	}
}
