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
	"errors"
	"fmt"
	"net/netip"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
	"github.com/suanova/cubestack-ipam/internal/ipam"
)

// IPRequestReconciler reconciles an IPRequest: it assigns an address from the
// referenced pool, and mints the claim's NetworkAttachmentDefinition from the
// pool's template when the claim asks for one.
//
// # Why this reconciler mints the NAD
//
// The NAD's Whereabouts range is the assigned address, so the address and the NAD
// are one fact. Splitting them across two controllers would give that fact two
// writers that can disagree; keeping both here makes disagreement structurally
// impossible rather than merely unlikely. It also keeps a claim usable on its own:
// a hand-written IPRequest with no VirtualMachine behind it still gets its NAD.
type IPRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Note the absent verbs. The claim controller never creates or deletes an
// IPRequest — that is a user's action, or the VirtualMachine controller's — and it
// never deletes a NAD, because the NAD it mints is collected by the garbage
// collector through its ownerReference. It also needs no delete permission on the
// objects it references: see controllerRef for why blockOwnerDeletion is off.
//
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprequests/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=ippools,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;update

// Reconcile assigns an address and mints a NAD.
//
// The sequence is deliberate: the NAD is created before the claim is marked Bound,
// so that Bound implies the network exists. The reverse order would advertise a
// claim as satisfied during the window in which its VM is still stuck in
// FailedCreatePodSandBox, which is the only failure this ordering can have and the
// one hardest to attribute afterwards.
//
// Every failure path writes a condition and returns without requeueing. Nothing
// here is time-dependent — the fix for each of them is an admin or user editing an
// object, and both objects are watched.
func (r *IPRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	claim := &ipamv1alpha1.IPRequest{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A deleting claim needs no work. The NAD carries an ownerReference to this
	// claim, so the garbage collector takes it; re-binding here would write an
	// address onto an object on its way out, and mint a NAD that is about to be
	// collected. There is no finalizer in this chain on purpose.
	if !claim.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	original := claim.DeepCopy()

	pool := &ipamv1alpha1.IPPool{}
	switch err := r.Get(ctx, client.ObjectKey{Name: claim.Spec.PoolRef}, pool); {
	case apierrors.IsNotFound(err):
		return r.fail(ctx, original, claim, ipamv1alpha1.ReasonPoolNotFound,
			fmt.Sprintf("IPPool %q does not exist", claim.Spec.PoolRef))
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading IPPool %s: %w", claim.Spec.PoolRef, err)
	}

	// A pool that failed its own validation cannot be allocated from. Checking it
	// again here rather than trusting status.conditions keeps this reconciler
	// correct when the two run concurrently, and the check is a pure function.
	if err := ipam.ValidatePoolRange(pool.Spec); err != nil {
		return r.fail(ctx, original, claim, ipamv1alpha1.ReasonRangeInvalid,
			fmt.Sprintf("IPPool %q is not usable: %s", pool.Name, err))
	}

	addr, err := r.assign(ctx, claim, pool)
	if err != nil {
		var alloc *allocError
		if errors.As(err, &alloc) {
			return r.fail(ctx, original, claim, alloc.reason, alloc.message)
		}
		return ctrl.Result{}, err
	}

	message := fmt.Sprintf("address %s assigned from pool %s", addr, pool.Name)

	if claim.Spec.NAD != "" {
		if pool.Spec.NADTemplate == nil {
			// A distinct condition, not just a reason: asking for a NAD against a
			// template-less pool is a misconfiguration the user can fix, and without
			// naming it the claim would look like a broken controller.
			detail := fmt.Sprintf("pool %s carries no nadTemplate, so NAD %s cannot be minted", pool.Name, claim.Spec.NAD)
			apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
				Type:               ipamv1alpha1.ConditionTemplateMissing,
				Status:             metav1.ConditionTrue,
				Reason:             ipamv1alpha1.ReasonTemplateAbsent,
				Message:            detail,
				ObservedGeneration: claim.Generation,
			})
			return r.fail(ctx, original, claim, ipamv1alpha1.ReasonTemplateAbsent,
				fmt.Sprintf("spec.nad is set but %s", detail))
		}

		if err := ensureNAD(ctx, r.Client, claim, pool, addr); err != nil {
			if errors.Is(err, errNADNotOwned) {
				apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
					Type:               ipamv1alpha1.ConditionNADNameTaken,
					Status:             metav1.ConditionTrue,
					Reason:             ipamv1alpha1.ReasonNADNotOwned,
					Message:            err.Error(),
					ObservedGeneration: claim.Generation,
				})
				return r.fail(ctx, original, claim, ipamv1alpha1.ReasonNADNotOwned, err.Error())
			}
			return ctrl.Result{}, fmt.Errorf("ensuring NAD %s/%s: %w", claim.Namespace, claim.Spec.NAD, err)
		}
		message += fmt.Sprintf("; NAD %s ready", claim.Spec.NAD)
	}

	return ctrl.Result{}, r.bind(ctx, original, claim, addr, message)
}

// allocError is a failure that is the user's or admin's to fix rather than a
// transient error, so Reconcile turns it into a condition instead of a retry.
type allocError struct {
	reason  string
	message string
}

func (e *allocError) Error() string { return e.message }

// assign returns the address this claim holds, allocating one if it holds none.
//
// An address already in status is reused rather than re-derived. That is the
// single most important line in this file: re-running the allocator on every
// reconcile would let a newly created claim with a lower address displace a
// running VM, and an address that moves under a live guest is precisely the
// instability this project exists to remove.
func (r *IPRequestReconciler) assign(ctx context.Context, claim *ipamv1alpha1.IPRequest, pool *ipamv1alpha1.IPPool) (netip.Addr, error) {
	addrs, err := ipam.ParseRange(pool.Spec.Range.Start, pool.Spec.Range.End)
	if err != nil {
		return netip.Addr{}, &allocError{reason: ipamv1alpha1.ReasonRangeInvalid, message: err.Error()}
	}

	if held := strings.TrimSpace(claim.Status.AssignedIP); held != "" {
		addr, err := netip.ParseAddr(held)
		if err != nil {
			return netip.Addr{}, &allocError{
				reason:  ipamv1alpha1.ReasonRangeInvalid,
				message: fmt.Sprintf("status.assignedIP %q is not a valid address; delete this IPRequest to release it", held),
			}
		}
		// Kept, not silently moved, when the pool's band no longer covers it: the NAD
		// on the running VM still carries this address, and an address that moves
		// under a live guest is worse than one that has drifted out of band. The
		// claim reports the drift and leaves the decision with the admin.
		if !containsAddr(addrs, addr) {
			return netip.Addr{}, &allocError{
				reason: ipamv1alpha1.ReasonRangeInvalid,
				message: fmt.Sprintf("assigned address %s is no longer within pool %s's range %s-%s; "+
					"the address is retained because the NAD still carries it", addr, pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End),
			}
		}
		return addr, nil
	}

	taken, err := r.takenAddresses(ctx, claim)
	if err != nil {
		return netip.Addr{}, err
	}

	addr, ok := ipam.FirstFree(addrs, taken)
	if !ok {
		return netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonRangeExhausted,
			message: fmt.Sprintf("all %d addresses in pool %s (%s-%s) are claimed",
				len(addrs), pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End),
		}
	}
	return addr, nil
}

// takenAddresses collects every address held by claims against the same pool,
// across all namespaces — the pool is cluster-scoped, so the ledger is too.
//
// This reads IPRequests rather than a pool-owned ledger, because the claim set is
// the only authoritative record (design §R3). A second list on the pool would be a
// second thing to keep in step, and the two would eventually disagree.
//
// KNOWN RACE: two claims created in the same instant can both be reconciled
// against a cache that does not yet show the other's assignment, and both pick the
// same address. The window is small and the consequence is bounded and loud: the
// per-VM NADs share one Whereabouts ledger, so the second is refused at CNI ADD
// and its VM does not start, rather than the two silently sharing an address. The
// audit sweep reports the collision after the fact. Closing it properly needs a
// compare-and-swap on something address-shaped, which this API gave up when
// per-address claim objects were dropped.
func (r *IPRequestReconciler) takenAddresses(ctx context.Context, claim *ipamv1alpha1.IPRequest) (map[netip.Addr]struct{}, error) {
	var claims ipamv1alpha1.IPRequestList
	if err := r.List(ctx, &claims); err != nil {
		return nil, fmt.Errorf("listing IPRequests: %w", err)
	}

	taken := make(map[netip.Addr]struct{}, len(claims.Items))
	for i := range claims.Items {
		other := &claims.Items[i]
		// Compared by UID rather than name: the claim in hand may not have one yet.
		if other.Spec.PoolRef != claim.Spec.PoolRef || other.UID == claim.UID {
			continue
		}
		held := strings.TrimSpace(other.Status.AssignedIP)
		if held == "" {
			// Pending and Failed claims hold nothing. A failed claim deliberately does
			// not consume an address, so a misconfigured one cannot exhaust a pool.
			continue
		}
		addr, err := netip.ParseAddr(held)
		if err != nil {
			// One claim's malformed status must not block every other claim in the
			// pool; it is that claim's own condition, and the audit's, to report.
			continue
		}
		taken[addr] = struct{}{}
	}
	return taken, nil
}

// fail records a claim that could not be satisfied.
//
// status.assignedIP is left untouched. A claim that was bound and later broke —
// its pool deleted, its template withdrawn — still holds its address, and the NAD
// it owns still carries it. Clearing the field would hand that address to someone
// else while the VM on it is still running.
func (r *IPRequestReconciler) fail(ctx context.Context, original, claim *ipamv1alpha1.IPRequest, reason, message string) (ctrl.Result, error) {
	claim.Status.Phase = ipamv1alpha1.IPRequestPhaseFailed
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               ipamv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})
	return ctrl.Result{}, patchStatusIfChanged(ctx, r.Client, original, claim, original.Status, claim.Status)
}

// bind records a satisfied claim.
func (r *IPRequestReconciler) bind(ctx context.Context, original, claim *ipamv1alpha1.IPRequest, addr netip.Addr, message string) error {
	claim.Status.Phase = ipamv1alpha1.IPRequestPhaseBound
	claim.Status.AssignedIP = addr.String()
	apimeta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               ipamv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ipamv1alpha1.ReasonBound,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})

	// Removed rather than set to False. These describe a problem; a problem that no
	// longer exists should not linger as a condition the reader has to interpret.
	// It is also the only thing that clears them, which is what makes them
	// trustworthy as a signal.
	apimeta.RemoveStatusCondition(&claim.Status.Conditions, ipamv1alpha1.ConditionTemplateMissing)
	apimeta.RemoveStatusCondition(&claim.Status.Conditions, ipamv1alpha1.ConditionNADNameTaken)

	return patchStatusIfChanged(ctx, r.Client, original, claim, original.Status, claim.Status)
}

// claimsForPool maps a pool to the claims that reference it.
//
// This is the useful direction of the pool/claim dependency: fixing a pool — a
// widened band, a template added — is what unblocks claims that previously failed
// with RangeExhausted or TemplateMissing. Without it the fix would appear to do
// nothing until each claim was touched by hand.
func (r *IPRequestReconciler) claimsForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	pool, ok := obj.(*ipamv1alpha1.IPPool)
	if !ok {
		return nil
	}

	var claims ipamv1alpha1.IPRequestList
	if err := r.List(ctx, &claims); err != nil {
		// Nothing to do but wait for the next pool event: a map function has nowhere
		// to report an error, and returning nil simply enqueues nothing.
		ctrl.LoggerFrom(ctx).Error(err, "listing IPRequests for pool", "pool", pool.Name)
		return nil
	}

	var reqs []reconcile.Request
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.PoolRef != pool.Name {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: claim.Namespace,
			Name:      claim.Name,
		}})
	}
	return reqs
}

// containsAddr reports whether addr is in addrs.
func containsAddr(addrs []netip.Addr, addr netip.Addr) bool {
	for _, a := range addrs {
		if a == addr {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1alpha1.IPRequest{}).
		// Owns the minted NAD, so a deleted or hand-edited NAD comes back. Deleting
		// it does not disturb a running VM immediately — no CNI DEL is issued until
		// the pod goes — which is exactly why the drift has to be corrected here
		// rather than noticed later (design R4).
		Owns(newNAD("", "")).
		Watches(&ipamv1alpha1.IPPool{}, handler.EnqueueRequestsFromMapFunc(r.claimsForPool)).
		Named("iprequest").
		Complete(r)
}
