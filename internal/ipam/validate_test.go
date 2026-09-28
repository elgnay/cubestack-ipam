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
	"strings"
	"testing"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

func TestValidatePoolRange(t *testing.T) {
	// The band the design proposes for 10.66.3.0/24 (design §5.4), which is
	// disjoint from the Whereabouts window (.200-.220) and the MetalLB pool
	// (.221-.240). Disjointness itself is not checkable from this object.
	validSpec := func() ipamv1alpha1.IPPoolSpec {
		return ipamv1alpha1.IPPoolSpec{
			Subnet:      "10.66.3.0/24",
			Range:       ipamv1alpha1.IPRange{Start: "10.66.3.150", End: "10.66.3.189"},
			Gateway:     "10.66.3.254",
			TemplateNAD: "default/vm-underlay-10-66-3-0",
		}
	}

	tests := []struct {
		name    string
		mutate  func(*ipamv1alpha1.IPPoolSpec)
		wantErr string // substring; empty means no error expected
	}{
		{
			name:   "valid band",
			mutate: func(*ipamv1alpha1.IPPoolSpec) {},
		},
		{
			// A single-address band is the shape the controller will mint per-VM
			// NADs from, so it must validate.
			name: "single address band",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Range.Start = "10.66.3.105"
				s.Range.End = "10.66.3.105"
			},
		},
		{
			// The check CEL cannot express: IP ordering.
			name: "reversed range",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Range.Start = "10.66.3.189"
				s.Range.End = "10.66.3.150"
			},
			wantErr: "must not be greater than",
		},
		{
			name: "gateway inside the range",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Gateway = "10.66.3.160"
			},
			wantErr: "must not fall inside",
		},
		{
			// Boundary: a gateway equal to a bound is still inside.
			name: "gateway equal to range start",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Gateway = "10.66.3.150"
			},
			wantErr: "must not fall inside",
		},
		{
			name: "gateway above the range is fine",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Gateway = "10.66.3.190"
			},
		},
		{
			// Lower than end, so containment is the only rule this can trip —
			// keeping the two checks independently observable.
			name: "start outside subnet",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Range.Start = "10.66.2.10"
			},
			wantErr: "must lie entirely within",
		},
		{
			name: "end outside subnet",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Range.End = "10.66.4.10"
			},
			wantErr: "must lie entirely within",
		},
		{
			name: "subnet is not a CIDR",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Subnet = "10.66.3.0"
			},
			wantErr: "not a valid CIDR",
		},
		{
			name: "malformed start",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Range.Start = "10.66.3.999"
			},
			wantErr: "not a valid address",
		},
		{
			name: "IPv6 is rejected",
			mutate: func(s *ipamv1alpha1.IPPoolSpec) {
				s.Subnet = "fd00::/64"
				s.Range.Start = "fd00::1"
				s.Range.End = "fd00::10"
				s.Gateway = "fd00::ffff"
			},
			wantErr: "IPv4-only",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validSpec()
			tt.mutate(&spec)

			err := ValidatePoolRange(spec)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("expected no error, got: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("expected an error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

// The design's own pools must pass, or the defaults in config/samples are wrong.
func TestValidatePoolRange_DesignSamples(t *testing.T) {
	for _, spec := range []ipamv1alpha1.IPPoolSpec{
		{
			Subnet:      "10.66.3.0/24",
			Range:       ipamv1alpha1.IPRange{Start: "10.66.3.150", End: "10.66.3.189"},
			Gateway:     "10.66.3.254",
			TemplateNAD: "default/vm-underlay-10-66-3-0",
		},
		{
			// The user guide's alternative band (design R5 notes the docs disagree).
			Subnet:      "10.66.3.0/24",
			Range:       ipamv1alpha1.IPRange{Start: "10.66.3.181", End: "10.66.3.199"},
			Gateway:     "10.66.3.254",
			TemplateNAD: "default/vm-underlay-10-66-3-0",
		},
	} {
		if err := ValidatePoolRange(spec); err != nil {
			t.Errorf("pool %s-%s should be valid: %v", spec.Range.Start, spec.Range.End, err)
		}
	}
}
