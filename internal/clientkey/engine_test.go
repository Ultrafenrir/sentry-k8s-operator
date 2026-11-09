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

package clientkey

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func TestEngineCreatesUpdatesAndDeletesOnlyPinnedKey(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	engine := &Engine{Adapter: adapter, Journal: newFakeJournal()}
	input := testInput()
	input.Desired["ingest"] = Desired{Active: true, RateLimit: &RateLimit{Count: 1000, Window: 60}, RotationRevision: 1}

	created := engine.Reconcile(context.Background(), input)
	if err := created.Err(); err != nil {
		t.Fatal(err)
	}
	current := created.Bindings["ingest"]
	if current.ExternalIdentity == "" || current.RemoteName != remoteName("ingest", input.ProjectUID, 1) || current.RateLimit == nil || len(created.PublicDSNs) != 1 {
		t.Fatalf("binding=%#v public=%#v", current, created.PublicDSNs)
	}
	encoded := fmt.Sprintf("%#v", current)
	if containsSensitive(encoded) {
		t.Fatalf("protected binding retained sensitive material: %s", encoded)
	}

	input.Current = created.Bindings
	input.Desired["ingest"] = Desired{Active: false, RotationRevision: 1}
	updated := engine.Reconcile(context.Background(), input)
	if err := updated.Err(); err != nil || adapter.updateCalls != 1 || updated.Bindings["ingest"].Active || len(updated.PublicDSNs) != 0 {
		t.Fatalf("updated=%#v calls=%d err=%v", updated.Bindings, adapter.updateCalls, updated.Err())
	}

	input.Current = updated.Bindings
	input.Desired = map[string]Desired{}
	deleted := engine.Reconcile(context.Background(), input)
	if err := deleted.Err(); err != nil || len(deleted.Bindings) != 0 || adapter.deleteCalls != 1 {
		t.Fatalf("deleted=%#v calls=%d err=%v", deleted.Bindings, adapter.deleteCalls, deleted.Err())
	}
}

func TestEngineRecoversLostCreateWithoutSecondPost(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	adapter.loseCreateResponse = true
	engine := &Engine{Adapter: adapter, Journal: newFakeJournal()}
	input := testInput()
	input.Desired["ingest"] = Desired{Active: true, RotationRevision: 1}

	first := engine.Reconcile(context.Background(), input)
	if first.Err() == nil || adapter.createCalls != 1 {
		t.Fatalf("first err=%v calls=%d", first.Err(), adapter.createCalls)
	}
	adapter.loseCreateResponse = false
	second := engine.Reconcile(context.Background(), input)
	if err := second.Err(); err != nil || adapter.createCalls != 1 || second.Bindings["ingest"].ExternalIdentity == "" {
		t.Fatalf("second=%#v err=%v calls=%d", second.Bindings, err, adapter.createCalls)
	}
}

func TestEngineStagesRotationUntilCutoverConfirmation(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	engine := &Engine{Adapter: adapter, Journal: newFakeJournal()}
	input := testInput()
	input.Desired["ingest"] = Desired{Active: true, RotationRevision: 1}
	initial := engine.Reconcile(context.Background(), input)
	if err := initial.Err(); err != nil {
		t.Fatal(err)
	}
	oldIdentity := initial.Bindings["ingest"].ExternalIdentity

	input.Current = initial.Bindings
	input.Desired["ingest"] = Desired{Active: true, RotationRevision: 2}
	staged := engine.Reconcile(context.Background(), input)
	stagedBinding := staged.Bindings["ingest"]
	if err := staged.Err(); err != nil || stagedBinding.ExternalIdentity != oldIdentity || stagedBinding.ReplacementIdentity == "" || adapter.deleteCalls != 0 || staged.Statuses["ingest"].State != StateRotationPending {
		t.Fatalf("staged=%#v status=%#v err=%v deletes=%d", stagedBinding, staged.Statuses, err, adapter.deleteCalls)
	}

	input.Current = staged.Bindings
	input.Promote["ingest"] = true
	promoted := engine.Reconcile(context.Background(), input)
	promotedBinding := promoted.Bindings["ingest"]
	if err := promoted.Err(); err != nil || promotedBinding.ExternalIdentity == oldIdentity || promotedBinding.ReplacementIdentity != "" || promotedBinding.RotationRevision != 2 || adapter.deleteCalls != 1 {
		t.Fatalf("promoted=%#v err=%v deletes=%d", promotedBinding, err, adapter.deleteCalls)
	}
	if _, found := adapter.items[oldIdentity]; found {
		t.Fatal("old key remains after confirmed cutover")
	}
}

func TestEngineRejectsDuplicateOwnershipLabelAndRollback(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	input := testInput()
	desired := Desired{Active: true, RotationRevision: 2}
	name := remoteName("ingest", input.ProjectUID, 2)
	adapter.addRemote(name, desired)
	adapter.addRemote(name, desired)
	input.Desired["ingest"] = desired
	result := (&Engine{Adapter: adapter, Journal: newFakeJournal()}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err(), ErrOwnershipLost) || adapter.createCalls != 0 {
		t.Fatalf("err=%v creates=%d", result.Err(), adapter.createCalls)
	}

	input.Current = map[string]binding.ClientKeyBinding{"ingest": {
		ExternalIdentity: "key-1", RemoteName: remoteName("ingest", input.ProjectUID, 3), RotationRevision: 3,
	}}
	rollback := (&Engine{Adapter: adapter, Journal: newFakeJournal()}).Reconcile(context.Background(), input)
	if !errors.Is(rollback.Err(), ErrRotationRollback) {
		t.Fatalf("rollback err=%v", rollback.Err())
	}
}

func TestEngineRejectsForeignBindingBeforeDelete(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	input := testInput()
	input.Current = map[string]binding.ClientKeyBinding{"ingest": {
		ExternalIdentity: "foreign", RemoteName: "Default", RotationRevision: 1,
	}}
	result := (&Engine{Adapter: adapter, Journal: newFakeJournal()}).Reconcile(context.Background(), input)
	if !errors.Is(result.Err(), ErrOwnershipLost) || adapter.deleteCalls != 0 {
		t.Fatalf("err=%v deletes=%d", result.Err(), adapter.deleteCalls)
	}
}

func testInput() Input {
	return Input{
		Organization: "company", ProjectSlug: "checkout", ProjectUID: "project-uid-123",
		Desired: map[string]Desired{}, Current: map[string]binding.ClientKeyBinding{}, Promote: map[string]bool{},
		Authorize: func(context.Context) error { return nil },
	}
}

func containsSensitive(value string) bool {
	return strings.Contains(value, "SECRET-SENTINEL") || strings.Contains(value, "https://public")
}

type fakeAdapter struct {
	items              map[string]Remote
	nextID             int
	createCalls        int
	updateCalls        int
	deleteCalls        int
	loseCreateResponse bool
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{items: make(map[string]Remote)}
}

func (f *fakeAdapter) List(context.Context, string, string) ([]Remote, error) {
	result := make([]Remote, 0, len(f.items))
	for _, item := range f.items {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Identity < result[j].Identity })
	return result, nil
}

func (f *fakeAdapter) Create(_ context.Context, _, _ string, request CreateRequest) (Remote, error) {
	f.createCalls++
	remote := f.addRemote(request.Name, Desired{Active: true, RateLimit: request.RateLimit})
	if f.loseCreateResponse {
		return Remote{}, &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "create-project-client-key"}
	}
	return remote, nil
}

func (f *fakeAdapter) Get(_ context.Context, _, _, identity string) (Remote, bool, error) {
	remote, found := f.items[identity]
	return remote, found, nil
}

func (f *fakeAdapter) Update(_ context.Context, _, _, identity string, request UpdateRequest) (Remote, error) {
	f.updateCalls++
	remote, found := f.items[identity]
	if !found {
		return Remote{}, errors.New("missing")
	}
	remote.Name = request.Name
	remote.IsActive = request.IsActive
	remote.RateLimit = cloneRateLimit(request.RateLimit)
	f.items[identity] = remote
	return remote, nil
}

func (f *fakeAdapter) Delete(_ context.Context, _, _, identity string) error {
	f.deleteCalls++
	delete(f.items, identity)
	return nil
}

func (f *fakeAdapter) addRemote(name string, desired Desired) Remote {
	f.nextID++
	remote := Remote{Identity: fmt.Sprintf("key-%d", f.nextID), Name: name, IsActive: desired.Active, RateLimit: cloneRateLimit(desired.RateLimit)}
	remote.DSN.Public = fmt.Sprintf("https://public-%d@example.invalid/1", f.nextID)
	f.items[remote.Identity] = remote
	return remote
}

type fakeJournal struct {
	items map[string]sentryapi.Operation
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{items: make(map[string]sentryapi.Operation)}
}

func (f *fakeJournal) Load(_ context.Context, key string) (sentryapi.Operation, bool, error) {
	value, found := f.items[key]
	return value, found, nil
}

func (f *fakeJournal) CompareAndSwap(_ context.Context, previousRevision int64, next sentryapi.Operation) error {
	current, found := f.items[next.Key]
	if !found {
		if previousRevision != 0 || next.Revision != 1 {
			return errors.New("stale journal create")
		}
		f.items[next.Key] = next
		return nil
	}
	if current.Revision != previousRevision || next.Revision != previousRevision+1 {
		return errors.New("stale journal update")
	}
	f.items[next.Key] = next
	return nil
}

func TestDesiredHashIsStable(t *testing.T) {
	t.Parallel()
	desired := Desired{Active: true, RateLimit: &RateLimit{Count: 1, Window: 60}, RotationRevision: 2}
	if desiredHash(desired) != desiredHash(desired) || reflect.DeepEqual(desiredHash(desired), desiredHash(Desired{Active: false, RotationRevision: 2})) {
		t.Fatal("desired hash is not deterministic or configuration-sensitive")
	}
}
