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

package ipam

import (
	"fmt"
	"net/netip"
)

// MaxRangeSize bounds how large a pool band may be.
//
// ParseRange materializes the band, so an unbounded one is a memory hazard: a
// typed-in 0.0.0.0-255.255.255.255 would be four billion netip.Addr. The real
// bands are sub-/24s, tens of addresses, so a /16 ceiling is far above any
// legitimate use and still small enough to hold comfortably.
//
// Exported because the CRD schema caps IPRangeRequest.spec.allocation.count with the same
// number, and a marker cannot reference a Go constant. The duplication is
// deliberate and TestMaxRangeSize pins it from this side; the schema spec pins
// the other. If you change this, both must move together.
const MaxRangeSize = 1 << 16

// ParseRange expands an inclusive address band into its addresses, lowest first.
//
// Both bounds are inclusive, and start == end is legal — that is the
// single-address shape this project generates NADs from.
//
// Callers should have run ValidatePoolRange first. ParseRange still rejects an
// inverted band rather than returning an empty slice, because an empty result and
// an exhausted band are indistinguishable at the call site and mean opposite
// things: one is a malformed pool, the other a full one.
func ParseRange(start, end string) ([]netip.Addr, error) {
	s, err := netip.ParseAddr(start)
	if err != nil {
		return nil, fmt.Errorf("range start %q is not a valid address: %w", start, err)
	}
	e, err := netip.ParseAddr(end)
	if err != nil {
		return nil, fmt.Errorf("range end %q is not a valid address: %w", end, err)
	}
	if s.Compare(e) > 0 {
		return nil, fmt.Errorf("range start %s is greater than range end %s", s, e)
	}

	addrs := make([]netip.Addr, 0, 1)
	// The IsValid check is load-bearing: Addr.Next() returns the zero Addr past
	// 255.255.255.255, and an invalid Addr compares less than a valid one, so
	// without it the loop would never terminate on a band ending at the top of the
	// address space.
	for a := s; a.IsValid() && a.Compare(e) <= 0; a = a.Next() {
		if len(addrs) >= MaxRangeSize {
			return nil, fmt.Errorf("range %s-%s holds more than %d addresses; this claim layer only serves sub-/16 bands", s, e, MaxRangeSize)
		}
		addrs = append(addrs, a)
	}
	return addrs, nil
}

// FirstFree returns the lowest address in addrs that is not in taken. The second
// result is false when every address is taken.
//
// Lowest-first rather than an arbitrary free address is deliberate: it makes
// allocation deterministic, so a pool with a given set of claims always hands out
// the same addresses, and a test can assert on a specific one without controlling
// iteration order.
func FirstFree(addrs []netip.Addr, taken map[netip.Addr]struct{}) (netip.Addr, bool) {
	for _, a := range addrs {
		if _, isTaken := taken[a]; !isTaken {
			return a, true
		}
	}
	return netip.Addr{}, false
}

// FirstFreeRun returns the lowest contiguous run of n addresses in addrs that
// contains no taken address, as its inclusive start and end.
//
// It requires addrs to be ascending and gap-free -- adjacent elements numerically
// adjacent, with no step over a skipped address -- which is exactly what
// ParseRange produces. That property is what makes a single O(len) pass correct:
// a run found in the slice is a run in the address space. Given an addrs built
// some other way (say, a filtered slice), the result is meaningless rather than
// merely wrong.
//
// The run is the lowest free one, not an aligned one. Power-of-two alignment was
// considered and rejected: on a band of, say, 31 addresses it would waste most of
// them, and nothing has asked for it. If a consumer ever needs alignment it is an
// optional field on the request, not a default here.
//
// The second and third results are false when no such run exists, which includes
// n <= 0: a request for nothing is not a run of nothing, it is a request that
// cannot be satisfied.
func FirstFreeRun(addrs []netip.Addr, taken map[netip.Addr]struct{}, n int) (start, end netip.Addr, ok bool) {
	if n <= 0 || n > len(addrs) {
		return netip.Addr{}, netip.Addr{}, false
	}
	run := 0
	for i, a := range addrs {
		if _, isTaken := taken[a]; isTaken {
			run = 0
			continue
		}
		run++
		if run == n {
			return addrs[i-n+1], a, true
		}
	}
	return netip.Addr{}, netip.Addr{}, false
}

// LongestFreeRun returns the length of the longest contiguous run of addresses in
// addrs that contains no taken address, and 0 when the band is full.
//
// Same ascending, gap-free requirement as FirstFreeRun. This exists for the
// failure message: when a request cannot be satisfied, "the band has 4 free
// addresses but not 8 together" is the difference between an exhausted pool and a
// fragmented one, and the caller cannot tell the user which without it.
func LongestFreeRun(addrs []netip.Addr, taken map[netip.Addr]struct{}) int {
	longest, run := 0, 0
	for _, a := range addrs {
		if _, isTaken := taken[a]; isTaken {
			run = 0
			continue
		}
		run++
		if run > longest {
			longest = run
		}
	}
	return longest
}
