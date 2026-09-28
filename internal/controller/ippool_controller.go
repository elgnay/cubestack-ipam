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
	"github.com/suanova/cubestack-ipam/internal/ipam"
)

// IPPoolReconciler reconciles a IPPool object.
//
// THIS IS A STUB. It reads the object and logs it, and does nothing else. The
// design (kubevirt-vm-static-ip-design.md) requires the spikes in §8 to run
// before any of this is implemented — see the README.
type IPPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=ippools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=ippools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ipam.cubestack.io,resources=ippools/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// NOT YET IMPLEMENTED — the intended responsibilities, from design §5.5:
//
//   - Verify templateNAD resolves to an existing NetworkAttachmentDefinition.
//     Without it a generated per-VM NAD cannot be templated, and every IPRequest
//     against this pool will fail at pod-sandbox time rather than here.
//
//   - Audit pool placement (design R5): the range must be disjoint from the
//     Whereabouts dynamic window AND from the MetalLB pool on the same subnet.
//     Neither is visible from this object, so the check has to read the template
//     NAD's effective IPAM config. Getting this wrong produces silent duplicate
//     allocation — the failure mode is a VM that boots with someone else's address.
//
//   - Report both outcomes via status.conditions.
//
// Deliberately no status ledger of allocated addresses: the set of IPRequests is
// the single source of truth (design §R3), and a second list would drift.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/reconcile
func (r *IPPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pool := &ipamv1alpha1.IPPool{}
	if err := r.Get(ctx, req.NamespacedName, pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if err := ipam.ValidatePoolRange(pool.Spec); err != nil {
		// The CRD schema catches containment but cannot catch IP ordering, because
		// the CEL net library has no ordering overload on IP values.
		// TODO(design §5.5): surface this as a status condition rather than a log line.
		log.Error(err, "IPPool spec failed validation")
	}

	log.Info("reconciled IPPool; audit not implemented yet",
		"subnet", pool.Spec.Subnet,
		"range", pool.Spec.Range.Start+"-"+pool.Spec.Range.End,
		"gateway", pool.Spec.Gateway,
		"templateNAD", pool.Spec.TemplateNAD,
	)

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *IPPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ipamv1alpha1.IPPool{}).
		Named("ippool").
		Complete(r)
}
