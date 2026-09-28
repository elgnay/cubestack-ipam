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
	"encoding/json"
	"net/netip"
	"strings"
	"testing"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

// The full template: everything the vm-underlay-10-66-3-0 NAD carries today. The
// addressing is part of the template now, so it is here rather than passed
// alongside; only the assigned address is injected per claim.
//
// It carries IPAMStatic, which is what the deployed pool uses. The whereabouts form
// is exercised separately rather than off this one, because the two modes disagree
// about almost every key in the ipam block and a shared fixture would make it easy
// to assert the wrong mode without noticing.
func fullTemplate() *ipamv1alpha1.NADTemplate {
	return &ipamv1alpha1.NADTemplate{
		Subnet:      "10.66.3.0/24",
		Gateway:     "10.66.3.254",
		IPAM:        IPAMStatic,
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
		`"ipam":{"type":"static","addresses":[` +
		`{"address":"10.66.3.152/24","gateway":"10.66.3.254"}],` +
		`"routes":[{"dst":"0.0.0.0/0"}]},` +
		`"dns":{"nameservers":["223.5.5.5","8.8.8.8"]}}`

	if got != want {
		t.Errorf("rendered NAD config differs\n got: %s\nwant: %s", got, want)
	}
}

// The whereabouts form, asserted with the same byte-exactness as the static one:
// it is the mode a pool gets for free, so it is the one most likely to change by
// accident.
func TestRenderNAD_GoldenConfigWhereabouts(t *testing.T) {
	tmpl := fullTemplate()
	tmpl.IPAM = IPAMWhereabouts

	got, err := RenderNAD(tmpl, "cubestack6-static", netip.MustParseAddr("10.66.3.152"))
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

// A template written before the ipam field existed carries no value for it, and
// must keep the behaviour it was created with. Defaulting the other way would
// silently strip CNI-enforced exclusivity from every pool on upgrade, and nothing
// in the rendered config would look wrong.
func TestRenderNAD_UnsetIPAMModeIsWhereabouts(t *testing.T) {
	tmpl := fullTemplate()
	tmpl.IPAM = ""

	got, err := RenderNAD(tmpl, "x-static", netip.MustParseAddr("10.66.3.152"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"type":"whereabouts"`) {
		t.Errorf("expected the whereabouts plugin for an unset mode, got: %s", got)
	}
}

// Unset optional fields must be absent, not present-and-zero. A "vlan":0 or an
// empty "routes":[] is not the same thing as no VLAN and no routes — the first
// puts the interface on VLAN 0.
func TestRenderNAD_OmitsUnsetOptionalFields(t *testing.T) {
	tmpl := &ipamv1alpha1.NADTemplate{
		Subnet:  "10.66.3.0/24",
		Gateway: "10.66.3.254",
		IPAM:    IPAMStatic,
		Bridge:  "br0",
	}
	got, err := RenderNAD(tmpl, "x-static", netip.MustParseAddr("10.66.3.150"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := `{"name":"x-static","type":"cnv-bridge","bridge":"br0",` +
		`"ipam":{"type":"static","addresses":[` +
		`{"address":"10.66.3.150/24","gateway":"10.66.3.254"}]}}`

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

// The mask on the static address comes from the template's subnet, which is the
// only place the prefix length is declared. Hard-coding /24 would put a VM on the
// wrong-sized segment on any other subnet, and the rendered config would still
// look entirely plausible.
func TestRenderNAD_AddressMaskComesFromTheSubnet(t *testing.T) {
	tmpl := fullTemplate()
	tmpl.Subnet = "10.66.9.0/25"
	tmpl.Gateway = "10.66.9.126"

	got, err := RenderNAD(tmpl, "x-static", netip.MustParseAddr("10.66.9.7"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, `"address":"10.66.9.7/25"`) {
		t.Errorf("expected the assigned address to carry the subnet's /25, got: %s", got)
	}
}

// Exactly one address, and it is the assigned one. Nothing arbitrates this NAD at
// the CNI any more -- that is the point of the static form -- so a second address
// appearing here would silently widen what this VM can be addressed as, with
// nothing left to notice it.
func TestRenderNAD_ServesOnlyTheAssignedAddress(t *testing.T) {
	got, err := RenderNAD(fullTemplate(), "x-static", netip.MustParseAddr("10.66.3.152"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg struct {
		IPAM struct {
			Type      string `json:"type"`
			Addresses []struct {
				Address string `json:"address"`
			} `json:"addresses"`
		} `json:"ipam"`
	}
	if err := json.Unmarshal([]byte(got), &cfg); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if cfg.IPAM.Type != "static" {
		t.Errorf("expected the static plugin, got %q", cfg.IPAM.Type)
	}
	if len(cfg.IPAM.Addresses) != 1 {
		t.Fatalf("expected exactly one address, got %d: %s", len(cfg.IPAM.Addresses), got)
	}
	if cfg.IPAM.Addresses[0].Address != "10.66.3.152/24" {
		t.Errorf("expected the assigned address, got %q", cfg.IPAM.Addresses[0].Address)
	}
}

// The whereabouts form bounds its candidate set the other way: range_start and
// range_end pinned to the same address inside the shared ledger. Widening either
// one would let this NAD take an address that belongs to another claim, so the
// three fields are asserted together -- a correct range_start with a widened
// range_end is the plausible half-right state, and it is the dangerous one.
func TestRenderNAD_WhereaboutsPinsTheRangeToOneAddress(t *testing.T) {
	tmpl := fullTemplate()
	tmpl.IPAM = IPAMWhereabouts

	got, err := RenderNAD(tmpl, "x-static", netip.MustParseAddr("10.66.3.152"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var cfg struct {
		IPAM struct {
			Type       string `json:"type"`
			Range      string `json:"range"`
			RangeStart string `json:"range_start"`
			RangeEnd   string `json:"range_end"`
			Addresses  []any  `json:"addresses"`
		} `json:"ipam"`
	}
	if err := json.Unmarshal([]byte(got), &cfg); err != nil {
		t.Fatalf("rendered config is not valid JSON: %v", err)
	}
	if cfg.IPAM.Type != "whereabouts" {
		t.Errorf("expected the whereabouts plugin, got %q", cfg.IPAM.Type)
	}
	// The ledger is keyed by the range's CIDR. Pointing it at the band instead would
	// put this NAD on a different ledger from the dynamic pods on the same subnet,
	// and the duplicate it is supposed to refuse would go unnoticed.
	if cfg.IPAM.Range != "10.66.3.0/24" {
		t.Errorf("expected the ledger keyed on the subnet, got %q", cfg.IPAM.Range)
	}
	if cfg.IPAM.RangeStart != "10.66.3.152" || cfg.IPAM.RangeEnd != "10.66.3.152" {
		t.Errorf("expected both bounds pinned to the assigned address, got %q..%q", cfg.IPAM.RangeStart, cfg.IPAM.RangeEnd)
	}
	// Whereabouts writes range_start/range_end as bare addresses, unlike the static
	// plugin's CIDR. A mask creeping into either would be a copy-paste from the other
	// mode, and Whereabouts parses neither gracefully.
	if strings.Contains(got, `"range_start":"10.66.3.152/`) || strings.Contains(got, `"range_end":"10.66.3.152/`) {
		t.Errorf("expected unmasked range bounds in %s", got)
	}
	if len(cfg.IPAM.Addresses) != 0 {
		t.Errorf("expected no static address list, got %s", got)
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
