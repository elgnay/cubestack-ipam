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
