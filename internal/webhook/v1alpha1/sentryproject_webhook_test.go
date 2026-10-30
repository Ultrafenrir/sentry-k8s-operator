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

	It("rejects a destination with multiple types", func() {
		obj := managedProject("checkout")
		obj.Spec.Destinations = map[string]sentryconfigurationv1alpha1.DestinationSpec{
			"oncall": {
				Slack:    &sentryconfigurationv1alpha1.SlackDestination{Channel: "alerts"},
				Telegram: &sentryconfigurationv1alpha1.TelegramDestination{Target: "alerts"},
			},
		}

		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("exactly one destination type")))
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
		oldObj.Spec.Destinations = map[string]sentryconfigurationv1alpha1.DestinationSpec{
			"telegram": {Telegram: &sentryconfigurationv1alpha1.TelegramDestination{Target: "payments-alerts"}},
		}
		oldObj.Status.Destinations = map[string]sentryconfigurationv1alpha1.DestinationStatus{
			"telegram": {Type: "telegram", State: "RegistrationRequired", Reason: "TelegramRegistrationRequired", RegistrationCommit: &sentryconfigurationv1alpha1.RegistrationCommit{
				OperationHash: strings.Repeat("a", 64), PolicyGeneration: 7, ExpiresAt: metav1.Now(),
			}},
		}
		newObj := oldObj.DeepCopy()
		delete(newObj.Spec.Destinations, "telegram")

		_, err := validator.ValidateUpdate(context.Background(), oldObj, newObj)
		Expect(err).To(MatchError(ContainSubstring("registrationCommit is active")))

		unrelated := oldObj.DeepCopy()
		unrelated.Spec.Paused = true
		_, err = validator.ValidateUpdate(context.Background(), oldObj, unrelated)
		Expect(err).NotTo(HaveOccurred())
	})

	It("blocks deletion while any destination registration fence is active", func() {
		obj := managedProject("checkout")
		obj.Status.Destinations = map[string]sentryconfigurationv1alpha1.DestinationStatus{
			"telegram": {Type: "telegram", State: "RegistrationRequired", Reason: "TelegramRegistrationRequired", RegistrationCommit: &sentryconfigurationv1alpha1.RegistrationCommit{
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
			Project: sentryconfigurationv1alpha1.ProjectSelector{
				Create: &sentryconfigurationv1alpha1.ProjectCreate{Team: "payments"},
			},
			Lifecycle: sentryconfigurationv1alpha1.LifecycleSpec{OnDelete: sentryconfigurationv1alpha1.OnDeleteRetain},
		},
	}
}
