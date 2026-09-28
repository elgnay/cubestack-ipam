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
	"errors"
	"fmt"
	"net/netip"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

// DefaultCNIType is used when a NADTemplate does not name a plugin type. It is
// the linux bridge binding KubeVirt's virt-launcher uses on this cluster's
// underlay, and mirrors the CRD's own default for the field.
const DefaultCNIType = "cnv-bridge"

// whereaboutsIPAM is the only IPAM plugin this project generates. It is not a
// template field: the whole premise is a thin claim layer over the Whereabouts
// already installed (design §2), so an alternative here would mean a different
// cluster design rather than a different pool.
const whereaboutsIPAM = "whereabouts"

// nadConfig mirrors the NetworkAttachmentDefinition spec.config this project
// generates. It is a separate type from v1alpha1.NADTemplate because the two have
// genuinely different shapes: the template omits everything the controller
// injects, and the output includes fields the template has no say over.
//
// Field order matters only in that encoding/json emits it in declaration order,
// which is what makes the rendered output stable enough to golden-test.
type nadConfig struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Bridge      string      `json:"bridge"`
	VLAN        *int32      `json:"vlan,omitempty"`
	MacSpoofChk *bool       `json:"macspoofchk,omitempty"`
	IPAM        nadIPAM     `json:"ipam"`
	DNS         *nadDNS     `json:"dns,omitempty"`
}

type nadIPAM struct {
	Type       string     `json:"type"`
	Range      string     `json:"range"`
	RangeStart string     `json:"range_start"`
	RangeEnd   string     `json:"range_end"`
	Gateway    string     `json:"gateway"`
	Routes     []nadRoute `json:"routes,omitempty"`
}

type nadDNS struct {
	Nameservers []string `json:"nameservers,omitempty"`
	Domain      string   `json:"domain,omitempty"`
	Search      []string `json:"search,omitempty"`
}

type nadRoute struct {
	Dst string `json:"dst"`
	GW  string `json:"gw,omitempty"`
}

// RenderNAD builds the spec.config of the per-VM NetworkAttachmentDefinition for
// one claim.
//
// RangeStart and RangeEnd are both set to addr, which is the whole mechanism: a
// band holding a single address has no second address to hand out, so the CNI
// itself enforces that this NAD can only ever serve the address assigned to it.
// The claim ledger and the dataplane therefore cannot drift apart as long as
// allocation is single-writer.
//
// The ipam range is the template's subnet rather than the assigned address,
// because Whereabouts derives its allocation ledger from the range — every NAD
// sharing a range shares one ledger, which is what makes a duplicate a loud CNI
// failure rather than a silent one.
//
// tmpl may not be nil: a pool without a template cannot mint, and the caller is
// expected to have reported that as a TemplateMissing condition already. The
// subnet and gateway come from the template itself, so there is no way to render
// a config whose addressing disagrees with the pool's declaration.
func RenderNAD(tmpl *ipamv1alpha1.NADTemplate, name string, addr netip.Addr) (string, error) {
	if tmpl == nil {
		return "", errors.New("pool has no nadTemplate, so no NAD can be rendered")
	}
	if name == "" {
		return "", errors.New("NAD name is empty")
	}
	if tmpl.Bridge == "" {
		return "", errors.New("nadTemplate.bridge is empty")
	}
	if tmpl.Subnet == "" {
		return "", errors.New("nadTemplate.subnet is empty")
	}
	if tmpl.Gateway == "" {
		return "", errors.New("nadTemplate.gateway is empty")
	}

	// The CRD requires an IPv4 pattern on the address, so these are defence
	// against objects that predate the schema rather than against user error.
	if !addr.Is4() {
		return "", fmt.Errorf("assigned address %q is not IPv4; this claim layer is IPv4-only", addr)
	}
	prefix, err := netip.ParsePrefix(tmpl.Subnet)
	if err != nil {
		return "", fmt.Errorf("nadTemplate.subnet %q is not a valid CIDR: %w", tmpl.Subnet, err)
	}
	if !prefix.Contains(addr) {
		return "", fmt.Errorf("assigned address %s is outside subnet %s", addr, prefix)
	}
	if _, err := netip.ParseAddr(tmpl.Gateway); err != nil {
		return "", fmt.Errorf("nadTemplate.gateway %q is not a valid address: %w", tmpl.Gateway, err)
	}

	cfg := nadConfig{
		Name:        name,
		Type:        cniType(tmpl),
		Bridge:      tmpl.Bridge,
		VLAN:        tmpl.VLAN,
		MacSpoofChk: tmpl.MacSpoofChk,
		IPAM: nadIPAM{
			Type:       whereaboutsIPAM,
			Range:      tmpl.Subnet,
			RangeStart: addr.String(),
			RangeEnd:   addr.String(),
			Gateway:    tmpl.Gateway,
			Routes:     routes(tmpl.Routes),
		},
	}
	if tmpl.DNS != nil {
		cfg.DNS = &nadDNS{
			Nameservers: tmpl.DNS.Nameservers,
			Domain:      tmpl.DNS.Domain,
			Search:      tmpl.DNS.Search,
		}
	}

	out, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("rendering NAD config: %w", err)
	}
	return string(out), nil
}

func cniType(tmpl *ipamv1alpha1.NADTemplate) string {
	if tmpl.Type == "" {
		return DefaultCNIType
	}
	return tmpl.Type
}

func routes(in []ipamv1alpha1.NADRoute) []nadRoute {
	if len(in) == 0 {
		return nil
	}
	out := make([]nadRoute, 0, len(in))
	for _, r := range in {
		out = append(out, nadRoute{Dst: r.Dst, GW: r.GW})
	}
	return out
}
