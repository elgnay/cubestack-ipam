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

	corev1 "k8s.io/api/core/v1"
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
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
	"github.com/suanova/cubestack-ipam/internal/ipam"
)

// IPRangeRequestReconciler reconciles an IPRangeRequest: it assigns a contiguous
// block of addresses from the referenced pool and records it in status.
//
// # What this reconciler deliberately does not do
//
// It mints nothing. An IPRequest earns its NAD because a VM needs one; a block is
// handed to something outside this cluster that configures its own networking, and
// guessing at that consumer's format would produce a NAD nobody asked for. So a
// bound block is a record of addresses and nothing else — see IPRangeRequestSpec
// for what that means for deletion.
//
// It also never writes to an IPRequest. Claims and blocks meet in exactly one
// place, takenInPool, and neither allocator touches the other's objects.
type IPRangeRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Recorder is optional: a test constructing this directly need not stand up an
	// event broadcaster. The one event this controller emits is the deletion
	// warning, and it is best-effort by construction — see releaseEvent.
	Recorder recorder.EventRecorder
}

// Note the absent verbs, the same set the claim controller leaves out. This
// controller never creates or deletes a range request — that is a user's action —
// and it never writes to an IPRequest or an IPPool.
//
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprangerequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprangerequests/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=ippools,verbs=get;list;watch
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprequests,verbs=get;list;watch
// (event RBAC is declared once in events.go -- the group is events.k8s.io, not "")

// Reconcile assigns a contiguous block and records it in status.
//
// Same shape as the claim controller's, minus the NAD half: failures write a
// condition and return without requeueing, because nothing here is time-dependent
// — every fix is an admin or a user editing an object, and both objects are
// watched. The block is recorded before anything else could observe the request as
// satisfied, which is a shorter story than the claim's only because there is no
// second resource to create.
func (r *IPRangeRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	block := &ipamv1alpha1.IPRangeRequest{}
	if err := r.Get(ctx, req.NamespacedName, block); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A deleting request needs no work: deleting it IS the release. Its addresses
	// are free the moment it is gone, because the ledger is built from live objects
	// and there is no finalizer holding it back. See IPRangeRequestSpec for what
	// that assumes about whatever was configured with the block.
	if !block.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	original := block.DeepCopy()

	pool := &ipamv1alpha1.IPPool{}
	switch err := r.Get(ctx, client.ObjectKey{Name: block.Spec.PoolRef}, pool); {
	case apierrors.IsNotFound(err):
		return r.fail(ctx, original, block, ipamv1alpha1.ReasonPoolNotFound,
			fmt.Sprintf("IPPool %q does not exist", block.Spec.PoolRef))
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading IPPool %s: %w", block.Spec.PoolRef, err)
	}

	// Re-checked here rather than read from the pool's status, for the same
	// concurrency reason the claim controller gives: the two run independently, and
	// the check is a pure function.
	if err := ipam.ValidatePoolRange(pool.Spec); err != nil {
		return r.fail(ctx, original, block, ipamv1alpha1.ReasonRangeInvalid,
			fmt.Sprintf("IPPool %q is not usable: %s", pool.Name, err))
	}

	start, end, err := r.assignRange(ctx, block, pool)
	if err != nil {
		var alloc *allocError
		if errors.As(err, &alloc) {
			return r.fail(ctx, original, block, alloc.reason, alloc.message, alloc.blockers...)
		}
		return ctrl.Result{}, err
	}

	message := blockAssignedMessage(block, start, end, pool.Name)
	return ctrl.Result{}, r.bind(ctx, original, block, start, end, message)
}

// blockAssignedMessage describes a satisfied block. An Auto request is described
// by its size, which is what its spec says; an Exact request is described by the
// band it named, which is what its spec says. Reading the count for an Exact
// request would print the block's length as if it had been requested that way.
func blockAssignedMessage(block *ipamv1alpha1.IPRangeRequest, start, end netip.Addr, pool string) string {
	if block.Spec.Allocation.StrategyOf() == ipamv1alpha1.AllocationExact {
		return fmt.Sprintf("requested block %s-%s assigned from pool %s", start, end, pool)
	}
	count := 0
	if block.Spec.Allocation.Count != nil {
		count = int(*block.Spec.Allocation.Count)
	} else if addrs, err := ipam.ParseRange(start.String(), end.String()); err == nil {
		// A request created before count moved under allocation, still holding a
		// block. The block's own length is the only size there is to report.
		count = len(addrs)
	}
	return fmt.Sprintf("block %s-%s (%d addresses) assigned from pool %s", start, end, count, pool)
}

// assignRange returns the block this request holds, allocating one if it holds
// none.
//
// The sticky rule is the same one assign follows for a single address, and the
// reasoning is the same: a block already in status is reused rather than
// re-derived. Re-running the allocator on every reconcile would let a newly
// created request with a lower start displace a block that some off-cluster
// consumer has already been configured with — and unlike a VM's address, nothing
// here would notice, because there is no NAD to disagree with.
func (r *IPRangeRequestReconciler) assignRange(ctx context.Context, block *ipamv1alpha1.IPRangeRequest, pool *ipamv1alpha1.IPPool) (start, end netip.Addr, err error) {
	addrs, err := ipam.ParseRange(pool.Spec.Range.Start, pool.Spec.Range.End)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, &allocError{reason: ipamv1alpha1.ReasonRangeInvalid, message: err.Error()}
	}

	held, ok, err := heldBlock(block)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	if ok {
		// Kept, not silently moved, when the pool's band no longer covers it — the
		// same rule and the same reason as a claim's address. Whatever is outside
		// the cluster is still using these; a block that moves under it is worse
		// than one that has drifted out of band. The request reports the drift and
		// leaves the decision with the admin.
		if !containsAddr(addrs, held.start) || !containsAddr(addrs, held.end) {
			return netip.Addr{}, netip.Addr{}, &allocError{
				reason: ipamv1alpha1.ReasonRangeInvalid,
				message: fmt.Sprintf("assigned block %s-%s is no longer within pool %s's range %s-%s; "+
					"the block is retained because a consumer outside this cluster may still be using it",
					held.start, held.end, pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End),
			}
		}
		// An Exact request whose spec disagrees with its status is only reachable by
		// hand-editing status: allocation is immutable at the schema. Retaining the
		// block and reporting the disagreement is the safe response — moving the
		// block would be worse than flagging it.
		if block.Spec.Allocation.StrategyOf() == ipamv1alpha1.AllocationExact {
			requested, _, err := requestedBand(block)
			if err != nil {
				return netip.Addr{}, netip.Addr{}, err
			}
			if requested.start != held.start || requested.end != held.end {
				return netip.Addr{}, netip.Addr{}, &allocError{
					reason: ipamv1alpha1.ReasonAllocationInvalid,
					message: fmt.Sprintf("status records block %s-%s but spec.allocation.range asks for %s-%s; "+
						"the block is retained because a consumer outside this cluster may still be using it",
						held.start, held.end, requested.start, requested.end),
				}
			}
		}
		return held.start, held.end, nil
	}

	l, err := readLedger(ctx, r.Client, block.Spec.PoolRef, block.UID)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}

	requested, exact, err := requestedBand(block)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	if exact {
		return exactBlock(pool, l, requested)
	}

	taken, err := l.withExclusions(pool.Spec.Exclusions())
	if err != nil {
		return netip.Addr{}, netip.Addr{}, &allocError{reason: ipamv1alpha1.ReasonRangeInvalid, message: err.Error()}
	}

	count := int(*block.Spec.Allocation.Count)
	free := countFree(addrs, taken)
	if free < count {
		qualifier := ""
		if len(pool.Spec.Exclusions()) > 0 {
			qualifier = " (excluding the pool's reserved bands)"
		}
		return netip.Addr{}, netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonRangeExhausted,
			message: fmt.Sprintf("pool %s (%s-%s) has %d free addresses%s and %d were requested",
				pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End, free, qualifier, count),
		}
	}

	start, end, ok = ipam.FirstFreeRun(addrs, taken, count)
	if !ok {
		// Enough addresses are free, just not together. Reporting RangeExhausted
		// here would be false — "all N are claimed" is not what happened — and the
		// two prescribe different fixes, so the longest run is named to make the
		// difference actionable.
		return netip.Addr{}, netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonRangeFragmented,
			message: fmt.Sprintf("pool %s (%s-%s) has %d free addresses but no run of %d together; "+
				"the longest contiguous run is %d. Release a claim or a block, or request fewer addresses",
				pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End, free, count,
				ipam.LongestFreeRun(addrs, taken)),
		}
	}
	return start, end, nil
}

// exactBlock decides whether the requested band can be bound.
//
// The checks run from the pool's policy outward: is Exact allowed at all, is the
// band even inside the pool, has an admin reserved part of it, and finally does a
// current holder intersect it. Each failure has its own reason and, where there is
// an object to inspect, a blocker naming it.
func exactBlock(pool *ipamv1alpha1.IPPool, l *ledger, requested heldRange) (netip.Addr, netip.Addr, error) {
	band := ipamv1alpha1.IPRange{Start: requested.start.String(), End: requested.end.String()}

	if !pool.Spec.AllowsExact() {
		return netip.Addr{}, netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonExactNotAllowed,
			message: fmt.Sprintf("pool %s sets allocation.allowExact to false, so block %s-%s cannot be requested by name; "+
				"use strategy Auto and let the pool choose", pool.Name, band.Start, band.End),
		}
	}

	if !bandCovers(pool.Spec.Range, band) {
		return netip.Addr{}, netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonRangeOutOfRange,
			message: fmt.Sprintf("requested block %s-%s is not entirely within pool %s's range %s-%s",
				band.Start, band.End, pool.Name, pool.Spec.Range.Start, pool.Spec.Range.End),
		}
	}

	for _, excluded := range pool.Spec.Exclusions() {
		if !bandIntersects(excluded, band) {
			continue
		}
		return netip.Addr{}, netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonRangeExcluded,
			message: fmt.Sprintf("requested block %s-%s intersects the excluded band %s-%s of pool %s",
				band.Start, band.End, excluded.Start, excluded.End, pool.Name),
			blockers: []ipamv1alpha1.Blocker{exclusionBlocker(pool.Name, excluded)},
		}
	}

	if blockers := l.blockersForRange(requested.start, requested.end); len(blockers) > 0 {
		return netip.Addr{}, netip.Addr{}, &allocError{
			reason: ipamv1alpha1.ReasonRangeUnavailable,
			message: fmt.Sprintf("requested block %s-%s overlaps addresses already held in pool %s; "+
				"status.blockers names the holders", band.Start, band.End, pool.Name),
			blockers: blockers,
		}
	}

	return requested.start, requested.end, nil
}

// heldBlock is the block recorded in status, if there is one.
//
// A half-recorded block — one end set and not the other — is an error rather than
// "no block": it can only come from something having written status by hand, and
// treating it as empty would let the allocator re-derive a block while a consumer
// was still reading the half that survived.
func heldBlock(block *ipamv1alpha1.IPRangeRequest) (heldRange, bool, error) {
	start := strings.TrimSpace(block.Status.Start)
	end := strings.TrimSpace(block.Status.End)

	if start == "" && end == "" {
		return heldRange{}, false, nil
	}
	if start == "" || end == "" {
		return heldRange{}, false, &allocError{
			reason: ipamv1alpha1.ReasonRangeInvalid,
			message: fmt.Sprintf("status records only one end of a block (start %q, end %q); "+
				"clear both fields to let the block be reallocated", block.Status.Start, block.Status.End),
		}
	}

	s, err := netip.ParseAddr(start)
	if err != nil {
		return heldRange{}, false, &allocError{
			reason:  ipamv1alpha1.ReasonRangeInvalid,
			message: fmt.Sprintf("status.start %q is not a valid address; delete this IPRangeRequest to release the block", start),
		}
	}
	e, err := netip.ParseAddr(end)
	if err != nil {
		return heldRange{}, false, &allocError{
			reason:  ipamv1alpha1.ReasonRangeInvalid,
			message: fmt.Sprintf("status.end %q is not a valid address; delete this IPRangeRequest to release the block", end),
		}
	}
	if s.Compare(e) > 0 {
		return heldRange{}, false, &allocError{
			reason:  ipamv1alpha1.ReasonRangeInvalid,
			message: fmt.Sprintf("status records an inverted block %s-%s; delete this IPRangeRequest to release it", s, e),
		}
	}
	return heldRange{start: s, end: e}, true, nil
}

// heldRange is a block read back from a request's status.
type heldRange struct {
	start netip.Addr
	end   netip.Addr
}

// countFree returns how many addresses in addrs are not in taken.
//
// This exists to tell exhaustion from fragmentation. The two are one comparison
// apart and lead to very different advice, so the allocator counts the free
// addresses before asking for a run rather than inferring the answer from the
// run's absence.
func countFree(addrs []netip.Addr, taken map[netip.Addr]struct{}) int {
	free := 0
	for _, a := range addrs {
		if _, isTaken := taken[a]; !isTaken {
			free++
		}
	}
	return free
}

// fail records a request that could not be satisfied.
//
// status.start and status.end are left untouched, for the reason fail gives on a
// claim: a request that was bound and later broke — its pool deleted, its band
// narrowed — still holds its block, and something outside this cluster is still
// configured with it. Clearing the fields would hand those addresses to the next
// holder while that consumer was still using them.
func (r *IPRangeRequestReconciler) fail(ctx context.Context, original, block *ipamv1alpha1.IPRangeRequest, reason, message string, blockers ...ipamv1alpha1.Blocker) (ctrl.Result, error) {
	block.Status.Phase = ipamv1alpha1.IPRangeRequestPhaseFailed
	// Replaced, not merged: see the claim controller's fail for why a stale blocker
	// is worse than none.
	block.Status.Blockers = blockers
	apimeta.SetStatusCondition(&block.Status.Conditions, metav1.Condition{
		Type:               ipamv1alpha1.ConditionReady,
		Status:             metav1.ConditionFalse,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: block.Generation,
	})
	return ctrl.Result{}, patchStatusIfChanged(ctx, r.Client, original, block, original.Status, block.Status)
}

// bind records a satisfied request.
func (r *IPRangeRequestReconciler) bind(ctx context.Context, original, block *ipamv1alpha1.IPRangeRequest, start, end netip.Addr, message string) error {
	block.Status.Phase = ipamv1alpha1.IPRangeRequestPhaseBound
	block.Status.Start = start.String()
	block.Status.End = end.String()
	// A bound request has nothing blocking it; see the claim controller's bind.
	block.Status.Blockers = nil
	apimeta.SetStatusCondition(&block.Status.Conditions, metav1.Condition{
		Type:               ipamv1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             ipamv1alpha1.ReasonBound,
		Message:            message,
		ObservedGeneration: block.Generation,
	})
	return patchStatusIfChanged(ctx, r.Client, original, block, original.Status, block.Status)
}

// requestsForPool maps a pool — or a claim drawn from one — to the range requests
// that reference the same pool, the same direction and for the same reason as
// claimsForPool: widening a band is what unblocks a request that failed with
// RangeExhausted or RangeFragmented, and so is releasing an address a block was
// waiting for.
func (r *IPRangeRequestReconciler) requestsForPool(ctx context.Context, obj client.Object) []reconcile.Request {
	poolRef, ok := poolRefOf(obj)
	if !ok {
		return nil
	}

	var blocks ipamv1alpha1.IPRangeRequestList
	if err := r.List(ctx, &blocks); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing IPRangeRequests for pool", "pool", poolRef)
		return nil
	}

	var reqs []reconcile.Request
	for i := range blocks.Items {
		block := &blocks.Items[i]
		if block.Spec.PoolRef != poolRef {
			continue
		}
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Namespace: block.Namespace,
			Name:      block.Name,
		}})
	}
	return reqs
}

// releaseEvent warns that a block's addresses are free again.
//
// The delete event carries the last cached object including its status, so the
// event can name the pool and the exact block that was released — the only chance
// to say it, because a deleted object leaves nothing behind to look up afterwards.
// That is also why this is worth calling best-effort rather than a guarantee:
// events are pruned on a TTL, one emitted while the API server is unreachable is
// dropped, and nothing here can detect that a consumer outside the cluster is
// still configured with these addresses. The audit findings catch the consequence
// later; this only makes the moment visible.
func (r *IPRangeRequestReconciler) releaseEvent(_ context.Context, e event.TypedDeleteEvent[client.Object], _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	block, ok := e.Object.(*ipamv1alpha1.IPRangeRequest)
	if !ok {
		return
	}

	// heldBlock is reused rather than the fields read directly, so a block recorded
	// half-way or backwards is described as what it is instead of being printed as
	// if it were a real range.
	note := fmt.Sprintf("IPRangeRequest %s/%s deleted; its addresses in pool %s are free to be allocated again. "+
		"Anything outside this cluster still configured with them will collide with the next holder.",
		block.Namespace, block.Name, block.Spec.PoolRef)
	if held, ok, err := heldBlock(block); err == nil && ok {
		note = fmt.Sprintf("IPRangeRequest %s/%s deleted; block %s-%s in pool %s is free to be allocated again. "+
			"Anything outside this cluster still configured with it will collide with the next holder.",
			block.Namespace, block.Name, held.start, held.end, block.Spec.PoolRef)
	}

	recordEvent(r.Recorder, block, corev1.EventTypeWarning, "RangeReleased", "Delete", note)
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPRangeRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1alpha1.IPRangeRequest{}).
		// A pool edit is the fix for everything this controller fails on, so it has
		// to reach the requests that failed on it.
		Watches(&ipamv1alpha1.IPPool{}, handler.EnqueueRequestsFromMapFunc(r.requestsForPool)).
		// A second handler on the same kind, because For's default delete handling
		// only enqueues a reconcile — which finds the object gone and returns. It
		// cannot emit an event, and neither could a predicate: predicates filter
		// events, they do not observe them. The deleted block also frees capacity, so
		// this enqueues the other requests in the pool at the same time.
		Watches(&ipamv1alpha1.IPRangeRequest{}, handler.Funcs{
			DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				r.releaseEvent(ctx, e, q)
				for _, req := range r.requestsForPool(ctx, e.Object) {
					q.Add(req)
				}
			},
		}).
		// A deleted claim frees its address, which is what a block failed with
		// RangeExhausted or RangeFragmented is waiting for — and what an Exact block
		// blocked by that address is waiting for. Before this watch, freeing an
		// address unfroze a single-address claim but not a block.
		Watches(&ipamv1alpha1.IPRequest{}, handler.Funcs{
			DeleteFunc: func(ctx context.Context, e event.TypedDeleteEvent[client.Object], q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
				for _, req := range r.requestsForPool(ctx, e.Object) {
					q.Add(req)
				}
			},
		}).
		Named("iprangerequest").
		Complete(r)
}
