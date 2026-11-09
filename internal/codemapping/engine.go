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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

const (
	StateReady   = "Ready"
	StateDeleted = "Deleted"
	StateError   = "Error"
)

var (
	ErrInvalidInput    = errors.New("code-mapping desired state is invalid")
	ErrCatalogMissing  = errors.New("source-code catalog alias did not resolve exactly once")
	ErrOwnershipLost   = errors.New("code-mapping ownership evidence is missing")
	ErrExternalMissing = errors.New("code-mapping external object is missing")
	ErrRecoveryPending = errors.New("code-mapping mutation recovery is pending")
	ErrVerification    = errors.New("code-mapping mutation verification failed")
	localAliasPattern  = regexp.MustCompile(`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`)
)

type Desired struct {
	IntegrationAlias string
	Provider         string
	IntegrationName  string
	RepositoryAlias  string
	RepositoryName   string
	StackRoot        string
	SourceRoot       string
	DefaultBranch    string
	ImportCodeowners bool
}

type Authorization func(context.Context) error

type Input struct {
	Organization    string
	ProjectSlug     string
	ProjectIdentity string
	ProjectUID      string
	Desired         map[string]Desired
	Current         map[string]binding.CodeMappingBinding
	Authorize       Authorization
}

type Status struct {
	State, Reason string
	Err           error
}
type Result struct {
	Bindings map[string]binding.CodeMappingBinding
	Statuses map[string]Status
}

func (r Result) Err() error {
	var values []error
	for _, status := range r.Statuses {
		if status.Err != nil {
			values = append(values, status.Err)
		}
	}
	return errors.Join(values...)
}

type Engine struct {
	Adapter Adapter
	Journal sentryapi.OperationJournal
}

func (e *Engine) Reconcile(ctx context.Context, input Input) Result {
	result := Result{Bindings: cloneBindings(input.Current), Statuses: map[string]Status{}}
	if e == nil || e.Adapter == nil || e.Journal == nil || input.Authorize == nil || input.Organization == "" || input.ProjectSlug == "" || input.ProjectIdentity == "" || input.ProjectUID == "" {
		result.Statuses[""] = errorStatus(ErrInvalidInput)
		return result
	}
	mappings, err := e.Adapter.ListMappings(ctx, input.Organization)
	if err != nil {
		result.Statuses[""] = errorStatus(err)
		return result
	}
	codeowners, err := e.Adapter.ListCodeowners(ctx, input.Organization, input.ProjectSlug)
	if err != nil {
		result.Statuses[""] = errorStatus(err)
		return result
	}
	for _, key := range removedKeys(input.Current, input.Desired) {
		if err := e.deleteOne(ctx, input, input.Current[key], mappings, codeowners); err != nil {
			result.Statuses[key] = errorStatus(err)
			continue
		}
		delete(result.Bindings, key)
		result.Statuses[key] = Status{State: StateDeleted, Reason: "CodeMappingDeleted"}
	}
	for _, key := range sortedKeys(input.Desired) {
		desired := input.Desired[key]
		if err := validateDesired(key, desired); err != nil {
			result.Statuses[key] = errorStatus(err)
			continue
		}
		integration, repository, err := e.resolve(ctx, input.Organization, desired)
		if err != nil {
			result.Statuses[key] = errorStatus(err)
			continue
		}
		current, found := input.Current[key]
		var next binding.CodeMappingBinding
		if !found {
			next, err = e.createOne(ctx, input, key, desired, integration, repository, mappings)
		} else {
			next, err = e.reconcileOne(ctx, input, desired, current, integration, repository, mappings)
		}
		if err == nil {
			next, err = e.reconcileCodeowners(ctx, input, key, desired, next, codeowners)
		}
		if err != nil {
			result.Statuses[key] = errorStatus(err)
			continue
		}
		result.Bindings[key] = next
		result.Statuses[key] = Status{State: StateReady, Reason: "CodeMappingReady"}
	}
	return result
}

func (e *Engine) resolve(ctx context.Context, organization string, desired Desired) (Integration, Repository, error) {
	integrations, err := e.Adapter.ListIntegrations(ctx, organization, desired.Provider)
	if err != nil {
		return Integration{}, Repository{}, err
	}
	var matched []Integration
	for _, value := range integrations {
		if value.Name == desired.IntegrationName && value.Provider.Key == desired.Provider {
			matched = append(matched, value)
		}
	}
	if len(matched) != 1 || matched[0].Identity == "" {
		return Integration{}, Repository{}, ErrCatalogMissing
	}
	repositories, err := e.Adapter.ListRepositories(ctx, organization, matched[0].Identity)
	if err != nil {
		return Integration{}, Repository{}, err
	}
	var repos []Repository
	for _, value := range repositories {
		if value.Name == desired.RepositoryName && value.IntegrationID == matched[0].Identity {
			repos = append(repos, value)
		}
	}
	if len(repos) != 1 || repos[0].Identity == "" {
		return Integration{}, Repository{}, ErrCatalogMissing
	}
	return matched[0], repos[0], nil
}

func (e *Engine) createOne(ctx context.Context, input Input, key string, desired Desired, integration Integration, repository Repository, mappings []Remote) (binding.CodeMappingBinding, error) {
	request := mappingRequest(input, desired, integration.Identity, repository.Identity)
	hash := desiredHash(desired)
	var observed Remote
	verify := func() bool {
		matches := exactMappings(mappings, request)
		if len(matches) == 1 {
			observed = matches[0]
			return observed.Identity != ""
		}
		return false
	}
	operationKey := "code-mappings:create:" + input.Organization + ":" + input.ProjectSlug + ":" + key
	operation, found, err := e.Journal.Load(ctx, operationKey)
	if err != nil {
		return binding.CodeMappingBinding{}, err
	}
	if found {
		if operation.IdempotencyHash != hash {
			return binding.CodeMappingBinding{}, ErrRecoveryPending
		}
		fresh, err := e.Adapter.ListMappings(ctx, input.Organization)
		if err != nil {
			return binding.CodeMappingBinding{}, err
		}
		mappings = fresh
		if verify() {
			if operation.Phase == sentryapi.OperationPlanned {
				return binding.CodeMappingBinding{}, ErrOwnershipLost
			}
			if operation.Phase != sentryapi.OperationVerified {
				completed := operation
				completed.Phase = sentryapi.OperationVerified
				completed.Revision++
				if err := e.Journal.CompareAndSwap(ctx, operation.Revision, completed); err != nil {
					return binding.CodeMappingBinding{}, err
				}
			}
			return newBinding(observed, desired), nil
		}
		if operation.Phase != sentryapi.OperationPlanned {
			return binding.CodeMappingBinding{}, ErrRecoveryPending
		}
	} else {
		if len(exactMappings(mappings, request)) != 0 {
			return binding.CodeMappingBinding{}, ErrOwnershipLost
		}
		operation = sentryapi.Operation{Key: operationKey, Kind: "CreateCodeMapping", LocalResource: input.ProjectSlug + "/" + key, IdempotencyHash: hash, Phase: sentryapi.OperationPlanned, Revision: 1}
		if err := e.Journal.CompareAndSwap(ctx, 0, operation); err != nil {
			return binding.CodeMappingBinding{}, err
		}
	}
	if err := input.Authorize(ctx); err != nil {
		return binding.CodeMappingBinding{}, err
	}
	submitted := operation
	submitted.Phase = sentryapi.OperationSubmitted
	submitted.Revision++
	if err := e.Journal.CompareAndSwap(ctx, operation.Revision, submitted); err != nil {
		return binding.CodeMappingBinding{}, err
	}
	created, err := e.Adapter.CreateMapping(ctx, input.Organization, request)
	if err != nil {
		return binding.CodeMappingBinding{}, err
	}
	if created.Identity == "" {
		return binding.CodeMappingBinding{}, ErrVerification
	}
	fresh, err := e.Adapter.ListMappings(ctx, input.Organization)
	if err != nil {
		return binding.CodeMappingBinding{}, err
	}
	matches := exactMappings(fresh, request)
	if len(matches) != 1 || matches[0].Identity != created.Identity {
		return binding.CodeMappingBinding{}, ErrVerification
	}
	completed := submitted
	completed.Phase = sentryapi.OperationVerified
	completed.Revision++
	if err := e.Journal.CompareAndSwap(ctx, submitted.Revision, completed); err != nil {
		return binding.CodeMappingBinding{}, err
	}
	return newBinding(matches[0], desired), nil
}

func (e *Engine) reconcileOne(ctx context.Context, input Input, desired Desired, current binding.CodeMappingBinding, integration Integration, repository Repository, mappings []Remote) (binding.CodeMappingBinding, error) {
	if current.ExternalIdentity == "" || current.IntegrationIdentity != integration.Identity || current.RepositoryIdentity != repository.Identity || current.IntegrationAlias != desired.IntegrationAlias || current.RepositoryAlias != desired.RepositoryAlias {
		return binding.CodeMappingBinding{}, ErrOwnershipLost
	}
	remote, found := mappingByID(mappings, current.ExternalIdentity)
	if !found {
		return binding.CodeMappingBinding{}, ErrExternalMissing
	}
	request := mappingRequest(input, desired, integration.Identity, repository.Identity)
	if mappingMatches(remote, request) {
		next := newBinding(remote, desired)
		next.CodeownersIdentity = current.CodeownersIdentity
		next.CodeownersContentHash = current.CodeownersContentHash
		return next, nil
	}
	if err := input.Authorize(ctx); err != nil {
		return binding.CodeMappingBinding{}, err
	}
	updated, err := e.Adapter.UpdateMapping(ctx, input.Organization, current.ExternalIdentity, request)
	if err != nil {
		return binding.CodeMappingBinding{}, err
	}
	if updated.Identity != current.ExternalIdentity || !mappingMatches(updated, request) {
		return binding.CodeMappingBinding{}, ErrVerification
	}
	next := newBinding(updated, desired)
	next.CodeownersIdentity = current.CodeownersIdentity
	next.CodeownersContentHash = current.CodeownersContentHash
	return next, nil
}

func (e *Engine) reconcileCodeowners(ctx context.Context, input Input, key string, desired Desired, current binding.CodeMappingBinding, items []Codeowners) (binding.CodeMappingBinding, error) {
	var exact []Codeowners
	for _, item := range items {
		if item.CodeMappingID == current.ExternalIdentity {
			exact = append(exact, item)
		}
	}
	if len(exact) > 1 {
		return current, ErrOwnershipLost
	}
	if !desired.ImportCodeowners {
		if current.CodeownersIdentity == "" {
			if len(exact) != 0 {
				return current, ErrOwnershipLost
			}
			return current, nil
		}
		if len(exact) == 0 {
			current.CodeownersIdentity, current.CodeownersContentHash = "", ""
			return current, nil
		}
		if exact[0].Identity != current.CodeownersIdentity {
			return current, ErrOwnershipLost
		}
		if err := input.Authorize(ctx); err != nil {
			return current, err
		}
		if err := e.Adapter.DeleteCodeowners(ctx, input.Organization, input.ProjectSlug, current.CodeownersIdentity); err != nil {
			return current, err
		}
		current.CodeownersIdentity, current.CodeownersContentHash = "", ""
		return current, nil
	}
	source, err := e.Adapter.FetchCodeowners(ctx, input.Organization, current.ExternalIdentity)
	if err != nil {
		return current, err
	}
	contentHash := hashString(source.Raw)
	if current.CodeownersIdentity != "" {
		if len(exact) != 1 || exact[0].Identity != current.CodeownersIdentity {
			return current, ErrOwnershipLost
		}
		if exact[0].Raw == source.Raw {
			current.CodeownersContentHash = contentHash
			return current, nil
		}
		if err := input.Authorize(ctx); err != nil {
			return current, err
		}
		updated, err := e.Adapter.UpdateCodeowners(ctx, input.Organization, input.ProjectSlug, current.CodeownersIdentity, current.ExternalIdentity, source.Raw)
		if err != nil {
			return current, err
		}
		if updated.Identity != current.CodeownersIdentity || updated.Raw != source.Raw {
			return current, ErrVerification
		}
		current.CodeownersContentHash = contentHash
		return current, nil
	}
	if len(exact) != 0 {
		return current, ErrOwnershipLost
	}
	operationKey := "codeowners:create:" + input.Organization + ":" + input.ProjectSlug + ":" + key
	hash := hashString(current.ExternalIdentity + "\x00" + source.Raw)
	operation, found, err := e.Journal.Load(ctx, operationKey)
	if err != nil {
		return current, err
	}
	if found {
		if operation.IdempotencyHash != hash {
			return current, ErrRecoveryPending
		}
		fresh, err := e.Adapter.ListCodeowners(ctx, input.Organization, input.ProjectSlug)
		if err != nil {
			return current, err
		}
		for _, item := range fresh {
			if item.CodeMappingID == current.ExternalIdentity {
				exact = append(exact, item)
			}
		}
		if len(exact) == 1 && exact[0].Raw == source.Raw && operation.Phase != sentryapi.OperationPlanned {
			if operation.Phase != sentryapi.OperationVerified {
				completed := operation
				completed.Phase = sentryapi.OperationVerified
				completed.Revision++
				if err := e.Journal.CompareAndSwap(ctx, operation.Revision, completed); err != nil {
					return current, err
				}
			}
			current.CodeownersIdentity, current.CodeownersContentHash = exact[0].Identity, contentHash
			return current, nil
		}
		if operation.Phase != sentryapi.OperationPlanned {
			return current, ErrRecoveryPending
		}
	} else {
		operation = sentryapi.Operation{Key: operationKey, Kind: "CreateCodeowners", LocalResource: input.ProjectSlug + "/" + key, IdempotencyHash: hash, Phase: sentryapi.OperationPlanned, Revision: 1}
		if err := e.Journal.CompareAndSwap(ctx, 0, operation); err != nil {
			return current, err
		}
	}
	if err := input.Authorize(ctx); err != nil {
		return current, err
	}
	submitted := operation
	submitted.Phase = sentryapi.OperationSubmitted
	submitted.Revision++
	if err := e.Journal.CompareAndSwap(ctx, operation.Revision, submitted); err != nil {
		return current, err
	}
	created, err := e.Adapter.CreateCodeowners(ctx, input.Organization, input.ProjectSlug, current.ExternalIdentity, source.Raw)
	if err != nil {
		return current, err
	}
	if created.Identity == "" || created.CodeMappingID != current.ExternalIdentity || created.Raw != source.Raw {
		return current, ErrVerification
	}
	completed := submitted
	completed.Phase = sentryapi.OperationVerified
	completed.Revision++
	if err := e.Journal.CompareAndSwap(ctx, submitted.Revision, completed); err != nil {
		return current, err
	}
	current.CodeownersIdentity, current.CodeownersContentHash = created.Identity, contentHash
	return current, nil
}

func (e *Engine) deleteOne(ctx context.Context, input Input, current binding.CodeMappingBinding, mappings []Remote, codeowners []Codeowners) error {
	if current.ExternalIdentity == "" {
		return ErrOwnershipLost
	}
	if current.CodeownersIdentity != "" {
		found := false
		for _, item := range codeowners {
			if item.Identity == current.CodeownersIdentity && item.CodeMappingID == current.ExternalIdentity {
				found = true
			}
		}
		if found {
			if err := input.Authorize(ctx); err != nil {
				return err
			}
			if err := e.Adapter.DeleteCodeowners(ctx, input.Organization, input.ProjectSlug, current.CodeownersIdentity); err != nil {
				return err
			}
			fresh, err := e.Adapter.ListCodeowners(ctx, input.Organization, input.ProjectSlug)
			if err != nil {
				return err
			}
			for _, item := range fresh {
				if item.Identity == current.CodeownersIdentity {
					return ErrVerification
				}
			}
		}
	}
	remote, found := mappingByID(mappings, current.ExternalIdentity)
	if !found {
		return nil
	}
	if remote.IntegrationID != current.IntegrationIdentity || remote.RepositoryID != current.RepositoryIdentity {
		return ErrOwnershipLost
	}
	if err := input.Authorize(ctx); err != nil {
		return err
	}
	if err := e.Adapter.DeleteMapping(ctx, input.Organization, current.ExternalIdentity); err != nil {
		return err
	}
	fresh, err := e.Adapter.ListMappings(ctx, input.Organization)
	if err != nil {
		return err
	}
	if _, found := mappingByID(fresh, current.ExternalIdentity); found {
		return ErrVerification
	}
	return nil
}

func mappingRequest(input Input, desired Desired, integrationID, repositoryID string) MappingRequest {
	return MappingRequest{IntegrationID: integrationID, RepositoryID: repositoryID, ProjectID: input.ProjectIdentity, StackRoot: desired.StackRoot, SourceRoot: desired.SourceRoot, DefaultBranch: desired.DefaultBranch}
}
func mappingMatches(remote Remote, request MappingRequest) bool {
	return remote.ProjectID == request.ProjectID && remote.RepositoryID == request.RepositoryID && remote.IntegrationID == request.IntegrationID && remote.StackRoot == request.StackRoot && remote.SourceRoot == request.SourceRoot && remote.DefaultBranch == request.DefaultBranch
}
func exactMappings(values []Remote, request MappingRequest) []Remote {
	var result []Remote
	for _, value := range values {
		if mappingMatches(value, request) {
			result = append(result, value)
		}
	}
	return result
}
func mappingByID(values []Remote, identity string) (Remote, bool) {
	for _, value := range values {
		if value.Identity == identity {
			return value, true
		}
	}
	return Remote{}, false
}
func newBinding(remote Remote, desired Desired) binding.CodeMappingBinding {
	return binding.CodeMappingBinding{ExternalIdentity: remote.Identity, IntegrationIdentity: remote.IntegrationID, RepositoryIdentity: remote.RepositoryID, IntegrationAlias: desired.IntegrationAlias, RepositoryAlias: desired.RepositoryAlias, DesiredHash: desiredHash(desired)}
}
func desiredHash(desired Desired) string {
	value, _ := json.Marshal(desired)
	return hashString(string(value))
}
func hashString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func validateDesired(key string, desired Desired) error {
	if len(key) > 32 || !localAliasPattern.MatchString(key) || !localAliasPattern.MatchString(desired.IntegrationAlias) || !localAliasPattern.MatchString(desired.RepositoryAlias) || desired.Provider == "" || desired.IntegrationName == "" || desired.RepositoryName == "" || desired.ImportCodeowners && desired.DefaultBranch == "" {
		return ErrInvalidInput
	}
	return nil
}
func cloneBindings(values map[string]binding.CodeMappingBinding) map[string]binding.CodeMappingBinding {
	result := make(map[string]binding.CodeMappingBinding, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
func sortedKeys(values map[string]Desired) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func removedKeys(current map[string]binding.CodeMappingBinding, desired map[string]Desired) []string {
	keys := []string{}
	for key := range current {
		if _, found := desired[key]; !found {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
func errorStatus(err error) Status {
	reason := "CodeMappingOperationFailed"
	switch {
	case errors.Is(err, ErrInvalidInput):
		reason = "InvalidCodeMapping"
	case errors.Is(err, ErrCatalogMissing):
		reason = "SourceCodeAliasUnresolved"
	case errors.Is(err, ErrOwnershipLost):
		reason = "CodeMappingOwnershipLost"
	case errors.Is(err, ErrExternalMissing):
		reason = "CodeMappingMissing"
	case errors.Is(err, ErrRecoveryPending):
		reason = "CodeMappingRecoveryPending"
	case errors.Is(err, ErrVerification):
		reason = "CodeMappingVerificationFailed"
	}
	return Status{State: StateError, Reason: reason, Err: err}
}
