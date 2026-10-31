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

package binding

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestCanonicalInstallationAndDeterministicClaimName(t *testing.T) {
	t.Parallel()

	aliases := []string{
		"https://SENTRY.example/",
		"https://sentry.example:443",
		"https://sentry.example/./",
	}
	var canonical string
	for _, alias := range aliases {
		value, err := CanonicalInstallation(alias)
		if err != nil {
			t.Fatalf("CanonicalInstallation(%q): %v", alias, err)
		}
		if canonical == "" {
			canonical = value
		} else if value != canonical {
			t.Fatalf("alias %q canonicalized as %q, want %q", alias, value, canonical)
		}
	}
	if ClaimName(canonical, "company", "checkout") != ClaimName("https://sentry.example", "company", "checkout") {
		t.Fatal("equivalent aliases produced different claim names")
	}
	if ClaimName(canonical, "company", "checkout") == ClaimName(canonical, "company", "other") {
		t.Fatal("different project slugs produced the same claim name")
	}
	httpAliases := []string{"http://SENTRY.example/", "http://sentry.example:80", "http://sentry.example/./"}
	for _, alias := range httpAliases {
		value, err := CanonicalInstallation(alias)
		if err != nil || value != "http://sentry.example" {
			t.Fatalf("HTTP alias %q canonicalized as %q, %v", alias, value, err)
		}
	}
	if ClaimName("http://sentry.example", "company", "checkout") == ClaimName("https://sentry.example", "company", "checkout") {
		t.Fatal("HTTP and HTTPS installations produced the same claim name")
	}
	for _, invalid := range []string{"ftp://sentry.example", "https://user:pass@sentry.example", "https://sentry.example/?token=secret"} {
		if _, err := CanonicalInstallation(invalid); err == nil {
			t.Errorf("accepted invalid installation URL %q", invalid)
		}
	}
}

func TestClaimCreateIsIdempotentForManagerAndConflictsForCompetitor(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	claim := testClaim("uid-one")
	first, created, err := store.AcquireClaim(context.Background(), claim)
	if err != nil || !created {
		t.Fatalf("first claim created=%v err=%v", created, err)
	}
	second, created, err := store.AcquireClaim(context.Background(), claim)
	if err != nil || created || second.Name != first.Name {
		t.Fatalf("idempotent claim created=%v record=%#v err=%v", created, second, err)
	}
	competitor := testClaim("uid-two")
	if _, _, err := store.AcquireClaim(context.Background(), competitor); !errors.Is(err, ErrOwnershipConflict) {
		t.Fatalf("competing claim error = %v", err)
	}
}

func TestBindingRequiresMarkerPairAndPinsExternalIdentity(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	marker := Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-operator"}
	binding := testBinding("uid-one", marker)
	record, err := store.CreateBinding(context.Background(), binding)
	if err != nil {
		t.Fatalf("CreateBinding: %v", err)
	}
	loaded, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || loaded.Name != record.Name || loaded.Binding.ExternalIdentity != "project-123" {
		t.Fatalf("GetBinding = %#v, %v", loaded, err)
	}
	if _, found, err := store.FindManaged(context.Background(), binding.Installation, binding.Organization, binding.Slug, binding.ExternalIdentity, marker); err != nil || !found {
		t.Fatalf("matching protected evidence found=%v err=%v", found, err)
	}
	foreignMarker := marker
	foreignMarker.Owner = "other"
	if _, found, err := store.FindManaged(context.Background(), binding.Installation, binding.Organization, binding.Slug, binding.ExternalIdentity, foreignMarker); err != nil || found {
		t.Fatalf("foreign marker found=%v err=%v", found, err)
	}
	if _, found, err := store.FindManaged(context.Background(), binding.Installation, binding.Organization, binding.Slug, "replacement-id", marker); err != nil || found {
		t.Fatalf("same-name replacement found=%v err=%v", found, err)
	}
}

func TestAdminHandoffRevokesSourceAndIsOneTime(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	claimRecord, _, err := store.AcquireClaim(context.Background(), testClaim("source-uid"))
	if err != nil {
		t.Fatal(err)
	}
	target := ProjectReference{Namespace: "payments", Name: "target", UID: "target-uid", Generation: 7}
	handed, err := store.Handoff(context.Background(), claimRecord, "source-uid", target)
	if err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	if handed.Claim.Manager.UID != target.UID || handed.Claim.Handoff == nil {
		t.Fatalf("handoff claim = %#v", handed.Claim)
	}
	if err := store.AuthorizeHandoff(handed, target, "checkout"); err != nil {
		t.Fatalf("AuthorizeHandoff: %v", err)
	}
	stale := target
	stale.Generation--
	if err := store.AuthorizeHandoff(handed, stale, "checkout"); !errors.Is(err, ErrHandoffDenied) {
		t.Fatalf("stale target authorization = %v", err)
	}
	completed, err := store.CompleteHandoff(context.Background(), handed, target, "checkout")
	if err != nil || completed.Claim.Handoff != nil {
		t.Fatalf("CompleteHandoff = %#v, %v", completed, err)
	}
	if err := store.AuthorizeHandoff(completed, target, "checkout"); !errors.Is(err, ErrHandoffDenied) {
		t.Fatalf("consumed authorization remained valid: %v", err)
	}
	if _, err := store.Handoff(context.Background(), completed, "source-uid", ProjectReference{Namespace: "payments", Name: "third", UID: "third-uid", Generation: 1}); !errors.Is(err, ErrHandoffDenied) {
		t.Fatalf("revoked source performed second handoff: %v", err)
	}
}

func TestBindingValidationRejectsExternalOwnerAndManagedWithoutMarker(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	external := testBinding("external-uid", Marker{})
	external.Origin = OriginExternal
	external.AccessMode = AccessReadOnly
	external.Owner = "payments"
	external.ManagerUID = ""
	external.Marker = nil
	external.ClaimName = ""
	if _, err := store.CreateBinding(context.Background(), external); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("external owner error = %v", err)
	}
	managed := testBinding("managed-uid", Marker{})
	managed.Marker = nil
	if _, err := store.CreateBinding(context.Background(), managed); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("managed without marker error = %v", err)
	}
}

func TestBindingValidationRejectsCorruptProtectedAlertState(t *testing.T) {
	t.Parallel()

	marker := Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-operator"}
	valid := testBinding("alert-uid", marker)
	valid.PinnedAdapters = map[string]string{"alerts": "legacy-issue-24.7.1"}
	valid.Alerts = map[string]AlertBinding{
		"new-issue": {
			Owner:   ChildOwner{Kind: ChildOwnerProject, Name: "checkout", UID: "alert-uid", Generation: 1},
			Adapter: "legacy-issue-24.7.1", ExternalIdentity: "alert-123", Trigger: "first-seen",
			DesiredHash: strings.Repeat("a", 64), Enabled: true, Destinations: []string{"telegram/chat"}, Marker: marker,
		},
	}
	if _, err := newTestStore(t).CreateBinding(context.Background(), valid); err != nil {
		t.Fatalf("valid alert binding: %v", err)
	}

	tests := map[string]func(*ProjectBinding){
		"invalid local key": func(value *ProjectBinding) {
			value.Alerts["Bad Key"] = value.Alerts["new-issue"]
			delete(value.Alerts, "new-issue")
		},
		"adapter drift": func(value *ProjectBinding) {
			alert := value.Alerts["new-issue"]
			alert.Adapter = "modern"
			value.Alerts["new-issue"] = alert
		},
		"malformed hash": func(value *ProjectBinding) {
			alert := value.Alerts["new-issue"]
			alert.DesiredHash = "not-a-digest"
			value.Alerts["new-issue"] = alert
		},
		"foreign marker": func(value *ProjectBinding) {
			alert := value.Alerts["new-issue"]
			alert.Marker.Owner = "foreign"
			value.Alerts["new-issue"] = alert
		},
		"duplicate destination": func(value *ProjectBinding) {
			alert := value.Alerts["new-issue"]
			alert.Destinations = []string{"chat", "chat"}
			value.Alerts["new-issue"] = alert
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Alerts = make(map[string]AlertBinding, len(valid.Alerts))
			for key, alert := range valid.Alerts {
				alert.Destinations = append([]string(nil), alert.Destinations...)
				candidate.Alerts[key] = alert
			}
			mutate(&candidate)
			if _, err := newTestStore(t).CreateBinding(context.Background(), candidate); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("corrupt alert state error = %v", err)
			}
		})
	}
}

func TestHasPolicyChildrenUsesExactOwnerUID(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	marker := Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-operator"}
	value := testBinding("project-uid", marker)
	value.Destinations = map[string]DestinationBinding{
		"slack/incidents": {
			Owner: ChildOwner{Kind: ChildOwnerPolicy, Name: "common-alerts", UID: "policy-uid", Generation: 3},
			Kind:  "slack", SelectorHash: "selector", ResourceName: "incidents", State: "Active",
		},
	}
	record, err := store.CreateBinding(context.Background(), value)
	if err != nil {
		t.Fatal(err)
	}
	hasChildren, err := store.HasPolicyChildren(context.Background(), "policy-uid")
	if err != nil || !hasChildren {
		t.Fatalf("policy children = %t, %v", hasChildren, err)
	}
	hasChildren, err = store.HasPolicyChildren(context.Background(), "recreated-policy-uid")
	if err != nil || hasChildren {
		t.Fatalf("same-name policy incarnation inherited children: %t, %v", hasChildren, err)
	}
	value.Destinations = nil
	if _, err := store.UpdateBinding(context.Background(), record, value); err != nil {
		t.Fatal(err)
	}
	hasChildren, err = store.HasPolicyChildren(context.Background(), "policy-uid")
	if err != nil || hasChildren {
		t.Fatalf("released policy children = %t, %v", hasChildren, err)
	}
}

func TestOperationJournalSurvivesStoreRecreationAndRejectsStaleRevision(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	planned := sentryapi.Operation{
		Key:             "payments/checkout/create",
		Kind:            "CreateProject",
		LocalResource:   "payments/checkout",
		IdempotencyHash: "f00d",
		Phase:           sentryapi.OperationPlanned,
		Revision:        1,
	}
	if err := store.CompareAndSwap(context.Background(), 0, planned); err != nil {
		t.Fatalf("create operation: %v", err)
	}
	restarted := &Store{Client: store.Client, Namespace: store.Namespace}
	loaded, found, err := restarted.Load(context.Background(), planned.Key)
	if err != nil || !found || loaded != planned {
		t.Fatalf("Load after restart = %#v, %v, %v", loaded, found, err)
	}
	submitted := planned
	submitted.Phase = sentryapi.OperationSubmitted
	submitted.Revision = 2
	if err := restarted.CompareAndSwap(context.Background(), 1, submitted); err != nil {
		t.Fatalf("advance operation: %v", err)
	}
	stale := submitted
	stale.Phase = sentryapi.OperationFailed
	stale.Revision = 2
	if err := restarted.CompareAndSwap(context.Background(), 1, stale); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("stale operation CAS error = %v", err)
	}
	if err := restarted.CompareAndSwap(context.Background(), 0, planned); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("duplicate operation create error = %v", err)
	}
}

func TestOperationJournalRejectsUnsafeTransitions(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	planned := sentryapi.Operation{
		Key: "alerts:update:checkout:rule", Kind: "UpdateAlert", LocalResource: "checkout/rule",
		IdempotencyHash: strings.Repeat("a", 64), Phase: sentryapi.OperationPlanned, Revision: 1,
	}
	submittedFirst := planned
	submittedFirst.Phase = sentryapi.OperationSubmitted
	if err := store.CompareAndSwap(context.Background(), 0, submittedFirst); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("journal created directly as Submitted: %v", err)
	}
	if err := store.CompareAndSwap(context.Background(), 0, planned); err != nil {
		t.Fatalf("create Planned: %v", err)
	}

	verifiedTooEarly := planned
	verifiedTooEarly.Phase = sentryapi.OperationVerified
	verifiedTooEarly.Revision = 2
	if err := store.CompareAndSwap(context.Background(), 1, verifiedTooEarly); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Planned -> Verified transition: %v", err)
	}
	changedKind := planned
	changedKind.Kind = "DeleteAlert"
	changedKind.Revision = 2
	if err := store.CompareAndSwap(context.Background(), 1, changedKind); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("operation kind mutation: %v", err)
	}

	submitted := planned
	submitted.Phase = sentryapi.OperationSubmitted
	submitted.Revision = 2
	if err := store.CompareAndSwap(context.Background(), 1, submitted); err != nil {
		t.Fatalf("Planned -> Submitted: %v", err)
	}
	replacedIntent := submitted
	replacedIntent.Phase = sentryapi.OperationVerified
	replacedIntent.IdempotencyHash = strings.Repeat("b", 64)
	replacedIntent.Revision = 3
	if err := store.CompareAndSwap(context.Background(), 2, replacedIntent); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Submitted hash replacement: %v", err)
	}
	verified := submitted
	verified.Phase = sentryapi.OperationVerified
	verified.Revision = 3
	if err := store.CompareAndSwap(context.Background(), 2, verified); err != nil {
		t.Fatalf("Submitted -> Verified: %v", err)
	}

	next := verified
	next.IdempotencyHash = strings.Repeat("b", 64)
	next.Phase = sentryapi.OperationPlanned
	next.Revision = 4
	if err := store.CompareAndSwap(context.Background(), 3, next); err != nil {
		t.Fatalf("Verified -> new Planned: %v", err)
	}
	invalidPhase := next
	invalidPhase.Phase = sentryapi.OperationPhase("NativeUnknown")
	invalidPhase.Revision = 5
	if err := store.CompareAndSwap(context.Background(), 4, invalidPhase); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("unknown operation phase: %v", err)
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Store{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "operator"}
}

func testClaim(uid string) ManagerClaim {
	return ManagerClaim{
		Version:      CurrentVersion,
		Installation: "https://sentry.example",
		Organization: "company",
		Slug:         "checkout",
		Owner:        "payments",
		Manager: ProjectReference{
			Namespace:  "payments",
			Name:       "checkout",
			UID:        uid,
			Generation: 1,
		},
	}
}

func testBinding(uid string, marker Marker) ProjectBinding {
	return ProjectBinding{
		Version:          CurrentVersion,
		Project:          ProjectReference{Namespace: "payments", Name: "checkout", UID: uid, Generation: 1},
		Connection:       "main",
		Installation:     "https://sentry.example",
		Organization:     "company",
		Slug:             "checkout",
		Owner:            "payments",
		Origin:           OriginManaged,
		AccessMode:       AccessManage,
		ExternalIdentity: "project-123",
		Marker:           &marker,
		ManagerUID:       uid,
		ClaimName:        ClaimName("https://sentry.example", "company", "checkout"),
	}
}
