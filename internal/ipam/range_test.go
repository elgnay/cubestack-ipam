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
	"net/netip"
	"strings"
	"testing"
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		name    string
		start   string
		end     string
		want    []string
		wantErr string
	}{
		{
			name:  "single address band",
			start: "10.66.3.105",
			end:   "10.66.3.105",
			want:  []string{"10.66.3.105"},
		},
		{
			name:  "ascending band",
			start: "10.66.3.150",
			end:   "10.66.3.152",
			want:  []string{"10.66.3.150", "10.66.3.151", "10.66.3.152"},
		},
		{
			name:  "band crossing an octet boundary",
			start: "10.66.3.253",
			end:   "10.66.4.1",
			want:  []string{"10.66.3.253", "10.66.3.254", "10.66.3.255", "10.66.4.0", "10.66.4.1"},
		},
		{
			// Regression: Addr.Next() returns an invalid Addr past the top of the
			// address space, and an invalid Addr compares LESS than a valid one, so
			// a naive loop condition runs forever here.
			name:  "band ending at the top of the address space terminates",
			start: "255.255.255.254",
			end:   "255.255.255.255",
			want:  []string{"255.255.255.254", "255.255.255.255"},
		},
		{
			name:    "inverted band is an error, not an empty result",
			start:   "10.66.3.189",
			end:     "10.66.3.150",
			wantErr: "greater than range end",
		},
		{
			name:    "malformed start",
			start:   "10.66.3.999",
			end:     "10.66.3.150",
			wantErr: "not a valid address",
		},
		{
			name:    "malformed end",
			start:   "10.66.3.150",
			end:     "nonsense",
			wantErr: "not a valid address",
		},
		{
			// 65537 addresses, one past the cap.
			name:    "band larger than the cap is refused",
			start:   "10.0.0.0",
			end:     "11.0.0.0",
			wantErr: "only serves sub-/16 bands",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseRange(tt.start, tt.end)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected an error containing %q, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			gotStrs := make([]string, 0, len(got))
			for _, a := range got {
				gotStrs = append(gotStrs, a.String())
			}
			if len(gotStrs) != len(tt.want) {
				t.Fatalf("got %d addresses %v, want %d %v", len(gotStrs), gotStrs, len(tt.want), tt.want)
			}
			for i := range gotStrs {
				if gotStrs[i] != tt.want[i] {
					t.Errorf("address %d: got %s, want %s", i, gotStrs[i], tt.want[i])
				}
			}
		})
	}
}

func TestFirstFree(t *testing.T) {
	band, err := ParseRange("10.66.3.150", "10.66.3.153")
	if err != nil {
		t.Fatalf("parsing the test band: %v", err)
	}

	taken := func(addrs ...string) map[netip.Addr]struct{} {
		set := make(map[netip.Addr]struct{}, len(addrs))
		for _, a := range addrs {
			set[netip.MustParseAddr(a)] = struct{}{}
		}
		return set
	}

	tests := []struct {
		name      string
		taken     map[netip.Addr]struct{}
		want      string
		wantFound bool
	}{
		{
			name:      "empty ledger yields the lowest address",
			taken:     taken(),
			want:      "10.66.3.150",
			wantFound: true,
		},
		{
			name:      "skips taken addresses",
			taken:     taken("10.66.3.150", "10.66.3.151"),
			want:      "10.66.3.152",
			wantFound: true,
		},
		{
			// Gaps below a taken address must not be passed over: lowest-first is
			// what makes allocation deterministic.
			name:      "fills a gap below a taken address",
			taken:     taken("10.66.3.151"),
			want:      "10.66.3.150",
			wantFound: true,
		},
		{
			name:      "a full band is exhausted",
			taken:     taken("10.66.3.150", "10.66.3.151", "10.66.3.152", "10.66.3.153"),
			wantFound: false,
		},
		{
			// Addresses outside the band are none of this pool's business; a
			// neighbour pool's ledger entries must not shrink it.
			name:      "addresses outside the band are ignored",
			taken:     taken("10.66.3.149", "10.66.3.154"),
			want:      "10.66.3.150",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := FirstFree(band, tt.taken)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if !tt.wantFound {
				return
			}
			if got.String() != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestFirstFree_EmptyBand(t *testing.T) {
	// An empty band is not a legitimate pool (ValidatePoolRange rejects an inverted
	// one, and ParseRange never returns empty), but FirstFree must still report
	// exhaustion rather than panic on the zero value.
	if got, found := FirstFree(nil, nil); found {
		t.Errorf("an empty band must report exhaustion, got %s", got)
	}
}

func TestFirstFreeRun(t *testing.T) {
	band, err := ParseRange("10.66.3.150", "10.66.3.157")
	if err != nil {
		t.Fatalf("parsing the test band: %v", err)
	}

	taken := func(addrs ...string) map[netip.Addr]struct{} {
		set := make(map[netip.Addr]struct{}, len(addrs))
		for _, a := range addrs {
			set[netip.MustParseAddr(a)] = struct{}{}
		}
		return set
	}

	tests := []struct {
		name      string
		taken     map[netip.Addr]struct{}
		n         int
		wantStart string
		wantEnd   string
		wantFound bool
	}{
		{
			name:      "empty ledger yields the lowest run",
			taken:     taken(),
			n:         3,
			wantStart: "10.66.3.150",
			wantEnd:   "10.66.3.152",
			wantFound: true,
		},
		{
			name:      "exact fit at the top of the band",
			taken:     taken("10.66.3.150", "10.66.3.151", "10.66.3.152", "10.66.3.153"),
			n:         4,
			wantStart: "10.66.3.154",
			wantEnd:   "10.66.3.157",
			wantFound: true,
		},
		{
			// The whole point of "run" over "count": a free address that cannot be
			// part of a long-enough run must not be handed out.
			name:      "a gap too small to satisfy n is skipped for a later run",
			taken:     taken("10.66.3.151", "10.66.3.152", "10.66.3.153"),
			n:         3,
			wantStart: "10.66.3.154",
			wantEnd:   "10.66.3.156",
			wantFound: true,
		},
		{
			name:      "a taken address splits a run",
			taken:     taken("10.66.3.152"),
			n:         3,
			wantStart: "10.66.3.153",
			wantEnd:   "10.66.3.155",
			wantFound: true,
		},
		{
			// Lowest-first, not best-fit: a longer run further up the band must not
			// outrank a shorter one that starts lower. Determinism is the reason.
			name:      "the lowest sufficient run wins over a longer later one",
			taken:     taken("10.66.3.152", "10.66.3.153"),
			n:         2,
			wantStart: "10.66.3.150",
			wantEnd:   "10.66.3.151",
			wantFound: true,
		},
		{
			name:      "n of one degenerates to FirstFree",
			taken:     taken("10.66.3.150"),
			n:         1,
			wantStart: "10.66.3.151",
			wantEnd:   "10.66.3.151",
			wantFound: true,
		},
		{
			name:      "a full band has no run",
			taken:     taken("10.66.3.150", "10.66.3.151", "10.66.3.152", "10.66.3.153", "10.66.3.154", "10.66.3.155", "10.66.3.156", "10.66.3.157"),
			n:         1,
			wantFound: false,
		},
		{
			name:      "free addresses fragmented below n have no qualifying run",
			taken:     taken("10.66.3.151", "10.66.3.153", "10.66.3.155", "10.66.3.157"),
			n:         2,
			wantFound: false,
		},
		{
			name:      "n larger than the band",
			taken:     taken(),
			n:         9,
			wantFound: false,
		},
		{
			// A request for nothing is not a run of nothing. Reporting ok would
			// hand the caller a zero netip.Addr to write into a status.
			name:      "n of zero is not satisfiable",
			taken:     taken(),
			n:         0,
			wantFound: false,
		},
		{
			name:      "negative n is not satisfiable",
			taken:     taken(),
			n:         -1,
			wantFound: false,
		},
		{
			// Addresses outside the band are none of this pool's business, the same
			// rule FirstFree follows.
			name:      "addresses outside the band are ignored",
			taken:     taken("10.66.3.149", "10.66.3.158"),
			n:         2,
			wantStart: "10.66.3.150",
			wantEnd:   "10.66.3.151",
			wantFound: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, found := FirstFreeRun(band, tt.taken, tt.n)
			if found != tt.wantFound {
				t.Fatalf("found = %v, want %v", found, tt.wantFound)
			}
			if !tt.wantFound {
				return
			}
			if start.String() != tt.wantStart || end.String() != tt.wantEnd {
				t.Errorf("got %s-%s, want %s-%s", start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestFirstFreeRun_SingleAddressBand(t *testing.T) {
	band, err := ParseRange("10.66.3.150", "10.66.3.150")
	if err != nil {
		t.Fatalf("parsing the test band: %v", err)
	}

	if start, end, found := FirstFreeRun(band, nil, 1); !found || start.String() != "10.66.3.150" || end.String() != "10.66.3.150" {
		t.Errorf("a free one-address band must yield 10.66.3.150-10.66.3.150, got %s-%s found=%v", start, end, found)
	}
	if _, _, found := FirstFreeRun(band, map[netip.Addr]struct{}{netip.MustParseAddr("10.66.3.150"): {}}, 1); found {
		t.Error("a taken one-address band must report no run")
	}
	if _, _, found := FirstFreeRun(band, nil, 2); found {
		t.Error("a one-address band cannot satisfy n=2")
	}
}

func TestFirstFreeRun_EmptyBand(t *testing.T) {
	// Same reasoning as TestFirstFree_EmptyBand: not a legitimate pool, but the
	// primitive must report rather than panic on the zero value.
	if _, _, found := FirstFreeRun(nil, nil, 1); found {
		t.Error("an empty band must report no run")
	}
	if got := LongestFreeRun(nil, nil); got != 0 {
		t.Errorf("an empty band has no run, got %d", got)
	}
}

func TestLongestFreeRun(t *testing.T) {
	band, err := ParseRange("10.66.3.150", "10.66.3.157")
	if err != nil {
		t.Fatalf("parsing the test band: %v", err)
	}

	taken := func(addrs ...string) map[netip.Addr]struct{} {
		set := make(map[netip.Addr]struct{}, len(addrs))
		for _, a := range addrs {
			set[netip.MustParseAddr(a)] = struct{}{}
		}
		return set
	}

	tests := []struct {
		name  string
		taken map[netip.Addr]struct{}
		want  int
	}{
		{name: "an empty ledger leaves the whole band", taken: taken(), want: 8},
		{name: "a full band has no run", taken: taken("10.66.3.150", "10.66.3.151", "10.66.3.152", "10.66.3.153", "10.66.3.154", "10.66.3.155", "10.66.3.156", "10.66.3.157"), want: 0},
		{name: "reports the longest, not the first", taken: taken("10.66.3.150"), want: 7},
		{name: "reports the longest, not the last", taken: taken("10.66.3.157"), want: 7},
		// This is the fragmentation diagnosis the failure message depends on: two
		// free addresses exist, but neither n=2 run is where the caller looked.
		{name: "fragmented free addresses", taken: taken("10.66.3.150", "10.66.3.152", "10.66.3.153", "10.66.3.155", "10.66.3.156", "10.66.3.157"), want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LongestFreeRun(band, tt.taken); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMaxRangeSize pins the exported cap against the literal in the CRD marker on
// IPRangeRequest.spec.allocation.count. A marker cannot reference a Go constant, so the two
// are necessarily duplicated; this fails loudly if this side moves without the
// schema. The schema side is pinned by the "rejects a count above the range
// ceiling" spec in the controller suite, which also pins the boundary itself.
func TestMaxRangeSize(t *testing.T) {
	if MaxRangeSize != 65536 {
		t.Fatalf("MaxRangeSize is %d; the IPRangeRequest.spec.allocation.count Maximum marker says 65536 and must be updated with it", MaxRangeSize)
	}
}
