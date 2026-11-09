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
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

func TestChallengeIsHashedAndReplaySafe(t *testing.T) {
	t.Parallel()

	store := registrationStore(t)
	reference := registrationBinding(t, store)
	now := time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	publisher := &fakePublisher{}
	auditSink := &registrationAuditSink{}
	service := &Service{Store: store, Publisher: publisher, Audit: auditSink, Now: func() time.Time { return now }, Random: deterministicRandom}
	code, err := service.Issue(context.Background(), issueInput(reference))
	if err != nil {
		t.Fatal(err)
	}
	if code == "" {
		t.Fatal("empty one-time code")
	}
	secret := &corev1.Secret{}
	if err := store.Client.Get(context.Background(), types.NamespacedName{Namespace: "system", Name: binding.BindingName(reference.UID)}, secret); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(secret.Data["state.json"]), code) {
		t.Fatal("raw one-time code was persisted")
	}
	verified := 0
	err = service.Redeem(context.Background(), reference.UID, "telegram/payments-alerts", code, binding.TelegramTarget{ChatIdentity: "private-chat", TopicIdentity: "private-topic"}, func(_ context.Context, challenge binding.TelegramChallenge) error {
		verified++
		if challenge.Project != reference || challenge.PolicyGeneration != 7 || challenge.PolicyResourceVersion != "policy-rv" {
			t.Fatalf("challenge=%#v", challenge)
		}
		return nil
	})
	if err != nil || verified != 1 || publisher.calls != 1 {
		t.Fatalf("redeem err=%v verified=%d publish=%d", err, verified, publisher.calls)
	}
	if len(auditSink.events) != 2 || auditSink.events[0].Outcome != audit.OutcomeStarted || auditSink.events[1].Outcome != audit.OutcomeSucceeded {
		t.Fatalf("audit events=%#v", auditSink.events)
	}
	payload, err := json.Marshal(auditSink.events)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private-chat", "private-topic", code, hash(code), "project-id"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("registration audit leaked %q: %s", forbidden, payload)
		}
	}
	record, err := store.GetBinding(context.Background(), reference.UID)
	if err != nil {
		t.Fatal(err)
	}
	destination := record.Binding.Destinations["telegram/payments-alerts"]
	if destination.State != "Active" || destination.Telegram == nil || destination.Telegram.Challenge != nil || destination.Telegram.Target.ChatIdentity != "private-chat" || destination.Telegram.RouteKey == "" || destination.Telegram.RouteRevoked {
		t.Fatalf("destination=%#v", destination)
	}
	if err := service.Redeem(context.Background(), reference.UID, "telegram/payments-alerts", code, binding.TelegramTarget{ChatIdentity: "other"}, func(context.Context, binding.TelegramChallenge) error { return nil }); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("replay err=%v", err)
	}
}

func TestPublishFailureLeavesResumableOutbox(t *testing.T) {
	t.Parallel()

	store := registrationStore(t)
	reference := registrationBinding(t, store)
	publisher := &fakePublisher{fail: true}
	service := &Service{Store: store, Publisher: publisher, Audit: &registrationAuditSink{}, Now: func() time.Time { return time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC) }, Random: deterministicRandom}
	code, err := service.Issue(context.Background(), issueInput(reference))
	if err != nil {
		t.Fatal(err)
	}
	err = service.Redeem(context.Background(), reference.UID, "telegram/payments-alerts", code, binding.TelegramTarget{ChatIdentity: "private-chat"}, func(context.Context, binding.TelegramChallenge) error { return nil })
	if !errors.Is(err, ErrPublishPending) {
		t.Fatalf("redeem err=%v", err)
	}
	if err := service.Redeem(context.Background(), reference.UID, "telegram/payments-alerts", code, binding.TelegramTarget{ChatIdentity: "other-chat"}, func(context.Context, binding.TelegramChallenge) error { return nil }); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("pending target replacement err=%v", err)
	}
	if _, err := service.Issue(context.Background(), issueInput(reference)); !errors.Is(err, ErrRegistrationRequired) {
		t.Fatalf("pending registration was overwritten: %v", err)
	}
	record, _ := store.GetBinding(context.Background(), reference.UID)
	if record.Binding.Destinations["telegram/payments-alerts"].Telegram.Phase != PhasePendingPublish {
		t.Fatalf("pending state=%#v", record.Binding.Destinations["telegram/payments-alerts"])
	}
	publisher.fail = false
	if err := service.Resume(context.Background(), reference.UID, "telegram/payments-alerts"); err != nil {
		t.Fatal(err)
	}
	record, _ = store.GetBinding(context.Background(), reference.UID)
	if record.Binding.Destinations["telegram/payments-alerts"].State != "Active" {
		t.Fatalf("resumed state=%#v", record.Binding.Destinations["telegram/payments-alerts"])
	}
}

func TestIndexedCodeRedemptionHidesProjectAndResumesSamePendingTarget(t *testing.T) {
	t.Parallel()

	store := registrationStore(t)
	reference := registrationBinding(t, store)
	now := time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC)
	publisher := &fakePublisher{fail: true}
	service := &Service{
		Store: store, Index: &ChallengeIndexStore{Client: store.Client, Namespace: store.Namespace},
		Publisher: publisher, Fences: noOpFences{}, Audit: &registrationAuditSink{}, Now: func() time.Time { return now }, Random: deterministicRandom,
	}
	code, err := service.Issue(context.Background(), issueInput(reference))
	if err != nil {
		t.Fatal(err)
	}
	index, err := service.Index.Get(context.Background(), hash(code))
	if err != nil || index.ProjectUID != reference.UID || index.Destination != "telegram/payments-alerts" {
		t.Fatalf("index=%#v err=%v", index, err)
	}
	target := binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"}
	if err := service.RedeemCode(context.Background(), code, target); !errors.Is(err, ErrPublishPending) {
		t.Fatalf("first redeem=%v", err)
	}
	if err := service.RedeemCode(context.Background(), code, binding.TelegramTarget{ChatIdentity: "-100999"}); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("target substitution=%v", err)
	}
	publisher.fail = false
	if err := service.RedeemCode(context.Background(), code, target); err != nil {
		t.Fatalf("resume=%v", err)
	}
	if _, err := service.Index.Get(context.Background(), hash(code)); !apierrors.IsNotFound(err) {
		t.Fatalf("consumed index=%v", err)
	}
	if err := service.RedeemCode(context.Background(), code, target); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("replay=%v", err)
	}
}

func TestLiveVerificationFailureDoesNotCommitTarget(t *testing.T) {
	t.Parallel()

	store := registrationStore(t)
	reference := registrationBinding(t, store)
	service := &Service{Store: store, Publisher: &fakePublisher{}, Audit: &registrationAuditSink{}, Now: func() time.Time { return time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC) }, Random: deterministicRandom}
	code, _ := service.Issue(context.Background(), issueInput(reference))
	if err := service.Redeem(context.Background(), reference.UID, "telegram/payments-alerts", code, binding.TelegramTarget{ChatIdentity: "private-chat"}, func(context.Context, binding.TelegramChallenge) error { return errors.New("generation changed") }); !errors.Is(err, ErrLiveVerification) {
		t.Fatalf("redeem err=%v", err)
	}
	record, _ := store.GetBinding(context.Background(), reference.UID)
	if record.Binding.Destinations["telegram/payments-alerts"].Telegram.Target != nil || record.Binding.Destinations["telegram/payments-alerts"].State != "RegistrationRequired" {
		t.Fatalf("target committed after failed verification: %#v", record.Binding.Destinations["telegram/payments-alerts"])
	}
}

func TestConcurrentRedemptionCommitsOneTarget(t *testing.T) {
	t.Parallel()

	store := registrationStore(t)
	reference := registrationBinding(t, store)
	// controller-runtime's fake client accepts concurrent stale Updates. Wrap it
	// with the resourceVersion precondition that the real API server enforces;
	// the envtest controller spec independently exercises the real CAS path.
	store.Client = &resourceVersionClient{Client: store.Client}
	publisher := &fakePublisher{}
	service := &Service{Store: store, Publisher: publisher, Audit: &registrationAuditSink{}, Now: func() time.Time { return time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC) }, Random: deterministicRandom}
	code, err := service.Issue(context.Background(), issueInput(reference))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, target := range []string{"chat-one", "chat-two"} {
		go func(target string) {
			<-start
			errs <- service.Redeem(context.Background(), reference.UID, "telegram/payments-alerts", code, binding.TelegramTarget{ChatIdentity: target}, func(context.Context, binding.TelegramChallenge) error { return nil })
		}(target)
	}
	close(start)
	successes := 0
	for range 2 {
		if err := <-errs; err == nil {
			successes++
		}
	}
	if successes != 1 || publisher.uniqueCount() != 1 {
		t.Fatalf("successes=%d publishCalls=%d uniquePublications=%d", successes, publisher.count(), publisher.uniqueCount())
	}
	record, err := store.GetBinding(context.Background(), reference.UID)
	if err != nil {
		t.Fatal(err)
	}
	target := record.Binding.Destinations["telegram/payments-alerts"].Telegram.Target.ChatIdentity
	if target != "chat-one" && target != "chat-two" {
		t.Fatalf("committed target=%q", target)
	}
}

func TestAuditFailureAfterRoutePublishKeepsResumablePendingState(t *testing.T) {
	t.Parallel()

	store := registrationStore(t)
	reference := registrationBinding(t, store)
	publisher := &fakePublisher{}
	auditSink := &registrationAuditSink{failAt: 2}
	service := &Service{
		Store: store, Publisher: publisher, Audit: auditSink,
		Now: func() time.Time { return time.Date(2026, 10, 29, 20, 0, 0, 0, time.UTC) }, Random: deterministicRandom,
	}
	code, err := service.Issue(context.Background(), issueInput(reference))
	if err != nil {
		t.Fatal(err)
	}
	target := binding.TelegramTarget{ChatIdentity: "private-chat"}
	if err := service.Redeem(context.Background(), reference.UID, "telegram/payments-alerts", code, target, func(context.Context, binding.TelegramChallenge) error { return nil }); !errors.Is(err, ErrAuditUnavailable) {
		t.Fatalf("redeem error=%v", err)
	}
	record, err := store.GetBinding(context.Background(), reference.UID)
	if err != nil {
		t.Fatal(err)
	}
	telegram := record.Binding.Destinations["telegram/payments-alerts"].Telegram
	if telegram.Phase != PhasePendingFenceRelease || record.Binding.Destinations["telegram/payments-alerts"].State != "RegistrationRequired" || publisher.uniqueCount() != 1 {
		t.Fatalf("pending binding=%#v unique routes=%d", record.Binding.Destinations["telegram/payments-alerts"], publisher.uniqueCount())
	}
	auditSink.failAt = 0
	if err := service.Resume(context.Background(), reference.UID, "telegram/payments-alerts"); err != nil {
		t.Fatal(err)
	}
	record, err = store.GetBinding(context.Background(), reference.UID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Binding.Destinations["telegram/payments-alerts"].State != "Active" || publisher.uniqueCount() != 1 {
		t.Fatalf("recovered binding=%#v unique routes=%d", record.Binding.Destinations["telegram/payments-alerts"], publisher.uniqueCount())
	}
}

func registrationStore(t *testing.T) *binding.Store {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &binding.Store{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "system"}
}

func registrationBinding(t *testing.T, store *binding.Store) binding.ProjectReference {
	t.Helper()
	reference := binding.ProjectReference{Namespace: "payments", Name: "checkout", UID: "project-uid", Generation: 3}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-op"}
	_, err := store.CreateBinding(context.Background(), binding.ProjectBinding{
		Version: binding.CurrentVersion, Project: reference, Connection: "company", Installation: "https://sentry.example", Organization: "company", Slug: "checkout", Owner: "payments", Origin: binding.OriginManaged, AccessMode: binding.AccessManage, ExternalIdentity: "project-id", Marker: &marker, ManagerUID: reference.UID, ClaimName: "claim",
		Destinations: map[string]binding.DestinationBinding{"telegram/payments-alerts": {
			Owner: binding.ChildOwner{Kind: binding.ChildOwnerProject, Name: reference.Name, UID: reference.UID},
			Kind:  "telegram", SelectorHash: "selector-hash", ResourceName: "payments-alerts", State: "RegistrationRequired",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return reference
}

func issueInput(reference binding.ProjectReference) IssueInput {
	return IssueInput{Project: reference, Destination: "telegram/payments-alerts", Owner: "payments", Connection: "company", PolicyGeneration: 7, PolicyResourceVersion: "policy-rv", TTL: 10 * time.Minute}
}

func deterministicRandom(buffer []byte) (int, error) {
	for i := range buffer {
		buffer[i] = byte(i + 1)
	}
	return len(buffer), nil
}

type fakePublisher struct {
	mu           sync.Mutex
	calls        int
	fail         bool
	publications map[string]binding.TelegramTarget
}

type resourceVersionClient struct {
	client.Client
	mu       sync.Mutex
	versions map[client.ObjectKey]string
}

func (c *resourceVersionClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Client.Get(ctx, key, object, options...); err != nil {
		return err
	}
	if c.versions == nil {
		c.versions = make(map[client.ObjectKey]string)
	}
	version, found := c.versions[key]
	if !found {
		version = "test:" + object.GetResourceVersion()
		c.versions[key] = version
	}
	object.SetResourceVersion(version)
	return nil
}

func (c *resourceVersionClient) Update(ctx context.Context, object client.Object, options ...client.UpdateOption) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := client.ObjectKeyFromObject(object)
	current := &corev1.Secret{}
	if err := c.Client.Get(ctx, key, current); err != nil {
		return err
	}
	if c.versions == nil {
		c.versions = make(map[client.ObjectKey]string)
	}
	version, found := c.versions[key]
	if !found {
		version = "test:" + current.ResourceVersion
		c.versions[key] = version
	}
	if object.GetResourceVersion() != version {
		return apierrors.NewConflict(corev1.Resource("secrets"), object.GetName(), errors.New("stale resourceVersion"))
	}
	object.SetResourceVersion(current.ResourceVersion)
	if err := c.Client.Update(ctx, object, options...); err != nil {
		object.SetResourceVersion(version)
		return err
	}
	version += ".next"
	c.versions[key] = version
	object.SetResourceVersion(version)
	return nil
}

type registrationAuditSink struct {
	mu     sync.Mutex
	fail   bool
	failAt int
	calls  int
	events []audit.Event
}

func (s *registrationAuditSink) Record(_ context.Context, event audit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail || (s.failAt > 0 && s.calls == s.failAt) {
		return errors.New("audit unavailable")
	}
	if err := audit.Validate(event); err != nil {
		return err
	}
	s.events = append(s.events, event)
	return nil
}

type noOpFences struct{}

func (noOpFences) Begin(context.Context, binding.TelegramChallenge, string) error { return nil }
func (noOpFences) Clear(context.Context, binding.TelegramChallenge, string) error { return nil }

func (p *fakePublisher) PublishTelegramRoute(_ context.Context, key string, target binding.TelegramTarget) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.fail {
		return errors.New("routing unavailable")
	}
	if p.publications == nil {
		p.publications = make(map[string]binding.TelegramTarget)
	}
	if current, found := p.publications[key]; found && current != target {
		return errors.New("routing conflict")
	}
	p.publications[key] = target
	return nil
}

func (p *fakePublisher) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *fakePublisher) uniqueCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.publications)
}
