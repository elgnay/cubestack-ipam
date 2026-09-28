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

// The two IPAM plugins this project can generate, selected by the pool template's
// ipam field. See NADTemplate.IPAM for the full statement of the trade-off; the
// short version is that whereabouts buys CNI-enforced exclusivity at the price of
// live migration, and static buys live migration at the price of that enforcement.
//
// They cannot be combined on one pool, and that is a property of the mechanism
// rather than a gap in this code: migration deliberately runs the target pod while
// the source pod still serves, so two pods must hold one address at once, and the
// Whereabouts ledger stores one allocation per address (spec.allocations is keyed
// by last octet, one podref each).
const (
	// IPAMWhereabouts leases the address from the Whereabouts ledger. A duplicate is
	// refused at CNI ADD and its VM does not start; live migration is impossible.
	IPAMWhereabouts = "whereabouts"

	// IPAMStatic writes the address into the NAD. Live migration works and the guest
	// keeps its address across it. Nothing refuses a duplicate, so exclusivity rests
	// on the allocator being the single writer over spec.range, on the pool's range
	// being disjoint from anything else on the subnet, and on the audit sweep.
	IPAMStatic = "static"
)

// ipamMode resolves the template's choice, treating unset as Whereabouts.
//
// That default matches the CRD's, and matters on upgrade: a template written before
// this field existed must keep the behaviour it was created with rather than
// silently losing its CNI-enforced exclusivity. Opting into static is a deliberate
// act with a stated cost, not something to inherit by accident.
func ipamMode(tmpl *ipamv1alpha1.NADTemplate) string {
	if tmpl.IPAM == "" {
		return IPAMWhereabouts
	}
	return tmpl.IPAM
}

// nadConfig mirrors the NetworkAttachmentDefinition spec.config this project
// generates. It is a separate type from v1alpha1.NADTemplate because the two have
// genuinely different shapes: the template omits everything the controller
// injects, and the output includes fields the template has no say over.
//
// Field order matters only in that encoding/json emits it in declaration order,
// which is what makes the rendered output stable enough to golden-test.
type nadConfig struct {
	Name        string  `json:"name"`
	Type        string  `json:"type"`
	Bridge      string  `json:"bridge"`
	VLAN        *int32  `json:"vlan,omitempty"`
	MacSpoofChk *bool   `json:"macspoofchk,omitempty"`
	IPAM        any     `json:"ipam"`
	DNS         *nadDNS `json:"dns,omitempty"`
}

// nadIPAMStatic is the ipam block for a static assignment: the address is written
// in, and nothing arbitrates it.
type nadIPAMStatic struct {
	Type      string       `json:"type"`
	Addresses []nadAddress `json:"addresses"`
	Routes    []nadRoute   `json:"routes,omitempty"`
}

// nadIPAMWhereabouts is the ipam block for a Whereabouts lease. Range identifies
// the allocation ledger -- every NAD declaring the same range shares one, which is
// what makes a duplicate a loud CNI failure -- while range_start == range_end
// bounds this NAD's candidates to the one address assigned to the claim.
type nadIPAMWhereabouts struct {
	Type       string     `json:"type"`
	Range      string     `json:"range"`
	RangeStart string     `json:"range_start"`
	RangeEnd   string     `json:"range_end"`
	Gateway    string     `json:"gateway"`
	Routes     []nadRoute `json:"routes,omitempty"`
}

// nadAddress is one entry of the static plugin's address list. The address
// carries its own prefix length, which is why the assigned address alone is not
// enough to render one: the mask comes from the template's subnet.
type nadAddress struct {
	Address string `json:"address"`
	Gateway string `json:"gateway,omitempty"`
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
// Whichever mode the template selects, this NAD serves addr and no other, and the
// invariant is upheld by the allocator, which is the sole writer over the pool's
// range. What differs is whether the CNI also enforces it:
//
//   - static bakes addr in, so nothing arbitrates it. Two pods may hold it at once,
//     which is precisely what live migration needs.
//   - whereabouts pins range_start == range_end inside the range's shared ledger, so
//     a second holder is refused at CNI ADD and the VM does not start.
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
		IPAM:        renderIPAM(tmpl, addr, prefix),
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

// renderIPAM builds the ipam block for the template's mode.
//
// It returns either concrete struct, and the caller stores it as `any`, so that
// only the selected mode's fields reach the config: the two shapes share almost no
// keys, and emitting both would hand the CNI an ipam block it would have to
// interpret. It never fails — the address and subnet were validated above, which is
// everything either shape needs.
func renderIPAM(tmpl *ipamv1alpha1.NADTemplate, addr netip.Addr, prefix netip.Prefix) any {
	if ipamMode(tmpl) == IPAMStatic {
		return nadIPAMStatic{
			Type: IPAMStatic,
			Addresses: []nadAddress{{
				// The mask comes from the template's subnet: it is the only place the
				// prefix length is declared, and the static plugin wants a CIDR rather
				// than a bare address.
				Address: netip.PrefixFrom(addr, prefix.Bits()).String(),
				Gateway: tmpl.Gateway,
			}},
			Routes: routes(tmpl.Routes),
		}
	}

	return nadIPAMWhereabouts{
		Type: IPAMWhereabouts,
		// The range, not the subnet: where the ledger lives. IPPool.spec.range is the
		// band within that subnet this pool may hand out, and the ledger is keyed by
		// the range's CIDR -- every NAD declaring the same one shares it, which is what
		// makes a duplicate a refusal rather than two VMs quietly sharing an address.
		Range:      tmpl.Subnet,
		RangeStart: addr.String(),
		// Pinning both ends to the assigned address is what bounds this NAD's
		// candidates to one, and it is the whole reason the per-VM NAD gives
		// exclusivity at all.
		RangeEnd: addr.String(),
		Gateway:  tmpl.Gateway,
		Routes:   routes(tmpl.Routes),
	}
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
