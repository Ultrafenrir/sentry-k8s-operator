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

package sentryhook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestLifecycleCreatesPublishesAndRotatesObservedSecret(t *testing.T) {
	t.Parallel()
	engine, adapter, record := lifecycleTestEngine(t)
	input := lifecycleInput(record)

	created, err := engine.Ensure(context.Background(), input)
	if err != nil || created.Binding.ServiceHook == nil || adapter.createCalls != 1 {
		t.Fatalf("created=%#v calls=%d err=%v", created.Binding.ServiceHook, adapter.createCalls, err)
	}
	remote := adapter.hooks["hook-id-1"]
	if len(adapter.createEndpoints) != 1 || adapter.createEndpoints[0] == created.Binding.ServiceHook.EndpointURL || remote.URL != created.Binding.ServiceHook.EndpointURL || !remote.Active {
		t.Fatalf("createEndpoints=%v remote=%#v protected=%#v", adapter.createEndpoints, remote, created.Binding.ServiceHook)
	}
	firstHash := created.Binding.ServiceHook.SecretHash
	payload, err := engine.Verifier.Get(context.Background(), created.Binding.ServiceHook.SourceBindingKey)
	if err != nil {
		t.Fatal(err)
	}
	configuration, secrets, err := decodeVerifierConfig(payload)
	if err != nil || configuration.ExpectedProjectSlug != "checkout" || len(secrets) != 1 || !bytes.Equal(secrets[0], []byte("service-hook-secret-1")) {
		t.Fatalf("configuration=%#v secrets=%q err=%v", configuration, secrets, err)
	}

	remote.Secret = []byte("rotated-service-hook-secret")
	adapter.hooks["hook-id-1"] = remote
	input.Binding = created
	rotated, err := engine.Ensure(context.Background(), input)
	if err != nil || rotated.Binding.ServiceHook.SecretHash == firstHash {
		t.Fatalf("rotated=%#v err=%v", rotated.Binding.ServiceHook, err)
	}
	payload, err = engine.Verifier.Get(context.Background(), rotated.Binding.ServiceHook.SourceBindingKey)
	if err != nil {
		t.Fatal(err)
	}
	_, secrets, err = decodeVerifierConfig(payload)
	if err != nil || len(secrets) != 1 || !bytes.Equal(secrets[0], remote.Secret) {
		t.Fatalf("rotated secrets=%q err=%v", secrets, err)
	}
}

func TestLifecycleRecoversLostCreateResponseWithoutSecondCreate(t *testing.T) {
	t.Parallel()
	engine, adapter, record := lifecycleTestEngine(t)
	adapter.loseCreateResponse = true
	input := lifecycleInput(record)
	if _, err := engine.Ensure(context.Background(), input); err == nil || adapter.createCalls != 1 {
		t.Fatalf("lost response err=%v calls=%d", err, adapter.createCalls)
	}
	pending, err := engine.Bindings.GetBinding(context.Background(), record.Binding.Project.UID)
	if err != nil || pending.Binding.ServiceHook == nil || pending.Binding.ServiceHook.Pending == nil {
		t.Fatalf("pending=%#v err=%v", pending.Binding.ServiceHook, err)
	}
	adapter.loseCreateResponse = false
	input.Binding = pending
	recovered, err := engine.Ensure(context.Background(), input)
	if err != nil || recovered.Binding.ServiceHook.ExternalIdentity != "hook-id-1" || adapter.createCalls != 1 {
		t.Fatalf("recovered=%#v calls=%d err=%v", recovered.Binding.ServiceHook, adapter.createCalls, err)
	}
}

func TestLifecycleUnpublishesBeforeExactDeleteAndRecoversLostResponse(t *testing.T) {
	t.Parallel()
	engine, adapter, record := lifecycleTestEngine(t)
	input := lifecycleInput(record)
	created, err := engine.Ensure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	adapter.loseDeleteResponse = true
	input.Binding = created
	removed, err := engine.Remove(context.Background(), input)
	if err != nil || removed.Binding.ServiceHook != nil || adapter.deleteCalls != 1 {
		t.Fatalf("removed=%#v calls=%d err=%v", removed.Binding.ServiceHook, adapter.deleteCalls, err)
	}
	if _, found := adapter.hooks["hook-id-1"]; found {
		t.Fatal("exact service hook remains after removal")
	}
	if _, err := engine.Verifier.Get(context.Background(), created.Binding.ServiceHook.SourceBindingKey); !apierrors.IsNotFound(err) {
		t.Fatalf("verifier remains after removal: %v", err)
	}
	if _, err := engine.Sources.Get(context.Background(), created.Binding.ServiceHook.SourceBindingKey); !apierrors.IsNotFound(err) {
		t.Fatalf("source remains after removal: %v", err)
	}
}

func TestLifecycleRotatesThroughUnpublishedStagingEndpointBeforeDeletingOldHook(t *testing.T) {
	t.Parallel()
	engine, adapter, record := lifecycleTestEngine(t)
	input := lifecycleInput(record)
	created, err := engine.Ensure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	input.Binding = created
	input.Revision = 2
	rotated, err := engine.Ensure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	protected := rotated.Binding.ServiceHook
	if protected == nil || protected.Revision != 2 || protected.ExternalIdentity != "hook-id-2" || protected.Pending != nil {
		t.Fatalf("rotated binding=%#v", protected)
	}
	if adapter.createCalls != 2 || adapter.deleteCalls != 1 || len(adapter.createActive) != 2 || adapter.createActive[0] || adapter.createActive[1] {
		t.Fatalf("createCalls=%d deleteCalls=%d createActive=%v", adapter.createCalls, adapter.deleteCalls, adapter.createActive)
	}
	if _, found := adapter.hooks["hook-id-1"]; found {
		t.Fatal("old service hook remains after rotation")
	}
	replacement, found := adapter.hooks["hook-id-2"]
	if !found || !replacement.Active {
		t.Fatalf("replacement=%#v found=%t", replacement, found)
	}
	payload, err := engine.Verifier.Get(context.Background(), protected.SourceBindingKey)
	if err != nil {
		t.Fatal(err)
	}
	_, secrets, err := decodeVerifierConfig(payload)
	if err != nil || len(secrets) != 1 || !bytes.Equal(secrets[0], replacement.Secret) {
		t.Fatalf("secrets=%q err=%v", secrets, err)
	}
}

func TestLifecycleRecoversLostReplacementCreateResponse(t *testing.T) {
	t.Parallel()
	engine, adapter, record := lifecycleTestEngine(t)
	input := lifecycleInput(record)
	created, err := engine.Ensure(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}

	adapter.loseCreateResponse = true
	input.Binding = created
	input.Revision = 2
	if _, err := engine.Ensure(context.Background(), input); err == nil || adapter.createCalls != 2 {
		t.Fatalf("lost replacement response err=%v calls=%d", err, adapter.createCalls)
	}
	pending, err := engine.Bindings.GetBinding(context.Background(), record.Binding.Project.UID)
	if err != nil || pending.Binding.ServiceHook == nil || pending.Binding.ServiceHook.Pending == nil || pending.Binding.ServiceHook.Pending.Kind != "RotateServiceHook" {
		t.Fatalf("pending=%#v err=%v", pending.Binding.ServiceHook, err)
	}

	adapter.loseCreateResponse = false
	input.Binding = pending
	input.Revision = 3
	rotated, err := engine.Ensure(context.Background(), input)
	if err != nil || rotated.Binding.ServiceHook == nil || rotated.Binding.ServiceHook.ExternalIdentity != "hook-id-2" || rotated.Binding.ServiceHook.Revision != 2 || adapter.createCalls != 2 {
		t.Fatalf("rotated=%#v calls=%d err=%v", rotated.Binding.ServiceHook, adapter.createCalls, err)
	}
	input.Binding = rotated
	latest, err := engine.Ensure(context.Background(), input)
	if err != nil || latest.Binding.ServiceHook == nil || latest.Binding.ServiceHook.ExternalIdentity != "hook-id-3" || latest.Binding.ServiceHook.Revision != 3 || adapter.createCalls != 3 {
		t.Fatalf("latest=%#v calls=%d err=%v", latest.Binding.ServiceHook, adapter.createCalls, err)
	}
}

func lifecycleTestEngine(t *testing.T) (*LifecycleEngine, *lifecycleAdapterFake, binding.BindingRecord) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	verifierSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "operator", Name: DefaultVerifierSecretName}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(verifierSecret).Build()
	bindings := &binding.Store{Client: client, Namespace: "state"}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-op"}
	record, err := bindings.CreateBinding(context.Background(), binding.ProjectBinding{
		Version: binding.CurrentVersion, Project: binding.ProjectReference{Namespace: "default", Name: "checkout", UID: "project-uid", Generation: 1},
		Connection: "main", Installation: "https://sentry.example", Organization: "company", Slug: "checkout", Owner: "payments",
		Origin: binding.OriginManaged, AccessMode: binding.AccessManage, ExternalIdentity: "project-id", Marker: &marker, ManagerUID: "project-uid", ClaimName: "claim-name",
	})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &lifecycleAdapterFake{hooks: map[string]RemoteServiceHook{}}
	return &LifecycleEngine{
		Bindings: bindings, Adapter: adapter,
		Sources:  &SourceStore{Client: client, Namespace: "hooks"},
		Verifier: &VerifierStore{Client: client, Namespace: "operator"},
	}, adapter, record
}

func lifecycleInput(record binding.BindingRecord) LifecycleInput {
	return LifecycleInput{
		Binding: record, ConnectionUID: "connection-uid", ReceiverBase: "https://hooks.example.test/events",
		Organization: "company", ProjectSlug: "checkout", Revision: 1,
		Authorize:  func(context.Context) error { return nil },
		Deliveries: []TelegramDelivery{{RouteKey: strings.Repeat("a", 64), Render: RenderInput{ProjectName: "checkout", AlertName: "regression", Severity: SeverityWarning}}},
	}
}

type lifecycleAdapterFake struct {
	hooks              map[string]RemoteServiceHook
	createCalls        int
	createActive       []bool
	createEndpoints    []string
	loseCreateResponse bool
	deleteCalls        int
	loseDeleteResponse bool
}

func (a *lifecycleAdapterFake) List(context.Context, string, string) ([]RemoteServiceHook, error) {
	result := make([]RemoteServiceHook, 0, len(a.hooks))
	for _, hook := range a.hooks {
		result = append(result, cloneServiceHook(hook))
	}
	return result, nil
}

func (a *lifecycleAdapterFake) Get(_ context.Context, _, _, identity string) (RemoteServiceHook, bool, error) {
	hook, found := a.hooks[identity]
	return cloneServiceHook(hook), found, nil
}

func (a *lifecycleAdapterFake) Create(_ context.Context, _, _, endpoint string, active bool) (RemoteServiceHook, error) {
	a.createCalls++
	a.createActive = append(a.createActive, active)
	a.createEndpoints = append(a.createEndpoints, endpoint)
	hook := RemoteServiceHook{
		// Both supported Sentry versions currently ignore isActive on create and return an
		// active hook. The lifecycle must therefore create at an unpublished staging URL.
		Identity: fmt.Sprintf("hook-id-%d", a.createCalls), URL: endpoint, Events: []string{AlertEvent}, Active: true,
		Secret: []byte(fmt.Sprintf("service-hook-secret-%d", a.createCalls)),
	}
	a.hooks[hook.Identity] = hook
	if a.loseCreateResponse {
		return RemoteServiceHook{}, &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "create-project-service-hook"}
	}
	return cloneServiceHook(hook), nil
}

func (a *lifecycleAdapterFake) Repair(_ context.Context, _, _, identity, endpoint string) (RemoteServiceHook, error) {
	hook, found := a.hooks[identity]
	if !found {
		return RemoteServiceHook{}, errors.New("hook not found")
	}
	hook.URL = endpoint
	hook.Events = []string{AlertEvent}
	hook.Active = true
	a.hooks[identity] = hook
	return cloneServiceHook(hook), nil
}

func (a *lifecycleAdapterFake) Delete(_ context.Context, _, _, identity string) error {
	a.deleteCalls++
	delete(a.hooks, identity)
	if a.loseDeleteResponse {
		return &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "delete-project-service-hook"}
	}
	return nil
}

func cloneServiceHook(hook RemoteServiceHook) RemoteServiceHook {
	hook.Events = append([]string(nil), hook.Events...)
	hook.Secret = append([]byte(nil), hook.Secret...)
	return hook
}
