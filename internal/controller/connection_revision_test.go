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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	projectruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/project"
)

func TestRevokeDSNWithoutProtectedBindingIsNotTemporaryFailure(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sentryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	reconciler := &SentryProjectReconciler{
		Client:   kubeClient,
		Bindings: &binding.Store{Client: kubeClient, Namespace: "operator-state"},
	}
	project := &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{
		Name: "checkout", Namespace: "payments", UID: types.UID("new-project-uid"),
	}}
	revoked, err := reconciler.revokeDSNIfDenied(context.Background(), project)
	if err != nil || revoked {
		t.Fatalf("new project DSN revoke: revoked=%v err=%v", revoked, err)
	}
}

func TestProjectDeletionRevokesOnlyItsSharedDSNKey(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sentryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deletedAt := metav1.Now()
	project := &sentryv1alpha1.SentryProject{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments", UID: types.UID("checkout-uid"), Generation: 1, DeletionTimestamp: &deletedAt, Finalizers: []string{sentryProjectFinalizer}},
		Spec:       sentryv1alpha1.SentryProjectSpec{Sentry: "main", Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}}, DSN: &sentryv1alpha1.DSNSpec{}},
	}
	connection := &sentryv1alpha1.SentryConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "main"},
		Spec:       sentryv1alpha1.SentryConnectionSpec{Access: []sentryv1alpha1.AccessRule{{Owner: "payments", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"payments"}}, Teams: []string{"payments"}, AllowCreate: true}}},
	}
	output := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sentryv1alpha1.DSNSecretName, Namespace: "payments"}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"checkout": []byte("https://public@example.invalid/1"), "other": []byte("preserve")}}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(project, connection, output).Build()
	store := &binding.Store{Client: kubeClient, Namespace: "operator-state"}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "sentry-operator"}
	if _, err := store.CreateBinding(context.Background(), binding.ProjectBinding{
		Version: binding.CurrentVersion, Project: binding.ProjectReference{Namespace: "payments", Name: "checkout", UID: "checkout-uid", Generation: 1}, Connection: "main", Installation: "https://sentry.example", Organization: "company", Slug: "checkout", Owner: "payments", Origin: binding.OriginManaged, AccessMode: binding.AccessManage, ExternalIdentity: "project-1", Marker: &marker, ManagerUID: "checkout-uid", ClaimName: binding.ClaimName("https://sentry.example", "company", "checkout"),
		DSN: &binding.DSNBinding{KeyIdentity: "key-1", KeyName: "default", SelectorHash: strings.Repeat("a", 64), CurrentSecret: sentryv1alpha1.DSNSecretName},
	}); err != nil {
		t.Fatal(err)
	}
	reconciler := &SentryProjectReconciler{Client: kubeClient, OutputSecrets: kubeClient, Bindings: store}
	if err := reconciler.revokeDSNForDeletion(context.Background(), project, connection); err != nil {
		t.Fatal(err)
	}
	updated := &corev1.Secret{}
	if err := kubeClient.Get(context.Background(), types.NamespacedName{Name: sentryv1alpha1.DSNSecretName, Namespace: "payments"}, updated); err != nil {
		t.Fatal(err)
	}
	if _, found := updated.Data["checkout"]; found || string(updated.Data["other"]) != "preserve" {
		t.Fatalf("shared Secret data = %#v", updated.Data)
	}
	record, err := store.GetBinding(context.Background(), "checkout-uid")
	if err != nil || record.Binding.DSN != nil {
		t.Fatalf("binding DSN = %#v, err = %v", record.Binding.DSN, err)
	}
}

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

func TestOnlyProtectedManagedStandbyCanContributeChildren(t *testing.T) {
	t.Parallel()
	project := &sentryv1alpha1.SentryProject{Spec: sentryv1alpha1.SentryProjectSpec{Role: sentryv1alpha1.ProjectRoleStandby}}
	managed := projectruntime.Result{Origin: binding.OriginManaged, AccessMode: binding.AccessReadOnly}
	if !canManageChildren(project, managed) {
		t.Fatal("protected managed standby was denied child ownership")
	}
	external := projectruntime.Result{Origin: binding.OriginExternal, AccessMode: binding.AccessReadOnly}
	if canManageChildren(project, external) {
		t.Fatal("unmanaged standby was allowed child ownership")
	}
	project.Spec.Role = sentryv1alpha1.ProjectRoleActive
	if canManageChildren(project, managed) {
		t.Fatal("active acquisition request without manager claim was allowed child ownership")
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
