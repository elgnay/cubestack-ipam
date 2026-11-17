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
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
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
// The NAD carries the assigned address, so the address and
// the NAD are one fact. Splitting them across two controllers would give that fact
// two writers that can disagree; keeping both here makes disagreement structurally
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
// list on range requests is the shared ledger's read, not an action on them --
// this controller never touches an IPRangeRequest, but it must see the blocks one
// holds or it will allocate straight through them.
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprangerequests,verbs=get;list;watch
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
			return r.fail(ctx, original, claim, alloc.reason, alloc.message, alloc.blockers...)
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
//
// blockers is the structured detail behind an Exact failure: the objects holding
// the requested address, or the pool exclusion that reserves it. It is empty for
// Auto failures, whose only cause is the pool itself.
type allocError struct {
	reason   string
	message  string
	blockers []ipamv1alpha1.Blocker
}

func (e *allocError) Error() string { return e.message }

// assign returns the address this claim holds, allocating one if it holds none.
//
// An address already in status is reused rather than re-derived. That is the
// single most important line in this file: re-running the allocator on every
// reconcile would let a newly created claim with a lower address displace a
// running VM, and an address that moves under a live guest is precisely the
// instability this project exists to remove.
//
// The strategy splits only the not-yet-bound case. Auto takes the lowest free
// address; Exact takes the requested one or fails. There is no fallback between
// them — substituting a free address for a requested one would make a typo
// indistinguishable from a successful claim.
func (r *IPRequestReconciler) assign(ctx context.Context, claim *ipamv1alpha1.IPRequest, pool *ipamv1alpha1.IPPool) (netip.Addr, error) {
	addrs, err := ipam.ParseRange(pool.Spec.Range.Start, pool.Spec.Range.End)
	if err != nil {
		return netip.Addr{}, &allocError{reason: ipamv1alpha1.ReasonRangeInvalid, message: err.Error()}
	}

	requested, err := requestedAddress(claim)
	if err != nil {
		return netip.Addr{}, err
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
		// The allocation is immutable at the schema, so this can only be a status
		// written by hand. It is still worth naming: the alternative is binding a
		// claim whose spec and status disagree about which address it wants.
		if requested.IsValid() && requested != addr {
			return netip.Addr{}, &allocError{
				reason: ipamv1alpha1.ReasonAllocationInvalid,
				message: fmt.Sprintf("status records address %s but spec.allocation.address asks for %s; "+
					"delete this IPRequest to release the address and claim a new one", addr, requested),
			}
		}
		return addr, nil
	}

	l, err := readLedger(ctx, r.Client, claim.Spec.PoolRef, claim.UID)
	if err != nil {
		return netip.Addr{}, err
	}

	if requested.IsValid() {
		return exactAddress(claim, pool, addrs, l, requested)
	}

	taken, err := l.withExclusions(pool.Spec.Exclusions())
	if err != nil {
		return netip.Addr{}, &allocError{reason: ipamv1alpha1.ReasonRangeInvalid, message: err.Error()}
	}

	addr, ok := ipam.FirstFree(addrs, taken)
	if !ok {
		qualifier := "claimed"
		if len(pool.Spec.Exclusions()) > 0 {
			qualifier = "claimed or excluded by the pool"
		}
		return netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonRangeExhausted,
			message: fmt.Sprintf("all %d addresses in pool %s (%s-%s) are %s",
				len(addrs), pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End, qualifier),
		}
	}
	return addr, nil
}

// exactAddress decides whether a requested address can be bound.
//
// The checks are ordered from the cheapest fact to the most specific one, and each
// has its own reason because each prescribes a different fix: widen the pool,
// change the pool's exclusions, or release/choose around the current holder. The
// final failure carries blockers so the holder can be looked up rather than
// guessed at.
func exactAddress(claim *ipamv1alpha1.IPRequest, pool *ipamv1alpha1.IPPool, addrs []netip.Addr, l *ledger, requested netip.Addr) (netip.Addr, error) {
	if !pool.Spec.AllowsExact() {
		return netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonExactNotAllowed,
			message: fmt.Sprintf("pool %s sets allocation.allowExact to false, so address %s cannot be requested by name; "+
				"remove spec.allocation and let the pool choose", pool.Name, requested),
		}
	}

	if !containsAddr(addrs, requested) {
		return netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonAddressOutOfRange,
			message: fmt.Sprintf("requested address %s is outside pool %s's range %s-%s",
				requested, pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End),
		}
	}

	for _, band := range pool.Spec.Exclusions() {
		if !bandContains(band, requested) {
			continue
		}
		return netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonAddressExcluded,
			message: fmt.Sprintf("requested address %s is inside an excluded band %s-%s of pool %s",
				requested, band.Start, band.End, pool.Name),
			blockers: []ipamv1alpha1.Blocker{exclusionBlocker(pool.Name, band)},
		}
	}

	if blockers := l.blockersForAddress(requested); len(blockers) > 0 {
		return netip.Addr{}, &allocError{
			reason:   ipamv1alpha1.ReasonAddressUnavailable,
			message:  fmt.Sprintf("requested address %s is already held in pool %s; status.blockers names the holder", requested, pool.Name),
			blockers: blockers,
		}
	}

	return requested, nil
}

// fail records a claim that could not be satisfied.
//
// status.assignedIP is left untouched. A claim that was bound and later broke —
// its pool deleted, its template withdrawn — still holds its address, and the NAD
// it owns still carries it. Clearing the field would hand that address to someone
// else while the VM on it is still running.
func (r *IPRequestReconciler) fail(ctx context.Context, original, claim *ipamv1alpha1.IPRequest, reason, message string, blockers ...ipamv1alpha1.Blocker) (ctrl.Result, error) {
	claim.Status.Phase = ipamv1alpha1.IPRequestPhaseFailed
	// Replaced, not merged: blockers describe why this attempt failed. Keeping a
	// stale holder after the failure reason has changed would point a reader at an
	// object that no longer has anything to do with the request.
	claim.Status.Blockers = blockers
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
	// A bound claim has nothing blocking it. Cleared here rather than in fail, so
	// the transition from "held by someone else" to "bound" cannot leave the old
	// blocker list behind for a reader to misread.
	claim.Status.Blockers = nil
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

// claimsForPool maps a pool -- or a block drawn from one -- to the claims that
// reference the same pool.
//
// This is the useful direction of both dependencies. Fixing a pool — a widened
// band, a template added — is what unblocks claims that previously failed with
// RangeExhausted or TemplateMissing, and deleting a range request is what returns
// capacity to a claim that failed the same way. Without the watch the fix would
// appear to do nothing until each claim was touched by hand.
//
// Enqueueing is all this does; it never writes. A watch that wrote to the objects
// it watches would be a feedback loop, and a claim that fails again simply
// re-reports the same condition, which patchStatusIfChanged then skips writing.
func (r *IPRequestReconciler) claimsForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	poolRef, ok := poolRefOf(obj)
	if !ok {
		return nil
	}

	var claims ipamv1alpha1.IPRequestList
	if err := r.List(ctx, &claims); err != nil {
		// Nothing to do but wait for the next pool event: a map function has nowhere
		// to report an error, and returning nil simply enqueues nothing.
		ctrl.LoggerFrom(ctx).Error(err, "listing IPRequests for pool", "pool", poolRef)
		return nil
	}

	var reqs []reconcile.Request
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.PoolRef != poolRef {
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
		// A deleted block returns capacity, which is the only event that can unstick a
		// claim failed with RangeExhausted. Note this is the *claim* side: a block
		// appearing later can never invalidate a bound claim, because a claim holds
		// its address stickily and the block allocator sees it in the ledger.
		Watches(&ipamv1alpha1.IPRangeRequest{}, handler.EnqueueRequestsFromMapFunc(r.claimsForPool)).
		// A deleted claim frees its address, which is what a *different* claim failed
		// with RangeExhausted — or an Exact request blocked by it — is waiting for.
		// For's own handler only enqueues the object being deleted, which finds
		// nothing and returns; the waiters have to be named explicitly. Delete-only
		// rather than every event, because a claim's status updates say nothing about
		// any other claim's capacity.
		Watches(&ipamv1alpha1.IPRequest{}, handler.Funcs{
			DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				for _, req := range r.claimsForPool(ctx, e.Object) {
					q.Add(req)
				}
			},
		}).
		Named("iprequest").
		Complete(r)
}
