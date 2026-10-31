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
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
)

var _ = Describe("Telegram registration status fences", func() {
	It("commits exactly one target through real Kubernetes CAS", func() {
		const namespace = "registration-concurrent-cas"
		ctx := context.Background()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
		})

		store := &binding.Store{Client: k8sClient, Namespace: namespace}
		reference := binding.ProjectReference{Namespace: "payments", Name: "checkout", UID: "registration-cas-project", Generation: 3}
		marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-op"}
		_, err := store.CreateBinding(ctx, binding.ProjectBinding{
			Version: binding.CurrentVersion, Project: reference, Connection: "company",
			Installation: "https://sentry.example", Organization: "company", Slug: "checkout",
			Owner: "payments", Origin: binding.OriginManaged, AccessMode: binding.AccessManage,
			ExternalIdentity: "project-id", Marker: &marker, ManagerUID: reference.UID, ClaimName: "claim",
			Destinations: map[string]binding.DestinationBinding{"telegram/payments-alerts": {
				Owner: binding.ChildOwner{Kind: binding.ChildOwnerProject, Name: reference.Name, UID: reference.UID, Generation: reference.Generation},
				Kind:  "telegram", SelectorHash: "selector-hash", ResourceName: "payments-alerts", State: "RegistrationRequired",
			}},
		})
		Expect(err).NotTo(HaveOccurred())
		routes := &registration.RouteStore{Client: k8sClient, Namespace: namespace}
		service := &registration.Service{
			Store: store, Publisher: routes, Audit: controllerAuditSink{}, Now: func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
			Random: func(buffer []byte) (int, error) {
				for i := range buffer {
					buffer[i] = byte(i + 1)
				}
				return len(buffer), nil
			},
		}
		code, err := service.Issue(ctx, registration.IssueInput{
			Project: reference, Destination: "telegram/payments-alerts", Owner: "payments", Connection: "company",
			PolicyGeneration: 7, PolicyResourceVersion: "policy-rv", TTL: 10 * time.Minute,
		})
		Expect(err).NotTo(HaveOccurred())

		start := make(chan struct{})
		errorsChannel := make(chan error, 2)
		var wait sync.WaitGroup
		for _, identity := range []string{"-100111", "-100222"} {
			wait.Add(1)
			go func(chatIdentity string) {
				defer GinkgoRecover()
				defer wait.Done()
				<-start
				errorsChannel <- service.Redeem(ctx, reference.UID, "telegram/payments-alerts", code, binding.TelegramTarget{ChatIdentity: chatIdentity}, func(context.Context, binding.TelegramChallenge) error { return nil })
			}(identity)
		}
		close(start)
		wait.Wait()
		close(errorsChannel)
		successes := 0
		for redeemErr := range errorsChannel {
			if redeemErr == nil {
				successes++
			}
		}
		Expect(successes).To(Equal(1))

		record, err := store.GetBinding(ctx, reference.UID)
		Expect(err).NotTo(HaveOccurred())
		telegram := record.Binding.Destinations["telegram/payments-alerts"].Telegram
		Expect(telegram).NotTo(BeNil())
		Expect(telegram.Target).NotTo(BeNil())
		Expect(telegram.RouteKey).NotTo(BeEmpty())
		published, err := routes.GetTelegramRoute(ctx, telegram.RouteKey)
		Expect(err).NotTo(HaveOccurred())
		Expect(published).To(Equal(*telegram.Target))
		secrets := &corev1.SecretList{}
		Expect(k8sClient.List(ctx, secrets, client.InNamespace(namespace), client.MatchingLabels{"sentryconfiguration.io/record-type": "telegram-route"})).To(Succeed())
		Expect(secrets.Items).To(HaveLen(1))
	})

	It("round-trips ordered two-resource fences through real status subresources", func() {
		const (
			connectionName = "registration-fence-connection"
			projectName    = "registration-fence-project"
			namespace      = "default"
		)
		ctx := context.Background()
		connection := &sentryv1alpha1.SentryConnection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName},
			Spec: sentryv1alpha1.SentryConnectionSpec{
				URL: "https://sentry.example.test", Organization: "company",
				Credentials: sentryv1alpha1.ConnectionCredentials{TokenSecretRef: sentryv1alpha1.SecretKeyReference{Name: "unused", Key: "token"}},
				Access: []sentryv1alpha1.AccessRule{{
					Owner: "payments", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{namespace}},
					Teams: []string{"payments"}, AllowCreate: true,
					NotificationTargets: &sentryv1alpha1.NotificationTargetAccess{TelegramTargets: []string{"payments-alerts"}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, connection)).To(Succeed())
		project := &sentryv1alpha1.SentryProject{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: sentryv1alpha1.SentryProjectSpec{
				Sentry:              connectionName,
				Project:             sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
				NotificationTargets: &sentryv1alpha1.NotificationTargets{Telegram: []string{"payments-alerts"}},
			},
		}
		Expect(k8sClient.Create(ctx, project)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(ctx, project)
			_ = k8sClient.Delete(ctx, connection)
		})

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: projectName}, project)).To(Succeed())
		flattened, _ := project.Spec.NotificationTargets.Flatten()
		_, selectorHash, err := destination.SelectorFingerprint(connection.Spec, flattened["telegram/payments-alerts"])
		Expect(err).NotTo(HaveOccurred())
		challenge := binding.TelegramChallenge{
			Project:     binding.ProjectReference{Namespace: namespace, Name: projectName, UID: string(project.UID), Generation: project.Generation},
			Destination: "telegram/payments-alerts", SelectorHash: selectorHash, Owner: "payments", Connection: connectionName,
			PolicyGeneration: connection.Generation, PolicyResourceVersion: connection.ResourceVersion,
			ExpiresAt: time.Now().UTC().Add(10 * time.Minute),
		}
		coordinator := &registration.FenceCoordinator{Client: k8sClient}
		operationHash := strings.Repeat("e", 64)
		Expect(coordinator.Begin(ctx, challenge, operationHash)).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
		Expect(connection.Status.RegistrationCommit).NotTo(BeNil())
		Expect(connection.Status.RegistrationCommit.OperationHash).To(Equal(operationHash))
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: projectName}, project)).To(Succeed())
		Expect(project.Status.NotificationTargets["telegram/payments-alerts"].RegistrationCommit).NotTo(BeNil())
		Expect(project.Status.NotificationTargets["telegram/payments-alerts"].RegistrationCommit.OperationHash).To(Equal(operationHash))

		Expect(coordinator.Clear(ctx, challenge, operationHash)).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
		Expect(connection.Status.RegistrationCommit).To(BeNil())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: projectName}, project)).To(Succeed())
		Expect(project.Status.NotificationTargets["telegram/payments-alerts"].RegistrationCommit).To(BeNil())
	})
})
