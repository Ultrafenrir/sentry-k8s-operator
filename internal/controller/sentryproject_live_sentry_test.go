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
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	alertsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/alerts"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	clientkeyruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/clientkey"
	codemappingruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/codemapping"
	connectionruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/connection"
	issueownershipruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/issueownership"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/ownership"
	projectruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/project"
	projectaccessruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/projectaccess"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
	settingsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/settings"
	symbolsourceruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/symbolsource"
)

var _ = Describe("Live Sentry project recovery", Label("live-sentry"), func() {
	It("recovers a lost successful create response through the Kubernetes journal without a second POST", func() {
		baseURL := os.Getenv("SENTRY_LIVE_BASE_URL")
		tokenFile := os.Getenv("SENTRY_LIVE_TOKEN_FILE")
		organization := os.Getenv("SENTRY_LIVE_ORGANIZATION")
		team := os.Getenv("SENTRY_LIVE_TEAM")
		version := os.Getenv("SENTRY_LIVE_VERSION")
		if baseURL == "" || tokenFile == "" || organization == "" || team == "" || version == "" {
			Skip("live Sentry acceptance environment is not configured")
		}

		tokenBytes, err := os.ReadFile(tokenFile)
		Expect(err).NotTo(HaveOccurred())
		token := strings.TrimSpace(string(tokenBytes))
		Expect(token).To(MatchRegexp(`^sntryu_[0-9a-f]{64}$`))
		upstream, err := url.Parse(baseURL)
		Expect(err).NotTo(HaveOccurred())
		Expect(upstream.Scheme).To(Equal("http"))
		Expect(upstream.Hostname()).To(Equal("127.0.0.1"))

		slug := "sko-controller-recovery-" + strings.ReplaceAll(version, ".", "-")
		proxy := newLostCreateResponseProxy(upstream, slug)
		defer proxy.Close()
		defer deleteLiveProject(upstream, token, organization, slug)

		clientConfig := sentryapi.Config{
			BaseURL:       proxy.URL,
			Token:         token,
			CABundle:      certificatePEM(proxy.Certificate()),
			Timeout:       30 * time.Second,
			MaxConcurrent: 2,
			MaxRetries:    0,
			UserAgent:     "sentry-kubernetes-operator/live-acceptance",
		}
		apiClient, err := sentryapi.NewClient(clientConfig)
		Expect(err).NotTo(HaveOccurred())

		store := &binding.Store{Client: k8sClient, Namespace: "default"}
		auditSink := &liveAuditSink{}
		engine := &projectruntime.Engine{
			Bindings:  store,
			Adapter:   &projectruntime.HTTPAdapter{Client: apiClient},
			ManagedBy: "sko",
			Audit:     auditSink,
		}
		input := liveManagedProjectInput(proxy.URL, organization, team, slug, version)

		_, err = engine.Reconcile(context.Background(), input)
		Expect(projectruntime.IsReason(err, projectruntime.ReasonCreationOutcomeUnknown)).To(BeTrue())
		Expect(proxy.createCalls.Load()).To(Equal(int32(1)))
		pending, err := store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(pending.Binding.Pending).NotTo(BeNil())
		Expect(pending.Binding.Pending.Phase).To(Equal(string(sentryapi.OperationSubmitted)))

		restartedClient, err := sentryapi.NewClient(clientConfig)
		Expect(err).NotTo(HaveOccurred())
		restartedStore := &binding.Store{Client: k8sClient, Namespace: "default"}
		restartedEngine := &projectruntime.Engine{
			Bindings:  restartedStore,
			Adapter:   &projectruntime.HTTPAdapter{Client: restartedClient},
			ManagedBy: "sko",
			Audit:     auditSink,
		}
		result, err := restartedEngine.Reconcile(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Origin).To(Equal(binding.OriginManaged))
		Expect(result.AccessMode).To(Equal(binding.AccessManage))
		Expect(proxy.createCalls.Load()).To(Equal(int32(1)))

		recovered, err := restartedStore.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(recovered.Binding.ExternalIdentity).NotTo(BeEmpty())
		Expect(recovered.Binding.Pending).To(BeNil())
		Expect(auditSink.has(audit.OutcomeRecovered, audit.ReasonResultRecovered)).To(BeTrue())

		var deletion projectruntime.DeletionResult
		for attempt := 0; attempt < 30; attempt++ {
			deletion, err = restartedEngine.Delete(context.Background(), input)
			if err == nil && deletion.Complete {
				break
			}
			if err != nil && !projectruntime.IsReason(err, projectruntime.ReasonDeletionPending) {
				Fail("live project deletion failed: " + err.Error())
			}
			time.Sleep(time.Second)
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(deletion.Complete).To(BeTrue())
		_, err = restartedStore.GetBinding(context.Background(), string(input.Project.UID))
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
		_, err = restartedStore.GetClaim(context.Background(), proxy.URL, organization, slug)
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
	})

	It("fences stale regional writers after a live revision takeover", func() {
		baseURL, token, organization, team, version := liveSentryEnvironment()
		upstream, err := url.Parse(baseURL)
		Expect(err).NotTo(HaveOccurred())

		versionSlug := strings.ReplaceAll(version, ".", "-")
		slug := "sko-regional-takeover-" + versionSlug
		defer deleteLiveProject(upstream, token, organization, slug)

		apiClient, err := sentryapi.NewClient(sentryapi.Config{
			BaseURL:           baseURL,
			Token:             token,
			AllowInsecureHTTP: true,
			Timeout:           30 * time.Second,
			MaxConcurrent:     2,
			MaxRetries:        0,
			UserAgent:         "sentry-kubernetes-operator/live-regional-takeover",
		})
		Expect(err).NotTo(HaveOccurred())

		store := &binding.Store{Client: k8sClient, Namespace: "default"}
		auditSink := &liveAuditSink{}
		adapter := &projectruntime.HTTPAdapter{Client: apiClient}
		engine := &projectruntime.Engine{
			Bindings:  store,
			Adapter:   adapter,
			ManagedBy: "sko",
			Audit:     auditSink,
		}
		input := liveManagedProjectInput(baseURL, organization, team, slug, version)
		input.Project.UID = types.UID("live-regional-" + versionSlug)
		input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
		}

		_, err = engine.Reconcile(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		initial, err := store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(initial.Binding.RegionalOwnership).NotTo(BeNil())
		Expect(initial.Binding.RegionalOwnership.Region).To(Equal("eu-west-1"))
		Expect(initial.Binding.RegionalOwnership.Revision).To(Equal(int64(8)))
		assertLiveRegionalEvidence(adapter, organization, slug, *initial.Binding.RegionalOwnership)

		input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
			Region: "us-east-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 9,
		}
		takeoverStarted := time.Now()
		_, err = engine.Reconcile(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(time.Since(takeoverStarted)).To(BeNumerically(">=", 5*time.Second))

		takenOver, err := store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(takenOver.Binding.RegionalOwnership).NotTo(BeNil())
		Expect(takenOver.Binding.RegionalOwnership.Region).To(Equal("us-east-1"))
		Expect(takenOver.Binding.RegionalOwnership.Revision).To(Equal(int64(9)))
		assertLiveRegionalEvidence(adapter, organization, slug, *takenOver.Binding.RegionalOwnership)
		Expect(auditSink.has(audit.OutcomeStarted, audit.ReasonJournalSubmitted)).To(BeTrue())
		Expect(auditSink.has(audit.OutcomeSucceeded, audit.ReasonMutationVerified)).To(BeTrue())

		staleInput := input
		staleInput.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
		}
		_, err = engine.Reconcile(context.Background(), staleInput)
		Expect(projectruntime.IsReason(err, projectruntime.ReasonRevisionFenced)).To(BeTrue())
		assertLiveRegionalEvidence(adapter, organization, slug, *takenOver.Binding.RegionalOwnership)

		conflictingInput := input
		conflictingInput.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
			Region: "ap-southeast-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 9,
		}
		_, err = engine.Reconcile(context.Background(), conflictingInput)
		Expect(projectruntime.IsReason(err, projectruntime.ReasonRevisionConflict)).To(BeTrue())
		assertLiveRegionalEvidence(adapter, organization, slug, *takenOver.Binding.RegionalOwnership)

		var deletion projectruntime.DeletionResult
		for attempt := 0; attempt < 30; attempt++ {
			deletion, err = engine.Delete(context.Background(), input)
			if err == nil && deletion.Complete {
				break
			}
			if err != nil && !projectruntime.IsReason(err, projectruntime.ReasonDeletionPending) {
				Fail("live regional project deletion failed: " + err.Error())
			}
			time.Sleep(time.Second)
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(deletion.Complete).To(BeTrue())
		_, err = store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
		_, err = store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
	})

	It("observes, explicitly adopts, and retains an initially unmarked project", func() {
		baseURL, token, organization, team, version := liveSentryEnvironment()
		upstream, err := url.Parse(baseURL)
		Expect(err).NotTo(HaveOccurred())

		versionSlug := strings.ReplaceAll(version, ".", "-")
		slug := "sko-adopt-retain-" + versionSlug
		defer deleteLiveProject(upstream, token, organization, slug)

		apiClient, err := sentryapi.NewClient(sentryapi.Config{
			BaseURL:           baseURL,
			Token:             token,
			AllowInsecureHTTP: true,
			Timeout:           30 * time.Second,
			MaxConcurrent:     2,
			MaxRetries:        0,
			UserAgent:         "sentry-kubernetes-operator/live-adopt-retain",
		})
		Expect(err).NotTo(HaveOccurred())

		externalIdentity := createLiveUnmanagedProject(apiClient, organization, team, slug)
		adapter := &projectruntime.HTTPAdapter{Client: apiClient}
		store := &binding.Store{Client: k8sClient, Namespace: "default"}
		auditSink := &liveAuditSink{}
		engine := &projectruntime.Engine{
			Bindings: store, Adapter: adapter, ManagedBy: "sko", Audit: auditSink,
		}
		input := liveManagedProjectInput(baseURL, organization, team, slug, version)
		input.Project.UID = types.UID("live-adopt-retain-" + versionSlug)
		input.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteRetain
		input.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
		input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
		}
		input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsStandby

		result, err := engine.Reconcile(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Origin).To(Equal(binding.OriginExternal))
		Expect(result.AccessMode).To(Equal(binding.AccessReadOnly))
		observed, err := store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(observed.Binding.ExternalIdentity).To(Equal(externalIdentity))
		Expect(observed.Binding.ManagerUID).To(BeEmpty())
		Expect(observed.Binding.ClaimName).To(BeEmpty())
		_, err = store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())

		input.Project.Spec.Role = sentryv1alpha1.ProjectRoleActive
		_, err = engine.Reconcile(context.Background(), input)
		Expect(projectruntime.IsReason(err, projectruntime.ReasonProjectAlreadyExists)).To(BeTrue())
		_, err = store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())

		input.Project.Generation = 2
		input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsAdopt
		result, err = engine.Reconcile(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Origin).To(Equal(binding.OriginManaged))
		Expect(result.AccessMode).To(Equal(binding.AccessManage))

		record, err := store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(record.Binding.ExternalIdentity).To(Equal(externalIdentity))
		Expect(record.Binding.Pending).To(BeNil())
		Expect(record.Binding.RegionalOwnership).NotTo(BeNil())
		assertLiveRegionalEvidence(adapter, organization, slug, *record.Binding.RegionalOwnership)
		Expect(auditSink.has(audit.OutcomeStarted, audit.ReasonJournalSubmitted)).To(BeTrue())
		Expect(auditSink.has(audit.OutcomeSucceeded, audit.ReasonMutationVerified)).To(BeTrue())

		deletion, err := engine.Delete(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(deletion.Complete).To(BeTrue())
		Expect(deletion.Retained).To(BeTrue())
		_, err = store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
		_, err = store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())

		retained, err := adapter.GetBySlug(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(retained.Identity).To(Equal(externalIdentity))
		Expect(retained.Marker).To(BeNil())
		Expect(retained.RegionalEvidence).To(BeNil())
	})

	It("promotes a managed standby candidate only after an exact handoff", func() {
		baseURL, token, organization, team, version := liveSentryEnvironment()
		upstream, err := url.Parse(baseURL)
		Expect(err).NotTo(HaveOccurred())

		versionSlug := strings.ReplaceAll(version, ".", "-")
		slug := "sko-managed-handoff-" + versionSlug
		defer deleteLiveProject(upstream, token, organization, slug)

		apiClient, err := sentryapi.NewClient(sentryapi.Config{
			BaseURL:           baseURL,
			Token:             token,
			AllowInsecureHTTP: true,
			Timeout:           30 * time.Second,
			MaxConcurrent:     2,
			MaxRetries:        0,
			UserAgent:         "sentry-kubernetes-operator/live-managed-handoff",
		})
		Expect(err).NotTo(HaveOccurred())

		adapter := &projectruntime.HTTPAdapter{Client: apiClient}
		store := &binding.Store{Client: k8sClient, Namespace: "default"}
		engine := &projectruntime.Engine{
			Bindings: store, Adapter: adapter, ManagedBy: "sko", Audit: &liveAuditSink{},
		}
		manager := liveManagedProjectInput(baseURL, organization, team, slug, version)
		manager.Project.UID = types.UID("live-handoff-manager-" + versionSlug)
		manager.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
		}
		managerResult, err := engine.Reconcile(context.Background(), manager)
		Expect(err).NotTo(HaveOccurred())
		Expect(managerResult.Origin).To(Equal(binding.OriginManaged))
		Expect(managerResult.AccessMode).To(Equal(binding.AccessManage))
		managerRecord, err := store.GetBinding(context.Background(), string(manager.Project.UID))
		Expect(err).NotTo(HaveOccurred())

		candidate := liveManagedProjectInput(baseURL, organization, team, slug, version)
		candidate.Project.UID = types.UID("live-handoff-candidate-" + versionSlug)
		candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
		candidate.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteRetain
		candidate.Connection.Ownership = manager.Connection.Ownership.DeepCopy()
		candidateResult, err := engine.Reconcile(context.Background(), candidate)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidateResult.Origin).To(Equal(binding.OriginManaged))
		Expect(candidateResult.AccessMode).To(Equal(binding.AccessReadOnly))
		candidateRecord, err := store.GetBinding(context.Background(), string(candidate.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(candidateRecord.Binding.ExternalIdentity).To(Equal(managerRecord.Binding.ExternalIdentity))
		Expect(candidateRecord.Binding.ManagerUID).To(BeEmpty())
		Expect(candidateRecord.Binding.ClaimName).To(BeEmpty())

		candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleActive
		_, err = engine.Reconcile(context.Background(), candidate)
		Expect(projectruntime.IsReason(err, projectruntime.ReasonOwnershipConflict)).To(BeTrue())

		claim, err := store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(err).NotTo(HaveOccurred())
		candidate.Project.Generation = 2
		target := binding.ProjectReference{
			UID: string(candidate.Project.UID), Namespace: candidate.Project.Namespace,
			Name: candidate.Project.Name, Generation: candidate.Project.Generation,
		}
		_, err = store.Handoff(context.Background(), claim, string(manager.Project.UID), target)
		Expect(err).NotTo(HaveOccurred())

		candidateResult, err = engine.Reconcile(context.Background(), candidate)
		Expect(err).NotTo(HaveOccurred())
		Expect(candidateResult.Origin).To(Equal(binding.OriginManaged))
		Expect(candidateResult.AccessMode).To(Equal(binding.AccessManage))
		promoted, err := store.GetBinding(context.Background(), string(candidate.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(promoted.Binding.ManagerUID).To(Equal(string(candidate.Project.UID)))
		Expect(promoted.Binding.ExternalIdentity).To(Equal(managerRecord.Binding.ExternalIdentity))
		claim, err = store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Claim.Manager.UID).To(Equal(string(candidate.Project.UID)))
		Expect(claim.Claim.Handoff).To(BeNil())
		remoteBeforeRelease, err := adapter.GetBySlug(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(remoteBeforeRelease.Identity).To(Equal(managerRecord.Binding.ExternalIdentity))
		assertLiveRegionalEvidence(adapter, organization, slug, *promoted.Binding.RegionalOwnership)

		deletion, err := engine.Delete(context.Background(), candidate)
		Expect(err).NotTo(HaveOccurred())
		Expect(deletion.Complete).To(BeTrue())
		Expect(deletion.Retained).To(BeTrue())
		retained, err := adapter.GetBySlug(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(retained.Identity).To(Equal(managerRecord.Binding.ExternalIdentity))
		Expect(retained.Marker).To(BeNil())
		Expect(retained.RegionalEvidence).To(BeNil())
		Expect(store.DeleteBinding(context.Background(), managerRecord)).To(Succeed())
	})

	It("cleans a managed standby candidate's alert before releasing its binding", func() {
		baseURL, token, organization, team, version := liveSentryEnvironment()
		upstream, err := url.Parse(baseURL)
		Expect(err).NotTo(HaveOccurred())

		versionSlug := strings.ReplaceAll(version, ".", "-")
		slug := "sko-standby-alert-" + versionSlug
		connectionName := "live-standby-alert-" + versionSlug
		key := types.NamespacedName{Name: slug, Namespace: "default"}
		defer deleteLiveControllerFixtures(key, connectionName)
		defer deleteLiveProject(upstream, token, organization, slug)

		registry := connectionruntime.NewRegistry()
		apiClient, _, err := registry.Apply(connectionName, baseURL, connectionruntime.SecretInputs{Token: []byte(token), AllowInsecureHTTP: true})
		Expect(err).NotTo(HaveOccurred())
		store := &binding.Store{Client: k8sClient, Namespace: "default"}
		engine := &projectruntime.Engine{
			Bindings: store, Adapter: &projectruntime.HTTPAdapter{Client: apiClient}, ManagedBy: "sko", Audit: &liveAuditSink{},
		}
		manager := liveManagedProjectInput(baseURL, organization, team, slug, version)
		manager.Project.UID = types.UID("live-standby-alert-manager-" + versionSlug)
		manager.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteRetain
		manager.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
		}
		_, err = engine.Reconcile(context.Background(), manager)
		Expect(err).NotTo(HaveOccurred())
		managerRecord, err := store.GetBinding(context.Background(), string(manager.Project.UID))
		Expect(err).NotTo(HaveOccurred())

		alertProfile := sentryv1alpha1.AlertCompatibilityModern26
		if version == "24.7.1" {
			alertProfile = sentryv1alpha1.AlertCompatibilityLegacy24
		}
		connection := &sentryv1alpha1.SentryConnection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName},
			Spec: sentryv1alpha1.SentryConnectionSpec{
				URL:          baseURL,
				Organization: organization,
				Transport:    &sentryv1alpha1.ConnectionTransport{AllowInsecureHTTP: true},
				Credentials: sentryv1alpha1.ConnectionCredentials{TokenSecretRef: sentryv1alpha1.SecretKeyReference{
					Name: "live-token", Key: "token",
				}},
				Ownership: manager.Connection.Ownership.DeepCopy(),
				Integrations: &sentryv1alpha1.ConnectionIntegrations{PagerDuty: &sentryv1alpha1.PagerDutyIntegration{
					Accounts: []sentryv1alpha1.IntegrationCatalogEntry{{Name: "local-pagerduty", SentryName: "SKO Demo PagerDuty"}},
				}},
				Access: []sentryv1alpha1.AccessRule{{
					Owner: "live", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"default"}},
					Teams: []string{team}, AllowCreate: true,
					NotificationTargets: &sentryv1alpha1.NotificationTargetAccess{PagerDutyAccounts: []string{"local-pagerduty"}},
				}},
				Compatibility: &sentryv1alpha1.ConnectionCompatibility{Alerts: alertProfile},
			},
		}
		Expect(k8sClient.Create(context.Background(), connection)).To(Succeed())

		window := int32(10)
		threshold := int64(50)
		candidate := &sentryv1alpha1.SentryProject{
			ObjectMeta: metav1.ObjectMeta{Name: slug, Namespace: "default"},
			Spec: sentryv1alpha1.SentryProjectSpec{
				Sentry:  connectionName,
				Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: team}},
				Role:    sentryv1alpha1.ProjectRoleStandby,
				NotificationTargets: &sentryv1alpha1.NotificationTargets{PagerDuty: map[string][]string{
					"local-pagerduty": {"Demo On-call"},
				}},
				Alerts: &map[string]sentryv1alpha1.AlertSpec{
					"affected-users": {
						Trigger: sentryv1alpha1.AlertTriggerUserCount, WindowMinutes: &window,
						Threshold: &threshold, Notify: []string{"pagerDuty/Demo On-call"},
					},
				},
				Lifecycle: sentryv1alpha1.LifecycleSpec{OnDelete: sentryv1alpha1.OnDeleteRetain},
			},
		}
		Expect(k8sClient.Create(context.Background(), candidate)).To(Succeed())
		reconciler := &SentryProjectReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Registry: registry,
			Bindings: store, ManagedBy: "sko", Audit: &liveAuditSink{},
		}
		result, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Requeue).To(BeTrue())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(context.Background(), key, candidate)).To(Succeed())
		Expect(candidate.Status.Origin).To(Equal(string(binding.OriginManaged)))
		Expect(candidate.Status.AccessMode).To(Equal(binding.AccessReadOnly))
		Expect(candidate.Status.Alerts).To(HaveKeyWithValue("affected-users", sentryv1alpha1.AlertStatus{State: alertsruntime.StateReady, Reason: "AlertReady"}))
		candidateRecord, err := store.GetBinding(context.Background(), string(candidate.UID))
		Expect(err).NotTo(HaveOccurred())
		alertBinding := candidateRecord.Binding.Alerts["affected-users"]
		Expect(alertBinding.ExternalIdentity).NotTo(BeEmpty())
		Expect(candidateRecord.Binding.ManagerUID).To(BeEmpty())
		claim, err := store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Claim.Manager.UID).To(Equal(string(manager.Project.UID)))

		Expect(k8sClient.Delete(context.Background(), candidate)).To(Succeed())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(context.Background(), key, &sentryv1alpha1.SentryProject{}))
		}).Should(BeTrue())
		alertAdapter := &alertsruntime.MuxAdapter{Adapters: map[string]alertsruntime.Adapter{
			alertsruntime.LegacyCombinedAdapter:    &alertsruntime.LegacyRouterAdapter{Legacy: &alertsruntime.LegacyHTTPAdapter{Client: apiClient, Destinations: candidateRecord.Binding.Destinations}},
			alertsruntime.ModernMetricIssueAdapter: &alertsruntime.ModernHTTPAdapter{Client: apiClient, Destinations: candidateRecord.Binding.Destinations},
		}}
		_, found, err := alertAdapter.Observe(context.Background(), alertBinding.Adapter, organization, slug, alertBinding.ExternalIdentity)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())
		_, err = store.GetBinding(context.Background(), string(candidate.UID))
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
		claim, err = store.GetClaim(context.Background(), baseURL, organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(claim.Claim.Manager.UID).To(Equal(string(manager.Project.UID)))
		remote, err := (&projectruntime.HTTPAdapter{Client: apiClient}).GetBySlug(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(remote.Identity).To(Equal(managerRecord.Binding.ExternalIdentity))
		Expect(remote.RegionalEvidence).NotTo(BeNil())
		Expect(ownership.ValidateEvidence(ownership.Local{Marker: *managerRecord.Binding.RegionalOwnership, Active: true}, *remote.RegionalEvidence)).To(Succeed())

		deletion, err := engine.Delete(context.Background(), manager)
		Expect(err).NotTo(HaveOccurred())
		Expect(deletion.Complete).To(BeTrue())
		Expect(deletion.Retained).To(BeTrue())
	})

	It("repairs settings and issue ownership drift through the full controller and remains idempotent", func() {
		baseURL, token, organization, team, version := liveSentryEnvironment()
		upstream, err := url.Parse(baseURL)
		Expect(err).NotTo(HaveOccurred())

		versionSlug := strings.ReplaceAll(version, ".", "-")
		slug := "live-settings-" + versionSlug
		grantTeam := "sko-grant-" + versionSlug + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
		proxy := newLostCreateResponseProxy(upstream, slug)
		proxy.dropped.Store(true)
		defer proxy.Close()
		defer deleteLiveProject(upstream, token, organization, slug)

		connectionName := "live-settings-" + versionSlug
		resourceName := slug
		key := types.NamespacedName{Name: resourceName, Namespace: "default"}
		alertProfile := sentryv1alpha1.AlertCompatibilityModern26
		if version == "24.7.1" {
			alertProfile = sentryv1alpha1.AlertCompatibilityLegacy24
		}
		connection := &sentryv1alpha1.SentryConnection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName},
			Spec: sentryv1alpha1.SentryConnectionSpec{
				URL:          proxy.URL,
				Organization: organization,
				Transport:    &sentryv1alpha1.ConnectionTransport{AllowInsecureHTTP: true},
				Credentials: sentryv1alpha1.ConnectionCredentials{TokenSecretRef: sentryv1alpha1.SecretKeyReference{
					Name: "live-token", Key: "token",
				}},
				Integrations: &sentryv1alpha1.ConnectionIntegrations{
					PagerDuty: &sentryv1alpha1.PagerDutyIntegration{Accounts: []sentryv1alpha1.IntegrationCatalogEntry{{
						Name: "local-pagerduty", SentryName: "SKO Demo PagerDuty",
					}}},
					SourceCode: &sentryv1alpha1.SourceCodeIntegrations{Installations: []sentryv1alpha1.SourceCodeIntegrationCatalogEntry{{
						Name: "local-github", Provider: "github", SentryName: "SKO local source fixture", Repositories: []sentryv1alpha1.SourceCodeRepositoryCatalogEntry{{Name: "local-repository", SentryName: "sko/local-repository"}},
					}}},
					Telegram: &sentryv1alpha1.TelegramIntegration{
						BotTokenSecretRef: sentryv1alpha1.SecretKeyReference{Name: "unused-live-hook-token", Key: "token"},
						ReceiverURL:       "https://hooks.example.invalid/events", ServiceHookRevision: 1,
					},
				},
				Access: []sentryv1alpha1.AccessRule{{
					Owner: "live", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"default"}},
					Teams: []string{team, grantTeam}, AllowCreate: true,
					NotificationTargets: &sentryv1alpha1.NotificationTargetAccess{PagerDutyAccounts: []string{"local-pagerduty"}},
				}},
				Compatibility: &sentryv1alpha1.ConnectionCompatibility{Alerts: alertProfile},
			},
		}
		Expect(k8sClient.Create(context.Background(), connection)).To(Succeed())
		defer deleteLiveControllerFixtures(key, connectionName)
		connection.Status.Capabilities = []string{
			projectruntime.CapabilityAtomicDefaultRules,
			projectruntime.CapabilityAutomaticActiveKey,
		}
		Expect(k8sClient.Status().Update(context.Background(), connection)).To(Succeed())

		trueValue := true
		falseValue := false
		desiredResolve := int64(72)
		desiredEnvironment := "production"
		desiredCrashLimit := int32(10)
		desiredAllowedDomains := []string{"*.checkout.example.com", "https://checkout.example.com:443"}
		desiredLegacy := []sentryv1alpha1.LegacyBrowserFilter{sentryv1alpha1.LegacyBrowserIE, sentryv1alpha1.LegacyBrowserChrome}
		desiredSensitive := []string{"authorization", "password"}
		desiredSafe := []string{"public-control"}
		desiredAdvanced := []sentryv1alpha1.AdvancedPIIRule{
			{Type: "email", Redaction: "remove", Applications: []sentryv1alpha1.AdvancedPIISelector{{Root: "message"}, {Root: "extra", Path: []string{"customer", "email"}}}},
			{Type: "anything", Redaction: "replace", Replacement: "[Filtered]", Applications: []sentryv1alpha1.AdvancedPIISelector{{Root: "request", Path: []string{"headers", "Authorization"}}}},
		}
		desiredEnhancements := []sentryv1alpha1.GroupingEnhancementRule{
			{Matcher: "path", Pattern: "*/node_modules/*", Action: "MarkOutOfApp"},
			{Matcher: "function", Pattern: "vendor_*", Negate: true, Action: "Include"},
		}
		desiredFingerprints := []sentryv1alpha1.FingerprintRule{
			{Matcher: "error.type", Pattern: "DatabaseUnavailable", Values: []string{"database-unavailable"}},
			{Matcher: "stack.function", Pattern: "query_*", Values: []string{"database"}, IncludeDefault: true},
		}
		desiredRelayPII := `{"rules":{"0":{"type":"email","redaction":{"method":"remove"}},"1":{"type":"anything","redaction":{"method":"replace","text":"[Filtered]"}}},"applications":{"$message":["0"],"extra.customer.email":["0"],"request.headers.Authorization":["1"]}}`
		desiredEnhancementText := "stack.abs_path:\"*/node_modules/*\" -app\n!stack.function:\"vendor_*\" +group"
		desiredFingerprintText := "error.type:\"DatabaseUnavailable\" -> \"database-unavailable\"\nstack.function:\"query_*\" -> \"database\", {{ default }}"
		desiredBuiltinSources := []sentryv1alpha1.BuiltinSymbolSource{"electron", "microsoft", "nuget"}
		desiredBuiltinSourceNames := []string{"electron", "microsoft", "nuget"}
		alertWindow := int32(10)
		alertThreshold := int64(50)
		symbolSecretName := resourceName + "-symbol-credentials"
		symbolSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: symbolSecretName, Namespace: "default"},
			Data: map[string][]byte{
				"http-password": []byte("sko-live-http-secret-one"),
				"s3-access":     []byte("sko-live-s3-access"),
				"s3-secret":     []byte("sko-live-s3-secret"),
				"gcs-private":   []byte("sko-live-gcs-private"),
			},
		}
		Expect(k8sClient.Create(context.Background(), symbolSecret)).To(Succeed())
		defer func() {
			if err := k8sClient.Delete(context.Background(), symbolSecret); err != nil && !apierrors.IsNotFound(err) {
				Fail("symbol credential Secret cleanup failed: " + err.Error())
			}
		}()
		resource := &sentryv1alpha1.SentryProject{
			ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: "default"},
			Spec: sentryv1alpha1.SentryProjectSpec{
				Sentry: connectionName,
				Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{
					Team: team,
				}},
				ClientKeys: &map[string]sentryv1alpha1.ManagedClientKeySpec{
					"ingest": {Active: true, RotationRevision: 1, RateLimit: &sentryv1alpha1.ClientKeyRateLimit{Count: 1000, WindowSeconds: 60}},
				},
				DSN: &sentryv1alpha1.DSNSpec{ManagedClientKey: "ingest"},
				NotificationTargets: &sentryv1alpha1.NotificationTargets{PagerDuty: map[string][]string{
					"local-pagerduty": {"Demo On-call"},
				}},
				Alerts: &map[string]sentryv1alpha1.AlertSpec{
					"affected-users": {
						Trigger: sentryv1alpha1.AlertTriggerUserCount, WindowMinutes: &alertWindow,
						Threshold: &alertThreshold, Enabled: &trueValue, Notify: []string{"pagerDuty/Demo On-call"},
					},
				},
				SymbolSources: &map[string]sentryv1alpha1.CustomSymbolSourceSpec{
					"http-symbols": {
						Layout: sentryv1alpha1.SymbolSourceLayout{Type: "native", Casing: "default"},
						HTTP: &sentryv1alpha1.HTTPSymbolSource{
							URL: "https://symbols.example.invalid", Username: "reader",
							PasswordSecretRef: &sentryv1alpha1.SecretKeyReference{Name: symbolSecretName, Key: "http-password"},
						},
					},
					"s3-symbols": {
						Layout: sentryv1alpha1.SymbolSourceLayout{Type: "symstore", Casing: "lowercase"},
						S3: &sentryv1alpha1.S3SymbolSource{
							Bucket: "sko-live-symbols", Region: "eu-west-1", Prefix: "native/",
							AccessKeySecretRef: sentryv1alpha1.SecretKeyReference{Name: symbolSecretName, Key: "s3-access"},
							SecretKeySecretRef: sentryv1alpha1.SecretKeyReference{Name: symbolSecretName, Key: "s3-secret"},
						},
					},
					"gcs-symbols": {
						Layout: sentryv1alpha1.SymbolSourceLayout{Type: "unified", Casing: "uppercase"},
						GCS: &sentryv1alpha1.GCSSymbolSource{
							Bucket: "sko-live-symbols", ClientEmail: "symbols@example.invalid", Prefix: "debug/",
							PrivateKeySecretRef: sentryv1alpha1.SecretKeyReference{Name: symbolSecretName, Key: "gcs-private"},
						},
					},
				},
				CodeMappings: &map[string]sentryv1alpha1.CodeMappingSpec{
					"checkout": {Integration: "local-github", Repository: "local-repository", StackRoot: "/", SourceRoot: "src", DefaultBranch: "main"},
				},
				Settings: &sentryv1alpha1.ProjectSettings{
					Filters: &sentryv1alpha1.FilterSettings{
						BrowserExtensions: &trueValue, Localhost: &trueValue, WebCrawlers: &trueValue,
						FilteredTransactions: &trueValue, LegacyBrowsers: &desiredLegacy,
					},
					PII: &sentryv1alpha1.PIISettings{
						ScrubData: &trueValue, ScrubDefaults: &trueValue, ScrubIPAddresses: &trueValue,
						SensitiveFields: &desiredSensitive, SafeFields: &desiredSafe, AdvancedRules: &desiredAdvanced,
					},
					Behavior: &sentryv1alpha1.BehaviorSettings{
						AutoResolveHours: &desiredResolve, DefaultEnvironment: &desiredEnvironment,
						CrashReportLimit: &desiredCrashLimit, ScrapeJavaScript: &falseValue,
						AllowedDomains: &desiredAllowedDomains,
					},
					Grouping:      &sentryv1alpha1.GroupingSettings{Enhancements: &desiredEnhancements, Fingerprints: &desiredFingerprints},
					Symbolication: &sentryv1alpha1.SymbolicationSettings{BuiltinSources: &desiredBuiltinSources},
				},
				IssueOwnership: &sentryv1alpha1.IssueOwnershipSpec{
					Rules: []sentryv1alpha1.IssueOwnershipRule{
						{Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/checkout/*"}, Teams: []string{team}},
						{Matcher: sentryv1alpha1.IssueOwnershipMatcher{Type: sentryv1alpha1.IssueOwnershipMatcherTag, TagKey: "environment", Pattern: "production"}, Teams: []string{team}},
					},
					Fallthrough: sentryv1alpha1.IssueOwnershipFallthroughNoOne, AutoAssignment: sentryv1alpha1.IssueOwnershipAutoAssignIssueOwner,
					CodeownersAutoSync: false,
				},
				GrantAccess: &sentryv1alpha1.ProjectGrantAccess{Teams: []string{grantTeam}},
				Lifecycle:   sentryv1alpha1.LifecycleSpec{OnDelete: sentryv1alpha1.OnDeleteDeleteProject},
			},
		}
		Expect(k8sClient.Create(context.Background(), resource)).To(Succeed())

		registry := connectionruntime.NewRegistry()
		apiClient, _, err := registry.Apply(connectionName, proxy.URL, connectionruntime.SecretInputs{
			Token: []byte(token), CABundle: certificatePEM(proxy.Certificate()),
		})
		Expect(err).NotTo(HaveOccurred())
		createLiveTeam(apiClient, organization, grantTeam)
		defer deleteLiveTeam(apiClient, organization, grantTeam)
		auditSink := &liveAuditSink{}
		verifierSecretName := "live-hook-verifier-" + versionSlug
		verifierSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: verifierSecretName, Namespace: "default"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
		Expect(k8sClient.Create(context.Background(), verifierSecret)).To(Succeed())
		defer func() {
			if err := k8sClient.Delete(context.Background(), verifierSecret); err != nil && !apierrors.IsNotFound(err) {
				Fail("hook verifier Secret cleanup failed: " + err.Error())
			}
		}()
		reconciler := &SentryProjectReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Registry: registry,
			Bindings: &binding.Store{Client: k8sClient, Namespace: "default"}, ManagedBy: "sko",
			Audit:        auditSink,
			HookSources:  &sentryhook.SourceStore{Client: k8sClient, Namespace: "default"},
			HookVerifier: &sentryhook.VerifierStore{Client: k8sClient, Namespace: "default", SecretName: verifierSecretName},
		}

		result, err := reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Requeue).To(BeTrue())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		accessAdapter := &projectaccessruntime.HTTPAdapter{Client: apiClient}
		teams, err := accessAdapter.ListTeams(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(liveTeamSlugs(teams)).To(And(ContainElement(team), ContainElement(grantTeam)))
		Expect(k8sClient.Get(context.Background(), key, resource)).To(Succeed())
		Expect(meta.FindStatusCondition(resource.Status.Conditions, "GrantAccessReady")).To(And(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "GrantAccessReady"),
		))
		Expect(meta.FindStatusCondition(resource.Status.Conditions, "ClientKeysReady")).To(And(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "ClientKeysReady"),
		))
		Expect(resource.Status.DSNClientKey).To(Equal("ingest"))
		Expect(meta.FindStatusCondition(resource.Status.Conditions, "AlertsReady")).To(And(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "AlertsReady"),
		))
		Expect(resource.Status.Alerts).To(HaveKeyWithValue("affected-users", sentryv1alpha1.AlertStatus{State: alertsruntime.StateReady, Reason: "AlertReady"}))
		Expect(meta.FindStatusCondition(resource.Status.Conditions, "SymbolSourcesReady")).To(And(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "SymbolSourcesReady"),
		))
		Expect(resource.Status.SymbolSources).To(HaveLen(3))
		codeMappingsCondition := meta.FindStatusCondition(resource.Status.Conditions, "CodeMappingsReady")
		Expect(codeMappingsCondition).NotTo(BeNil())
		Expect(codeMappingsCondition).To(And(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "CodeMappingsReady"),
		), codeMappingsCondition.Reason+": "+codeMappingsCondition.Message)
		Expect(resource.Status.CodeMappings).To(HaveKeyWithValue("checkout", sentryv1alpha1.CodeMappingStatus{State: codemappingruntime.StateReady, Reason: "CodeMappingReady"}))
		hookDeliveries := []sentryhook.TelegramDelivery{{
			RouteKey: strings.Repeat("a", 64),
			Render:   sentryhook.RenderInput{ProjectName: resourceName, AlertName: "live-hook", Severity: sentryhook.SeverityCritical},
		}}
		hookRecord, err := reconciler.Bindings.GetBinding(context.Background(), string(resource.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(reconciler.ensureServiceHook(context.Background(), resource, connection, apiClient, hookRecord, connection.Spec.Integrations.Telegram.ReceiverURL, hookDeliveries)).To(Succeed())
		hookRecord, err = reconciler.Bindings.GetBinding(context.Background(), string(resource.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(hookRecord.Binding.ServiceHook).NotTo(BeNil())
		firstHookIdentity := hookRecord.Binding.ServiceHook.ExternalIdentity
		Expect(hookRecord.Binding.ServiceHook.EndpointURL).To(HavePrefix(connection.Spec.Integrations.Telegram.ReceiverURL + "/"))
		hookAdapter := &sentryhook.HTTPServiceHookAdapter{Client: apiClient}
		hooks, err := hookAdapter.List(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(hooks).To(ConsistOf(And(
			HaveField("Identity", firstHookIdentity), HaveField("URL", hookRecord.Binding.ServiceHook.EndpointURL), HaveField("Active", true),
		)))
		hookBindingJSON, err := json.Marshal(hookRecord.Binding.ServiceHook)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(hookBindingJSON)).NotTo(ContainSubstring(string(hooks[0].Secret)))

		connection.Spec.Integrations.Telegram.ServiceHookRevision = 2
		Expect(k8sClient.Update(context.Background(), connection)).To(Succeed())
		Expect(reconciler.ensureServiceHook(context.Background(), resource, connection, apiClient, hookRecord, connection.Spec.Integrations.Telegram.ReceiverURL, hookDeliveries)).To(Succeed())
		hookRecord, err = reconciler.Bindings.GetBinding(context.Background(), string(resource.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(hookRecord.Binding.ServiceHook).NotTo(BeNil())
		Expect(hookRecord.Binding.ServiceHook.Revision).To(Equal(int64(2)))
		Expect(hookRecord.Binding.ServiceHook.ExternalIdentity).NotTo(Equal(firstHookIdentity))
		hooks, err = hookAdapter.List(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(hooks).To(ConsistOf(HaveField("Identity", hookRecord.Binding.ServiceHook.ExternalIdentity)))

		Expect(reconciler.removeServiceHook(context.Background(), resource, connection, apiClient, hookRecord)).To(Succeed())
		hookRecord, err = reconciler.Bindings.GetBinding(context.Background(), string(resource.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(hookRecord.Binding.ServiceHook).To(BeNil())
		hooks, err = hookAdapter.List(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(hooks).To(BeEmpty())
		clientKeyAdapter := &clientkeyruntime.HTTPAdapter{Client: apiClient}
		assertLiveManagedClientKeys(clientKeyAdapter, organization, slug, ":r1", 1)
		publishedDSN := &corev1.Secret{}
		Expect(k8sClient.Get(context.Background(), types.NamespacedName{Name: resourceName + sentryv1alpha1.DefaultDSNSecretSuffix, Namespace: "default"}, publishedDSN)).To(Succeed())
		Expect(publishedDSN.Data).To(HaveKey(sentryv1alpha1.DefaultDSNSecretKey))
		issueOwnershipAdapter := &issueownershipruntime.HTTPAdapter{Client: apiClient}
		desiredOwnershipRaw := "path:src/checkout/* #" + team + "\ntags.environment:production #" + team
		assertLiveIssueOwnership(issueOwnershipAdapter, organization, slug, desiredOwnershipRaw, false, "Auto Assign to Issue Owner", false)
		symbolSourceAdapter := &symbolsourceruntime.HTTPAdapter{Client: apiClient}
		assertLiveCustomSymbolSources(symbolSourceAdapter, organization, slug, "https://symbols.example.invalid")
		codeMappingAdapter := &codemappingruntime.HTTPAdapter{Client: apiClient}
		assertLiveCodeMapping(codeMappingAdapter, organization, slug, "/", "src", "main")
		symbolBindingBefore, err := reconciler.Bindings.GetBinding(context.Background(), string(resource.UID))
		Expect(err).NotTo(HaveOccurred())
		httpHashBefore := symbolBindingBefore.Binding.SymbolSources["http-symbols"].DesiredHash
		bindingJSON, err := json.Marshal(symbolBindingBefore.Binding.SymbolSources)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(bindingJSON)).NotTo(ContainSubstring("sko-live-http-secret"))
		Expect(string(bindingJSON)).NotTo(ContainSubstring("sko-live-s3"))
		Expect(string(bindingJSON)).NotTo(ContainSubstring("sko-live-gcs"))

		resource.Spec.GrantAccess = &sentryv1alpha1.ProjectGrantAccess{}
		Expect(k8sClient.Update(context.Background(), resource)).To(Succeed())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		teams, err = accessAdapter.ListTeams(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		Expect(liveTeamSlugs(teams)).To(ContainElement(team))
		Expect(liveTeamSlugs(teams)).NotTo(ContainElement(grantTeam))

		adapter := &settingsruntime.HTTPAdapter{Client: apiClient}
		assertLiveSettings(adapter, organization, slug, trueValue, desiredLegacy, desiredSensitive, desiredSafe, desiredResolve, desiredEnvironment, desiredCrashLimit, falseValue, desiredAllowedDomains, desiredRelayPII, desiredEnhancementText, desiredFingerprintText, desiredBuiltinSourceNames)
		for _, filterID := range []string{"browser-extensions", "localhost", "web-crawlers", "filtered-transaction"} {
			Expect(adapter.UpdateFilter(context.Background(), organization, slug, filterID, settingsruntime.FilterPatch{Active: &falseValue})).To(Succeed())
		}
		emptyLegacy := []string{}
		Expect(adapter.UpdateFilter(context.Background(), organization, slug, "legacy-browsers", settingsruntime.FilterPatch{Subfilters: &emptyLegacy})).To(Succeed())
		state, err := adapter.GetProjectState(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		driftSensitive := []string{"drifted"}
		driftSafe := []string{"drift-control"}
		Expect(adapter.UpdateProjectState(context.Background(), organization, slug, settingsruntime.ProjectPatch{
			ScrubData: &falseValue, ScrubDefaults: &falseValue, ScrubIPAddresses: &falseValue,
			SensitiveFields: &driftSensitive, SafeFields: &driftSafe,
		}, state.ETag)).To(Succeed())
		state, err = adapter.GetProjectState(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		driftResolve := int64(24)
		driftEnvironment := "staging"
		driftCrashLimit := int32(0)
		driftAllowedDomains := []string{"drifted.example.com"}
		Expect(adapter.UpdateProjectState(context.Background(), organization, slug, settingsruntime.ProjectPatch{
			ResolveAge: &driftResolve, DefaultEnvironment: &driftEnvironment,
			StoreCrashReports: &driftCrashLimit, ScrapeJavaScript: &trueValue,
			AllowedDomains: &driftAllowedDomains,
		}, state.ETag)).To(Succeed())
		state, err = adapter.GetProjectState(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		driftRelayPII := `{"rules":{},"applications":{}}`
		driftEnhancements := `stack.module:"drifted.*" -group`
		driftFingerprints := `message:"drifted *" -> "drifted"`
		driftBuiltinSources := []string{"microsoft"}
		Expect(adapter.UpdateProjectState(context.Background(), organization, slug, settingsruntime.ProjectPatch{
			RelayPIIConfig: &driftRelayPII, GroupingEnhancements: &driftEnhancements, FingerprintingRules: &driftFingerprints,
			BuiltinSymbolSources: &driftBuiltinSources,
		}, state.ETag)).To(Succeed())
		Expect(issueOwnershipAdapter.Update(context.Background(), organization, slug, issueownershipruntime.Patch{
			Raw: "module:drifted.* #" + team, Fallthrough: true,
			AutoAssignment: "Turn off Auto-Assignment", CodeownersAutoSync: true,
		})).To(Succeed())
		mappings, err := codeMappingAdapter.ListMappings(context.Background(), organization)
		Expect(err).NotTo(HaveOccurred())
		var ownedMapping codemappingruntime.Remote
		for _, mapping := range mappings {
			if mapping.ProjectSlug == slug {
				ownedMapping = mapping
				break
			}
		}
		Expect(ownedMapping.Identity).NotTo(BeEmpty())
		_, err = codeMappingAdapter.UpdateMapping(context.Background(), organization, ownedMapping.Identity, codemappingruntime.MappingRequest{IntegrationID: ownedMapping.IntegrationID, RepositoryID: ownedMapping.RepositoryID, ProjectID: ownedMapping.ProjectID, StackRoot: "/drifted", SourceRoot: ownedMapping.SourceRoot, DefaultBranch: ownedMapping.DefaultBranch})
		Expect(err).NotTo(HaveOccurred())
		customSources, err := symbolSourceAdapter.List(context.Background(), organization, slug)
		Expect(err).NotTo(HaveOccurred())
		var httpSource symbolsourceruntime.Remote
		for _, source := range customSources {
			if source.Type == "http" {
				httpSource = source
				break
			}
		}
		Expect(httpSource.Identity).NotTo(BeEmpty())
		_, err = symbolSourceAdapter.Update(context.Background(), organization, slug, httpSource.Identity, symbolsourceruntime.Request{
			Identity: httpSource.Identity, Name: httpSource.Name, Type: "http", Layout: httpSource.Layout,
			URL: "https://drifted-symbols.example.invalid", Username: "reader", Password: "external-drift-secret",
		})
		Expect(err).NotTo(HaveOccurred())
		symbolSecret.Data["http-password"] = []byte("sko-live-http-secret-two")
		Expect(k8sClient.Update(context.Background(), symbolSecret)).To(Succeed())
		filterUpdatesBefore := proxy.filterUpdateCalls.Load()
		projectUpdatesBefore := proxy.projectUpdateCalls.Load()
		ownershipUpdatesBefore := proxy.ownershipUpdateCalls.Load()
		symbolSourceUpdatesBefore := proxy.symbolSourceMutationCalls.Load()

		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(proxy.filterUpdateCalls.Load() - filterUpdatesBefore).To(Equal(int32(5)))
		Expect(proxy.projectUpdateCalls.Load() - projectUpdatesBefore).To(Equal(int32(4)))
		Expect(proxy.ownershipUpdateCalls.Load() - ownershipUpdatesBefore).To(Equal(int32(1)))
		Expect(proxy.symbolSourceMutationCalls.Load() - symbolSourceUpdatesBefore).To(Equal(int32(1)))
		assertLiveSettings(adapter, organization, slug, trueValue, desiredLegacy, desiredSensitive, desiredSafe, desiredResolve, desiredEnvironment, desiredCrashLimit, falseValue, desiredAllowedDomains, desiredRelayPII, desiredEnhancementText, desiredFingerprintText, desiredBuiltinSourceNames)
		assertLiveIssueOwnership(issueOwnershipAdapter, organization, slug, desiredOwnershipRaw, false, "Auto Assign to Issue Owner", false)
		assertLiveCustomSymbolSources(symbolSourceAdapter, organization, slug, "https://symbols.example.invalid")
		assertLiveCodeMapping(codeMappingAdapter, organization, slug, "/", "src", "main")
		symbolBindingAfter, err := reconciler.Bindings.GetBinding(context.Background(), string(resource.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(symbolBindingAfter.Binding.SymbolSources["http-symbols"].DesiredHash).NotTo(Equal(httpHashBefore))

		Expect(k8sClient.Get(context.Background(), key, resource)).To(Succeed())
		Expect(meta.FindStatusCondition(resource.Status.Conditions, "SettingsReady")).To(And(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "SettingsReady"),
		))
		Expect(meta.FindStatusCondition(resource.Status.Conditions, "IssueOwnershipReady")).To(And(
			HaveField("Status", metav1.ConditionTrue), HaveField("Reason", "IssueOwnershipReady"),
		))
		Expect(meta.FindStatusCondition(resource.Status.Conditions, "Ready")).To(HaveField("Status", metav1.ConditionTrue))

		filterUpdatesBefore = proxy.filterUpdateCalls.Load()
		projectUpdatesBefore = proxy.projectUpdateCalls.Load()
		ownershipUpdatesBefore = proxy.ownershipUpdateCalls.Load()
		symbolSourceUpdatesBefore = proxy.symbolSourceMutationCalls.Load()
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(proxy.filterUpdateCalls.Load()).To(Equal(filterUpdatesBefore))
		Expect(proxy.projectUpdateCalls.Load()).To(Equal(projectUpdatesBefore))
		Expect(proxy.ownershipUpdateCalls.Load()).To(Equal(ownershipUpdatesBefore))
		Expect(proxy.symbolSourceMutationCalls.Load()).To(Equal(symbolSourceUpdatesBefore))

		Expect(k8sClient.Get(context.Background(), key, resource)).To(Succeed())
		rotatingKeys := *resource.Spec.ClientKeys
		rotating := rotatingKeys["ingest"]
		rotating.RotationRevision = 2
		rotatingKeys["ingest"] = rotating
		resource.Spec.ClientKeys = &rotatingKeys
		Expect(k8sClient.Update(context.Background(), resource)).To(Succeed())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(context.Background(), key, resource)).To(Succeed())
		Expect(resource.Status.ClientKeys["ingest"].State).To(Equal(clientkeyruntime.StateRotationPending))
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Expect(k8sClient.Get(context.Background(), key, resource)).To(Succeed())
		Expect(resource.Status.ClientKeys["ingest"]).To(Equal(sentryv1alpha1.ClientKeyStatus{State: clientkeyruntime.StateReady, Reason: "ClientKeyReady", RotationRevision: 2}))
		assertLiveManagedClientKeys(clientKeyAdapter, organization, slug, ":r2", 1)
		statusJSON, err := json.Marshal(resource.Status)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(statusJSON)).NotTo(ContainSubstring("sko:ingest:"))
		Expect(string(statusJSON)).NotTo(ContainSubstring("dsn.public"))

		emptyClientKeys := map[string]sentryv1alpha1.ManagedClientKeySpec{}
		emptySymbolSources := map[string]sentryv1alpha1.CustomSymbolSourceSpec{}
		emptyCodeMappings := map[string]sentryv1alpha1.CodeMappingSpec{}
		resource.Spec.ClientKeys = &emptyClientKeys
		resource.Spec.SymbolSources = &emptySymbolSources
		resource.Spec.CodeMappings = &emptyCodeMappings
		resource.Spec.DSN = nil
		Expect(k8sClient.Update(context.Background(), resource)).To(Succeed())
		_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		assertLiveManagedClientKeys(clientKeyAdapter, organization, slug, "", 0)
		assertLiveCustomSymbolSourcesEmpty(symbolSourceAdapter, organization, slug)
		assertLiveCodeMappingEmpty(codeMappingAdapter, organization, slug)

		Expect(k8sClient.Delete(context.Background(), resource)).To(Succeed())
		for attempt := 0; attempt < 30; attempt++ {
			_, err = reconciler.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			if apierrors.IsNotFound(k8sClient.Get(context.Background(), key, &sentryv1alpha1.SentryProject{})) {
				break
			}
			time.Sleep(time.Second)
		}
		Expect(apierrors.IsNotFound(k8sClient.Get(context.Background(), key, &sentryv1alpha1.SentryProject{}))).To(BeTrue())
	})
})

type lostCreateResponseProxy struct {
	*httptest.Server
	upstream                  *url.URL
	slug                      string
	createCalls               atomic.Int32
	dropped                   atomic.Bool
	filterUpdateCalls         atomic.Int32
	projectUpdateCalls        atomic.Int32
	ownershipUpdateCalls      atomic.Int32
	symbolSourceMutationCalls atomic.Int32
}

func newLostCreateResponseProxy(upstream *url.URL, slug string) *lostCreateResponseProxy {
	proxy := &lostCreateResponseProxy{upstream: upstream, slug: slug}
	proxy.Server = httptest.NewTLSServer(http.HandlerFunc(proxy.serveHTTP))
	return proxy
}

func (p *lostCreateResponseProxy) serveHTTP(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(w, "request read failed", http.StatusBadGateway)
		return
	}
	target := *p.upstream
	target.Path = request.URL.Path
	target.RawQuery = request.URL.RawQuery
	forward, err := http.NewRequestWithContext(request.Context(), request.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "request build failed", http.StatusBadGateway)
		return
	}
	forward.Header = request.Header.Clone()
	forward.Host = p.upstream.Host
	response, err := http.DefaultClient.Do(forward)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 && request.Method != http.MethodGet && strings.HasSuffix(request.URL.Path, "/symbol-sources/") {
		p.symbolSourceMutationCalls.Add(1)
	} else if response.StatusCode >= 200 && response.StatusCode < 300 && request.Method == http.MethodPut {
		if strings.Contains(request.URL.Path, "/filters/") {
			p.filterUpdateCalls.Add(1)
		} else if strings.HasSuffix(request.URL.Path, "/ownership/") {
			p.ownershipUpdateCalls.Add(1)
		} else if strings.Contains(request.URL.Path, "/projects/") {
			p.projectUpdateCalls.Add(1)
		}
	}

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/projects/") && bodyHasSlug(body, p.slug) {
		p.createCalls.Add(1)
		if response.StatusCode >= 200 && response.StatusCode < 300 && p.dropped.CompareAndSwap(false, true) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "response hijacking unavailable", http.StatusInternalServerError)
				return
			}
			connection, _, hijackErr := hijacker.Hijack()
			if hijackErr == nil {
				_ = connection.Close()
			}
			return
		}
	}
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func bodyHasSlug(body []byte, slug string) bool {
	var payload struct {
		Slug string `json:"slug"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Slug == slug
}

func certificatePEM(certificate *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

func liveSentryEnvironment() (string, string, string, string, string) {
	baseURL := os.Getenv("SENTRY_LIVE_BASE_URL")
	tokenFile := os.Getenv("SENTRY_LIVE_TOKEN_FILE")
	organization := os.Getenv("SENTRY_LIVE_ORGANIZATION")
	team := os.Getenv("SENTRY_LIVE_TEAM")
	version := os.Getenv("SENTRY_LIVE_VERSION")
	if baseURL == "" || tokenFile == "" || organization == "" || team == "" || version == "" {
		Skip("live Sentry acceptance environment is not configured")
	}
	tokenBytes, err := os.ReadFile(tokenFile)
	Expect(err).NotTo(HaveOccurred())
	token := strings.TrimSpace(string(tokenBytes))
	Expect(token).To(MatchRegexp(`^sntryu_[0-9a-f]{64}$`))
	return baseURL, token, organization, team, version
}

func assertLiveRegionalEvidence(adapter *projectruntime.HTTPAdapter, organization, slug string, marker ownership.Marker) {
	remote, err := adapter.GetBySlug(context.Background(), organization, slug)
	Expect(err).NotTo(HaveOccurred())
	Expect(remote.RegionalEvidence).NotTo(BeNil())
	Expect(ownership.ValidateEvidence(ownership.Local{Marker: marker, Active: true}, *remote.RegionalEvidence)).To(Succeed())
}

func assertLiveSettings(adapter *settingsruntime.HTTPAdapter, organization, slug string, active bool, legacy []sentryv1alpha1.LegacyBrowserFilter, sensitive, safe []string, resolve int64, environment string, crashLimit int32, scrape bool, allowedDomains []string, relayPII, enhancements, fingerprints string, builtinSources []string) {
	filters, err := adapter.ListFilters(context.Background(), organization, slug)
	Expect(err).NotTo(HaveOccurred())
	observed := make(map[string]bool, len(filters))
	for _, filter := range filters {
		observed[filter.ID] = filter.Active
	}
	for _, filterID := range []string{"browser-extensions", "localhost", "web-crawlers", "filtered-transaction"} {
		Expect(observed).To(HaveKeyWithValue(filterID, active))
	}
	legacyFilter := settingsruntime.Filter{}
	found := false
	for _, filter := range filters {
		if filter.ID == "legacy-browsers" {
			legacyFilter = filter
			found = true
			break
		}
	}
	Expect(found).To(BeTrue())
	expectedLegacy := make([]string, len(legacy))
	for index, value := range legacy {
		expectedLegacy[index] = string(value)
	}
	Expect(legacyFilter.SubfiltersPresent).To(BeTrue())
	Expect(legacyFilter.Subfilters).To(ConsistOf(expectedLegacy))
	state, err := adapter.GetProjectState(context.Background(), organization, slug)
	Expect(err).NotTo(HaveOccurred())
	Expect(state.ScrubData).NotTo(BeNil())
	Expect(*state.ScrubData).To(Equal(active))
	Expect(state.ScrubDefaults).NotTo(BeNil())
	Expect(*state.ScrubDefaults).To(Equal(active))
	Expect(state.ScrubIPAddresses).NotTo(BeNil())
	Expect(*state.ScrubIPAddresses).To(Equal(active))
	Expect(state.SensitiveFields).NotTo(BeNil())
	Expect(*state.SensitiveFields).To(Equal(sensitive))
	Expect(state.SafeFields).NotTo(BeNil())
	Expect(*state.SafeFields).To(Equal(safe))
	Expect(state.ResolveAge).NotTo(BeNil())
	Expect(*state.ResolveAge).To(Equal(resolve))
	Expect(state.DefaultEnvironment).NotTo(BeNil())
	Expect(*state.DefaultEnvironment).To(Equal(environment))
	Expect(state.StoreCrashReports).NotTo(BeNil())
	Expect(*state.StoreCrashReports).To(Equal(crashLimit))
	Expect(state.ScrapeJavaScript).NotTo(BeNil())
	Expect(*state.ScrapeJavaScript).To(Equal(scrape))
	Expect(state.AllowedDomains).NotTo(BeNil())
	Expect(*state.AllowedDomains).To(ConsistOf(allowedDomains))
	Expect(state.RelayPIIConfig).NotTo(BeNil())
	Expect(*state.RelayPIIConfig).To(MatchJSON(relayPII))
	Expect(state.GroupingEnhancements).NotTo(BeNil())
	Expect(*state.GroupingEnhancements).To(Equal(enhancements))
	Expect(state.FingerprintingRules).NotTo(BeNil())
	Expect(*state.FingerprintingRules).To(Equal(fingerprints))
	Expect(state.BuiltinSymbolSources).NotTo(BeNil())
	Expect(*state.BuiltinSymbolSources).To(ConsistOf(builtinSources))
}

func assertLiveIssueOwnership(adapter *issueownershipruntime.HTTPAdapter, organization, slug, raw string, fallthroughValue bool, autoAssignment string, codeownersAutoSync bool) {
	state, err := adapter.Get(context.Background(), organization, slug)
	Expect(err).NotTo(HaveOccurred())
	Expect(state.Raw).NotTo(BeNil())
	Expect(*state.Raw).To(Equal(raw))
	Expect(state.Fallthrough).NotTo(BeNil())
	Expect(*state.Fallthrough).To(Equal(fallthroughValue))
	Expect(state.AutoAssignment).NotTo(BeNil())
	Expect(*state.AutoAssignment).To(Equal(autoAssignment))
	Expect(state.CodeownersAutoSync).NotTo(BeNil())
	Expect(*state.CodeownersAutoSync).To(Equal(codeownersAutoSync))
}

func assertLiveManagedClientKeys(adapter *clientkeyruntime.HTTPAdapter, organization, slug, revisionSuffix string, expected int) {
	keys, err := adapter.List(context.Background(), organization, slug)
	Expect(err).NotTo(HaveOccurred())
	managed := make([]clientkeyruntime.Remote, 0, expected)
	for _, key := range keys {
		if strings.HasPrefix(key.Name, "sko:ingest:") {
			managed = append(managed, key)
		}
	}
	Expect(managed).To(HaveLen(expected))
	if expected == 1 {
		Expect(managed[0].Name).To(HaveSuffix(revisionSuffix))
		Expect(managed[0].IsActive).To(BeTrue())
		Expect(managed[0].RateLimit).To(Equal(&clientkeyruntime.RateLimit{Count: 1000, Window: 60}))
		Expect(managed[0].DSN.Public).NotTo(BeEmpty())
	}
}

func assertLiveCustomSymbolSources(adapter *symbolsourceruntime.HTTPAdapter, organization, slug, httpURL string) {
	sources, err := adapter.List(context.Background(), organization, slug)
	Expect(err).NotTo(HaveOccurred())
	counts := map[string]int{}
	observedHTTPURL := ""
	for _, source := range sources {
		if !strings.HasPrefix(source.Name, "sko:") {
			continue
		}
		counts[source.Type]++
		if source.Type == "http" {
			observedHTTPURL = source.URL
		}
	}
	Expect(counts).To(Equal(map[string]int{"http": 1, "s3": 1, "gcs": 1}))
	Expect(observedHTTPURL).To(Equal(httpURL))
}

func assertLiveCustomSymbolSourcesEmpty(adapter *symbolsourceruntime.HTTPAdapter, organization, slug string) {
	sources, err := adapter.List(context.Background(), organization, slug)
	Expect(err).NotTo(HaveOccurred())
	managed := 0
	for _, source := range sources {
		if strings.HasPrefix(source.Name, "sko:") {
			managed++
		}
	}
	Expect(managed).To(BeZero())
}

func assertLiveCodeMapping(adapter *codemappingruntime.HTTPAdapter, organization, slug, stackRoot, sourceRoot, branch string) {
	mappings, err := adapter.ListMappings(context.Background(), organization)
	Expect(err).NotTo(HaveOccurred())
	var matches []codemappingruntime.Remote
	for _, mapping := range mappings {
		if mapping.ProjectSlug == slug {
			matches = append(matches, mapping)
		}
	}
	Expect(matches).To(HaveLen(1))
	Expect(matches[0].StackRoot).To(Equal(stackRoot))
	Expect(matches[0].SourceRoot).To(Equal(sourceRoot))
	Expect(matches[0].DefaultBranch).To(Equal(branch))
}

func assertLiveCodeMappingEmpty(adapter *codemappingruntime.HTTPAdapter, organization, slug string) {
	mappings, err := adapter.ListMappings(context.Background(), organization)
	Expect(err).NotTo(HaveOccurred())
	for _, mapping := range mappings {
		Expect(mapping.ProjectSlug).NotTo(Equal(slug))
	}
}

func createLiveTeam(client *sentryapi.Client, organization, slug string) {
	var response struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	_, err := client.DoJSON(context.Background(), sentryapi.Request{
		Method: http.MethodPost,
		Path:   "/api/0/organizations/" + url.PathEscape(organization) + "/teams/",
		Body: struct {
			Slug string `json:"slug"`
		}{Slug: slug},
		RetrySafe: false,
		Operation: "create-live-grant-team",
	}, &response)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.ID).NotTo(BeEmpty())
	Expect(response.Slug).To(Equal(slug))
}

func createLiveUnmanagedProject(client *sentryapi.Client, organization, team, slug string) string {
	var response struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	_, err := client.DoJSON(context.Background(), sentryapi.Request{
		Method: http.MethodPost,
		Path:   "/api/0/teams/" + url.PathEscape(organization) + "/" + url.PathEscape(team) + "/projects/",
		Body: struct {
			Name         string `json:"name"`
			Slug         string `json:"slug"`
			DefaultRules bool   `json:"default_rules"`
		}{Name: slug, Slug: slug, DefaultRules: false},
		RetrySafe: false,
		Operation: "create-live-unmanaged-project",
	}, &response)
	Expect(err).NotTo(HaveOccurred())
	Expect(response.ID).NotTo(BeEmpty())
	Expect(response.Slug).To(Equal(slug))
	return response.ID
}

func deleteLiveTeam(client *sentryapi.Client, organization, slug string) {
	_, _ = client.DoJSON(context.Background(), sentryapi.Request{
		Method:    http.MethodDelete,
		Path:      "/api/0/teams/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/",
		RetrySafe: false,
		Operation: "delete-live-grant-team",
	}, nil)
}

func liveTeamSlugs(teams []projectaccessruntime.Team) []string {
	result := make([]string, 0, len(teams))
	for _, team := range teams {
		result = append(result, team.Slug)
	}
	return result
}

func deleteLiveControllerFixtures(key types.NamespacedName, connectionName string) {
	ctx := context.Background()
	_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: key.Name + sentryv1alpha1.DefaultDSNSecretSuffix, Namespace: key.Namespace,
	}})
	_ = k8sClient.Delete(ctx, &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}})
	_ = k8sClient.Delete(ctx, &sentryv1alpha1.SentryConnection{ObjectMeta: metav1.ObjectMeta{Name: connectionName}})
}

func liveManagedProjectInput(baseURL, organization, team, slug, version string) projectruntime.Input {
	return projectruntime.Input{
		Project: &sentryv1alpha1.SentryProject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       slug,
				Namespace:  "default",
				UID:        types.UID("live-recovery-" + strings.ReplaceAll(version, ".", "-")),
				Generation: 1,
			},
			Spec: sentryv1alpha1.SentryProjectSpec{
				Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{
					Team: team,
				}},
				Lifecycle: sentryv1alpha1.LifecycleSpec{OnDelete: sentryv1alpha1.OnDeleteDeleteProject},
			},
		},
		ConnectionName: "live-sentry",
		Connection: sentryv1alpha1.SentryConnectionSpec{
			URL:          baseURL,
			Organization: organization,
			Access: []sentryv1alpha1.AccessRule{{
				Owner: "live", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"default"}},
				Teams: []string{team}, AllowCreate: true,
			}},
		},
	}
}

func deleteLiveProject(upstream *url.URL, token, organization, slug string) {
	target := *upstream
	target.Path = "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/"
	request, err := http.NewRequest(http.MethodDelete, target.String(), nil)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
	}
}

type liveAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *liveAuditSink) Record(_ context.Context, event audit.Event) error {
	if err := audit.Validate(event); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *liveAuditSink) has(outcome audit.Outcome, reason audit.Reason) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event.Outcome == outcome && event.Reason == reason {
			return true
		}
	}
	return false
}
