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
	"testing"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

func TestSentryAlertPolicyValidator(t *testing.T) {
	validator := &SentryAlertPolicyValidator{}
	valid := &sentryv1alpha1.SentryAlertPolicy{Spec: sentryv1alpha1.SentryAlertPolicySpec{
		Sentry: "company-sentry",
		NotificationTargets: &sentryv1alpha1.NotificationTargets{
			Slack: map[string][]string{"company-slack": {"backend-alerts"}},
		},
		Alerts: map[string]sentryv1alpha1.AlertSpec{
			"new-errors": {Trigger: sentryv1alpha1.AlertTriggerFirstSeen, Notify: []string{"slack/backend-alerts"}},
		},
	}}
	if _, err := validator.ValidateCreate(context.Background(), valid); err != nil {
		t.Fatalf("valid policy: %v", err)
	}

	duplicate := valid.DeepCopy()
	duplicate.Spec.NotificationTargets.Slack["other-slack"] = []string{"backend-alerts"}
	if _, err := validator.ValidateCreate(context.Background(), duplicate); err == nil {
		t.Fatal("duplicate provider target name was accepted")
	}

	missing := valid.DeepCopy()
	missing.Spec.Alerts["new-errors"] = sentryv1alpha1.AlertSpec{Trigger: sentryv1alpha1.AlertTriggerFirstSeen, Notify: []string{"slack/missing"}}
	if _, err := validator.ValidateCreate(context.Background(), missing); err == nil {
		t.Fatal("missing target reference was accepted")
	}

	changed := valid.DeepCopy()
	changed.Spec.Sentry = "other-sentry"
	if _, err := validator.ValidateUpdate(context.Background(), valid, changed); err == nil {
		t.Fatal("connection change was accepted")
	}
}
