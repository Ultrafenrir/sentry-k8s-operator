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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	alertsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/alerts"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	clientkeyruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/clientkey"
	codemappingruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/codemapping"
	connectionruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/connection"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/dsn"
	issueownershipruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/issueownership"
	projectruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/project"
	projectaccessruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/projectaccess"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
	settingsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/settings"
	symbolsourceruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/symbolsource"
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
		var issueOwnershipAdapter *controllerFakeIssueOwnershipAdapter
		var clientKeyAdapter *controllerFakeClientKeyAdapter
		var symbolSourceAdapter *controllerFakeSymbolSourceAdapter
		var projectAccessAdapter *controllerFakeProjectAccessAdapter
		var destinationCatalog *controllerFakeDestinationCatalog
		var alertAdapter *controllerFakeAlertAdapter
		var hookVerifierSecretName string

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
						Teams:       []string{"payments", "backend"},
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
			issueOwnershipAdapter = &controllerFakeIssueOwnershipAdapter{}
			clientKeyAdapter = &controllerFakeClientKeyAdapter{items: make(map[string]clientkeyruntime.Remote)}
			symbolSourceAdapter = &controllerFakeSymbolSourceAdapter{items: make(map[string]symbolsourceruntime.Remote)}
			projectAccessAdapter = &controllerFakeProjectAccessAdapter{teams: []projectaccessruntime.Team{{Identity: "team-payments", Slug: "payments"}}}
			destinationCatalog = &controllerFakeDestinationCatalog{}
			alertAdapter = &controllerFakeAlertAdapter{objects: make(map[string]alertsruntime.Observed)}
			hookVerifierSecretName = ""
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
				NewSettingsAdapter: func(*sentryapi.Client) settingsruntime.Adapter { return settingsAdapter },
				NewIssueOwnershipAdapter: func(*sentryapi.Client) issueownershipruntime.Adapter {
					return issueOwnershipAdapter
				},
				NewClientKeyAdapter: func(*sentryapi.Client) clientkeyruntime.Adapter { return clientKeyAdapter },
				NewSymbolSourceAdapter: func(*sentryapi.Client) symbolsourceruntime.Adapter {
					return symbolSourceAdapter
				},
				NewProjectAccessAdapter: func(*sentryapi.Client) projectaccessruntime.Adapter {
					return projectAccessAdapter
				},
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
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection); err == nil {
				connection.Spec.Access = []sentryconfigurationv1alpha1.AccessRule{{
					Owner:       "payments",
					Namespaces:  sentryconfigurationv1alpha1.NamespaceAccess{MatchNames: []string{resourceNamespace}},
					Teams:       []string{"payments", "backend"},
					AllowCreate: true,
				}}
				Expect(k8sClient.Update(ctx, connection)).To(Succeed())
				connection.Status.Capabilities = []string{projectruntime.CapabilityAtomicDefaultRules, projectruntime.CapabilityAutomaticActiveKey}
				Expect(k8sClient.Status().Update(ctx, connection)).To(Succeed())
			}
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			if err := k8sClient.Get(ctx, key, resource); err == nil {
				Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			}
			Eventually(func() bool {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				if err != nil {
					return false
				}
				current := &sentryconfigurationv1alpha1.SentryProject{}
				getErr := k8sClient.Get(ctx, key, current)
				return apierrors.IsNotFound(getErr)
			}).Should(BeTrue())
			connection = &sentryconfigurationv1alpha1.SentryConnection{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection); err == nil {
				Expect(k8sClient.Delete(ctx, connection)).To(Succeed())
			}
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, &sentryconfigurationv1alpha1.SentryConnection{}))
			}).Should(BeTrue())
			if hookVerifierSecretName != "" {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hookVerifierSecretName, Namespace: resourceNamespace}}
				if err := k8sClient.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			}
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

		It("creates, publishes, and safely rotates a managed client key", func() {
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			keys := map[string]sentryconfigurationv1alpha1.ManagedClientKeySpec{
				"ingest": {Active: true, RotationRevision: 1, RateLimit: &sentryconfigurationv1alpha1.ClientKeyRateLimit{Count: 1000, WindowSeconds: 60}},
			}
			resource.Spec.ClientKeys = &keys
			resource.Spec.DSN = &sentryconfigurationv1alpha1.DSNSpec{ManagedClientKey: "ingest"}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			debugResource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, debugResource)).To(Succeed())
			Expect(clientKeyAdapter.createCalls).To(Equal(1), "status=%#v", debugResource.Status)
			Expect(keyListCalls).To(Equal(0))

			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.DSNClientKey).To(Equal("ingest"))
			Expect(updated.Status.ClientKeys["ingest"]).To(Equal(sentryconfigurationv1alpha1.ClientKeyStatus{State: clientkeyruntime.StateReady, Reason: "ClientKeyReady", RotationRevision: 1}))
			statusJSON, err := json.Marshal(updated.Status)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(statusJSON)).NotTo(ContainSubstring("managed-client-key-id"))
			Expect(string(statusJSON)).NotTo(ContainSubstring("https://managed-public"))
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(string(secret.Data[sentryconfigurationv1alpha1.DefaultDSNSecretKey])).To(ContainSubstring("https://managed-public-1@"))

			rotating := updated.DeepCopy()
			rotatingKeys := *rotating.Spec.ClientKeys
			entry := rotatingKeys["ingest"]
			entry.RotationRevision = 2
			rotatingKeys["ingest"] = entry
			rotating.Spec.ClientKeys = &rotatingKeys
			Expect(k8sClient.Update(ctx, rotating)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(clientKeyAdapter.createCalls).To(Equal(2))
			Expect(clientKeyAdapter.deleteCalls).To(Equal(0))
			pending := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, pending)).To(Succeed())
			Expect(pending.Status.ClientKeys["ingest"].State).To(Equal(clientkeyruntime.StateRotationPending))
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix, Namespace: resourceNamespace}, secret)).To(Succeed())
			Expect(string(secret.Data[sentryconfigurationv1alpha1.DefaultDSNSecretKey])).To(ContainSubstring("https://managed-public-2@"))

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(clientKeyAdapter.deleteCalls).To(Equal(1))
			ready := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, ready)).To(Succeed())
			Expect(ready.Status.ClientKeys["ingest"].State).To(Equal(clientkeyruntime.StateReady))
			Expect(ready.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "ClientKeysReady"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "ClientKeysReady"),
			)))
		})

		It("loads namespace-local symbol-source credentials and owns exact lifecycle children", func() {
			credentialName := resourceName + "-symbol-creds"
			credentials := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: credentialName, Namespace: resourceNamespace},
				Data: map[string][]byte{
					"http-password": []byte("SECRET-SENTINEL-one"),
					"s3-access":     []byte("ACCESS-SENTINEL"),
					"s3-secret":     []byte("SECRET-SENTINEL-s3"),
					"gcs-key":       []byte("SECRET-SENTINEL-gcs"),
				},
			}
			Expect(k8sClient.Create(ctx, credentials)).To(Succeed())
			DeferCleanup(func() {
				if err := k8sClient.Delete(ctx, credentials); err != nil && !apierrors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			})

			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			sources := map[string]sentryconfigurationv1alpha1.CustomSymbolSourceSpec{
				"http-symbols": {
					Layout: sentryconfigurationv1alpha1.SymbolSourceLayout{Type: "native", Casing: "default"},
					HTTP: &sentryconfigurationv1alpha1.HTTPSymbolSource{
						URL: "https://symbols.example.invalid", Username: "reader",
						PasswordSecretRef: &sentryconfigurationv1alpha1.SecretKeyReference{Name: credentialName, Key: "http-password"},
					},
				},
				"s3-symbols": {
					Layout: sentryconfigurationv1alpha1.SymbolSourceLayout{Type: "symstore", Casing: "lowercase"},
					S3: &sentryconfigurationv1alpha1.S3SymbolSource{
						Bucket: "native-symbols", Region: "eu-west-1", Prefix: "release/",
						AccessKeySecretRef: sentryconfigurationv1alpha1.SecretKeyReference{Name: credentialName, Key: "s3-access"},
						SecretKeySecretRef: sentryconfigurationv1alpha1.SecretKeyReference{Name: credentialName, Key: "s3-secret"},
					},
				},
				"gcs-symbols": {
					Layout: sentryconfigurationv1alpha1.SymbolSourceLayout{Type: "unified", Casing: "uppercase"},
					GCS: &sentryconfigurationv1alpha1.GCSSymbolSource{
						Bucket: "gcs-native-symbols", ClientEmail: "symbols@example.invalid", Prefix: "debug/",
						PrivateKeySecretRef: sentryconfigurationv1alpha1.SecretKeyReference{Name: credentialName, Key: "gcs-key"},
					},
				},
			}
			resource.Spec.SymbolSources = &sources
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(symbolSourceAdapter.createCalls).To(Equal(3))
			Expect(symbolSourceAdapter.updateCalls).To(Equal(0))

			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.SymbolSources).To(HaveLen(3))
			Expect(updated.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "SymbolSourcesReady"), HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "SymbolSourcesReady"),
			)))
			statusJSON, err := json.Marshal(updated.Status)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(statusJSON)).NotTo(ContainSubstring("SECRET-SENTINEL"))
			Expect(string(statusJSON)).NotTo(ContainSubstring("ACCESS-SENTINEL"))
			Expect(string(statusJSON)).NotTo(ContainSubstring("sko-"))

			record, err := reconciler.Bindings.GetBinding(ctx, string(updated.UID))
			Expect(err).NotTo(HaveOccurred())
			bindingJSON, err := json.Marshal(record.Binding.SymbolSources)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(bindingJSON)).NotTo(ContainSubstring("SECRET-SENTINEL"))
			Expect(string(bindingJSON)).NotTo(ContainSubstring("ACCESS-SENTINEL"))

			credentials.Data["http-password"] = []byte("SECRET-SENTINEL-two")
			Expect(k8sClient.Update(ctx, credentials)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(symbolSourceAdapter.updateCalls).To(Equal(1))

			empty := map[string]sentryconfigurationv1alpha1.CustomSymbolSourceSpec{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			updated.Spec.SymbolSources = &empty
			Expect(k8sClient.Update(ctx, updated)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(symbolSourceAdapter.deleteCalls).To(Equal(3))
			Expect(symbolSourceAdapter.items).To(BeEmpty())
		})

		It("resolves connection-owned repository aliases and imports CODEOWNERS without public IDs", func() {
			codeAdapter := newControllerFakeCodeMappingAdapter()
			reconciler.NewCodeMappingAdapter = func(*sentryapi.Client) codemappingruntime.Adapter { return codeAdapter }
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Integrations = &sentryconfigurationv1alpha1.ConnectionIntegrations{SourceCode: &sentryconfigurationv1alpha1.SourceCodeIntegrations{Installations: []sentryconfigurationv1alpha1.SourceCodeIntegrationCatalogEntry{{
				Name: "github", Provider: "github", SentryName: "acme", Repositories: []sentryconfigurationv1alpha1.SourceCodeRepositoryCatalogEntry{{Name: "checkout", SentryName: "acme/checkout"}},
			}}}}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			mappings := map[string]sentryconfigurationv1alpha1.CodeMappingSpec{"checkout": {Integration: "github", Repository: "checkout", StackRoot: "/", SourceRoot: "src", DefaultBranch: "main", ImportCodeowners: true}}
			resource.Spec.CodeMappings = &mappings
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			Eventually(func() bool {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				if err != nil {
					return false
				}
				current := &sentryconfigurationv1alpha1.SentryProject{}
				if k8sClient.Get(ctx, key, current) != nil {
					return false
				}
				condition := meta.FindStatusCondition(current.Status.Conditions, "CodeMappingsReady")
				return condition != nil && condition.Status == metav1.ConditionTrue && current.Status.CodeMappings["checkout"].State == codemappingruntime.StateReady
			}).Should(BeTrue())
			current := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, current)).To(Succeed())
			statusJSON, err := json.Marshal(current.Status.CodeMappings)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(statusJSON)).NotTo(ContainSubstring("mapping-"))
			record, err := reconciler.Bindings.GetBinding(ctx, string(current.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.CodeMappings["checkout"].ExternalIdentity).To(Equal("mapping-1"))
			Expect(record.Binding.CodeMappings["checkout"].CodeownersIdentity).To(Equal("codeowners-1"))
		})

		It("cleans protected state and releases ownership before completing Retain deletion", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			uid := string(resource.UID)
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, key, &sentryconfigurationv1alpha1.SentryProject{}))
			}).Should(BeTrue())
			Expect(adapter.releaseCalls).To(Equal(1))
			_, err = reconciler.Bindings.GetBinding(ctx, uid)
			Expect(err).To(Equal(binding.ErrNotFound))
			_, err = reconciler.Bindings.GetClaim(ctx, "https://sentry.example.test", "company", resourceName)
			Expect(err).To(Equal(binding.ErrNotFound))
		})

		It("observes an unmanaged standby candidate and publishes its authorized DSN without a manager claim", func() {
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.ProjectManagement.UnmanagedProjects = sentryconfigurationv1alpha1.UnmanagedProjectsStandby
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())

			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.Role = sentryconfigurationv1alpha1.ProjectRoleStandby
			resource.Spec.DSN = &sentryconfigurationv1alpha1.DSNSpec{}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			adapter.projects[resourceName] = projectruntime.RemoteProject{Identity: "existing-project", Slug: resourceName, URL: "https://sentry.example.test/projects/" + resourceName}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(adapter.createCalls).To(Equal(0))

			updated := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, updated)).To(Succeed())
			Expect(updated.Status.Origin).To(Equal("External"))
			Expect(updated.Status.AccessMode).To(Equal("ReadOnly"))
			Expect(updated.Status.EffectiveRole).To(Equal("Standby"))
			Expect(updated.Status.DSNSecret).To(Equal(resourceName + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix))
			_, claimErr := reconciler.Bindings.GetClaim(ctx, "https://sentry.example.test", "company", resourceName)
			Expect(claimErr).To(MatchError(binding.ErrNotFound))
		})

		It("removes managed standby alert children before releasing its read-only binding", func() {
			candidateNamespace := fmt.Sprintf("standby-candidate-%d", projectSerial)
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: candidateNamespace}}
			Expect(k8sClient.Create(ctx, namespace)).To(Succeed())
			defer func() {
				if err := k8sClient.Delete(ctx, namespace); err != nil && !apierrors.IsNotFound(err) {
					Expect(err).NotTo(HaveOccurred())
				}
			}()

			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Compatibility = &sentryconfigurationv1alpha1.ConnectionCompatibility{Alerts: sentryconfigurationv1alpha1.AlertCompatibilityLegacy24}
			connection.Spec.Integrations = &sentryconfigurationv1alpha1.ConnectionIntegrations{
				Slack: &sentryconfigurationv1alpha1.SlackIntegration{Workspaces: []sentryconfigurationv1alpha1.IntegrationCatalogEntry{{Name: "company", SentryName: "company"}}},
			}
			connection.Spec.Access[0].Namespaces.MatchNames = []string{resourceNamespace, candidateNamespace}
			connection.Spec.Access[0].NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargetAccess{SlackWorkspaces: []string{"company"}}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			destinationCatalog.integrations = map[string][]destination.Integration{"slack": {{Identity: "12", Name: "company"}}}
			destinationCatalog.slack = map[string][]destination.Resource{"12": {{Identity: "C123", Name: "incidents"}}}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			manager := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, manager)).To(Succeed())

			alerts := map[string]sentryconfigurationv1alpha1.AlertSpec{
				"new-errors": {Trigger: sentryconfigurationv1alpha1.AlertTriggerFirstSeen, Notify: []string{"slack/incidents"}},
			}
			candidateKey := types.NamespacedName{Name: resourceName, Namespace: candidateNamespace}
			candidate := &sentryconfigurationv1alpha1.SentryProject{
				ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: candidateNamespace},
				Spec: sentryconfigurationv1alpha1.SentryProjectSpec{
					Sentry:              connectionName,
					Project:             sentryconfigurationv1alpha1.ProjectSelector{Create: &sentryconfigurationv1alpha1.ProjectCreate{Team: "payments"}},
					Role:                sentryconfigurationv1alpha1.ProjectRoleStandby,
					NotificationTargets: &sentryconfigurationv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"incidents"}}},
					Alerts:              &alerts,
					Lifecycle:           sentryconfigurationv1alpha1.LifecycleSpec{OnDelete: sentryconfigurationv1alpha1.OnDeleteRetain},
				},
			}
			Expect(k8sClient.Create(ctx, candidate)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: candidateKey})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: candidateKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, candidateKey, candidate)).To(Succeed())
			Expect(candidate.Status.Origin).To(Equal(string(binding.OriginManaged)))
			Expect(candidate.Status.AccessMode).To(Equal(binding.AccessReadOnly))
			Expect(candidate.Status.Alerts).To(HaveKeyWithValue("new-errors", sentryconfigurationv1alpha1.AlertStatus{State: alertsruntime.StateReady, Reason: "AlertReady"}))
			candidateRecord, err := reconciler.Bindings.GetBinding(ctx, string(candidate.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(candidateRecord.Binding.Alerts).To(HaveKey("new-errors"))
			Expect(candidateRecord.Binding.ManagerUID).To(BeEmpty())
			claim, err := reconciler.Bindings.GetClaim(ctx, connection.Spec.URL, connection.Spec.Organization, resourceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(claim.Claim.Manager.UID).To(Equal(string(manager.UID)))

			Expect(k8sClient.Delete(ctx, candidate)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: candidateKey})
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() bool {
				return apierrors.IsNotFound(k8sClient.Get(ctx, candidateKey, &sentryconfigurationv1alpha1.SentryProject{}))
			}).Should(BeTrue())
			Expect(alertAdapter.deleteCalls).To(Equal(1))
			Expect(alertAdapter.objects).To(BeEmpty())
			_, err = reconciler.Bindings.GetBinding(ctx, string(candidate.UID))
			Expect(err).To(Equal(binding.ErrNotFound))
			claim, err = reconciler.Bindings.GetClaim(ctx, connection.Spec.URL, connection.Spec.Organization, resourceName)
			Expect(err).NotTo(HaveOccurred())
			Expect(claim.Claim.Manager.UID).To(Equal(string(manager.UID)))
		})

		It("grants and revokes additional team access while preserving the primary team", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.GrantAccess = &sentryconfigurationv1alpha1.ProjectGrantAccess{Teams: []string{"backend"}}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "GrantAccessReady"), HaveField("Status", metav1.ConditionTrue),
			)))
			record, err := reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.GrantedTeams).To(Equal(map[string]string{"backend": "team-backend"}))

			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Access[0].Teams = []string{"payments"}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "GrantAccessReady"), HaveField("Status", metav1.ConditionFalse),
				HaveField("Reason", "AccessDenied"),
			)))
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.GrantedTeams).To(BeEmpty())

			connection.Spec.Access[0].Teams = []string{"payments", "backend"}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.GrantAccess = &sentryconfigurationv1alpha1.ProjectGrantAccess{}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.GrantedTeams).To(BeEmpty())
			Expect(projectAccessAdapter.teams).To(Equal([]projectaccessruntime.Team{{Identity: "team-payments", Slug: "payments"}}))
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
			autoResolve := int64(72)
			currentResolve := int64(24)
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.Settings = &sentryconfigurationv1alpha1.ProjectSettings{
				Filters:  &sentryconfigurationv1alpha1.FilterSettings{BrowserExtensions: &trueValue},
				PII:      &sentryconfigurationv1alpha1.PIISettings{ScrubData: &trueValue},
				Behavior: &sentryconfigurationv1alpha1.BehaviorSettings{AutoResolveHours: &autoResolve},
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			settingsAdapter.filters = []settingsruntime.Filter{{ID: "localhost", Active: false}}
			settingsAdapter.state = settingsruntime.ProjectState{ScrubData: &falseValue, ResolveAge: &currentResolve}

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(settingsAdapter.projectUpdates).To(HaveLen(2))
			Expect(settingsAdapter.projectUpdates[1].ResolveAge).NotTo(BeNil())
			Expect(*settingsAdapter.projectUpdates[1].ResolveAge).To(Equal(int64(72)))
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

		It("reconciles team-only issue ownership after project access and reports a dedicated condition", func() {
			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.GrantAccess = &sentryconfigurationv1alpha1.ProjectGrantAccess{Teams: []string{"backend"}}
			resource.Spec.IssueOwnership = &sentryconfigurationv1alpha1.IssueOwnershipSpec{
				Rules: []sentryconfigurationv1alpha1.IssueOwnershipRule{{
					Matcher: sentryconfigurationv1alpha1.IssueOwnershipMatcher{Type: sentryconfigurationv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/backend/*"},
					Teams:   []string{"backend"},
				}},
				Fallthrough:        sentryconfigurationv1alpha1.IssueOwnershipFallthroughNoOne,
				AutoAssignment:     sentryconfigurationv1alpha1.IssueOwnershipAutoAssignIssueOwner,
				CodeownersAutoSync: true,
			}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			issueOwnershipAdapter.state = controllerIssueOwnershipState("path:old/* #payments", true, "Turn off Auto-Assignment", false)

			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(issueOwnershipAdapter.updates).To(HaveLen(1))
			Expect(issueOwnershipAdapter.updates[0]).To(Equal(issueownershipruntime.Patch{
				Raw: "path:src/backend/* #backend", Fallthrough: false,
				AutoAssignment: "Auto Assign to Issue Owner", CodeownersAutoSync: true,
			}))
			Expect(projectAccessAdapter.teams).To(ContainElement(projectaccessruntime.Team{Identity: "team-backend", Slug: "backend"}))
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			Expect(resource.Status.Conditions).To(ContainElement(And(
				HaveField("Type", "IssueOwnershipReady"),
				HaveField("Status", metav1.ConditionTrue),
				HaveField("Reason", "IssueOwnershipReady"),
			)))

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			Expect(issueOwnershipAdapter.updates).To(HaveLen(1))
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

		It("owns one exact Sentry service hook for active Telegram alert routes", func() {
			connection := &sentryconfigurationv1alpha1.SentryConnection{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Compatibility = &sentryconfigurationv1alpha1.ConnectionCompatibility{Alerts: sentryconfigurationv1alpha1.AlertCompatibilityLegacy24}
			connection.Spec.Integrations = &sentryconfigurationv1alpha1.ConnectionIntegrations{Telegram: &sentryconfigurationv1alpha1.TelegramIntegration{
				BotTokenSecretRef: sentryconfigurationv1alpha1.SecretKeyReference{Name: "telegram", Key: "bot"},
				ReceiverURL:       "https://hooks.example.test/events",
			}}
			connection.Spec.Access[0].NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargetAccess{TelegramTargets: []string{"ops"}}
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())

			hookVerifierSecretName = fmt.Sprintf("hook-verifier-%d", projectSerial)
			Expect(k8sClient.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: hookVerifierSecretName, Namespace: resourceNamespace}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}})).To(Succeed())
			hookAdapter := &controllerFakeServiceHookAdapter{hooks: map[string]sentryhook.RemoteServiceHook{}}
			reconciler.HookSources = &sentryhook.SourceStore{Client: k8sClient, Namespace: resourceNamespace}
			reconciler.HookVerifier = &sentryhook.VerifierStore{Client: k8sClient, Namespace: resourceNamespace, SecretName: hookVerifierSecretName}
			reconciler.NewServiceHookAdapter = func(*sentryapi.Client) sentryhook.ServiceHookAdapter { return hookAdapter }
			reconciler.RegistrationRoutes = &recoveryRoutes{}
			reconciler.DestinationAudit = &destinationAuditRecorder{}

			resource := &sentryconfigurationv1alpha1.SentryProject{}
			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			resource.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Telegram: []string{"ops"}}
			alerts := map[string]sentryconfigurationv1alpha1.AlertSpec{"regression": {Trigger: sentryconfigurationv1alpha1.AlertTriggerRegression, Notify: []string{"telegram/ops"}}}
			resource.Spec.Alerts = &alerts
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			record, err := reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			target := record.Binding.Destinations["telegram/ops"]
			target.State = destination.StateActive
			target.Reason = "TelegramReady"
			target.Telegram = &binding.TelegramBinding{Target: &binding.TelegramTarget{ChatIdentity: "-1001"}, RouteKey: strings.Repeat("a", 64)}
			next := record.Binding
			next.Destinations["telegram/ops"] = target
			_, err = reconciler.Bindings.UpdateBinding(ctx, record, next)
			Expect(err).NotTo(HaveOccurred())

			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.ServiceHook).NotTo(BeNil())
			Expect(hookAdapter.createCalls).To(Equal(1))
			_, err = reconciler.HookSources.Get(ctx, record.Binding.ServiceHook.SourceBindingKey)
			Expect(err).NotTo(HaveOccurred())
			_, err = reconciler.HookVerifier.Get(ctx, record.Binding.ServiceHook.SourceBindingKey)
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName}, connection)).To(Succeed())
			connection.Spec.Integrations.Telegram.ServiceHookRevision = 2
			Expect(k8sClient.Update(ctx, connection)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.ServiceHook).NotTo(BeNil())
			Expect(record.Binding.ServiceHook.Revision).To(Equal(int64(2)))
			Expect(record.Binding.ServiceHook.ExternalIdentity).To(Equal("hook-id-2"))
			Expect(hookAdapter.createCalls).To(Equal(2))
			Expect(hookAdapter.deleteCalls).To(Equal(1))

			Expect(k8sClient.Get(ctx, key, resource)).To(Succeed())
			emptyAlerts := map[string]sentryconfigurationv1alpha1.AlertSpec{}
			resource.Spec.Alerts = &emptyAlerts
			resource.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{}
			Expect(k8sClient.Update(ctx, resource)).To(Succeed())
			_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			record, err = reconciler.Bindings.GetBinding(ctx, string(resource.UID))
			Expect(err).NotTo(HaveOccurred())
			Expect(record.Binding.ServiceHook).To(BeNil())
			Expect(hookAdapter.deleteCalls).To(Equal(2))
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
	mu           sync.Mutex
	projects     map[string]projectruntime.RemoteProject
	createCalls  int
	releaseCalls int
}

type controllerFakeServiceHookAdapter struct {
	hooks       map[string]sentryhook.RemoteServiceHook
	createCalls int
	deleteCalls int
}

func (a *controllerFakeServiceHookAdapter) List(context.Context, string, string) ([]sentryhook.RemoteServiceHook, error) {
	result := make([]sentryhook.RemoteServiceHook, 0, len(a.hooks))
	for _, hook := range a.hooks {
		result = append(result, hook)
	}
	return result, nil
}

func (a *controllerFakeServiceHookAdapter) Get(_ context.Context, _, _, identity string) (sentryhook.RemoteServiceHook, bool, error) {
	hook, found := a.hooks[identity]
	return hook, found, nil
}

func (a *controllerFakeServiceHookAdapter) Create(_ context.Context, _, _, endpoint string, active bool) (sentryhook.RemoteServiceHook, error) {
	a.createCalls++
	hook := sentryhook.RemoteServiceHook{
		Identity: fmt.Sprintf("hook-id-%d", a.createCalls), URL: endpoint, Events: []string{sentryhook.AlertEvent}, Active: active,
		Secret: []byte(fmt.Sprintf("controller-service-hook-secret-%d", a.createCalls)),
	}
	a.hooks[hook.Identity] = hook
	return hook, nil
}

func (a *controllerFakeServiceHookAdapter) Repair(_ context.Context, _, _, identity, endpoint string) (sentryhook.RemoteServiceHook, error) {
	hook := a.hooks[identity]
	hook.URL = endpoint
	hook.Events = []string{sentryhook.AlertEvent}
	hook.Active = true
	a.hooks[identity] = hook
	return hook, nil
}

func (a *controllerFakeServiceHookAdapter) Delete(_ context.Context, _, _, identity string) error {
	a.deleteCalls++
	delete(a.hooks, identity)
	return nil
}

type controllerFakeSettingsAdapter struct {
	filters        []settingsruntime.Filter
	state          settingsruntime.ProjectState
	filterUpdates  int
	projectUpdates []settingsruntime.ProjectPatch
}

type controllerFakeClientKeyAdapter struct {
	items       map[string]clientkeyruntime.Remote
	nextID      int
	createCalls int
	updateCalls int
	deleteCalls int
}

type controllerFakeSymbolSourceAdapter struct {
	items       map[string]symbolsourceruntime.Remote
	createCalls int
	updateCalls int
	deleteCalls int
}

func (a *controllerFakeSymbolSourceAdapter) List(context.Context, string, string) ([]symbolsourceruntime.Remote, error) {
	result := make([]symbolsourceruntime.Remote, 0, len(a.items))
	for _, item := range a.items {
		result = append(result, item)
	}
	return result, nil
}

func (a *controllerFakeSymbolSourceAdapter) Create(_ context.Context, _, _ string, request symbolsourceruntime.Request) (symbolsourceruntime.Remote, error) {
	a.createCalls++
	remote := controllerSymbolSourceRemote(request)
	a.items[remote.Identity] = remote
	return remote, nil
}

func (a *controllerFakeSymbolSourceAdapter) Update(_ context.Context, _, _, identity string, request symbolsourceruntime.Request) (symbolsourceruntime.Remote, error) {
	a.updateCalls++
	if _, found := a.items[identity]; !found {
		return symbolsourceruntime.Remote{}, errors.New("symbol source not found")
	}
	remote := controllerSymbolSourceRemote(request)
	a.items[identity] = remote
	return remote, nil
}

func (a *controllerFakeSymbolSourceAdapter) Delete(_ context.Context, _, _, identity string) error {
	a.deleteCalls++
	delete(a.items, identity)
	return nil
}

func controllerSymbolSourceRemote(request symbolsourceruntime.Request) symbolsourceruntime.Remote {
	return symbolsourceruntime.Remote{
		Identity: request.Identity, Name: request.Name, Type: request.Type, Layout: request.Layout,
		URL: request.URL, Username: request.Username, Bucket: request.Bucket, Region: request.Region,
		AccessKey: request.AccessKey, Prefix: request.Prefix, ClientEmail: request.ClientEmail,
	}
}

func (a *controllerFakeClientKeyAdapter) List(context.Context, string, string) ([]clientkeyruntime.Remote, error) {
	result := make([]clientkeyruntime.Remote, 0, len(a.items))
	for _, item := range a.items {
		result = append(result, item)
	}
	return result, nil
}

func (a *controllerFakeClientKeyAdapter) Create(_ context.Context, _, _ string, request clientkeyruntime.CreateRequest) (clientkeyruntime.Remote, error) {
	a.nextID++
	a.createCalls++
	remote := clientkeyruntime.Remote{
		Identity: fmt.Sprintf("managed-client-key-id-%d-SHOULD-NOT-LEAK", a.nextID),
		Name:     request.Name, IsActive: true, RateLimit: request.RateLimit,
	}
	remote.DSN.Public = fmt.Sprintf("https://managed-public-%d@example.invalid/42", a.nextID)
	a.items[remote.Identity] = remote
	return remote, nil
}

func (a *controllerFakeClientKeyAdapter) Get(_ context.Context, _, _, identity string) (clientkeyruntime.Remote, bool, error) {
	remote, found := a.items[identity]
	return remote, found, nil
}

func (a *controllerFakeClientKeyAdapter) Update(_ context.Context, _, _, identity string, request clientkeyruntime.UpdateRequest) (clientkeyruntime.Remote, error) {
	a.updateCalls++
	remote := a.items[identity]
	remote.Name = request.Name
	remote.IsActive = request.IsActive
	remote.RateLimit = request.RateLimit
	a.items[identity] = remote
	return remote, nil
}

func (a *controllerFakeClientKeyAdapter) Delete(_ context.Context, _, _, identity string) error {
	a.deleteCalls++
	delete(a.items, identity)
	return nil
}

type controllerFakeIssueOwnershipAdapter struct {
	state   issueownershipruntime.State
	updates []issueownershipruntime.Patch
}

func (a *controllerFakeIssueOwnershipAdapter) Get(context.Context, string, string) (issueownershipruntime.State, error) {
	return a.state, nil
}

func (a *controllerFakeIssueOwnershipAdapter) Update(_ context.Context, _, _ string, patch issueownershipruntime.Patch) error {
	a.updates = append(a.updates, patch)
	a.state = controllerIssueOwnershipState(patch.Raw, patch.Fallthrough, patch.AutoAssignment, patch.CodeownersAutoSync)
	return nil
}

func controllerIssueOwnershipState(raw string, fallthroughValue bool, autoAssignment string, codeownersAutoSync bool) issueownershipruntime.State {
	return issueownershipruntime.State{
		Raw: &raw, Fallthrough: &fallthroughValue, AutoAssignment: &autoAssignment, CodeownersAutoSync: &codeownersAutoSync,
	}
}

type controllerFakeProjectAccessAdapter struct {
	teams []projectaccessruntime.Team
}

func (a *controllerFakeProjectAccessAdapter) ListTeams(context.Context, string, string) ([]projectaccessruntime.Team, error) {
	return append([]projectaccessruntime.Team(nil), a.teams...), nil
}

func (a *controllerFakeProjectAccessAdapter) GrantTeam(_ context.Context, _, _, slug string) error {
	a.teams = append(a.teams, projectaccessruntime.Team{Identity: "team-" + slug, Slug: slug})
	return nil
}

func (a *controllerFakeProjectAccessAdapter) RevokeTeam(_ context.Context, _, _, identity string) error {
	for index, team := range a.teams {
		if team.Identity == identity {
			a.teams = append(a.teams[:index], a.teams[index+1:]...)
			return nil
		}
	}
	return errors.New("team not found")
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

func (a *controllerFakeSettingsAdapter) UpdateFilter(context.Context, string, string, string, settingsruntime.FilterPatch) error {
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

func (a *controllerFakeProjectAdapter) ReleaseOwnership(_ context.Context, _, slug string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	project, found := a.projects[slug]
	if !found {
		return &sentryapi.APIError{Kind: sentryapi.ErrorNotFound, StatusCode: 404, Operation: "release-project-ownership"}
	}
	a.releaseCalls++
	project.Marker = nil
	project.RegionalEvidence = nil
	a.projects[slug] = project
	return nil
}

type controllerFakeCodeMappingAdapter struct {
	integrations []codemappingruntime.Integration
	repositories []codemappingruntime.Repository
	mappings     []codemappingruntime.Remote
	codeowners   []codemappingruntime.Codeowners
}

func newControllerFakeCodeMappingAdapter() *controllerFakeCodeMappingAdapter {
	integration := codemappingruntime.Integration{Identity: "integration-1", Name: "acme"}
	integration.Provider.Key = "github"
	return &controllerFakeCodeMappingAdapter{integrations: []codemappingruntime.Integration{integration}, repositories: []codemappingruntime.Repository{{Identity: "repository-1", Name: "acme/checkout", IntegrationID: "integration-1"}}}
}

func (a *controllerFakeCodeMappingAdapter) ListIntegrations(context.Context, string, string) ([]codemappingruntime.Integration, error) {
	return append([]codemappingruntime.Integration{}, a.integrations...), nil
}
func (a *controllerFakeCodeMappingAdapter) ListRepositories(context.Context, string, string) ([]codemappingruntime.Repository, error) {
	return append([]codemappingruntime.Repository{}, a.repositories...), nil
}
func (a *controllerFakeCodeMappingAdapter) ListMappings(context.Context, string) ([]codemappingruntime.Remote, error) {
	return append([]codemappingruntime.Remote{}, a.mappings...), nil
}
func (a *controllerFakeCodeMappingAdapter) CreateMapping(_ context.Context, _ string, request codemappingruntime.MappingRequest) (codemappingruntime.Remote, error) {
	value := codemappingruntime.Remote{Identity: "mapping-1", ProjectID: request.ProjectID, RepositoryID: request.RepositoryID, IntegrationID: request.IntegrationID, StackRoot: request.StackRoot, SourceRoot: request.SourceRoot, DefaultBranch: request.DefaultBranch}
	a.mappings = append(a.mappings, value)
	return value, nil
}
func (a *controllerFakeCodeMappingAdapter) UpdateMapping(_ context.Context, _ string, identity string, request codemappingruntime.MappingRequest) (codemappingruntime.Remote, error) {
	return codemappingruntime.Remote{Identity: identity, ProjectID: request.ProjectID, RepositoryID: request.RepositoryID, IntegrationID: request.IntegrationID, StackRoot: request.StackRoot, SourceRoot: request.SourceRoot, DefaultBranch: request.DefaultBranch}, nil
}
func (a *controllerFakeCodeMappingAdapter) DeleteMapping(_ context.Context, _ string, identity string) error {
	for index := range a.mappings {
		if a.mappings[index].Identity == identity {
			a.mappings = append(a.mappings[:index], a.mappings[index+1:]...)
			break
		}
	}
	return nil
}
func (a *controllerFakeCodeMappingAdapter) FetchCodeowners(context.Context, string, string) (codemappingruntime.CodeownersSource, error) {
	return codemappingruntime.CodeownersSource{Raw: "src/* @acme/backend"}, nil
}
func (a *controllerFakeCodeMappingAdapter) ListCodeowners(context.Context, string, string) ([]codemappingruntime.Codeowners, error) {
	return append([]codemappingruntime.Codeowners{}, a.codeowners...), nil
}
func (a *controllerFakeCodeMappingAdapter) CreateCodeowners(_ context.Context, _, _, mappingID, raw string) (codemappingruntime.Codeowners, error) {
	value := codemappingruntime.Codeowners{Identity: "codeowners-1", CodeMappingID: mappingID, Raw: raw}
	a.codeowners = append(a.codeowners, value)
	return value, nil
}
func (a *controllerFakeCodeMappingAdapter) UpdateCodeowners(_ context.Context, _, _, identity, mappingID, raw string) (codemappingruntime.Codeowners, error) {
	return codemappingruntime.Codeowners{Identity: identity, CodeMappingID: mappingID, Raw: raw}, nil
}
func (a *controllerFakeCodeMappingAdapter) DeleteCodeowners(_ context.Context, _, _, identity string) error {
	for index := range a.codeowners {
		if a.codeowners[index].Identity == identity {
			a.codeowners = append(a.codeowners[:index], a.codeowners[index+1:]...)
			break
		}
	}
	return nil
}
