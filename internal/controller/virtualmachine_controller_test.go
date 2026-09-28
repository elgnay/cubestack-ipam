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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ipamv1alpha1 "github.com/suanova/cubestack-ipam/api/v1alpha1"
)

var _ = Describe("VirtualMachine Controller", func() {
	reconcileVM := func(namespace, name string) {
		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// createVM writes a VirtualMachine with the annotations that drive this
	// controller, plus a spec body it is not expected to read — which is what makes
	// the unstructured handling testable: nothing here should depend on spec.
	createVM := func(namespace, name string, annotations map[string]string) *unstructured.Unstructured {
		vm := newVirtualMachine()
		vm.SetNamespace(namespace)
		vm.SetName(name)
		vm.SetAnnotations(annotations)
		ExpectWithOffset(1, unstructured.SetNestedSlice(vm.Object,
			[]any{map[string]any{"name": "default", "multus": map[string]any{"networkName": name + "-static"}}},
			"spec", "template", "spec", "networks")).To(Succeed())
		ExpectWithOffset(1, k8sClient.Create(ctx, vm)).To(Succeed())
		DeferCleanup(func() {
			ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, vm))).To(Succeed())
		})
		return vm
	}

	deleteClaim := func(namespace, name string) {
		claim := &ipamv1alpha1.IPRequest{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
		DeferCleanup(func() {
			ExpectWithOffset(1, client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
		})
	}

	It("creates an owned claim for an annotated VirtualMachine", func() {
		vm := createVM("default", "vm-basic", map[string]string{ipamv1alpha1.AnnotationPool: "vm-pool"})
		deleteClaim("default", "vm-basic-ip")

		reconcileVM("default", "vm-basic")

		claim := &ipamv1alpha1.IPRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm-basic-ip"}, claim)).To(Succeed())
		Expect(claim.Spec.PoolRef).To(Equal("vm-pool"))
		Expect(claim.Spec.NAD).To(Equal("vm-basic-static"))
		// The ownerReference is the whole release mechanism: no finalizer anywhere in
		// the chain, so deleting the VM has to be enough.
		Expect(metav1.IsControlledBy(claim, vm)).To(BeTrue())
	})

	It("honours the nad-name annotation", func() {
		createVM("default", "vm-named", map[string]string{
			ipamv1alpha1.AnnotationPool:    "vm-pool",
			ipamv1alpha1.AnnotationNADName: "custom-static",
		})
		deleteClaim("default", "vm-named-ip")

		reconcileVM("default", "vm-named")

		claim := &ipamv1alpha1.IPRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm-named-ip"}, claim)).To(Succeed())
		Expect(claim.Spec.NAD).To(Equal("custom-static"))
	})

	It("does nothing for an unannotated VirtualMachine", func() {
		createVM("default", "vm-plain", nil)

		reconcileVM("default", "vm-plain")

		claim := &ipamv1alpha1.IPRequest{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm-plain-ip"}, claim)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("does not touch an existing claim when the annotation changes", func() {
		// The create-only contract. It is the reason the controller emits an event
		// on divergence: the object does not change, so nothing else would say the
		// edit was ignored.
		vm := createVM("default", "vm-edited", map[string]string{ipamv1alpha1.AnnotationPool: "vm-pool"})
		deleteClaim("default", "vm-edited-ip")

		reconcileVM("default", "vm-edited")

		annotations := vm.GetAnnotations()
		annotations[ipamv1alpha1.AnnotationPool] = "vm-other-pool"
		vm.SetAnnotations(annotations)
		Expect(k8sClient.Update(ctx, vm)).To(Succeed())

		reconcileVM("default", "vm-edited")

		claim := &ipamv1alpha1.IPRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm-edited-ip"}, claim)).To(Succeed())
		Expect(claim.Spec.PoolRef).To(Equal("vm-pool"))
	})

	It("reports a claim name that belongs to another VirtualMachine", func() {
		createVM("default", "vm-conflict", map[string]string{ipamv1alpha1.AnnotationPool: "vm-pool"})
		deleteClaim("default", "vm-conflict-ip")

		// A claim with the name controller #2 would use, owned by nobody.
		foreign := &ipamv1alpha1.IPRequest{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "vm-conflict-ip"},
			Spec:       ipamv1alpha1.IPRequestSpec{PoolRef: "someone-elses-pool"},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())

		r := &VirtualMachineReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "vm-conflict"}})
		// An error, not a silent no-op: it is retried with backoff, so the conflict
		// resolves itself if the other claim is deleted — and it is never overwritten.
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("not controlled by"))

		still := &ipamv1alpha1.IPRequest{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm-conflict-ip"}, still)).To(Succeed())
		Expect(still.Spec.PoolRef).To(Equal("someone-elses-pool"))
	})

	It("does nothing for a VirtualMachine being deleted", func() {
		vm := createVM("default", "vm-deleting", map[string]string{ipamv1alpha1.AnnotationPool: "vm-pool"})
		deleteClaim("default", "vm-deleting-ip")

		Expect(k8sClient.Delete(ctx, vm)).To(Succeed())
		// No finalizer on the VM in envtest, so it goes immediately; reconciling the
		// stale key is the case that matters — the request arrives after the delete.
		reconcileVM("default", "vm-deleting")

		claim := &ipamv1alpha1.IPRequest{}
		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm-deleting-ip"}, claim)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})
})
