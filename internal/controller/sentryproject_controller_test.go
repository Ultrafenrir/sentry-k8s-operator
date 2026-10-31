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
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	alertsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/alerts"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/connection"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/dsn"
	projectruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/project"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	settingsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/settings"
)

var _ = Describe("SentryProject Controller", func() {
	Context("When reconciling a managed project", func() {
		const (
			resourceNamespace = "default"
			connectionName    = "project-test-connection"
		)

		ctx := context.Background()
		var resourceName string
		var key types.NamespacedName
		var reconciler *SentryProjectReconciler
		var adapter *controllerFakeProjectAdapter
		var keyListCalls int
		var keyListErr error
		var keyActive bool
		var keyLabel string
		var projectSerial int
		var settingsAdapter *controllerFakeSettingsAdapter
		var destinationCatalog *controllerFakeDestinationCatalog
		var alertAdapter *controllerFakeAlertAdapter

		BeforeEach(func() {
			keyListCalls = 0
			keyListErr = nil
			keyActive = true
			keyLabel = "automatic"
			projectSerial++
			resourceName = fmt.Sprintf("project-test-resource-%d", projectSerial)
			key = types.NamespacedName{Name: resourceName, Namespace: resourceNamespace}
			staleSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}}
			if err := k8sClient.Delete(ctx, staleSecret); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			connection := &sentryconfigurationv1alpha1.SentryConnection{
				ObjectMeta: metav1.ObjectMeta{Name: connectionName},
				Spec: sentryconfigurationv1alpha1.SentryConnectionSpec{
					URL:          "https://sentry.example.test",
					Organization: "company",
					Credentials:  sentryconfigurationv1alpha1.ConnectionCredentials{TokenSecretRef: sentryconfigurationv1alpha1.SecretKeyReference{Name: "unused", Key: "token"}},
					Access: []sentryconfigurationv1alpha1.AccessRule{{
						Owner:       "payments",
						Namespaces:  sentryconfigurationv1alpha1.NamespaceAccess{MatchNames: []string{resourceNamespace}},
						Teams:       []string{"payments"},
						AllowCreate: true,
					}},
				},
			}
			if err := k8sClient.Create(ctx, connection); err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Status.Capabilities = []string{projectruntime.CapabilityAtomicDefaultRules, projectruntime.CapabilityAutomaticActiveKey}
			Expect(k8sClient.Status().Update(ctx, connection)).To(Succeed())
			resource := &sentryconfigurationv1alpha1.SentryProject{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
				Spec: sentryconfigurationv1alpha1.SentryProjectSpec{
					Sentry: connectionName,
					Project: sentryconfigurationv1alpha1.ProjectSelector{Create: &sentryconfigurationv1alpha1.ProjectCreate{
						Team: "payments",
					}},
				},
			}
			if err := k8sClient.Create(ctx, resource); err != nil && !apierrors.IsAlreadyExists(err) {
				Expect(err).NotTo(HaveOccurred())
			}

			registry := connectionruntime.NewRegistry()
			_, _, err := registry.Apply(connectionName, connection.Spec.URL, connectionruntime.SecretInputs{Token: []byte("test-token")})
			Expect(err).NotTo(HaveOccurred())
			adapter = &controllerFakeProjectAdapter{projects: make(map[string]projectruntime.RemoteProject)}
			settingsAdapter = &controllerFakeSettingsAdapter{}
			destinationCatalog = &controllerFakeDestinationCatalog{}
			alertAdapter = &controllerFakeAlertAdapter{objects: make(map[string]alertsruntime.Observed)}
			reconciler = &SentryProjectReconciler{
				Client:    k8sClient,
				Scheme:    k8sClient.Scheme(),
				Registry:  registry,
				Bindings:  &binding.Store{Client: k8sClient, Namespace: resourceNamespace},
				ManagedBy: "company-sentry-op",
				NewAdapter: func(*sentryapi.Client) projectruntime.Adapter {
					return adapter
				},
				ListKeys: func(context.Context, *sentryapi.Client, string, string) ([]dsn.ClientKey, error) {
					keyListCalls++
					if keyListErr != nil {
						return nil, keyListErr
					}
					key := controllerClientKey("private-client-key-id-SHOULD-NOT-LEAK", "https://public@example.invalid/42")
					key.IsActive = keyActive
					key.Name = keyLabel
					key.Label = keyLabel
					return []dsn.ClientKey{key}, nil
				},
				NewSettingsAdapter:    func(*sentryapi.Client) settingsruntime.Adapter { return settingsAdapter },
				NewDestinationCatalog: func(*sentryapi.Client) destination.Catalog { return destinationCatalog },
				NewAlertAdapter: func(*sentryapi.Client, map[string]binding.DestinationBinding) alertsruntime.Adapter {
					return alertAdapter
				},
				Audit: controllerAuditSink{},
			}
		})

		AfterEach(func() {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}}
			if err := k8sClient.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				Expect(err).NotTo(HaveOccurred())
			}
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: secret.Name, Namespace: secret.Namespace}, &corev1.Secret{}))
			}).Should(BeTrue())
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			if err := k8sClient.Get(ctx, key, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
				_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &sentryconfigurationv1alpha1.SentryProject{}))
			}).Should(BeTrue())
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection); err == nil {
				Expect(k8sClient.Delete(ctx, connection)).To(Succeed())
			}
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, &sentryconfigurationv1alpha1.SentryConnection{}))
			}).Should(BeTrue())
		})

		It("adds the finalizer before one external create and reports verified status", func() {
			result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Requeue).To(BeTrue())
			Expect(adapter.createCalls).To(Equal(0))

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(adapter.createCalls).To(Equal(1))
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(adapter.createCalls).To(Equal(1))

			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Finalizers).To(ContainElement(sentryProjectFinalizer))
			Expect(updated.Status.Origin).To(Equal("Managed"))
			Expect(updated.Status.AccessMode).To(Equal("Manage"))
			Expect(updated.Status.DSNSecret).To(Equal(resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix))
			Expect(updated.Status.DSNClientKey).To(Equal("automatic"))
			statusJSON, err := json.Marshal(updated.Status)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(statusJSON)).NotTo(ContainSubstring("private-client-key-id-SHOULD-NOT-LEAK"))
			Expect(string(statusJSON)).NotTo(ContainSubstring("https://public@example.invalid/42"))
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: updated.Status.DSNSecret, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(secret.Type).To(Equal(corev1.SecretTypeOpaque))
			Expect(secret.Data).To(Equal(map[string][]byte{sentryconfigurationv1alpha1.DefaultDSNSecretKey: []byte("https://public@example.invalid/42")}))
			Expect(keyListCalls).To(Equal(2))
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "DSNReady"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "DSNReady"),
			)))
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "ProjectVerified"),
			)))
		})

		It("reports a persistent readable client-key rename without changing the pinned identity", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			keyLabel = "renamed-key"
			for range 2 {
				_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
				updated := &sentryconfigurationv1alpha1.SentryProject{}
				Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
				Expect(updated.Status.DSNClientKey).To(Equal("renamed-key"))
				Expect(updated.Status.Conditions).To(ContainElement(And(
					HaveField("Type", "DSNReady"),
					HaveField("Status", metav1.ConditionTrue),
					HaveField("Reason", "ExternalNameChanged"),
				)))
			}

			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Spec.DSN = &sentryconfigurationv1alpha1.DSNSpec{ClientKey: "renamed-key"}
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "DSNReady"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "DSNReady"),
			)))
			record, err := reconciler.Bindings.GetBinding(ctx, string(updated.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.DSN.KeyIdentity).To(Equal("private-client-key-id-SHOULD-NOT-LEAK"))
			Expect(record.Binding.DSN.KeyName).To(Equal("renamed-key"))
		})

		It("blocks managed create when critical connection capabilities are not proven", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Status.Capabilities = nil
			Expect(k8sClient.Status().Update(ctx, connection)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(adapter.createCalls).To(Equal(0))
			Expect(keyListCalls).To(Equal(0))
			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "ManagedCreateUnsupported"),
			)))
		})

		It("revokes only the bound DSN Secret after current policy denial is proven", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			secretKey := types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}
			beforeRevoke := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, beforeRevoke)).To(Succeed())
			Expect(beforeRevoke.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "Ready"),
				HaveField("Status", metav1.ConditionTrue),
			)))
			Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed())
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Access = nil
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(MatchError(apierrors.IsNotFound, "be not found"))
			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.DSNSecret).To(BeEmpty())
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "DSNReady"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "DSNAccessRevoked"),
			)))
		})

		It("preserves a working DSN Secret when the key read fails temporarily", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			secretKey := types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}
			Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed())

			keyListErr = errors.New("temporary key read failure")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed())
			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.DSNSecret).To(Equal(secretKey.Name))
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "DSNReady"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "TemporaryFailure"),
			)))
		})

		It("removes the output Secret when the pinned key is proven inactive", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			secretKey := types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}
			Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed())

			keyActive = false
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue())
			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.DSNSecret).To(BeEmpty())
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "DSNReady"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "DSNKeyRevoked"),
			)))
		})

		It("publishes a reader Secret without gaining Sentry mutation rights", func() {
			slug := fmt.Sprintf("external-%d", projectSerial)
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Access[0].ExistingProjects = &sentryconfigurationv1alpha1.ExistingProjectAccess{
				Observe:    []string{slug},
				ConsumeDSN: []string{slug},
			}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			adapter.projects[slug] = projectruntime.RemoteProject{Identity: "external-id", Slug: slug, URL: "https://sentry.example.test/projects/" + slug}
			readerName := fmt.Sprintf("reader-%d", projectSerial)
			readerKey := types.NamespacedName{Name: readerName, Namespace: resourceNamespace}
			reader := &sentryconfigurationv1alpha1.SentryProject{
				ObjectMeta: metav1.ObjectMeta{Name: readerName, Namespace: resourceNamespace},
				Spec: sentryconfigurationv1alpha1.SentryProjectSpec{
					Sentry:  connectionName,
					Project: sentryconfigurationv1alpha1.ProjectSelector{Use: &slug},
					DSN:     &sentryconfigurationv1alpha1.DSNSpec{},
				},
			}
			Expect(k8sClient.Create(ctx, reader)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: readerKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: readerKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, readerKey, reader)).To(Succeed())
			Expect(reader.Status.Origin).To(Equal("External"))
			Expect(reader.Status.AccessMode).To(Equal("ReadOnly"))
			Expect(adapter.createCalls).To(Equal(0))
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: readerName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(secret.Labels).NotTo(HaveKey("sentryconfiguration.io/owner"))
			Expect(k8sClient.Delete(ctx, reader)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: readerKey})
			Expect(err).NotTo(HaveOccurred())
		})

		It("does not read client keys for a reader without ConsumeDSN", func() {
			slug := fmt.Sprintf("observe-only-%d", projectSerial)
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Access[0].ExistingProjects = &sentryconfigurationv1alpha1.ExistingProjectAccess{Observe: []string{slug}}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			readerName := fmt.Sprintf("observe-only-reader-%d", projectSerial)
			readerKey := types.NamespacedName{Name: readerName, Namespace: resourceNamespace}
			reader := &sentryconfigurationv1alpha1.SentryProject{
				ObjectMeta: metav1.ObjectMeta{Name: readerName, Namespace: resourceNamespace},
				Spec: sentryconfigurationv1alpha1.SentryProjectSpec{
					Sentry:  connectionName,
					Project: sentryconfigurationv1alpha1.ProjectSelector{Use: &slug},
					DSN:     &sentryconfigurationv1alpha1.DSNSpec{},
				},
			}
			Expect(k8sClient.Create(ctx, reader)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: readerKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: readerKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(keyListCalls).To(Equal(0))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: readerName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}, &corev1.Secret{})).To(MatchError(apierrors.IsNotFound, "be not found"))
			Expect(k8sClient.Delete(ctx, reader)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: readerKey})
			Expect(err).NotTo(HaveOccurred())
		})

		It("reports partial settings without blocking an independent DSN", func() {
			trueValue := true
			falseValue := false
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.Settings = &sentryconfigurationv1alpha1.ProjectSettings{
				Filters: &sentryconfigurationv1alpha1.FilterSettings{BrowserExtensions: &trueValue},
				PII:     &sentryconfigurationv1alpha1.PIISettings{ScrubData: &trueValue},
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			settingsAdapter.filters = []settingsruntime.Filter{{ID: "localhost", Active: false}}
			settingsAdapter.state = settingsruntime.ProjectState{ScrubData: &falseValue}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(settingsAdapter.projectUpdates).To(HaveLen(1))
			secretKey := types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}
			Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "SettingsReady"),
				HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "SettingsPartiallyApplied"),
			)))
			Expect(resource.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "DSNReady"),
				HaveField("Status", metav1.ConditionTrue),
			)))
		})

		It("publishes only safe destination status while pinning private identities", func() {
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Integrations = &sentryconfigurationv1alpha1.ConnectionIntegrations{
				Slack: &sentryconfigurationv1alpha1.SlackIntegration{Workspaces: []sentryconfigurationv1alpha1.IntegrationCatalogEntry{{Name: "company", SentryName: "company"}}},
			}
			connection.Spec.Access[0].NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargetAccess{SlackWorkspaces: []string{"company"}}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"incidents"}}}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			destinationCatalog.integrations = map[string][]destination.Integration{
				"slack": {{Identity: "private-workspace-id", Name: "company"}},
			}
			destinationCatalog.slack = map[string][]destination.Resource{
				"private-workspace-id": {{Identity: "private-channel-id", Name: "incidents"}},
			}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.NotificationTargets).To(HaveKeyWithValue("slack/incidents", sentryconfigurationv1alpha1.DestinationStatus{Type: "slack", State: destination.StateActive, Reason: "DestinationResolved"}))
			statusJSON, err := json.Marshal(resource.Status)
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.Contains(string(statusJSON), "private-workspace-id")).To(BeFalse())
			Expect(strings.Contains(string(statusJSON), "private-channel-id")).To(BeFalse())
			record, err := reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.Destinations["slack/incidents"].IntegrationIdentity).To(Equal("private-workspace-id"))
			Expect(record.Binding.Destinations["slack/incidents"].ResourceIdentity).To(Equal("private-channel-id"))
			status := resource.Status.NotificationTargets["slack/incidents"]
			status.RegistrationCommit = &sentryconfigurationv1alpha1.RegistrationCommit{OperationHash: strings.Repeat("b", 64), PolicyGeneration: connection.Generation, ExpiresAt: metav1.Now()}
			resource.Status.NotificationTargets["slack/incidents"] = status
			Expect(k8sClient.Status().Update(ctx, resource)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.NotificationTargets["slack/incidents"].RegistrationCommit).NotTo(BeNil())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Access[0].NotificationTargets = nil
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.NotificationTargets["slack/incidents"].State).To(Equal(destination.StateRevoked))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}, &corev1.Secret{})).To(Succeed())
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.Destinations["slack/incidents"].State).To(Equal(destination.StateRevoked))
			Expect(record.Binding.Destinations["slack/incidents"].ResourceIdentity).To(Equal("private-channel-id"))
		})

		It("reconciles legacy alerts through protected bindings and safe status", func() {
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Compatibility = &sentryconfigurationv1alpha1.ConnectionCompatibility{Alerts: sentryconfigurationv1alpha1.AlertCompatibilityLegacy24}
			connection.Spec.Integrations = &sentryconfigurationv1alpha1.ConnectionIntegrations{Slack: &sentryconfigurationv1alpha1.SlackIntegration{Workspaces: []sentryconfigurationv1alpha1.IntegrationCatalogEntry{{Name: "company", SentryName: "company"}}}}
			connection.Spec.Access[0].NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargetAccess{SlackWorkspaces: []string{"company"}}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"incidents"}}}
			alerts := map[string]sentryconfigurationv1alpha1.AlertSpec{"new-errors": {Trigger: sentryconfigurationv1alpha1.AlertTriggerFirstSeen, Notify: []string{"slack/incidents"}}}
			resource.Spec.Alerts = &alerts
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			destinationCatalog.integrations = map[string][]destination.Integration{"slack": {{Identity: "12", Name: "company"}}}
			destinationCatalog.slack = map[string][]destination.Resource{"12": {{Identity: "C123", Name: "incidents"}}}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.Alerts).To(HaveKeyWithValue("new-errors", sentryconfigurationv1alpha1.AlertStatus{State: alertsruntime.StateReady, Reason: "AlertReady"}))
			Expect(resource.Status.Conditions).To(ContainElement(And(HaveField("Type", "AlertsReady"), HaveField("Status", metav1.ConditionTrue))))
			statusJSON, err := json.Marshal(resource.Status)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(statusJSON)).NotTo(ContainSubstring("private-alert-id-SHOULD-NOT-LEAK"))
			record, err := reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.PinnedAdapters["alerts"]).To(Equal(alertsruntime.LegacyCombinedAdapter))
			Expect(record.Binding.Alerts["new-errors"].ExternalIdentity).To(Equal("private-alert-id-SHOULD-NOT-LEAK"))

			emptyAlerts := map[string]sentryconfigurationv1alpha1.AlertSpec{}
			resource.Spec.Alerts = &emptyAlerts
			resource.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			alertAdapter.deleteErr = errors.New("temporary Sentry delete failure")
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.Alerts).To(HaveKey("new-errors"))
			Expect(record.Binding.Destinations["slack/incidents"].State).To(Equal(destination.StateCleanupPending))
			Expect(alertAdapter.deleteCalls).To(Equal(0))

			alertAdapter.deleteErr = nil
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.Alerts).To(HaveKeyWithValue("new-errors", sentryconfigurationv1alpha1.AlertStatus{State: alertsruntime.StateDeleted, Reason: "AlertDeleted"}))
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.Alerts).To(BeEmpty())
			Expect(record.Binding.Tombstones).To(HaveKey("new-errors"))
			Expect(record.Binding.Destinations).To(BeEmpty())
			Expect(alertAdapter.deleteCalls).To(Equal(1))
		})

		It("revokes one destination grant without blocking an allowed sibling", func() {
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Integrations = &sentryconfigurationv1alpha1.ConnectionIntegrations{
				Slack:     &sentryconfigurationv1alpha1.SlackIntegration{Workspaces: []sentryconfigurationv1alpha1.IntegrationCatalogEntry{{Name: "company", SentryName: "company"}}},
				PagerDuty: &sentryconfigurationv1alpha1.PagerDutyIntegration{Accounts: []sentryconfigurationv1alpha1.IntegrationCatalogEntry{{Name: "primary", SentryName: "primary"}}},
			}
			connection.Spec.Access[0].NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargetAccess{SlackWorkspaces: []string{"company"}, PagerDutyAccounts: []string{"primary"}}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"incidents"}}, PagerDuty: map[string][]string{"primary": {"checkout"}}}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			destinationCatalog.integrations = map[string][]destination.Integration{
				"slack": {{Identity: "workspace-id", Name: "company"}}, "pagerduty": {{Identity: "account-id", Name: "primary"}},
			}
			destinationCatalog.slack = map[string][]destination.Resource{"workspace-id": {{Identity: "channel-id", Name: "incidents"}}}
			destinationCatalog.pagerDuty = map[string][]destination.Resource{"account-id": {{Identity: "service-id", Name: "checkout"}}}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Access[0].NotificationTargets.SlackWorkspaces = nil
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.NotificationTargets["slack/incidents"].State).To(Equal(destination.StateRevoked))
			Expect(resource.Status.NotificationTargets["pagerDuty/checkout"].State).To(Equal(destination.StateActive))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}, &corev1.Secret{})).To(Succeed())
			record, err := reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.Destinations["slack/incidents"].State).To(Equal(destination.StateRevoked))
			Expect(record.Binding.Destinations["slack/incidents"].ResourceIdentity).To(Equal("channel-id"))
			Expect(record.Binding.Destinations["pagerDuty/checkout"].State).To(Equal(destination.StateActive))
			Expect(record.Binding.Destinations["pagerDuty/checkout"].ResourceIdentity).To(Equal("service-id"))
		})
	})
})

func controllerClientKey(id, publicDSN string) dsn.ClientKey {
	key := dsn.ClientKey{ID: id, Name: id, IsActive: true}
	key.DSN.Public = publicDSN
	return key
}

type controllerAuditSink struct{}

func (controllerAuditSink) Record(_ context.Context, event audit.Event) error {
	return audit.Validate(event)
}

type controllerFakeProjectAdapter struct {
	mu          sync.Mutex
	projects    map[string]projectruntime.RemoteProject
	createCalls int
}

type controllerFakeSettingsAdapter struct {
	filters        []settingsruntime.Filter
	state          settingsruntime.ProjectState
	filterUpdates  int
	projectUpdates []settingsruntime.ProjectPatch
}

type controllerFakeAlertAdapter struct {
	mu          sync.Mutex
	objects     map[string]alertsruntime.Observed
	deleteCalls int
	deleteErr   error
}

func (a *controllerFakeAlertAdapter) Ensure(_ context.Context, adapter, _, _, _ string, desired alertsruntime.Desired, marker binding.Marker) (alertsruntime.Observed, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	identity := "private-alert-id-SHOULD-NOT-LEAK"
	if current, found := a.objects[identity]; found {
		return current, nil
	}
	observed := alertsruntime.Observed{Identity: identity, Adapter: adapter, Marker: marker, DesiredHash: controllerAlertHash(desired), Enabled: desired.Enabled}
	a.objects[identity] = observed
	return observed, nil
}

func (a *controllerFakeAlertAdapter) Observe(_ context.Context, _, _, _, identity string) (alertsruntime.Observed, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	value, found := a.objects[identity]
	return value, found, nil
}

func (a *controllerFakeAlertAdapter) Update(_ context.Context, _, _, _, identity string, desired alertsruntime.Desired) (alertsruntime.Observed, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	value := a.objects[identity]
	value.DesiredHash = controllerAlertHash(desired)
	a.objects[identity] = value
	return value, nil
}

func (a *controllerFakeAlertAdapter) SetEnabled(_ context.Context, _, _, _, identity string, enabled bool) (alertsruntime.Observed, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	value := a.objects[identity]
	value.Enabled = enabled
	a.objects[identity] = value
	return value, nil
}

func (a *controllerFakeAlertAdapter) Delete(_ context.Context, _, _, _, identity string, marker binding.Marker) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.deleteErr != nil {
		return a.deleteErr
	}
	value, found := a.objects[identity]
	if !found {
		return nil
	}
	if !value.Marker.Matches(marker) {
		return alertsruntime.ErrOwnershipLost
	}
	delete(a.objects, identity)
	a.deleteCalls++
	return nil
}

func controllerAlertHash(desired alertsruntime.Desired) string {
	notify := append([]string(nil), desired.Notify...)
	sort.Strings(notify)
	payload, _ := json.Marshal(struct {
		Trigger       alertsruntime.Trigger `json:"trigger"`
		WindowMinutes int                   `json:"windowMinutes,omitempty"`
		Threshold     int64                 `json:"threshold,omitempty"`
		Notify        []string              `json:"notify"`
	}{Trigger: desired.Trigger, WindowMinutes: desired.WindowMinutes, Threshold: desired.Threshold, Notify: notify})
	digest := sha256.Sum256(payload)
	return fmt.Sprintf("%x", digest[:])
}

type controllerFakeDestinationCatalog struct {
	integrations map[string][]destination.Integration
	slack        map[string][]destination.Resource
	pagerDuty    map[string][]destination.Resource
}

func (c *controllerFakeDestinationCatalog) ListIntegrations(_ context.Context, _, provider string) ([]destination.Integration, error) {
	return append([]destination.Integration(nil), c.integrations[provider]...), nil
}

func (c *controllerFakeDestinationCatalog) ListSlackChannels(_ context.Context, _, integration string) ([]destination.Resource, error) {
	return append([]destination.Resource(nil), c.slack[integration]...), nil
}

func (c *controllerFakeDestinationCatalog) ListPagerDutyServices(_ context.Context, _, integration string) ([]destination.Resource, error) {
	return append([]destination.Resource(nil), c.pagerDuty[integration]...), nil
}

func (a *controllerFakeSettingsAdapter) ListFilters(context.Context, string, string) ([]settingsruntime.Filter, error) {
	return append([]settingsruntime.Filter(nil), a.filters...), nil
}

func (a *controllerFakeSettingsAdapter) UpdateFilter(context.Context, string, string, string, bool) error {
	a.filterUpdates++
	return nil
}

func (a *controllerFakeSettingsAdapter) GetProjectState(context.Context, string, string) (settingsruntime.ProjectState, error) {
	return a.state, nil
}

func (a *controllerFakeSettingsAdapter) UpdateProjectState(_ context.Context, _, _ string, patch settingsruntime.ProjectPatch, _ string) error {
	a.projectUpdates = append(a.projectUpdates, patch)
	return nil
}

func (a *controllerFakeProjectAdapter) GetBySlug(_ context.Context, _, slug string) (projectruntime.RemoteProject, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	project, found := a.projects[slug]
	if !found {
		return projectruntime.RemoteProject{}, &sentryapi.APIError{Kind: sentryapi.ErrorNotFound, StatusCode: 404, Operation: "get-project"}
	}
	return project, nil
}

func (a *controllerFakeProjectAdapter) Create(_ context.Context, request projectruntime.CreateRequest) (projectruntime.RemoteProject, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.createCalls++
	marker := request.Marker
	project := projectruntime.RemoteProject{Identity: "project-id", Slug: request.Slug, URL: "https://sentry.example.test/projects/" + request.Slug, Marker: &marker}
	a.projects[request.Slug] = project
	return project, nil
}

func (a *controllerFakeProjectAdapter) Delete(_ context.Context, _, slug string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.projects, slug)
	return nil
}
