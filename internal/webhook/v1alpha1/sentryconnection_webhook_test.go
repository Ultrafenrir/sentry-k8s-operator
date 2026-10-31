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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

var _ = Describe("SentryConnection Webhook", func() {
	var validator SentryConnectionValidator

	BeforeEach(func() {
		validator = SentryConnectionValidator{}
	})

	It("accepts a valid source-only connection", func() {
		_, err := validator.ValidateCreate(context.Background(), validConnection())
		Expect(err).NotTo(HaveOccurred())
	})

	It("accepts only explicit supported alert compatibility profiles", func() {
		obj := validConnection()
		obj.Spec.Compatibility = &sentryconfigurationv1alpha1.ConnectionCompatibility{Alerts: sentryconfigurationv1alpha1.AlertCompatibilityLegacy24}
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())

		obj.Spec.Compatibility.Alerts = "guessed-latest"
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("Unsupported value")))
	})

	It("requires an explicit opt-in for insecure connection transports", func() {
		obj := validConnection()
		obj.Spec.URL = "http://sentry.example.invalid"

		warnings, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("plain HTTP requires spec.transport.allowInsecureHTTP=true")))
		Expect(warnings).To(BeEmpty())

		obj.Spec.Transport = &sentryconfigurationv1alpha1.ConnectionTransport{AllowInsecureHTTP: true}
		warnings, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("without transport encryption")))

		obj.Spec.URL = "https://sentry.example.invalid"
		obj.Spec.Transport = &sentryconfigurationv1alpha1.ConnectionTransport{InsecureSkipTLSVerify: true}
		warnings, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("certificate and hostname verification")))

		obj.Spec.Credentials.CABundleSecretRef = &sentryconfigurationv1alpha1.SecretKeyReference{Name: "sentry-ca", Key: "ca.crt"}
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("mutually exclusive")))

		obj.Spec.URL = "https://token@example.invalid"
		obj.Spec.Transport = nil
		obj.Spec.Credentials.CABundleSecretRef = nil
		_, err = validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("without userinfo")))
	})

	It("requires the Sentry hook receiver to be an exact HTTPS events base", func() {
		obj := validConnection()
		obj.Spec.Integrations = &sentryconfigurationv1alpha1.ConnectionIntegrations{Telegram: &sentryconfigurationv1alpha1.TelegramIntegration{
			BotTokenSecretRef:    sentryconfigurationv1alpha1.SecretKeyReference{Name: "telegram", Key: "bot"},
			ReceiverURL:          "https://hooks.example.test/events",
			WebhookAuthSecretRef: sentryconfigurationv1alpha1.SecretKeyReference{Name: "telegram", Key: "sentry-hook-secret"},
		}}
		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).NotTo(HaveOccurred())
		for _, invalid := range []string{"https://hooks.example.test/events/", "https://hooks.example.test/events?source=private", "https://hooks.example.test/other"} {
			obj.Spec.Integrations.Telegram.ReceiverURL = invalid
			_, err = validator.ValidateCreate(context.Background(), obj)
			Expect(err).To(MatchError(ContainSubstring("exact HTTPS /events base")), invalid)
		}
	})

	It("requires consumeDSN to be a subset of observe", func() {
		obj := validConnection()
		obj.Spec.Access[0].ExistingProjects.ConsumeDSN = []string{"unobserved"}

		_, err := validator.ValidateCreate(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("consumeDSN must be a subset of observe")))
	})

	It("blocks policy changes while a registration fence is active", func() {
		oldObj := validConnection()
		oldObj.Status.RegistrationCommit = &sentryconfigurationv1alpha1.RegistrationCommit{
			OperationHash:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			PolicyGeneration: 1,
			ExpiresAt:        metav1.NewTime(time.Now().Add(time.Minute)),
		}
		newObj := oldObj.DeepCopy()
		newObj.Spec.Access[0].AllowCreate = false

		_, err := validator.ValidateUpdate(context.Background(), oldObj, newObj)
		Expect(err).To(MatchError(ContainSubstring("registrationCommit is active")))
	})

	It("requires ownership revisions to move monotonically", func() {
		oldObj := validConnection()
		oldObj.Spec.Ownership = &sentryconfigurationv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryconfigurationv1alpha1.ConnectionRoleStandby, Revision: 7,
		}

		removed := oldObj.DeepCopy()
		removed.Spec.Ownership = nil
		_, err := validator.ValidateUpdate(context.Background(), oldObj, removed)
		Expect(err).To(MatchError(ContainSubstring("ownership fencing cannot be disabled")))

		decreased := oldObj.DeepCopy()
		decreased.Spec.Ownership.Revision = 6
		_, err = validator.ValidateUpdate(context.Background(), oldObj, decreased)
		Expect(err).To(MatchError(ContainSubstring("must not decrease")))

		promotion := oldObj.DeepCopy()
		promotion.Spec.Ownership.Role = sentryconfigurationv1alpha1.ConnectionRoleActive
		_, err = validator.ValidateUpdate(context.Background(), oldObj, promotion)
		Expect(err).To(MatchError(ContainSubstring("must increase when promoting Standby to Active")))

		regionChange := oldObj.DeepCopy()
		regionChange.Spec.Ownership.Region = "us-east-1"
		_, err = validator.ValidateUpdate(context.Background(), oldObj, regionChange)
		Expect(err).To(MatchError(ContainSubstring("must increase when region changes")))

		validPromotion := oldObj.DeepCopy()
		validPromotion.Spec.Ownership.Role = sentryconfigurationv1alpha1.ConnectionRoleActive
		validPromotion.Spec.Ownership.Revision = 8
		_, err = validator.ValidateUpdate(context.Background(), oldObj, validPromotion)
		Expect(err).NotTo(HaveOccurred())
	})

	It("blocks deletion while a registration fence is active", func() {
		obj := validConnection()
		obj.Status.RegistrationCommit = &sentryconfigurationv1alpha1.RegistrationCommit{
			OperationHash: strings.Repeat("a", 64), PolicyGeneration: 1, ExpiresAt: metav1.NewTime(time.Now().Add(time.Minute)),
		}

		_, err := validator.ValidateDelete(context.Background(), obj)
		Expect(err).To(MatchError(ContainSubstring("deletion is blocked while registrationCommit is active")))
	})
})

func validConnection() *sentryconfigurationv1alpha1.SentryConnection {
	return &sentryconfigurationv1alpha1.SentryConnection{
		Spec: sentryconfigurationv1alpha1.SentryConnectionSpec{
			URL:          "https://sentry.example.test",
			Organization: "company",
			Credentials: sentryconfigurationv1alpha1.ConnectionCredentials{
				TokenSecretRef: sentryconfigurationv1alpha1.SecretKeyReference{Name: "sentry-token", Key: "token"},
			},
			Access: []sentryconfigurationv1alpha1.AccessRule{
				{
					Owner:       "payments",
					Namespaces:  sentryconfigurationv1alpha1.NamespaceAccess{MatchNames: []string{"checkout-prod"}},
					Teams:       []string{"payments"},
					AllowCreate: true,
					ExistingProjects: &sentryconfigurationv1alpha1.ExistingProjectAccess{
						Observe:    []string{"checkout-api"},
						ConsumeDSN: []string{"checkout-api"},
					},
				},
			},
		},
	}
}
