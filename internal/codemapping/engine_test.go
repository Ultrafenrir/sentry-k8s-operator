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

package codemapping

import (
	"context"
	"errors"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func TestEngineOwnsMappingAndImportedCodeownersLifecycle(t *testing.T) {
	adapter := newFakeAdapter()
	journal := &fakeJournal{values: map[string]sentryapi.Operation{}}
	authorized := 0
	input := testInput(func(context.Context) error { authorized++; return nil })
	engine := &Engine{Adapter: adapter, Journal: journal}

	created := engine.Reconcile(context.Background(), input)
	if err := created.Err(); err != nil {
		t.Fatal(err)
	}
	binding := created.Bindings["checkout"]
	if binding.ExternalIdentity != "mapping-1" || binding.IntegrationIdentity != "integration-1" || binding.RepositoryIdentity != "repository-1" || binding.CodeownersIdentity != "codeowners-1" || authorized != 2 {
		t.Fatalf("binding=%#v authorized=%d", binding, authorized)
	}

	input.Current = created.Bindings
	converged := engine.Reconcile(context.Background(), input)
	if err := converged.Err(); err != nil {
		t.Fatal(err)
	}
	if adapter.mappingCreates != 1 || adapter.mappingUpdates != 0 || adapter.codeownersCreates != 1 || adapter.codeownersUpdates != 0 || authorized != 2 {
		t.Fatalf("unexpected converged mutations: %#v authorized=%d", adapter, authorized)
	}

	adapter.source.Raw = "src/* @acme/platform"
	updated := engine.Reconcile(context.Background(), input)
	if err := updated.Err(); err != nil {
		t.Fatal(err)
	}
	if adapter.codeownersUpdates != 1 || updated.Bindings["checkout"].CodeownersContentHash == binding.CodeownersContentHash {
		t.Fatalf("updated=%#v adapter=%#v", updated, adapter)
	}

	input.Current = updated.Bindings
	input.Desired = map[string]Desired{}
	deleted := engine.Reconcile(context.Background(), input)
	if err := deleted.Err(); err != nil {
		t.Fatal(err)
	}
	if len(deleted.Bindings) != 0 || adapter.codeownersDeletes != 1 || adapter.mappingDeletes != 1 {
		t.Fatalf("deleted=%#v adapter=%#v", deleted, adapter)
	}
}

func TestEngineRecoversLostMappingCreateAndRejectsUnjournaledExactTuple(t *testing.T) {
	input := testInput(func(context.Context) error { return nil })
	desired := input.Desired["checkout"]
	adapter := newFakeAdapter()
	request := mappingRequest(input, desired, "integration-1", "repository-1")
	adapter.mappings = []Remote{{Identity: "mapping-recovered", ProjectID: request.ProjectID, RepositoryID: request.RepositoryID, IntegrationID: request.IntegrationID, StackRoot: request.StackRoot, SourceRoot: request.SourceRoot, DefaultBranch: request.DefaultBranch}}
	operationKey := "code-mappings:create:company:checkout:checkout"
	journal := &fakeJournal{values: map[string]sentryapi.Operation{operationKey: {Key: operationKey, Kind: "CreateCodeMapping", LocalResource: "checkout/checkout", IdempotencyHash: desiredHash(desired), Phase: sentryapi.OperationSubmitted, Revision: 2}}}
	result := (&Engine{Adapter: adapter, Journal: journal}).Reconcile(context.Background(), input)
	if err := result.Err(); err != nil {
		t.Fatal(err)
	}
	if result.Bindings["checkout"].ExternalIdentity != "mapping-recovered" || adapter.mappingCreates != 0 || journal.values[operationKey].Phase != sentryapi.OperationVerified {
		t.Fatalf("result=%#v adapter=%#v journal=%#v", result, adapter, journal.values)
	}

	foreign := newFakeAdapter()
	foreign.mappings = adapter.mappings
	rejected := (&Engine{Adapter: foreign, Journal: &fakeJournal{values: map[string]sentryapi.Operation{}}}).Reconcile(context.Background(), input)
	if !errors.Is(rejected.Err(), ErrOwnershipLost) || foreign.mappingCreates != 0 {
		t.Fatalf("rejected=%#v", rejected)
	}
}

func testInput(authorize Authorization) Input {
	return Input{Organization: "company", ProjectSlug: "checkout", ProjectIdentity: "project-1", ProjectUID: "uid-1", Authorize: authorize, Desired: map[string]Desired{"checkout": {IntegrationAlias: "github", Provider: "github", IntegrationName: "acme", RepositoryAlias: "checkout", RepositoryName: "acme/checkout", StackRoot: "/", SourceRoot: "src", DefaultBranch: "main", ImportCodeowners: true}}}
}

type fakeAdapter struct {
	integrations                                            []Integration
	repositories                                            []Repository
	mappings                                                []Remote
	codeowners                                              []Codeowners
	source                                                  CodeownersSource
	mappingCreates, mappingUpdates, mappingDeletes          int
	codeownersCreates, codeownersUpdates, codeownersDeletes int
}

func newFakeAdapter() *fakeAdapter {
	integration := Integration{Identity: "integration-1", Name: "acme"}
	integration.Provider.Key = "github"
	return &fakeAdapter{integrations: []Integration{integration}, repositories: []Repository{{Identity: "repository-1", Name: "acme/checkout", IntegrationID: "integration-1"}}, source: CodeownersSource{Raw: "src/* @acme/backend"}}
}
func (f *fakeAdapter) ListIntegrations(context.Context, string, string) ([]Integration, error) {
	return append([]Integration{}, f.integrations...), nil
}
func (f *fakeAdapter) ListRepositories(context.Context, string, string) ([]Repository, error) {
	return append([]Repository{}, f.repositories...), nil
}
func (f *fakeAdapter) ListMappings(context.Context, string) ([]Remote, error) {
	return append([]Remote{}, f.mappings...), nil
}
func (f *fakeAdapter) CreateMapping(_ context.Context, _ string, request MappingRequest) (Remote, error) {
	f.mappingCreates++
	value := Remote{Identity: "mapping-1", ProjectID: request.ProjectID, RepositoryID: request.RepositoryID, IntegrationID: request.IntegrationID, StackRoot: request.StackRoot, SourceRoot: request.SourceRoot, DefaultBranch: request.DefaultBranch}
	f.mappings = append(f.mappings, value)
	return value, nil
}
func (f *fakeAdapter) UpdateMapping(_ context.Context, _ string, identity string, request MappingRequest) (Remote, error) {
	f.mappingUpdates++
	value := Remote{Identity: identity, ProjectID: request.ProjectID, RepositoryID: request.RepositoryID, IntegrationID: request.IntegrationID, StackRoot: request.StackRoot, SourceRoot: request.SourceRoot, DefaultBranch: request.DefaultBranch}
	for index := range f.mappings {
		if f.mappings[index].Identity == identity {
			f.mappings[index] = value
		}
	}
	return value, nil
}
func (f *fakeAdapter) DeleteMapping(_ context.Context, _ string, identity string) error {
	f.mappingDeletes++
	for index := range f.mappings {
		if f.mappings[index].Identity == identity {
			f.mappings = append(f.mappings[:index], f.mappings[index+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeAdapter) FetchCodeowners(context.Context, string, string) (CodeownersSource, error) {
	return f.source, nil
}
func (f *fakeAdapter) ListCodeowners(context.Context, string, string) ([]Codeowners, error) {
	return append([]Codeowners{}, f.codeowners...), nil
}
func (f *fakeAdapter) CreateCodeowners(_ context.Context, _, _, mappingID, raw string) (Codeowners, error) {
	f.codeownersCreates++
	value := Codeowners{Identity: "codeowners-1", Raw: raw, CodeMappingID: mappingID}
	f.codeowners = append(f.codeowners, value)
	return value, nil
}
func (f *fakeAdapter) UpdateCodeowners(_ context.Context, _, _, identity, mappingID, raw string) (Codeowners, error) {
	f.codeownersUpdates++
	value := Codeowners{Identity: identity, Raw: raw, CodeMappingID: mappingID}
	for index := range f.codeowners {
		if f.codeowners[index].Identity == identity {
			f.codeowners[index] = value
		}
	}
	return value, nil
}
func (f *fakeAdapter) DeleteCodeowners(_ context.Context, _, _, identity string) error {
	f.codeownersDeletes++
	for index := range f.codeowners {
		if f.codeowners[index].Identity == identity {
			f.codeowners = append(f.codeowners[:index], f.codeowners[index+1:]...)
			break
		}
	}
	return nil
}

type fakeJournal struct {
	values map[string]sentryapi.Operation
}

func (f *fakeJournal) Load(_ context.Context, key string) (sentryapi.Operation, bool, error) {
	value, found := f.values[key]
	return value, found, nil
}
func (f *fakeJournal) CompareAndSwap(_ context.Context, previous int64, next sentryapi.Operation) error {
	current, found := f.values[next.Key]
	if !found && previous != 0 || found && current.Revision != previous {
		return errors.New("journal conflict")
	}
	f.values[next.Key] = next
	return nil
}
