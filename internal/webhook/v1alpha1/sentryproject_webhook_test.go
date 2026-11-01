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

package v1alpha1

import (
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("SentryProject Webhook", func() {
	var validator SentryProjectValidator
	var defaulter SentryProjectDefaulter

	BeforeEach(func() {
		validator = SentryProjectValidator{}
		defaulter = SentryProjectDefaulter{}
	})

	It("defaults lifecycle and an explicitly requested DSN block", func() {
		obj := managedProject("checkout")
		obj.Spec.DSN = &sentryconfigurationv1alpha1.DSNSpec{}

		Expect(defaulter.Default(context.Background(), obj)).To(Succeed())
		Expect(obj.Spec.Lifecycle.OnDelete).To(Equal(sentryconfigurationv1alpha1.OnDeleteRetain))
		Expect(obj.Spec.DSN.Secret).To(Equal("checkout-sentry"))
	})

	It("keeps an omitted DSN block omitted for policy-driven read-only access", func() {
		obj := managedProject("checkout")

		Expect(defaulter.Default(context.Background(), obj)).To(Succeed())
		Expect(obj.Spec.DSN).To(BeNil())
	})

	It("rejects create and use together", func() {
		obj := managedProject("checkout")
		use := "existing"
		obj.Spec.Project.Use = &use

		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("exactly one of create or use")))
	})

	It("rejects managed fields on read-only use", func() {
		use := "existing"
		obj := &sentryconfigurationv1alpha1.SentryProject{
			Spec: sentryconfigurationv1alpha1.SentryProjectSpec{
				Project:   sentryconfigurationv1alpha1.ProjectSelector{Use: &use},
				Settings:  &sentryconfigurationv1alpha1.ProjectSettings{},
				Lifecycle: sentryconfigurationv1alpha1.LifecycleSpec{OnDelete: sentryconfigurationv1alpha1.OnDeleteRetain},
			},
		}

		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("project.use is read-only")))
	})

	It("accepts additional team slugs and rejects the primary team", func() {
		obj := managedProject("checkout")
		obj.Spec.GrantAccess = &sentryconfigurationv1alpha1.ProjectGrantAccess{Teams: []string{"backend", "security"}}
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())

		obj.Spec.GrantAccess.Teams = []string{"payments"}
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("must not repeat project.create.team")))
	})

	It("accepts attached issue ownership teams and rejects unattached teams", func() {
		obj := managedProject("checkout")
		obj.Spec.GrantAccess = &sentryconfigurationv1alpha1.ProjectGrantAccess{Teams: []string{"backend"}}
		obj.Spec.IssueOwnership = &sentryconfigurationv1alpha1.IssueOwnershipSpec{
			Rules: []sentryconfigurationv1alpha1.IssueOwnershipRule{{
				Matcher: sentryconfigurationv1alpha1.IssueOwnershipMatcher{Type: sentryconfigurationv1alpha1.IssueOwnershipMatcherPath, Pattern: "src/*"},
				Teams:   []string{"payments", "backend"},
			}},
			Fallthrough:        sentryconfigurationv1alpha1.IssueOwnershipFallthroughNoOne,
			AutoAssignment:     sentryconfigurationv1alpha1.IssueOwnershipAutoAssignIssueOwner,
			CodeownersAutoSync: true,
		}
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())

		obj.Spec.IssueOwnership.Rules[0].Teams = append(obj.Spec.IssueOwnership.Rules[0].Teams, "security")
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("must be the primary project team or appear in grantAccess")))
	})

	It("validates code-mapping aliases and CODEOWNERS import branch selection", func() {
		obj := managedProject("checkout")
		mappings := map[string]sentryconfigurationv1alpha1.CodeMappingSpec{"checkout": {Integration: "github", Repository: "checkout", ImportCodeowners: true}}
		obj.Spec.CodeMappings = &mappings
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("required when importCodeowners is true")))
		entry := mappings["checkout"]
		entry.DefaultBranch = "main"
		mappings["checkout"] = entry
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())
	})

	It("validates managed client-key selection and monotonic rotation", func() {
		obj := managedProject("checkout")
		keys := map[string]sentryconfigurationv1alpha1.ManagedClientKeySpec{
			"ingest": {Active: true, RotationRevision: 2, RateLimit: &sentryconfigurationv1alpha1.ClientKeyRateLimit{Count: 1000, WindowSeconds: 60}},
		}
		obj.Spec.ClientKeys = &keys
		obj.Spec.DSN = &sentryconfigurationv1alpha1.DSNSpec{ManagedClientKey: "ingest"}
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())

		missing := obj.DeepCopy()
		missing.Spec.DSN.ManagedClientKey = "missing"
		_, err = validator.ValidateCreate(context.Background(), missing)
		Expect(err).To(MatchError(ContainSubstring("Not found")))

		rollback := obj.DeepCopy()
		rollbackKeys := *rollback.Spec.ClientKeys
		entry := rollbackKeys["ingest"]
		entry.RotationRevision = 1
		rollbackKeys["ingest"] = entry
		rollback.Spec.ClientKeys = &rollbackKeys
		_, err = validator.ValidateUpdate(context.Background(), obj, rollback)
		Expect(err).To(MatchError(ContainSubstring("rotationRevision cannot decrease")))
	})

	It("blocks client-key changes during a pending rotation cutover", func() {
		obj := managedProject("checkout")
		keys := map[string]sentryconfigurationv1alpha1.ManagedClientKeySpec{"ingest": {Active: true, RotationRevision: 2}}
		obj.Spec.ClientKeys = &keys
		obj.Status.ClientKeys = map[string]sentryconfigurationv1alpha1.ClientKeyStatus{
			"ingest": {State: "RotationPending", Reason: "ClientKeyCutoverPending", RotationRevision: 2},
		}
		changed := obj.DeepCopy()
		changedKeys := *changed.Spec.ClientKeys
		entry := changedKeys["ingest"]
		entry.Active = false
		changedKeys["ingest"] = entry
		changed.Spec.ClientKeys = &changedKeys
		_, err := validator.ValidateUpdate(context.Background(), obj, changed)
		Expect(err).To(MatchError(ContainSubstring("rotation cutover is pending")))
	})

	It("validates custom symbol-source variants and namespace-local Secret references", func() {
		obj := managedProject("checkout")
		sources := map[string]sentryconfigurationv1alpha1.CustomSymbolSourceSpec{
			"native": {
				Layout: sentryconfigurationv1alpha1.SymbolSourceLayout{Type: "native", Casing: "default"},
				HTTP: &sentryconfigurationv1alpha1.HTTPSymbolSource{
					URL:               "https://symbols.example.invalid",
					PasswordSecretRef: &sentryconfigurationv1alpha1.SecretKeyReference{Name: "symbol-creds", Key: "password"},
				},
			},
		}
		obj.Spec.SymbolSources = &sources
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())

		invalid := obj.DeepCopy()
		entry := (*invalid.Spec.SymbolSources)["native"]
		entry.S3 = &sentryconfigurationv1alpha1.S3SymbolSource{}
		(*invalid.Spec.SymbolSources)["native"] = entry
		_, err = validator.ValidateCreate(context.Background(), invalid)
		Expect(err).To(MatchError(ContainSubstring("exactly one of http, s3 or gcs")))

		invalid = obj.DeepCopy()
		entry = (*invalid.Spec.SymbolSources)["native"]
		entry.HTTP.PasswordSecretRef.Key = ""
		(*invalid.Spec.SymbolSources)["native"] = entry
		_, err = validator.ValidateCreate(context.Background(), invalid)
		Expect(err).To(MatchError(ContainSubstring("key is required")))

		invalid = obj.DeepCopy()
		entry = (*invalid.Spec.SymbolSources)["native"]
		entry.HTTP.URL = "https://reader:secret@symbols.example.invalid"
		(*invalid.Spec.SymbolSources)["native"] = entry
		_, err = validator.ValidateCreate(context.Background(), invalid)
		Expect(err).To(MatchError(ContainSubstring("without userinfo")))
	})

	It("rejects ambiguous names within one provider", func() {
		obj := managedProject("checkout")
		obj.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Slack: map[string][]string{
			"company": {"alerts"}, "other": {"alerts"},
		}}

		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("target names must be unique")))
	})

	It("accepts typed alerts and rejects missing or Telegram destinations", func() {
		enabled := true
		window := int32(5)
		threshold := int64(200)
		obj := managedProject("checkout")
		obj.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"incidents"}}, Telegram: []string{"alerts"}}
		alerts := map[string]sentryconfigurationv1alpha1.AlertSpec{
			"new-errors": {Trigger: sentryconfigurationv1alpha1.AlertTriggerFirstSeen, Enabled: &enabled, Notify: []string{"slack/incidents"}},
			"volume":     {Trigger: sentryconfigurationv1alpha1.AlertTriggerEventCount, WindowMinutes: &window, Threshold: &threshold, Enabled: &enabled, Notify: []string{"slack/incidents"}},
			"users":      {Trigger: sentryconfigurationv1alpha1.AlertTriggerUserCount, WindowMinutes: &window, Threshold: &threshold, Enabled: &enabled, Notify: []string{"slack/incidents"}},
		}
		obj.Spec.Alerts = &alerts
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())

		alerts["new-errors"] = sentryconfigurationv1alpha1.AlertSpec{Trigger: sentryconfigurationv1alpha1.AlertTriggerFirstSeen, Notify: []string{"slack/missing"}}
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("Not found")))

		alerts["new-errors"] = sentryconfigurationv1alpha1.AlertSpec{Trigger: sentryconfigurationv1alpha1.AlertTriggerFirstSeen, Notify: []string{"telegram/alerts"}}
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("Unsupported value")))
	})

	It("rejects invalid alert trigger fields", func() {
		window := int32(5)
		obj := managedProject("checkout")
		obj.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Slack: map[string][]string{"company": {"incidents"}}}
		alerts := map[string]sentryconfigurationv1alpha1.AlertSpec{
			"bad": {Trigger: sentryconfigurationv1alpha1.AlertTriggerEventCount, WindowMinutes: &window, Notify: []string{"slack/incidents"}},
		}
		obj.Spec.Alerts = &alerts
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("metric triggers require")))
	})

	It("rejects identity changes", func() {
		oldObj := managedProject("checkout")
		newObj := oldObj.DeepCopy()
		newObj.Spec.Project.Create.Team = "other-team"

		_, err := validator.ValidateUpdate(context.Background(), oldObj, newObj)
		Expect(err).To(MatchError(ContainSubstring("project identity is immutable")))
	})

	It("fences only the destination participating in a registration commit", func() {
		oldObj := managedProject("checkout")
		oldObj.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{Telegram: []string{"payments-alerts"}}
		oldObj.Status.NotificationTargets = map[string]sentryconfigurationv1alpha1.DestinationStatus{
			"telegram/payments-alerts": {Type: "telegram", State: "RegistrationRequired", Reason: "TelegramRegistrationRequired", RegistrationCommit: &sentryconfigurationv1alpha1.RegistrationCommit{
				OperationHash: strings.Repeat("a", 64), PolicyGeneration: 7, ExpiresAt: metav1.Now(),
			}},
		}
		newObj := oldObj.DeepCopy()
		newObj.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{}

		_, err := validator.ValidateUpdate(context.Background(), oldObj, newObj)
		Expect(err).To(MatchError(ContainSubstring("registrationCommit is active")))

		unrelated := oldObj.DeepCopy()
		unrelated.Spec.Paused = true
		_, err = validator.ValidateUpdate(context.Background(), oldObj, unrelated)
		Expect(err).NotTo(HaveOccurred())
	})

	It("blocks deletion while any destination registration fence is active", func() {
		obj := managedProject("checkout")
		obj.Status.NotificationTargets = map[string]sentryconfigurationv1alpha1.DestinationStatus{
			"telegram/payments-alerts": {Type: "telegram", State: "RegistrationRequired", Reason: "TelegramRegistrationRequired", RegistrationCommit: &sentryconfigurationv1alpha1.RegistrationCommit{
				OperationHash: strings.Repeat("a", 64), PolicyGeneration: 7, ExpiresAt: metav1.Now(),
			}},
		}

		_, err := validator.ValidateDelete(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("deletion is blocked while registrationCommit is active")))
	})
})

func managedProject(name string) *sentryconfigurationv1alpha1.SentryProject {
	return &sentryconfigurationv1alpha1.SentryProject{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: sentryconfigurationv1alpha1.SentryProjectSpec{
			Sentry: "company-sentry",
			Project: sentryconfigurationv1alpha1.ProjectSelector{
				Create: &sentryconfigurationv1alpha1.ProjectCreate{Team: "payments"},
			},
			Lifecycle: sentryconfigurationv1alpha1.LifecycleSpec{OnDelete: sentryconfigurationv1alpha1.OnDeleteRetain},
		},
	}
}
