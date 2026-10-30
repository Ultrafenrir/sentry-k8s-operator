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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
)

func TestProjectControllerRecoversPendingRegistrationWithoutSentryClient(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := &binding.Store{Client: k8sClient, Namespace: "state"}
	reference := binding.ProjectReference{Namespace: "payments", Name: "checkout", UID: "project-uid", Generation: 3}
	challenge := binding.TelegramChallenge{
		CodeHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Project:  reference, Destination: "telegram", SelectorHash: "selector", Owner: "payments", Connection: "company",
		PolicyGeneration: 7, PolicyResourceVersion: "12", ExpiresAt: time.Now().Add(10 * time.Minute),
	}
	_, err := store.CreateBinding(context.Background(), binding.ProjectBinding{
		Version: binding.CurrentVersion, Project: reference, Connection: "company", Installation: "https://sentry.example",
		Organization: "company", Slug: "checkout", Owner: "payments", Origin: binding.OriginManaged, AccessMode: binding.AccessManage,
		ExternalIdentity: "project-id", Marker: &binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "operator"},
		ManagerUID: reference.UID, ClaimName: "claim",
		Destinations: map[string]binding.DestinationBinding{"telegram": {
			Kind: "telegram", SelectorHash: "selector", State: "RegistrationRequired",
			Telegram: &binding.TelegramBinding{
				Challenge: &challenge, Target: &binding.TelegramTarget{ChatIdentity: "-100123"},
				Phase: registration.PhasePendingPublish, OutboxKey: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
				FenceHash: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210",
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &recoveryPublisher{fail: true}
	service := &registration.Service{Store: store, Publisher: publisher, Fences: recoveryFences{}, Audit: controllerAuditSink{}}
	reconciler := &SentryProjectReconciler{RegistrationRecovery: service}
	resource := &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{UID: types.UID(reference.UID)}}

	attempted, err := reconciler.resumePendingRegistration(context.Background(), resource)
	if !attempted || !errors.Is(err, registration.ErrPublishPending) {
		t.Fatalf("attempted=%t err=%v", attempted, err)
	}
	publisher.fail = false
	attempted, err = reconciler.resumePendingRegistration(context.Background(), resource)
	if !attempted || err != nil {
		t.Fatalf("attempted=%t err=%v", attempted, err)
	}
	record, err := store.GetBinding(context.Background(), reference.UID)
	if err != nil {
		t.Fatal(err)
	}
	telegram := record.Binding.Destinations["telegram"].Telegram
	if record.Binding.Destinations["telegram"].State != "Active" || telegram == nil || telegram.Challenge != nil || telegram.Phase != "" || publisher.calls != 2 {
		t.Fatalf("binding=%#v calls=%d", record.Binding.Destinations["telegram"], publisher.calls)
	}
	attempted, err = reconciler.resumePendingRegistration(context.Background(), resource)
	if attempted || err != nil {
		t.Fatalf("post-recovery attempted=%t err=%v", attempted, err)
	}
}

func TestProjectControllerRevokesAndRestoresExactTelegramRoute(t *testing.T) {
	t.Parallel()

	routes := &recoveryRoutes{}
	reconciler := &SentryProjectReconciler{RegistrationRoutes: routes, DestinationAudit: &destinationAuditRecorder{}}
	resource := &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "checkout", Generation: 3}}
	key := strings.Repeat("a", 64)
	target := binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"}
	result := destination.Result{
		Bindings: map[string]binding.DestinationBinding{"telegram": {
			Kind: "telegram", State: destination.StateRevoked,
			Telegram: &binding.TelegramBinding{Target: &target, RouteKey: key},
		}},
		Statuses: map[string]destination.Status{"telegram": {Kind: "telegram", State: destination.StateRevoked}},
	}
	if err := reconciler.reconcileTelegramRoutes(context.Background(), resource, nil, &result, nil); err != nil {
		t.Fatal(err)
	}
	if len(routes.revoked) != 1 || routes.revoked[0] != key || !result.Bindings["telegram"].Telegram.RouteRevoked {
		t.Fatalf("routes=%#v binding=%#v", routes, result.Bindings["telegram"])
	}

	restore := result.Bindings["telegram"]
	restore.State = destination.StateRestorePending
	result.Bindings["telegram"] = restore
	if err := reconciler.reconcileTelegramRoutes(context.Background(), resource, nil, &result, nil); err != nil {
		t.Fatal(err)
	}
	got := result.Bindings["telegram"]
	if len(routes.published) != 1 || routes.published[0] != key || got.State != destination.StateActive || got.Telegram.RouteRevoked || *got.Telegram.Target != target {
		t.Fatalf("routes=%#v binding=%#v", routes, got)
	}
	if err := reconciler.reconcileTelegramRoutes(context.Background(), resource, nil, &result, nil); err != nil {
		t.Fatal(err)
	}
	if len(routes.published) != 2 || routes.published[1] != key {
		t.Fatalf("active route was not idempotently verified: %#v", routes)
	}
}

func TestProjectControllerFailsClosedWhenLegacyActiveRouteKeyIsMissing(t *testing.T) {
	t.Parallel()

	reconciler := &SentryProjectReconciler{RegistrationRoutes: &recoveryRoutes{}}
	result := destination.Result{Bindings: map[string]binding.DestinationBinding{"telegram": {
		Kind: "telegram", State: destination.StateCleanupPending,
		Telegram: &binding.TelegramBinding{Target: &binding.TelegramTarget{ChatIdentity: "-100123"}},
	}}}
	if err := reconciler.reconcileTelegramRoutes(context.Background(), nil, nil, &result, map[string]bool{"telegram": true}); !errors.Is(err, registration.ErrRouteIdentityLost) {
		t.Fatalf("error=%v", err)
	}
}

func TestProjectControllerDeletesExactChallengeIndexBeforeRevokedBindingCAS(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	indexes := &registration.ChallengeIndexStore{Client: k8sClient, Namespace: "state"}
	challenge := &binding.TelegramChallenge{
		CodeHash: strings.Repeat("c", 64), Project: binding.ProjectReference{UID: "project-uid"},
		Destination: "telegram", ExpiresAt: time.Now().Add(time.Minute),
	}
	index := registration.ChallengeIndex{
		Version: binding.CurrentVersion, CodeHash: challenge.CodeHash, ProjectUID: challenge.Project.UID,
		Destination: challenge.Destination, ExpiresAt: challenge.ExpiresAt,
	}
	if err := indexes.Create(context.Background(), index); err != nil {
		t.Fatal(err)
	}
	current := map[string]binding.DestinationBinding{"telegram": {
		Kind: "telegram", State: destination.StateRegistrationRequired,
		Telegram: &binding.TelegramBinding{Challenge: challenge},
	}}
	result := destination.Result{Bindings: map[string]binding.DestinationBinding{"telegram": {
		Kind: "telegram", State: destination.StateRevoked,
		Telegram: &binding.TelegramBinding{Challenge: challenge},
	}}}
	reconciler := &SentryProjectReconciler{RegistrationIndexes: indexes}
	if err := reconciler.reconcileTelegramRoutes(context.Background(), nil, current, &result, nil); err != nil {
		t.Fatal(err)
	}
	if result.Bindings["telegram"].Telegram.Challenge != nil {
		t.Fatal("revoked binding retained usable challenge")
	}
	if _, err := indexes.Get(context.Background(), challenge.CodeHash); !apierrors.IsNotFound(err) {
		t.Fatalf("index after revoke=%v", err)
	}
}

func TestProjectControllerDestinationCleanupAuditFailsClosedAndRecoversDeletedRoute(t *testing.T) {
	t.Parallel()

	key := strings.Repeat("a", 64)
	target := binding.TelegramTarget{ChatIdentity: "-100123456789", TopicIdentity: "42"}
	routes := &observableRecoveryRoutes{routes: map[string]binding.TelegramTarget{key: target}}
	auditSink := &destinationAuditRecorder{failAt: 1}
	reconciler := &SentryProjectReconciler{RegistrationRoutes: routes, DestinationAudit: auditSink}
	resource := &sentryv1alpha1.SentryProject{ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "checkout", Generation: 9}}
	newResult := func() destination.Result {
		return destination.Result{Bindings: map[string]binding.DestinationBinding{"telegram": {
			Kind: "telegram", State: destination.StateCleanupPending,
			Telegram: &binding.TelegramBinding{Target: &target, RouteKey: key},
		}}}
	}

	result := newResult()
	if err := reconciler.reconcileTelegramRoutes(context.Background(), resource, nil, &result, map[string]bool{"telegram": true}); !errors.Is(err, errDestinationAuditUnavailable) {
		t.Fatalf("pre-mutation audit error=%v", err)
	}
	if routes.revokeCalls != 0 || result.Bindings["telegram"].Telegram.RouteRevoked {
		t.Fatalf("audit outage mutated route: calls=%d binding=%#v", routes.revokeCalls, result.Bindings["telegram"])
	}

	auditSink.failAt = 2
	auditSink.calls = 0
	auditSink.events = nil
	result = newResult()
	if err := reconciler.reconcileTelegramRoutes(context.Background(), resource, nil, &result, map[string]bool{"telegram": true}); !errors.Is(err, errDestinationAuditUnavailable) {
		t.Fatalf("post-mutation audit error=%v", err)
	}
	if routes.revokeCalls != 1 || routes.routes[key] != (binding.TelegramTarget{}) || result.Bindings["telegram"].Telegram.RouteRevoked {
		t.Fatalf("post-mutation boundary calls=%d routes=%#v binding=%#v", routes.revokeCalls, routes.routes, result.Bindings["telegram"])
	}

	auditSink.failAt = 0
	auditSink.calls = 0
	auditSink.events = nil
	if err := reconciler.reconcileTelegramRoutes(context.Background(), resource, nil, &result, map[string]bool{"telegram": true}); err != nil {
		t.Fatal(err)
	}
	if routes.revokeCalls != 1 || !result.Bindings["telegram"].Telegram.RouteRevoked || len(auditSink.events) != 1 || auditSink.events[0].Outcome != audit.OutcomeRecovered || auditSink.events[0].Reason != audit.ReasonResultRecovered {
		t.Fatalf("recovery routes=%#v events=%#v binding=%#v", routes, auditSink.events, result.Bindings["telegram"])
	}
	payload, err := json.Marshal(auditSink.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{target.ChatIdentity, target.TopicIdentity, key} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("protected route identity leaked into audit: %s", payload)
		}
	}
}

type recoveryPublisher struct {
	calls int
	fail  bool
}

func (p *recoveryPublisher) PublishTelegramRoute(context.Context, string, binding.TelegramTarget) error {
	p.calls++
	if p.fail {
		return errors.New("unavailable")
	}
	return nil
}

type recoveryFences struct{}

func (recoveryFences) Begin(context.Context, binding.TelegramChallenge, string) error { return nil }
func (recoveryFences) Clear(context.Context, binding.TelegramChallenge, string) error { return nil }

type recoveryRoutes struct {
	published []string
	revoked   []string
}

type observableRecoveryRoutes struct {
	routes      map[string]binding.TelegramTarget
	revokeCalls int
}

func (r *observableRecoveryRoutes) PublishTelegramRoute(_ context.Context, key string, target binding.TelegramTarget) error {
	r.routes[key] = target
	return nil
}

func (r *observableRecoveryRoutes) GetTelegramRoute(_ context.Context, key string) (binding.TelegramTarget, error) {
	target, found := r.routes[key]
	if !found || target == (binding.TelegramTarget{}) {
		return binding.TelegramTarget{}, apierrors.NewNotFound(corev1.Resource("secrets"), key)
	}
	return target, nil
}

func (r *observableRecoveryRoutes) RevokeTelegramRoute(_ context.Context, key string, expected binding.TelegramTarget) error {
	current, found := r.routes[key]
	if !found || current == (binding.TelegramTarget{}) {
		return nil
	}
	if current != expected {
		return registration.ErrRouteConflict
	}
	r.revokeCalls++
	delete(r.routes, key)
	return nil
}

type destinationAuditRecorder struct {
	calls  int
	failAt int
	events []audit.Event
}

func (r *destinationAuditRecorder) Record(_ context.Context, event audit.Event) error {
	r.calls++
	if r.calls == r.failAt {
		return errors.New("audit unavailable")
	}
	r.events = append(r.events, event)
	return nil
}

func (r *recoveryRoutes) PublishTelegramRoute(_ context.Context, key string, _ binding.TelegramTarget) error {
	r.published = append(r.published, key)
	return nil
}

func (r *recoveryRoutes) RevokeTelegramRoute(_ context.Context, key string, _ binding.TelegramTarget) error {
	r.revoked = append(r.revoked, key)
	return nil
}
