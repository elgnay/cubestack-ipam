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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
	"github.com/suanova/cubestack-ipam/internal/ipam"
)

// virtualMachineGVK is the KubeVirt type this controller watches.
//
// Handled as unstructured, and not only to avoid a vendor dependency. A typed
// VirtualMachine would mean vendoring a large API surface of which this controller
// reads two annotations, and every field it did not model would be a field it
// could silently drop if it ever wrote the object back. It never writes the VM —
// that is the contract below — so the typed view would be a liability with no
// compensating safety.
var virtualMachineGVK = schema.GroupVersionKind{
	Group:   "kubevirt.io",
	Version: "v1",
	Kind:    "VirtualMachine",
}

// claimNameSuffix is appended to a VM's name to form the name of its IPRequest.
// The claim is created in the VM's namespace, so the pair is unique.
const claimNameSuffix = "-ip"

// VirtualMachineReconciler turns an annotated VirtualMachine into an IPRequest.
//
// It is the thin end of the design: it decides nothing about addresses. It
// translates one annotation into one claim and stops.
//
// CREATE-ONLY. It never patches the VirtualMachine, and it never patches an
// IPRequest that already exists. The reason is that the NAD name has to be known
// to the VM manifest before the address exists — the VM's own network list names
// the NAD — so a controller that rewrote either side would be racing the object
// that depends on it. The escape hatch for a wrong annotation is to delete the
// IPRequest and let this controller recreate it, and that is what the events below
// exist to make legible: an edit that produces no object change looks like a
// controller ignoring it unless something says otherwise.
type VirtualMachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder recorder.EventRecorder
}

// The VM controller reads VirtualMachines and creates claims; it never writes a
// VirtualMachine. It needs no permission on IPPools either — it copies the pool
// name out of an annotation and lets the claim controller be the one that reports
// a pool that does not exist. It also needs no delete permission on the VMs it
// references: see controllerRef for why blockOwnerDeletion is off.
//
// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=iprequests,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile creates the IPRequest for a VirtualMachine that asks for one.
func (r *VirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	vm := newVirtualMachine()
	if err := r.Get(ctx, req.NamespacedName, vm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// A deleting VM needs nothing. The claim is owned by this VM, so the garbage
	// collector takes the claim, and the claim's NAD follows it down the chain.
	if !vm.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}

	pool := vm.GetAnnotations()[ipamv1alpha1.AnnotationPool]
	if pool == "" {
		// The annotation is the trigger. Its absence is the ordinary case, not an
		// error: most VMs on this cluster do not want a static underlay address.
		return ctrl.Result{}, nil
	}

	nadName := vm.GetAnnotations()[ipamv1alpha1.AnnotationNADName]
	if nadName == "" {
		// Derived from the VM's name alone, never from the address: the VM manifest
		// has to name its NAD before any address has been assigned.
		nadName = ipam.DefaultNADName(vm.GetName())
	}

	claimName := vm.GetName() + claimNameSuffix
	key := types.NamespacedName{Namespace: vm.GetNamespace(), Name: claimName}

	claim := &ipamv1alpha1.IPRequest{}
	switch err := r.Get(ctx, key, claim); {
	case apierrors.IsNotFound(err):
		return r.createClaim(ctx, vm, claimName, pool, nadName)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("reading IPRequest %s/%s: %w", key.Namespace, key.Name, err)
	}

	if !metav1.IsControlledBy(claim, vm) {
		// Someone else's claim is squatting the name. Reported and retried with
		// backoff rather than overwritten: the other claim may belong to a running
		// VM, and the retry means this resolves itself if that claim goes away.
		msg := fmt.Sprintf("IPRequest %s/%s already exists and is not controlled by this VirtualMachine", key.Namespace, key.Name)
		recordEvent(r.Recorder, vm, corev1.EventTypeWarning, "ClaimNameTaken", "Reconcile", msg)
		return ctrl.Result{}, errors.New(msg)
	}

	if claim.Spec.PoolRef != pool || claim.Spec.NAD != nadName {
		// The divergence a create-only controller has to explain. Emphatically not
		// an error: the claim is doing its job with the values it was created with,
		// and the annotation change was ignored on purpose.
		msg := fmt.Sprintf("annotation change ignored: IPRequest %s/%s was created with pool %q and nad %q; "+
			"delete the IPRequest to re-claim with pool %q and nad %q",
			key.Namespace, key.Name, claim.Spec.PoolRef, claim.Spec.NAD, pool, nadName)
		recordEvent(r.Recorder, vm, corev1.EventTypeWarning, "ClaimExists", "Reconcile", msg)
	}

	return ctrl.Result{}, nil
}

// createClaim writes the IPRequest owned by the VirtualMachine.
func (r *VirtualMachineReconciler) createClaim(ctx context.Context, vm *unstructured.Unstructured, claimName, pool, nadName string) (ctrl.Result, error) {
	claim := &ipamv1alpha1.IPRequest{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: vm.GetNamespace(),
			Name:      claimName,
			// The VM is the owner, which is what makes the whole chain one garbage
			// collection: VM -> IPRequest -> NAD, all by ownerReference, no finalizers.
			OwnerReferences: []metav1.OwnerReference{controllerRef(vm, virtualMachineGVK)},
		},
		Spec: ipamv1alpha1.IPRequestSpec{PoolRef: pool, NAD: nadName},
	}

	switch err := r.Create(ctx, claim); {
	case err == nil:
		return ctrl.Result{}, nil
	case apierrors.IsAlreadyExists(err):
		msg := fmt.Sprintf("IPRequest %s/%s already exists and is not controlled by this VirtualMachine", vm.GetNamespace(), claimName)
		recordEvent(r.Recorder, vm, corev1.EventTypeWarning, "ClaimNameTaken", "Create", msg)
		return ctrl.Result{}, errors.New(msg)
	case apierrors.IsInvalid(err):
		// The claim failed the CRD's own validation — a malformed nad-name
		// annotation, most likely. Retrying cannot help, and it does not need to:
		// the annotation is the input, and any change to it re-triggers this
		// reconcile through the VM watch. The alternative, retrying forever, would
		// turn one bad annotation into a permanent error stream.
		recordEvent(r.Recorder, vm, corev1.EventTypeWarning, "InvalidClaim", "Create", err.Error())
		return ctrl.Result{}, nil
	default:
		return ctrl.Result{}, fmt.Errorf("creating IPRequest %s/%s: %w", vm.GetNamespace(), claimName, err)
	}
}

// newVirtualMachine returns an empty VirtualMachine to decode into.
func newVirtualMachine() *unstructured.Unstructured {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(virtualMachineGVK)
	return vm
}

// SetupWithManager sets up the controller with the Manager.
//
// This controller requires KubeVirt's CRDs to be installed: the manager resolves
// the watch through discovery at startup and will not run without them. That is
// acceptable here because this project exists only on KubeVirt clusters, and it is
// the price of watching a type we deliberately do not vendor.
func (r *VirtualMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(newVirtualMachine()).
		Named("virtualmachine").
		Complete(r)
}
