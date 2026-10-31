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

package registration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
)

func TestChallengeHandlerAuthenticatesAuthorizesExactProjectAndReturnsCodeOnce(t *testing.T) {
	t.Parallel()

	handler, tokens, access, store, now := challengeEndpointFixture(t)
	request := httptest.NewRequest(http.MethodPost, "/v1/telegram/challenges", strings.NewReader(`{"namespace":"payments","project":"checkout","destination":"telegram/payments-alerts"}`))
	request.Header.Set("Authorization", "Bearer caller-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if tokens.token != "caller-token" || len(tokens.audiences) != 1 || tokens.audiences[0] != "sentry-registration" {
		t.Fatalf("token review token=%q audiences=%v", tokens.token, tokens.audiences)
	}
	attributes := access.review.Spec.ResourceAttributes
	if access.calls != 1 || access.review.Spec.User != "alice" || attributes == nil || attributes.Verb != "update" || attributes.Group != sentryv1alpha1.GroupVersion.Group || attributes.Version != sentryv1alpha1.GroupVersion.Version || attributes.Resource != "sentryprojects" || attributes.Namespace != "payments" || attributes.Name != "checkout" {
		t.Fatalf("subject access review=%#v", access.review)
	}
	var result challengeResponse
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Command, "/register ") || result.Command == "/register " || !result.ExpiresAt.Time.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("response=%#v", result)
	}
	code := strings.TrimPrefix(result.Command, "/register ")
	record, err := store.GetBinding(context.Background(), "project-uid")
	if err != nil {
		t.Fatal(err)
	}
	challenge := record.Binding.Destinations["telegram/payments-alerts"].Telegram.Challenge
	if challenge == nil || challenge.CodeHash == code || challenge.PolicyResourceVersion != "1" || challenge.Project.Generation != 3 {
		t.Fatalf("protected challenge=%#v", challenge)
	}
	if strings.Contains(response.Body.String(), "caller-token") {
		t.Fatal("bearer token leaked into response")
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache control=%q", response.Header().Get("Cache-Control"))
	}
}

func TestChallengeHandlerDeniesBeforeProtectedStateReadOrWrite(t *testing.T) {
	t.Parallel()

	handler, _, access, store, _ := challengeEndpointFixture(t)
	access.allowed = false
	request := httptest.NewRequest(http.MethodPost, "/v1/telegram/challenges", strings.NewReader(`{"namespace":"payments","project":"checkout","destination":"telegram/payments-alerts"}`))
	request.Header.Set("Authorization", "Bearer caller-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "checkout") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	record, err := store.GetBinding(context.Background(), "project-uid")
	if err != nil {
		t.Fatal(err)
	}
	if record.Binding.Destinations["telegram/payments-alerts"].Telegram != nil {
		t.Fatal("denied request changed protected state")
	}
}

func TestChallengeHandlerRejectsUnreviewedTokensAndUnknownJSON(t *testing.T) {
	t.Parallel()

	handler, tokens, access, _, _ := challengeEndpointFixture(t)
	tokens.authenticated = false
	request := httptest.NewRequest(http.MethodPost, "/v1/telegram/challenges", strings.NewReader(`{"namespace":"payments","project":"checkout","destination":"telegram/payments-alerts"}`))
	request.Header.Set("Authorization", "Bearer rejected-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || access.calls != 0 {
		t.Fatalf("status=%d sar calls=%d", response.Code, access.calls)
	}

	tokens.authenticated = true
	request = httptest.NewRequest(http.MethodPost, "/v1/telegram/challenges", strings.NewReader(`{"namespace":"payments","project":"checkout","destination":"telegram/payments-alerts","code":"must-not-be-accepted"}`))
	request.Header.Set("Authorization", "Bearer caller-token")
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || access.calls != 0 {
		t.Fatalf("status=%d sar calls=%d body=%s", response.Code, access.calls, response.Body.String())
	}
}

func challengeEndpointFixture(t *testing.T) (*ChallengeHandler, *fakeTokenReviewer, *fakeAccessReviewer, *binding.Store, time.Time) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := sentryv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	connection := &sentryv1alpha1.SentryConnection{
		ObjectMeta: metav1.ObjectMeta{Name: "company", UID: types.UID("connection-uid"), Generation: 7, ResourceVersion: "1"},
		Spec: sentryv1alpha1.SentryConnectionSpec{
			URL: "https://sentry.example", Organization: "company",
			Credentials: sentryv1alpha1.ConnectionCredentials{TokenSecretRef: sentryv1alpha1.SecretKeyReference{Name: "token", Key: "token"}},
			Access: []sentryv1alpha1.AccessRule{{
				Owner: "payments", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"payments"}}, Teams: []string{"payments"}, AllowCreate: true,
				NotificationTargets: &sentryv1alpha1.NotificationTargetAccess{TelegramTargets: []string{"payments-alerts"}},
			}},
		},
	}
	project := &sentryv1alpha1.SentryProject{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments", UID: types.UID("project-uid"), Generation: 3, ResourceVersion: "1"},
		Spec: sentryv1alpha1.SentryProjectSpec{
			Sentry: "company", Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}},
			NotificationTargets: &sentryv1alpha1.NotificationTargets{Telegram: []string{"payments-alerts"}},
		},
	}
	flattened, _ := project.Spec.NotificationTargets.Flatten()
	_, selectorHash, err := destination.SelectorFingerprint(connection.Spec, flattened["telegram/payments-alerts"])
	if err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(connection, project).Build()
	store := &binding.Store{Client: k8sClient, Namespace: "system"}
	_, err = store.CreateBinding(context.Background(), binding.ProjectBinding{
		Version:    binding.CurrentVersion,
		Project:    binding.ProjectReference{Namespace: project.Namespace, Name: project.Name, UID: string(project.UID), Generation: project.Generation},
		Connection: connection.Name, Installation: connection.Spec.URL, Organization: connection.Spec.Organization, Slug: "checkout", Owner: "payments",
		Origin: binding.OriginManaged, AccessMode: binding.AccessManage, ExternalIdentity: "project-id",
		Marker: &binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-op"}, ManagerUID: string(project.UID), ClaimName: "claim",
		Destinations: map[string]binding.DestinationBinding{"telegram/payments-alerts": {
			Owner: binding.ChildOwner{Kind: binding.ChildOwnerProject, Name: project.Name, UID: string(project.UID)},
			Kind:  "telegram", SelectorHash: selectorHash, ResourceName: "payments-alerts", State: destination.StateRegistrationRequired,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 29, 22, 0, 0, 0, time.UTC)
	tokens := &fakeTokenReviewer{authenticated: true}
	access := &fakeAccessReviewer{allowed: true}
	handler := &ChallengeHandler{
		Tokens: tokens, Access: access, Client: k8sClient,
		Registration: &Service{
			Store: store, Index: &ChallengeIndexStore{Client: k8sClient, Namespace: "system"},
			Now: func() time.Time { return now }, Random: deterministicRandom,
		},
		Audiences: []string{"sentry-registration"}, Now: func() time.Time { return now },
	}
	return handler, tokens, access, store, now
}

type fakeTokenReviewer struct {
	authenticated bool
	token         string
	audiences     []string
}

func (f *fakeTokenReviewer) Create(_ context.Context, review *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	f.token = review.Spec.Token
	f.audiences = append([]string(nil), review.Spec.Audiences...)
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: f.authenticated, Audiences: append([]string(nil), review.Spec.Audiences...),
		User: authenticationv1.UserInfo{Username: "alice", UID: "alice-uid", Groups: []string{"developers"}, Extra: map[string]authenticationv1.ExtraValue{"scope": {"projects"}}},
	}}, nil
}

type fakeAccessReviewer struct {
	allowed bool
	calls   int
	review  *authorizationv1.SubjectAccessReview
}

func (f *fakeAccessReviewer) Create(_ context.Context, review *authorizationv1.SubjectAccessReview, _ metav1.CreateOptions) (*authorizationv1.SubjectAccessReview, error) {
	f.calls++
	f.review = review.DeepCopy()
	return &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: f.allowed, Denied: !f.allowed}}, nil
}
