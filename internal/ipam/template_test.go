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

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

// The full template: everything the vm-underlay-10-66-3-0 NAD carries today. The
// addressing is part of the template now, so it is here rather than passed
// alongside; range_start/range_end are still injected per claim.
func fullTemplate() *ipamv1alpha1.NADTemplate {
	return &ipamv1alpha1.NADTemplate{
		Subnet:      "10.66.3.0/24",
		Gateway:     "10.66.3.254",
		Type:        "cnv-bridge",
		Bridge:      "br0",
		MacSpoofChk: ptr(false),
		Routes:      []ipamv1alpha1.NADRoute{{Dst: "0.0.0.0/0"}},
		DNS:         &ipamv1alpha1.NADDNS{Nameservers: []string{"223.5.5.5", "8.8.8.8"}},
	}
}

// RenderNAD's output is what the CNI actually reads, and it is invisible until a
// VM either boots or does not. Asserting the exact bytes is the only way to catch
// a silent change of a key name or of the ipam range, both of which produce a VM
// that comes up on the wrong network rather than an error here.
func TestRenderNAD_GoldenConfig(t *testing.T) {
	got, err := RenderNAD(fullTemplate(), "cubestack6-static", netip.MustParseAddr("10.66.3.152"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := `{"name":"cubestack6-static","type":"cnv-bridge","bridge":"br0",` +
		`"macspoofchk":false,` +
		`"ipam":{"type":"whereabouts","range":"10.66.3.0/24",` +
		`"range_start":"10.66.3.152","range_end":"10.66.3.152",` +
		`"gateway":"10.66.3.254","routes":[{"dst":"0.0.0.0/0"}]},` +
		`"dns":{"nameservers":["223.5.5.5","8.8.8.8"]}}`

	if got != want {
		t.Errorf("rendered NAD config differs\n got: %s\nwant: %s", got, want)
	}
}

// Unset optional fields must be absent, not present-and-zero. A "vlan":0 or an
// empty "routes":[] is not the same thing as no VLAN and no routes — the first
// puts the interface on VLAN 0.
func TestRenderNAD_OmitsUnsetOptionalFields(t *testing.T) {
	tmpl := &ipamv1alpha1.NADTemplate{
		Subnet:  "10.66.3.0/24",
		Gateway: "10.66.3.254",
		Bridge:  "br0",
	}
	got, err := RenderNAD(tmpl, "x-static", netip.MustParseAddr("10.66.3.150"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := `{"name":"x-static","type":"cnv-bridge","bridge":"br0",` +
		`"ipam":{"type":"whereabouts","range":"10.66.3.0/24",` +
		`"range_start":"10.66.3.150","range_end":"10.66.3.150",` +
		`"gateway":"10.66.3.254"}}`

	if got != want {
		t.Errorf("rendered NAD config differs\n got: %s\nwant: %s", got, want)
	}
}

func TestRenderNAD_VLANIsEmittedWhenSet(t *testing.T) {
	tmpl := fullTemplate()
	tmpl.VLAN = ptr(int32(100))

	got, err := RenderNAD(tmpl, "x-static", netip.MustParseAddr("10.66.3.150"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"vlan":100`) {
		t.Errorf("expected vlan 100 in %s", got)
	}
}

// The ipam range is the template's subnet, not the assigned address. Whereabouts
// derives its allocation ledger from the range, so a per-address range here would
// give every VM its own private ledger and lose the shared one that makes a
// duplicate collision loud.
func TestRenderNAD_RangeIsTheSubnetNotTheAddress(t *testing.T) {
	tmpl := fullTemplate()
	tmpl.Subnet = "10.66.9.0/24"
	tmpl.Gateway = "10.66.9.254"

	got, err := RenderNAD(tmpl, "x-static", netip.MustParseAddr("10.66.9.7"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"range":"10.66.9.0/24"`) {
		t.Errorf("expected the ipam range to be the subnet, got: %s", got)
	}
	if !strings.Contains(got, `"range_start":"10.66.9.7"`) || !strings.Contains(got, `"range_end":"10.66.9.7"`) {
		t.Errorf("expected a single-address band, got: %s", got)
	}
}

// A single-address band is the mechanism by which the CNI enforces exclusivity.
// If start and end ever diverged, the NAD would be able to serve a second
// address and the whole claim layer's guarantee would be gone.
func TestRenderNAD_RangeStartEqualsRangeEnd(t *testing.T) {
	got, err := RenderNAD(fullTemplate(), "x-static", netip.MustParseAddr("10.66.3.152"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	start := strings.Index(got, `"range_start":"10.66.3.152"`)
	end := strings.Index(got, `"range_end":"10.66.3.152"`)
	if start < 0 || end < 0 {
		t.Fatalf("expected both bounds to be the assigned address, got: %s", got)
	}
}

func TestRenderNAD_Errors(t *testing.T) {
	valid := netip.MustParseAddr("10.66.3.152")

	// template returns a template with one field replaced, so each case varies
	// exactly the thing it is about.
	template := func(mutate func(*ipamv1alpha1.NADTemplate)) *ipamv1alpha1.NADTemplate {
		tmpl := fullTemplate()
		mutate(tmpl)
		return tmpl
	}

	tests := []struct {
		name    string
		tmpl    *ipamv1alpha1.NADTemplate
		nadName string
		addr    netip.Addr
		wantErr string
	}{
		{
			name:    "no template on the pool",
			tmpl:    nil,
			nadName: "x-static",
			addr:    valid,
			wantErr: "no nadTemplate",
		},
		{
			name:    "empty NAD name",
			tmpl:    fullTemplate(),
			nadName: "",
			addr:    valid,
			wantErr: "name is empty",
		},
		{
			// The CRD requires these fields, so these cases only guard hand-built
			// templates — but they would otherwise render an unusable NAD.
			name:    "empty bridge",
			tmpl:    template(func(t *ipamv1alpha1.NADTemplate) { t.Bridge = "" }),
			nadName: "x-static",
			addr:    valid,
			wantErr: "bridge is empty",
		},
		{
			name:    "empty subnet",
			tmpl:    template(func(t *ipamv1alpha1.NADTemplate) { t.Subnet = "" }),
			nadName: "x-static",
			addr:    valid,
			wantErr: "subnet is empty",
		},
		{
			// Without this check the config would carry "gateway":"", which is a
			// default route pointing nowhere.
			name:    "empty gateway",
			tmpl:    template(func(t *ipamv1alpha1.NADTemplate) { t.Gateway = "" }),
			nadName: "x-static",
			addr:    valid,
			wantErr: "gateway is empty",
		},
		{
			name:    "assigned address outside the pool subnet",
			tmpl:    fullTemplate(),
			nadName: "x-static",
			addr:    netip.MustParseAddr("10.66.9.152"),
			wantErr: "outside subnet",
		},
		{
			name:    "IPv6 assigned address",
			tmpl:    fullTemplate(),
			nadName: "x-static",
			addr:    netip.MustParseAddr("fd00::1"),
			wantErr: "IPv4-only",
		},
		{
			name:    "subnet is not a CIDR",
			tmpl:    template(func(t *ipamv1alpha1.NADTemplate) { t.Subnet = "10.66.3.0" }),
			nadName: "x-static",
			addr:    valid,
			wantErr: "not a valid CIDR",
		},
		{
			name:    "gateway is not an address",
			tmpl:    template(func(t *ipamv1alpha1.NADTemplate) { t.Gateway = "10.66.3.999" }),
			nadName: "x-static",
			addr:    valid,
			wantErr: "not a valid address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RenderNAD(tt.tmpl, tt.nadName, tt.addr)
			if err == nil {
				t.Fatalf("expected an error containing %q, got config: %s", tt.wantErr, got)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected an error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}
