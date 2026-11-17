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
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
	"github.com/suanova/cubestack-ipam/internal/ipam"
)

// claimHold is one IPRequest's address, remembered with enough of its identity to
// name it in a blocker.
type claimHold struct {
	namespace string
	name      string
	addr      netip.Addr
}

// ledger is everything currently held in one pool, across all namespaces — the
// pool is cluster-scoped, so the ledger is too.
//
// It reads both kinds that hold addresses: a single-address IPRequest and a
// contiguous block held by an IPRangeRequest. One reader for both is the point,
// not a convenience. When only IPRequests were read, a block delegated out of a
// pool was invisible to the claim allocator, which would happily place a VM's
// address in the middle of it — the exact collision this project exists to
// prevent, arrived at by construction rather than by a typo. Neither allocator may
// re-derive its own view of what is held; that is how two views drift.
//
// The holder lists are not redundant with taken. taken answers "is this address
// free"; the lists answer "who has it", which is what an Exact request needs in
// status.blockers to be actionable. Both come from the same List so they cannot
// disagree.
type ledger struct {
	taken  map[netip.Addr]struct{}
	claims []claimHold
	blocks []blockHold
}

// readLedger lists every address currently held in poolRef.
//
// selfUID is skipped so a request is not blocked by its own hold. Compared by UID
// rather than name, because the object in hand may not have one yet; a persisted
// object always does, so an empty selfUID matches nothing a List can return.
//
// HOLDING IS NOT PHASE-DEPENDENT. Anything with a recorded address or block holds
// it, whatever its phase. That is deliberate and it is what makes the sticky rule
// on the allocators safe: a request whose pool was deleted goes to Failed but
// keeps its status, and if the ledger only counted Bound objects it would silently
// free addresses that are still written into a running VM's NAD or into some
// off-cluster consumer's config. A request with an empty or unparseable status
// holds nothing, mirroring the claim rule, and is that object's own condition —
// and the audit's — to report rather than a reason to fail the whole pool.
//
// This reads IPRequests and IPRangeRequests rather than a pool-owned ledger,
// because those two sets together are the only authoritative record (design §R3).
// A second list on the pool would be a second thing to keep in step, and the two
// would eventually disagree.
//
// KNOWN RACE: two holders created in the same instant can both be reconciled
// against a cache that does not yet show the other's assignment, and both pick the
// same address — and an Exact request makes that race much easier to hit, because
// two callers naming one address is exactly what it looks like. The window is
// small, but how bad it is depends on the pool's ipam mode, and that is worth
// knowing before assuming the CNI is behind this. On a whereabouts pool the second
// CNI ADD is refused, so the collision is loud and the audit explains a failure
// that already happened. On a static pool nothing refuses it: both VMs come up
// sharing an address and the symptom is intermittent connectivity, which makes the
// audit sweep's DuplicateAddress the only detector rather than a backstop. Closing
// the race properly needs a compare-and-swap on something address-shaped, which
// this API gave up when per-address claim objects were dropped. A delegated block
// inherits this race and adds no new one — but its consequence is worse, because
// nothing downstream refuses a duplicate at all, so AddressInBlock and RangeOverlap
// are the whole of the detection there.
func readLedger(ctx context.Context, c client.Client, poolRef string, selfUID types.UID) (*ledger, error) {
	var claims ipamv1alpha1.IPRequestList
	if err := c.List(ctx, &claims); err != nil {
		return nil, fmt.Errorf("listing IPRequests: %w", err)
	}

	var ranges ipamv1alpha1.IPRangeRequestList
	if err := c.List(ctx, &ranges); err != nil {
		return nil, fmt.Errorf("listing IPRangeRequests: %w", err)
	}

	l := &ledger{taken: make(map[netip.Addr]struct{}, len(claims.Items))}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.PoolRef != poolRef || claim.UID == selfUID {
			continue
		}
		held := strings.TrimSpace(claim.Status.AssignedIP)
		if held == "" {
			// Pending and Failed claims hold nothing. A failed claim deliberately does
			// not consume an address, so a misconfigured one cannot exhaust a pool.
			continue
		}
		addr, err := netip.ParseAddr(held)
		if err != nil {
			// One claim's malformed status must not block every other request in the
			// pool; it is that claim's own condition, and the audit's, to report.
			continue
		}
		l.taken[addr] = struct{}{}
		l.claims = append(l.claims, claimHold{namespace: claim.Namespace, name: claim.Name, addr: addr})
	}

	for i := range ranges.Items {
		block := &ranges.Items[i]
		if block.Spec.PoolRef != poolRef || block.UID == selfUID {
			continue
		}
		// Read through heldBlock and expanded from the stored start/end rather than
		// re-derived from the pool's band. A block that has drifted out of the band is
		// still held — the reconciler keeps it and reports the drift — so re-deriving
		// here would free addresses the block still covers.
		held, ok, err := heldBlock(block)
		if err != nil || !ok {
			// Same rule as a malformed assignedIP: this block holds nothing it can
			// name, and its own condition and the audit say so.
			continue
		}
		addrs, err := ipam.ParseRange(held.start.String(), held.end.String())
		if err != nil {
			continue
		}
		for _, a := range addrs {
			l.taken[a] = struct{}{}
		}
		l.blocks = append(l.blocks, blockHold{req: block, start: held.start, end: held.end})
	}

	return l, nil
}

// withExclusions returns the taken set plus the pool's excluded bands.
//
// Exclusions are merged into taken rather than checked in a second place, so Auto
// allocation cannot forget them: FirstFree and FirstFreeRun already skip anything
// in taken, and an exclusion is exactly an address that is permanently taken by
// the pool itself. The caller keeps the holder-only set for blocker messages, so
// an excluded address is never reported as held by an object that does not exist.
func (l *ledger) withExclusions(excludes []ipamv1alpha1.IPRange) (map[netip.Addr]struct{}, error) {
	out := make(map[netip.Addr]struct{}, len(l.taken))
	for a := range l.taken {
		out[a] = struct{}{}
	}
	for _, band := range excludes {
		addrs, err := ipam.ParseRange(band.Start, band.End)
		if err != nil {
			return nil, fmt.Errorf("pool exclusion %s-%s is not usable: %w", band.Start, band.End, err)
		}
		for _, a := range addrs {
			out[a] = struct{}{}
		}
	}
	return out, nil
}

// blockersForAddress names every holder of addr: the claims that record it, and
// the blocks that cover it. A bracket of blocks is possible because a block can
// be contiguous with another without overlapping it, and either one could have
// been allocated first.
func (l *ledger) blockersForAddress(addr netip.Addr) []ipamv1alpha1.Blocker {
	var out []ipamv1alpha1.Blocker
	for _, claim := range l.claims {
		if claim.addr != addr {
			continue
		}
		out = append(out, ipamv1alpha1.Blocker{
			Kind:      "IPRequest",
			Namespace: claim.namespace,
			Name:      claim.name,
			Address:   addr.String(),
		})
	}
	for _, block := range l.blocks {
		if !block.covers(addr) {
			continue
		}
		out = append(out, blockBlocker(block))
	}
	sortBlockers(out)
	return out
}

// blockersForRange names every holder that intersects the inclusive band: claims
// whose address falls inside it, and blocks it overlaps. Claims are compared per
// address because the band may be up to MaxRangeSize long and the claim set is
// small; blocks are compared as bands for the same reason the audit does.
func (l *ledger) blockersForRange(start, end netip.Addr) []ipamv1alpha1.Blocker {
	var out []ipamv1alpha1.Blocker
	for _, claim := range l.claims {
		if claim.addr.Compare(start) < 0 || claim.addr.Compare(end) > 0 {
			continue
		}
		out = append(out, ipamv1alpha1.Blocker{
			Kind:      "IPRequest",
			Namespace: claim.namespace,
			Name:      claim.name,
			Address:   claim.addr.String(),
		})
	}
	requested := blockHold{start: start, end: end}
	for _, block := range l.blocks {
		if _, _, overlap := intersect(block, requested); !overlap {
			continue
		}
		out = append(out, blockBlocker(block))
	}
	sortBlockers(out)
	return out
}

// blockBlocker renders a block as a status blocker.
func blockBlocker(block blockHold) ipamv1alpha1.Blocker {
	return ipamv1alpha1.Blocker{
		Kind:      "IPRangeRequest",
		Namespace: block.req.Namespace,
		Name:      block.req.Name,
		Range:     &ipamv1alpha1.IPRange{Start: block.start.String(), End: block.end.String()},
	}
}

// sortBlockers orders blockers so the same failed request always reports the same
// list. Map iteration and list order are not guaranteed to match between
// reconciles, and a status that churns on identical inputs would be rewritten on
// every pass.
func sortBlockers(blockers []ipamv1alpha1.Blocker) {
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].Kind != blockers[j].Kind {
			return blockers[i].Kind < blockers[j].Kind
		}
		if blockers[i].Namespace != blockers[j].Namespace {
			return blockers[i].Namespace < blockers[j].Namespace
		}
		if blockers[i].Name != blockers[j].Name {
			return blockers[i].Name < blockers[j].Name
		}
		return blockers[i].Address < blockers[j].Address
	})
}

// poolRefOf returns the pool an object draws addresses from, for watch map
// functions: a pool names itself, and everything else that holds an address names
// the pool it drew from.
//
// The bool is false for an object this project does not know how to attribute,
// which a map function turns into "enqueue nothing" rather than a pool named "".
func poolRefOf(obj client.Object) (string, bool) {
	switch o := obj.(type) {
	case *ipamv1alpha1.IPPool:
		return o.Name, true
	case *ipamv1alpha1.IPRequest:
		return o.Spec.PoolRef, true
	case *ipamv1alpha1.IPRangeRequest:
		return o.Spec.PoolRef, true
	default:
		return "", false
	}
}

// blockAddrs expands a range request's recorded block into its addresses.
//
// An empty or unparseable status is an error, not an empty block: the caller skips
// the object either way, but the distinguishing is worth keeping because "this
// block records nothing" and "this block records something unreadable" are
// different problems for whoever reads the condition.
func blockAddrs(block *ipamv1alpha1.IPRangeRequest) ([]netip.Addr, error) {
	start := strings.TrimSpace(block.Status.Start)
	end := strings.TrimSpace(block.Status.End)
	if start == "" || end == "" {
		return nil, fmt.Errorf("range request %s/%s records no block", block.Namespace, block.Name)
	}
	addrs, err := ipam.ParseRange(start, end)
	if err != nil {
		return nil, fmt.Errorf("range request %s/%s has an unusable block %s-%s: %w", block.Namespace, block.Name, start, end, err)
	}
	return addrs, nil
}
