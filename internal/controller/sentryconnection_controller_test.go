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
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sentryconfigurationv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

var _ = Describe("SentryConnection Controller", func() {
	It("converges a bounded connection thundering herd without dynamic metric labels", func() {
		const connectionCount = 40
		loadContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		DeferCleanup(cancel)
		names := make([]string, connectionCount)
		for index := range connectionCount {
			names[index] = fmt.Sprintf("load-connection-%02d", index)
			resource := &sentryconfigurationv1alpha1.SentryConnection{
				ObjectMeta: metav1.ObjectMeta{Name: names[index]},
				Spec: sentryconfigurationv1alpha1.SentryConnectionSpec{
					URL: "https://sentry.example.invalid", Organization: "company",
					Credentials: sentryconfigurationv1alpha1.ConnectionCredentials{
						TokenSecretRef: sentryconfigurationv1alpha1.SecretKeyReference{
							Name: fmt.Sprintf("missing-load-secret-%02d", index), Key: "token",
						},
					},
				},
			}
			Expect(k8sClient.Create(loadContext, resource)).To(Succeed())
		}
		DeferCleanup(func() {
			for _, name := range names {
				resource := &sentryconfigurationv1alpha1.SentryConnection{ObjectMeta: metav1.ObjectMeta{Name: name}}
				if err := k8sClient.Delete(context.Background(), resource); err != nil {
					Expect(errors.IsNotFound(err)).To(BeTrue())
				}
			}
		})

		metrics, err := NewMetrics(prometheus.NewRegistry())
		Expect(err).NotTo(HaveOccurred())
		reconciler := &SentryConnectionReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), OperatorNamespace: "default",
			Registry: connectionruntime.NewRegistry(), Metrics: metrics,
		}
		type reconcileResult struct {
			name   string
			result reconcile.Result
			err    error
		}
		results := make(chan reconcileResult, connectionCount)
		start := make(chan struct{})
		var wait sync.WaitGroup
		wait.Add(connectionCount)
		for _, name := range names {
			go func() {
				defer wait.Done()
				<-start
				result, err := reconciler.Reconcile(loadContext, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
				results <- reconcileResult{name: name, result: result, err: err}
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		for result := range results {
			Expect(result.err).NotTo(HaveOccurred(), result.name)
			Expect(result.result.RequeueAfter).To(BeNumerically(">", 0), result.name)
		}

		for _, name := range names {
			resource := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(loadContext, types.NamespacedName{Name: name}, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "CredentialsReady"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "SecretInputInvalid"),
			)))
			Expect(resource.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "SecretInputInvalid"),
			)))
		}
		reconcileMetric := &dto.Metric{}
		Expect(metrics.reconciles.WithLabelValues(controllerConnection, "requeue").Write(reconcileMetric)).To(Succeed())
		Expect(reconcileMetric.GetCounter().GetValue()).To(Equal(float64(connectionCount)))
		conditionMetric := &dto.Metric{}
		Expect(metrics.conditionTransitions.WithLabelValues(controllerConnection, "CredentialsReady", "False").Write(conditionMetric)).To(Succeed())
		Expect(conditionMetric.GetCounter().GetValue()).To(Equal(float64(connectionCount)))
	})

	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			operatorNamespace = "default"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name: resourceName,
		}
		sentryconnection := &sentryconfigurationv1alpha1.SentryConnection{}

		BeforeEach(func() {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "sentry-token", Namespace: operatorNamespace},
				Data:       map[string][]byte{"token": []byte("token-one")},
			}
			if err := k8sClient.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			By("creating the custom resource for the Kind SentryConnection")
			err := k8sClient.Get(ctx, typeNamespacedName, sentryconnection)
			if err != nil && errors.IsNotFound(err) {
				resource := &sentryconfigurationv1alpha1.SentryConnection{
					ObjectMeta: metav1.ObjectMeta{
						Name: resourceName,
					},
					Spec: sentryconfigurationv1alpha1.SentryConnectionSpec{
						URL:          "https://sentry.example.test",
						Organization: "company",
						Credentials: sentryconfigurationv1alpha1.ConnectionCredentials{
							TokenSecretRef: sentryconfigurationv1alpha1.SecretKeyReference{
								Name: "sentry-token",
								Key:  "token",
							},
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			// TODO(user): Cleanup logic after each test, like removing the resource instance.
			resource := &sentryconfigurationv1alpha1.SentryConnection{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance SentryConnection")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sentry-token", Namespace: operatorNamespace}}
			if err := k8sClient.Delete(ctx, secret); err != nil {
				Expect(errors.IsNotFound(err)).To(BeTrue())
			}
		})
		It("loads and rotates Secret inputs without recreating the connection client", func() {
			By("Reconciling the created resource")
			registry := connectionruntime.NewRegistry()
			controllerReconciler := &SentryConnectionReconciler{
				Client:            k8sClient,
				Scheme:            k8sClient.Scheme(),
				OperatorNamespace: operatorNamespace,
				Registry:          registry,
				ProbeConnection: func(context.Context, *sentryapi.Client, string) (string, error) {
					return "company", nil
				},
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: typeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			clientBefore, found := registry.Get(resourceName)
			Expect(found).To(BeTrue())
			versionBefore, found := registry.Versions(resourceName)
			Expect(found).To(BeTrue())

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "sentry-token", Namespace: operatorNamespace}, secret)).To(Succeed())
			requests := controllerReconciler.connectionsForSecret(ctx, secret)
			Expect(requests).To(ConsistOf(reconcile.Request{NamespacedName: typeNamespacedName}))
			outside := secret.DeepCopy()
			outside.Namespace = "other"
			Expect(controllerReconciler.connectionsForSecret(ctx, outside)).To(BeEmpty())
			secret.Data["token"] = []byte("token-two")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			clientAfter, found := registry.Get(resourceName)
			Expect(found).To(BeTrue())
			versionAfter, _ := registry.Versions(resourceName)
			Expect(clientAfter).To(BeIdenticalTo(clientBefore))
			Expect(versionAfter).NotTo(Equal(versionBefore))

			updated := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, typeNamespacedName, updated)).To(Succeed())
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "CredentialsReady"),
				HaveField("Status", metav1.ConditionTrue),
			)))
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "ConnectionVerified"),
			)))
		})
	})
})
