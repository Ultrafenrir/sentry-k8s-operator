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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

func TestConnectionOwnershipStatusDoesNotEchoUnverifiedActiveRevision(t *testing.T) {
	t.Parallel()
	connection := &sentryv1alpha1.SentryConnection{
		ObjectMeta: metav1.ObjectMeta{Generation: 4},
		Spec: sentryv1alpha1.SentryConnectionSpec{Ownership: &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
		}},
	}

	setConnectionOwnershipStatus(connection)
	condition := meta.FindStatusCondition(connection.Status.Conditions, "OwnershipReady")
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "RevisionUnverified" {
		t.Fatalf("ownership condition = %#v", condition)
	}
	if connection.Status.EffectiveRole != string(sentryv1alpha1.ConnectionRoleStandby) || connection.Status.ObservedRevision != 0 || connection.Status.Region != "eu-west-1" {
		t.Fatalf("ownership status = %#v", connection.Status)
	}
}

func TestConnectionStandbyIsSafeWithoutClaimingRemoteRevision(t *testing.T) {
	t.Parallel()
	connection := &sentryv1alpha1.SentryConnection{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec: sentryv1alpha1.SentryConnectionSpec{Ownership: &sentryv1alpha1.ConnectionOwnership{
			Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleStandby, Revision: 7,
		}},
	}

	setConnectionOwnershipStatus(connection)
	condition := meta.FindStatusCondition(connection.Status.Conditions, "OwnershipReady")
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "StandbyConfigured" {
		t.Fatalf("ownership condition = %#v", condition)
	}
	if connection.Status.EffectiveRole != string(sentryv1alpha1.ConnectionRoleStandby) || connection.Status.ObservedRevision != 0 {
		t.Fatalf("ownership status = %#v", connection.Status)
	}
}
