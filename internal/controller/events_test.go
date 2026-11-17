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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	sentryconfigurationv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestConditionEventsOnlyOnSafeTransitions(t *testing.T) {
	recorder := events.NewFakeRecorder(4)
	resource := &sentryconfigurationv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments"}}
	before := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Timeout", Message: "A safe fixed message."}}

	emitConditionChanges(recorder, resource, before, before)
	select {
	case event := <-recorder.Events:
		t.Fatalf("unchanged condition emitted event %q", event)
	default:
	}

	after := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "AccessDenied", Message: "Current policy denies the operation."}}
	emitConditionChanges(recorder, resource, before, after)
	if event := <-recorder.Events; event != "Warning AccessDenied Current policy denies the operation." {
		t.Fatalf("event = %q", event)
	}

	ready := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "ProjectVerified", Message: "Project identity is verified."}}
	emitConditionChanges(recorder, resource, after, ready)
	if event := <-recorder.Events; event != "Normal ProjectVerified Project identity is verified." {
		t.Fatalf("event = %q", event)
	}
}

func TestConditionEventsSuppressDefaultRegionalOwnershipNoise(t *testing.T) {
	recorder := events.NewFakeRecorder(1)
	resource := &sentryconfigurationv1alpha1.SentryConnection{ObjectMeta: metav1.ObjectMeta{Name: "company"}}
	after := []metav1.Condition{{
		Type: "OwnershipReady", Status: metav1.ConditionUnknown, Reason: "NotConfigured",
		Message: "Regional ownership fencing is not configured.",
	}}

	emitConditionChanges(recorder, resource, nil, after)
	select {
	case event := <-recorder.Events:
		t.Fatalf("default regional ownership condition emitted event %q", event)
	default:
	}
}
