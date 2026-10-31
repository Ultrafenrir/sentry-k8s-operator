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
		Expect(err).To(MatchError(ContainSubstring("event-count requires")))
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
