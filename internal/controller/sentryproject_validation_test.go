/*
Copyright 2026 Sentry Kubernetes Operator Authors.

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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
)

var _ = Describe("SentryProject validation", func() {
	It("rejects create and use before connection resolution or Sentry access", func() {
		ctx := context.Background()
		slug := "existing-project"
		project := &sentryv1alpha1.SentryProject{
			ObjectMeta: metav1.ObjectMeta{Name: "invalid-create-and-use", Namespace: "default"},
			Spec: sentryv1alpha1.SentryProjectSpec{
				Sentry: "connection-that-does-not-exist",
				Project: sentryv1alpha1.ProjectSelector{
					Create: &sentryv1alpha1.ProjectCreate{Team: "payments"},
					Use:    &slug,
				},
			},
		}
		Expect(k8sClient.Create(ctx, project)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), project) })

		reconciler := &SentryProjectReconciler{
			Client: k8sClient, Registry: connectionruntime.NewRegistry(),
			Bindings: &binding.Store{Client: k8sClient, Namespace: "default"}, ManagedBy: "test-operator",
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: project.Namespace, Name: project.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(result).To(Equal(reconcile.Result{}))

		updated := &sentryv1alpha1.SentryProject{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: project.Name}, updated)).To(Succeed())
		Expect(updated.Finalizers).To(BeEmpty())
		Expect(updated.Status.Conditions).To(ContainElement(And(
			HaveField("Type", "Ready"),
			HaveField("Status", metav1.ConditionFalse),
			HaveField("Reason", "SpecInvalid"),
		)))
	})
})
