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

package symbolsource

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestEngineOwnsCreateSecretRotationDriftAndExactDelete(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	engine := &Engine{Adapter: adapter}
	input := testInput()
	input.Desired["symbols"] = Desired{
		Type: "http", Layout: Layout{Type: "native", Casing: "default"},
		URL: "https://symbols.example.invalid", Username: "reader", Password: "SECRET-SENTINEL-one",
	}
	created := engine.Reconcile(context.Background(), input)
	if err := created.Err(); err != nil || created.Statuses["symbols"].State != StateReady || adapter.createCalls != 1 {
		t.Fatalf("created=%#v err=%v creates=%d", created, err, adapter.createCalls)
	}
	if containsSecret(created.Bindings["symbols"].DesiredHash) {
		t.Fatal("protected binding contains a credential")
	}

	input.Current = created.Bindings
	quiet := engine.Reconcile(context.Background(), input)
	if err := quiet.Err(); err != nil || adapter.updateCalls != 0 {
		t.Fatalf("quiet err=%v updates=%d", err, adapter.updateCalls)
	}

	input.Desired["symbols"] = Desired{
		Type: "http", Layout: Layout{Type: "native", Casing: "default"},
		URL: "https://symbols.example.invalid", Username: "reader", Password: "SECRET-SENTINEL-two",
	}
	rotated := engine.Reconcile(context.Background(), input)
	if err := rotated.Err(); err != nil || adapter.updateCalls != 1 || rotated.Bindings["symbols"].DesiredHash == created.Bindings["symbols"].DesiredHash {
		t.Fatalf("rotated=%#v err=%v updates=%d", rotated, err, adapter.updateCalls)
	}

	input.Current = rotated.Bindings
	identity := input.Current["symbols"].ExternalIdentity
	remote := adapter.items[identity]
	remote.URL = "https://drift.example.invalid"
	adapter.items[identity] = remote
	repaired := engine.Reconcile(context.Background(), input)
	if err := repaired.Err(); err != nil || adapter.updateCalls != 2 || adapter.items[identity].URL != "https://symbols.example.invalid" {
		t.Fatalf("repaired=%#v err=%v updates=%d remote=%#v", repaired, err, adapter.updateCalls, adapter.items[identity])
	}

	input.Current = repaired.Bindings
	input.Desired = map[string]Desired{}
	deleted := engine.Reconcile(context.Background(), input)
	if err := deleted.Err(); err != nil || adapter.deleteCalls != 1 || len(deleted.Bindings) != 0 {
		t.Fatalf("deleted=%#v err=%v deletes=%d", deleted, err, adapter.deleteCalls)
	}
}

func TestEngineRecoversLostCreateResponseAndRejectsForeignIdentity(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	adapter.loseCreateResponse = true
	input := testInput()
	input.Desired["cloud"] = Desired{
		Type: "s3", Layout: Layout{Type: "symstore", Casing: "lowercase"}, Bucket: "symbols",
		Region: "eu-west-1", AccessKey: "access", SecretKey: "SECRET-SENTINEL", Prefix: "native/",
	}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), input)
	if err := result.Err(); err != nil || adapter.createCalls != 1 || adapter.updateCalls != 1 || result.Bindings["cloud"].ExternalIdentity == "" {
		t.Fatalf("result=%#v err=%v creates=%d updates=%d", result, err, adapter.createCalls, adapter.updateCalls)
	}

	input.Current = map[string]binding.SymbolSourceBinding{"cloud": {
		ExternalIdentity: "foreign", RemoteName: "foreign", Type: "s3", DesiredHash: desiredHash(input.Desired["cloud"]),
	}}
	foreign := (&Engine{Adapter: adapter}).Reconcile(context.Background(), input)
	if !errors.Is(foreign.Err(), ErrOwnershipLost) || adapter.deleteCalls != 0 {
		t.Fatalf("err=%v deletes=%d", foreign.Err(), adapter.deleteCalls)
	}
}

func TestEngineValidatesEveryChildBeforeMutation(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	input := testInput()
	input.Desired["good"] = Desired{Type: "gcs", Layout: Layout{Type: "unified", Casing: "default"}, Bucket: "symbols", ClientEmail: "svc@example.invalid", PrivateKey: "SECRET-SENTINEL"}
	input.Desired["bad"] = Desired{Type: "http", Layout: Layout{Type: "native", Casing: "default"}, URL: "http://insecure.example.invalid"}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err(), ErrInvalidInput) || adapter.createCalls != 0 || adapter.updateCalls != 0 || adapter.deleteCalls != 0 {
		t.Fatalf("result=%#v calls=%d/%d/%d", result, adapter.createCalls, adapter.updateCalls, adapter.deleteCalls)
	}
}

func TestEngineRejectsHTTPURLUserinfoBeforeMutation(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	input := testInput()
	input.Desired["symbols"] = Desired{
		Type: "http", Layout: Layout{Type: "native", Casing: "default"},
		URL: "https://reader:secret@symbols.example.invalid", Username: "reader", Password: "SECRET-SENTINEL",
	}
	result := (&Engine{Adapter: adapter}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err(), ErrInvalidInput) || adapter.createCalls != 0 || adapter.updateCalls != 0 || adapter.deleteCalls != 0 {
		t.Fatalf("result=%#v calls=%d/%d/%d", result, adapter.createCalls, adapter.updateCalls, adapter.deleteCalls)
	}
}

func testInput() Input {
	return Input{
		Organization: "company", ProjectSlug: "checkout", ProjectUID: "project-uid-123",
		Desired: map[string]Desired{}, Current: map[string]binding.SymbolSourceBinding{},
		Authorize: func(context.Context) error { return nil },
	}
}

func containsSecret(value string) bool {
	return value == "SECRET-SENTINEL" || value == "SECRET-SENTINEL-one" || value == "SECRET-SENTINEL-two"
}

type fakeAdapter struct {
	items              map[string]Remote
	createCalls        int
	updateCalls        int
	deleteCalls        int
	loseCreateResponse bool
}

func newFakeAdapter() *fakeAdapter { return &fakeAdapter{items: make(map[string]Remote)} }

func (f *fakeAdapter) List(context.Context, string, string) ([]Remote, error) {
	result := make([]Remote, 0, len(f.items))
	for _, item := range f.items {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Identity < result[j].Identity })
	return result, nil
}

func (f *fakeAdapter) Create(_ context.Context, _, _ string, request Request) (Remote, error) {
	f.createCalls++
	remote := remoteFromRequest(request)
	f.items[remote.Identity] = remote
	if f.loseCreateResponse {
		return Remote{}, &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "create-project-symbol-source"}
	}
	return remote, nil
}

func (f *fakeAdapter) Update(_ context.Context, _, _, identity string, request Request) (Remote, error) {
	f.updateCalls++
	if _, found := f.items[identity]; !found {
		return Remote{}, errors.New("missing")
	}
	remote := remoteFromRequest(request)
	f.items[identity] = remote
	return remote, nil
}

func (f *fakeAdapter) Delete(_ context.Context, _, _, identity string) error {
	f.deleteCalls++
	delete(f.items, identity)
	return nil
}

func remoteFromRequest(request Request) Remote {
	return Remote{
		Identity: request.Identity, Name: request.Name, Type: request.Type, Layout: request.Layout,
		URL: request.URL, Username: request.Username, Bucket: request.Bucket, Region: request.Region,
		AccessKey: request.AccessKey, Prefix: request.Prefix, ClientEmail: request.ClientEmail,
	}
}
