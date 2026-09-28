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

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

// NADCleanupFinalizer is removed only after the per-VM NetworkAttachmentDefinition
// minted for this claim has been deleted, so that the address returns to its pool.
//
// Ordering matters more than it looks (design R4): deleting a NAD out from under a
// running VM does not disrupt it immediately, because no CNI DEL is issued until
// the pod goes. The damage therefore surfaces later, as an address that is free in
// the ledger but still in use. This finalizer is the only thing standing between
// the two.
const NADCleanupFinalizer = "ipam.cubestack.io/nad-cleanup"

// IPRequestReconciler reconciles a IPRequest object.
//
// THIS IS A STUB. It reads the object and logs it, and does nothing else — it
// neither assigns an address nor creates a NAD. The design (see README) requires
// the §8 spikes to run first: until spike 1 confirms a single-address NAD holds
// its address across a restart with the guest on plain DHCP, the allocation
// primitive this controller would build on is unverified.
type IPRequestReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprequests,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprequests/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprequests/finalizers,verbs=update
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=ippools,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// NOT YET IMPLEMENTED — the intended responsibilities, from design §5.5:
//
//   - Allocate. On create, claim an address and mint the per-VM NAD. The create of
//     an object named after the address IS the atomic claim: the API server
//     enforces name uniqueness, so a lost race is a 409 rather than a silent
//     double-book (design §5.2 step 3). Retry with another address on collision.
//
//   - Mint the NAD. Template the pool's templateNAD (bridge, dns, routes) into a
//     NAD whose Whereabouts range is range_start == range_end == the assigned
//     address. The VM must be created AFTER this exists — Multus resolves the NAD
//     by name at pod-sandbox creation, and an absent NAD is a pod stuck in
//     FailedCreatePodSandBox, not a clean failure (design §7 R2).
//     Naming: "vm-static-<ip-dashed>", e.g. vm-static-10-66-3-105.
//
//   - Adopt. Once the referenced VM exists, set its ownerReference on this
//     IPRequest so that deleting the VM garbage-collects the claim.
//
//   - Release. The finalizer above deletes the NAD before this object goes.
//
//   - Audit (design §5.5 — the part that must not be skipped). Three directions:
//     IPRequests with no NAD; NADs with no IPRequest (orphaned address, invisible
//     to the ledger); and VMs whose address is no longer covered by a claim (the
//     duplicate direction). Note there are two ledgers by construction — Whereabouts
//     knows each per-VM pool, and nothing knows the parent IPPool — so this loop is
//     the only thing that catches drift.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *IPRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	ipReq := &ipamv1alpha1.IPRequest{}
	if err := r.Get(ctx, req.NamespacedName, ipReq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	requested := "<auto>"
	if ipReq.Spec.RequestedIP != nil {
		requested = *ipReq.Spec.RequestedIP
	}

	log.Info("reconciled IPRequest; allocation not implemented yet",
		"poolRef", ipReq.Spec.PoolRef,
		"requestedIP", requested,
		"phase", ipReq.Status.Phase,
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1alpha1.IPRequest{}).
		Named("iprequest").
		Complete(r)
}
