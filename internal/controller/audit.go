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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
	"github.com/suanova/cubestack-ipam/internal/ipam"
)

// DefaultAuditInterval is how often the auditor sweeps a cluster with no
// configured interval.
//
// Minutes, not seconds: every check here reports a state that only a human
// changes, and the sweep lists whole kinds. Running it on the reconcile
// timescale would be work with nothing to find.
const DefaultAuditInterval = 10 * time.Minute

// Finding is one inconsistency the auditor noticed.
type Finding struct {
	// GVK, Namespace and Name identify the object the finding is attached to, so
	// it can be reported as an event on the thing itself rather than only in a log
	// nobody reads.
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
	// Reason is the event reason, in CamelCase, matching the convention of the
	// conditions the reconcilers write.
	Reason string
	// Message explains the inconsistency and, where it is obvious, what to do.
	Message string
}

// ref builds the object an event about this finding is recorded against. It is
// deliberately a stub — only the reference matters to the event recorder.
func (f Finding) ref() *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(f.GVK)
	obj.SetNamespace(f.Namespace)
	obj.SetName(f.Name)
	return obj
}

// Auditor is a manager.Runnable that periodically checks the invariants no single
// reconcile can observe from the object it is working on.
//
// This is the part that must not be skipped. Every reconciler here sees one object
// and its direct dependencies, so an inconsistency that is spread across two
// objects — or whose owning object is gone — is invisible to all of them. The
// sweep is the only thing that reads the whole picture.
//
// It reports by event and log, and deliberately writes no status. A condition is
// owned by whichever reconciler serves that object; a second writer would race it,
// and the audit's job is to be the check on the reconcilers, not another of them.
type Auditor struct {
	Client   client.Client
	Recorder recorder.EventRecorder
	// Interval is the time between sweeps. Zero means DefaultAuditInterval.
	Interval time.Duration
}

// Compile-time check that the auditor can be handed to mgr.Add.
var _ manager.Runnable = (*Auditor)(nil)

// NeedLeaderElection keeps the sweep to a single replica. Findings are advisory,
// so duplicated events would be noise rather than a correctness problem — but it
// is noise on the objects users are looking at.
func (a *Auditor) NeedLeaderElection() bool { return true }

// Start runs the sweep loop until the context is cancelled.
func (a *Auditor) Start(ctx context.Context) error {
	interval := a.Interval
	if interval <= 0 {
		interval = DefaultAuditInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		// The first sweep runs immediately: starting the process is exactly when a
		// stale or half-migrated cluster is worth looking at.
		findings, err := a.RunOnce(ctx)
		if err != nil {
			a.log().Error(err, "audit sweep failed")
		}
		for _, f := range findings {
			a.log().Info("audit finding", "reason", f.Reason, "namespace", f.Namespace, "name", f.Name, "message", f.Message)
			recordEvent(a.Recorder, f.ref(), corev1.EventTypeWarning, f.Reason, "Audit", f.Message)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// log returns the auditor's logger. logf.Log is the logger the binary installs,
// which is what a component wired from main should use; the manager's own
// context carries no logger for a Runnable.
func (a *Auditor) log() logr.Logger { return logf.Log.WithName("audit") }

// RunOnce runs every check and returns what it found. Exported so it can be
// tested without running the loop.
func (a *Auditor) RunOnce(ctx context.Context) ([]Finding, error) {
	var findings []Finding

	claims, err := a.listClaims(ctx)
	if err != nil {
		return nil, err
	}

	// Listed once and shared by the two checks that need it. They ask different
	// questions of the same set -- which pools exist, and how each one addresses its
	// claims -- and a second List would only invite the two answers to disagree.
	pools, err := a.listPools(ctx)
	if err != nil {
		return nil, err
	}

	orphans, err := a.orphanedNADs(ctx, claims)
	if err != nil {
		return nil, err
	}
	findings = append(findings, orphans...)

	findings = append(findings, a.duplicateAddresses(claims, pools)...)
	findings = append(findings, a.danglingPoolRefs(claims, pools)...)

	return findings, nil
}

// listPools returns every IPPool by name. The name is what a claim's poolRef holds,
// so a claim can be matched to its pool without a second lookup.
func (a *Auditor) listPools(ctx context.Context) (map[string]ipamv1alpha1.IPPool, error) {
	var pools ipamv1alpha1.IPPoolList
	if err := a.Client.List(ctx, &pools); err != nil {
		return nil, fmt.Errorf("listing IPPools: %w", err)
	}
	out := make(map[string]ipamv1alpha1.IPPool, len(pools.Items))
	for i := range pools.Items {
		out[pools.Items[i].Name] = pools.Items[i]
	}
	return out, nil
}

// poolIPAMMode reads a pool's addressing mode, treating an absent template or an
// unset field as whereabouts -- the same fallback the renderer makes, so a finding's
// wording cannot disagree with the config the CNI was handed.
func poolIPAMMode(pool *ipamv1alpha1.IPPool) string {
	if pool.Spec.NADTemplate == nil || pool.Spec.NADTemplate.IPAM == "" {
		return ipam.IPAMWhereabouts
	}
	return pool.Spec.NADTemplate.IPAM
}

func (a *Auditor) listClaims(ctx context.Context) ([]ipamv1alpha1.IPRequest, error) {
	var claims ipamv1alpha1.IPRequestList
	if err := a.Client.List(ctx, &claims); err != nil {
		return nil, fmt.Errorf("listing IPRequests: %w", err)
	}
	return claims.Items, nil
}

// orphanedNADs finds NADs carrying this project's label whose claim is gone.
//
// The direction matters. A NAD is normally collected by the garbage collector
// through its ownerReference, so an orphan means that chain was broken — most
// likely the ownerReference was stripped by hand. The address it carries is no
// longer held by any claim, so the allocator considers it free and can hand it to
// a new claim. If the orphaned NAD's VM is still running, the two now share an
// address and nothing will refuse it.
func (a *Auditor) orphanedNADs(ctx context.Context, claims []ipamv1alpha1.IPRequest) ([]Finding, error) {
	// Indexed by namespace/name: the NAD is always created in its claim's
	// namespace, so the label value alone would not be enough to identify it.
	known := make(map[types.NamespacedName]struct{}, len(claims))
	for i := range claims {
		known[types.NamespacedName{Namespace: claims[i].Namespace, Name: claims[i].Name}] = struct{}{}
	}

	var nads unstructured.UnstructuredList
	nads.SetGroupVersionKind(networkAttachmentDefinitionGVK.GroupVersion().WithKind("NetworkAttachmentDefinitionList"))
	if err := a.Client.List(ctx, &nads, client.HasLabels{ipamv1alpha1.LabelRequest}); err != nil {
		return nil, fmt.Errorf("listing NetworkAttachmentDefinitions: %w", err)
	}

	var findings []Finding
	for i := range nads.Items {
		nad := &nads.Items[i]
		owner := nad.GetLabels()[ipamv1alpha1.LabelRequest]
		if owner == "" {
			continue
		}
		key := types.NamespacedName{Namespace: nad.GetNamespace(), Name: owner}
		if _, ok := known[key]; ok {
			continue
		}
		findings = append(findings, Finding{
			GVK:       networkAttachmentDefinitionGVK,
			Namespace: nad.GetNamespace(),
			Name:      nad.GetName(),
			Reason:    "OrphanedNAD",
			Message: fmt.Sprintf("owned by IPRequest %s/%s, which no longer exists; "+
				"its address is held by no claim and can be reassigned to another",
				key.Namespace, key.Name),
		})
	}
	sortFindings(findings)
	return findings, nil
}

// duplicateAddresses finds two claims in one pool holding the same address.
//
// This is the residue of the allocation race documented in takenAddresses. Its
// severity depends on the pool's mode, which is why the message says which one
// applies rather than describing a fixed consequence:
//
//   - whereabouts: the ledger has one allocation slot per address, so the second CNI
//     ADD is refused and that claim's VM does not start. Loud, and this finding
//     explains a failure that already happened.
//   - static: nothing refuses it. Both VMs come up on the address and the symptom is
//     intermittent connectivity, so this is the only place the collision is stated at
//     all.
//
// Naming both claims is what makes either fixable, since neither claim's own status
// shows anything wrong.
func (a *Auditor) duplicateAddresses(claims []ipamv1alpha1.IPRequest, pools map[string]ipamv1alpha1.IPPool) []Finding {
	holders := map[string][]ipamv1alpha1.IPRequest{}
	for i := range claims {
		claim := claims[i]
		addr := strings.TrimSpace(claim.Status.AssignedIP)
		if addr == "" {
			continue
		}
		// Keyed on the parsed address rather than the string, so that two spellings
		// of one address (10.66.3.5 and 010.066.003.005, say) do not read as two
		// distinct addresses. Unparseable values are skipped: nothing can be
		// concluded about them, and the claim's own condition covers them.
		parsed, err := netip.ParseAddr(addr)
		if err != nil {
			continue
		}
		key := claim.Spec.PoolRef + "\x00" + parsed.String()
		holders[key] = append(holders[key], claim)
	}

	var findings []Finding
	for key, group := range holders {
		if len(group) < 2 {
			continue
		}
		pool, addr, _ := strings.Cut(key, "\x00")

		names := make([]string, 0, len(group))
		for _, claim := range group {
			names = append(names, claim.Namespace+"/"+claim.Name)
		}
		sort.Strings(names)

		message := fmt.Sprintf("address %s in pool %s is held by %d claims: %s; %s",
			addr, pool, len(group), strings.Join(names, ", "), duplicateConsequence(pools, pool))

		// Reported on each claim, not once: whichever one a reader is looking at
		// should say what is wrong with it.
		for _, claim := range group {
			findings = append(findings, Finding{
				GVK:       ipamv1alpha1.GroupVersion.WithKind("IPRequest"),
				Namespace: claim.Namespace,
				Name:      claim.Name,
				Reason:    "DuplicateAddress",
				Message:   message,
			})
		}
	}
	sortFindings(findings)
	return findings
}

// duplicateConsequence is the second half of the DuplicateAddress message: what the
// collision actually does to the VMs.
func duplicateConsequence(pools map[string]ipamv1alpha1.IPPool, poolName string) string {
	pool, known := pools[poolName]
	if !known {
		// The pool is gone, so its mode is unknown. Say what is certainly true --
		// two claims disagree about one address -- rather than guessing a mode and
		// describing a consequence that may not follow.
		return "the pool no longer exists, so which of them survives a CNI ADD depends on the NADs already out there"
	}
	if poolIPAMMode(&pool) == ipam.IPAMStatic {
		return "this pool addresses statically, so nothing refuses a second holder: " +
			"all of these VMs are up on one address and the symptom is intermittent connectivity, " +
			"not a failed start"
	}
	return "this pool leases from Whereabouts, so only one can hold it at CNI ADD and " +
		"the others' VMs will not start"
}

// danglingPoolRefs finds claims left behind by a deleted pool.
//
// Reported once per missing pool rather than once per claim. The per-claim
// IPRequest reconciler already sets PoolNotFound on each of them, so repeating
// that here would add nothing; what no single reconcile can say is how many claims
// a deletion stranded, and that is the number an admin deciding whether to
// recreate the pool needs.
func (a *Auditor) danglingPoolRefs(claims []ipamv1alpha1.IPRequest, pools map[string]ipamv1alpha1.IPPool) []Finding {
	var findings []Finding
	for pool, group := range groupByPoolRef(claims, pools) {
		names := make([]string, 0, len(group))
		for _, claim := range group {
			names = append(names, claim.Namespace+"/"+claim.Name)
		}
		sort.Strings(names)

		first := group[0]
		findings = append(findings, Finding{
			GVK:       ipamv1alpha1.GroupVersion.WithKind("IPRequest"),
			Namespace: first.Namespace,
			Name:      first.Name,
			Reason:    "PoolDeleted",
			Message:   fmt.Sprintf("IPPool %q does not exist but %d claim(s) still reference it: %s", pool, len(group), strings.Join(names, ", ")),
		})
	}
	sortFindings(findings)
	return findings
}

func groupByPoolRef(claims []ipamv1alpha1.IPRequest, pools map[string]ipamv1alpha1.IPPool) map[string][]ipamv1alpha1.IPRequest {
	groups := map[string][]ipamv1alpha1.IPRequest{}
	for i := range claims {
		claim := claims[i]
		if _, ok := pools[claim.Spec.PoolRef]; ok {
			continue
		}
		groups[claim.Spec.PoolRef] = append(groups[claim.Spec.PoolRef], claim)
	}
	return groups
}

// sortFindings orders findings so that a sweep is reproducible. Map iteration is
// randomised, and an audit whose output order changes between runs is an audit
// whose diffs cannot be read.
func sortFindings(findings []Finding) {
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Reason != findings[j].Reason {
			return findings[i].Reason < findings[j].Reason
		}
		if findings[i].Namespace != findings[j].Namespace {
			return findings[i].Namespace < findings[j].Namespace
		}
		return findings[i].Name < findings[j].Name
	})
}
