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

// Package project implements the ownership-safe project reconciliation state machine.
package project

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/ownership"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/policy"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

type ErrorReason string

const (
	ReasonProjectAlreadyExists   ErrorReason = "ProjectAlreadyExists"
	ReasonOwnershipConflict      ErrorReason = "OwnershipConflict"
	ReasonOwnershipLost          ErrorReason = "OwnershipLost"
	ReasonConnectionStandby      ErrorReason = "ConnectionStandby"
	ReasonProjectStandby         ErrorReason = "ProjectStandby"
	ReasonRevisionFenced         ErrorReason = "RevisionFenced"
	ReasonRevisionConflict       ErrorReason = "RevisionConflict"
	ReasonExternalDeleted        ErrorReason = "ExternalDeleted"
	ReasonCreationOutcomeUnknown ErrorReason = "CreationOutcomeUnknown"
	ReasonDeletionPending        ErrorReason = "DeletionPending"
	ReasonReleasePending         ErrorReason = "ReleasePending"
	ReasonInvalidState           ErrorReason = "InvalidState"
)

const (
	CapabilityAtomicDefaultRules = "project.create.atomic-default-rules"
	CapabilityAutomaticActiveKey = "project.create.automatic-active-key"
	defaultRevisionFencingDelay  = 5 * time.Second
)

// ManagedCreateSupported requires both source-critical lifecycle capabilities.
func ManagedCreateSupported(capabilities []string) bool {
	atomicDefaults := false
	automaticKey := false
	for _, capability := range capabilities {
		switch capability {
		case CapabilityAtomicDefaultRules:
			atomicDefaults = true
		case CapabilityAutomaticActiveKey:
			automaticKey = true
		}
	}
	return atomicDefaults && automaticKey
}

// Error contains a stable reason but no external identity or response body.
type Error struct {
	Reason ErrorReason
	cause  error
}

func (e *Error) Error() string { return fmt.Sprintf("project reconciliation stopped: %s", e.Reason) }
func (e *Error) Unwrap() error { return e.cause }

func IsReason(err error, reason ErrorReason) bool {
	var projectErr *Error
	return errors.As(err, &projectErr) && projectErr.Reason == reason
}

// RemoteProject is internal adapter data. Identity never enters public spec or status.
type RemoteProject struct {
	Identity         string
	Slug             string
	URL              string
	Marker           *binding.Marker
	RegionalEvidence *ownership.Evidence
}

// CreateRequest is the source-confirmed atomic project create contract.
type CreateRequest struct {
	Organization string
	Slug         string
	DisplayName  string
	Team         string
	Platform     string
	DefaultRules bool
	Marker       binding.Marker
	// RegionalEvidence replaces the legacy readable marker in the remote project name when
	// connection-wide revision fencing is enabled. Marker remains protected child ownership.
	RegionalEvidence *ownership.Evidence
}

// Adapter isolates version-specific Sentry HTTP details from ownership logic.
type Adapter interface {
	GetBySlug(ctx context.Context, organization, slug string) (RemoteProject, error)
	Create(ctx context.Context, request CreateRequest) (RemoteProject, error)
	Delete(ctx context.Context, organization, slug string) error
}

// RegionalOwnershipAdapter updates only the compact regional evidence embedded in the
// project display name. Engines require it before taking over an existing lower revision.
type RegionalOwnershipAdapter interface {
	UpdateRegionalEvidence(ctx context.Context, organization, slug string, evidence ownership.Evidence) error
}

// OwnershipReleaseAdapter removes only the operator marker while preserving the project.
type OwnershipReleaseAdapter interface {
	ReleaseOwnership(ctx context.Context, organization, slug string) error
}

// Input contains one already-resolved connection and project.
type Input struct {
	Project        *sentryv1alpha1.SentryProject
	ConnectionName string
	Connection     sentryv1alpha1.SentryConnectionSpec
	// CanonicalInstallation may come from safe discovery; empty means normalize spec.URL.
	CanonicalInstallation string
}

type Result struct {
	Origin     binding.Origin
	AccessMode string
	Owner      string
	ProjectURL string
}

type DeletionResult struct {
	Complete bool
	Retained bool
}

type Engine struct {
	Bindings  *binding.Store
	Adapter   Adapter
	ManagedBy string
	Audit     audit.Sink
	Clock     ownership.Clock
	// FencingDelay defaults to five seconds when zero.
	FencingDelay time.Duration
}

var ErrAuditUnavailable = errors.New("project mutation audit is unavailable")

func (e *Engine) Reconcile(ctx context.Context, input Input) (Result, error) {
	if e.Bindings == nil || e.Adapter == nil || e.ManagedBy == "" || input.Project == nil || input.Project.UID == "" {
		return Result{}, &Error{Reason: ReasonInvalidState}
	}
	installation := input.CanonicalInstallation
	var err error
	if installation == "" {
		installation, err = binding.CanonicalInstallation(input.Connection.URL)
		if err != nil {
			return Result{}, &Error{Reason: ReasonInvalidState, cause: err}
		}
	}
	decision, err := policy.EvaluateProject(input.Project.Namespace, input.Project.Spec, input.Connection)
	if err != nil {
		return Result{}, err
	}
	if input.Project.Spec.Project.Create != nil {
		if input.Project.Spec.Role == sentryv1alpha1.ProjectRoleStandby {
			return e.reconcileStandby(ctx, input, installation)
		}
		decision, err = e.preserveBoundOwner(ctx, input, decision)
		if err != nil {
			return Result{}, err
		}
		if e.Audit == nil {
			return Result{}, &Error{Reason: ReasonInvalidState, cause: ErrAuditUnavailable}
		}
		return e.reconcileManaged(ctx, input, installation, decision)
	}
	return e.reconcileReadOnly(ctx, input, installation, decision)
}

func (e *Engine) reconcileStandby(ctx context.Context, input Input, installation string) (Result, error) {
	project := input.Project
	slug := project.Name
	remote, err := e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
	if err != nil {
		return Result{}, err
	}
	if remote.Identity == "" || remote.Slug != slug {
		return Result{}, &Error{Reason: ReasonInvalidState}
	}
	record, err := e.Bindings.GetBinding(ctx, string(project.UID))
	if err == nil {
		if record.Binding.Project.UID != string(project.UID) || record.Binding.Project.Namespace != project.Namespace || record.Binding.Project.Name != project.Name || record.Binding.Connection != input.ConnectionName || record.Binding.Installation != installation || record.Binding.Organization != input.Connection.Organization || record.Binding.Slug != slug || record.Binding.ExternalIdentity != remote.Identity || record.Binding.AccessMode != binding.AccessReadOnly {
			return Result{}, &Error{Reason: ReasonOwnershipLost}
		}
		if record.Binding.Origin == binding.OriginExternal {
			if input.Connection.ProjectManagement.UnmanagedProjects != sentryv1alpha1.UnmanagedProjectsStandby || remote.Marker != nil || remote.RegionalEvidence != nil {
				return Result{}, &Error{Reason: ReasonOwnershipLost}
			}
		} else if !e.matchesStandbyManagedSource(ctx, record.Binding, remote) {
			return Result{}, &Error{Reason: ReasonOwnershipLost}
		}
		return Result{Origin: record.Binding.Origin, AccessMode: binding.AccessReadOnly, Owner: record.Binding.Owner, ProjectURL: remote.URL}, nil
	}
	if !errors.Is(err, binding.ErrNotFound) {
		return Result{}, err
	}

	state := binding.ProjectBinding{
		Version: binding.CurrentVersion, Project: projectReference(project), Connection: input.ConnectionName,
		Installation: installation, Organization: input.Connection.Organization, Slug: slug,
		Origin: binding.OriginExternal, AccessMode: binding.AccessReadOnly, ExternalIdentity: remote.Identity,
	}
	managed, found, err := e.findStandbyManagedSource(ctx, installation, input.Connection.Organization, slug, remote)
	if err != nil {
		return Result{}, err
	}
	if found {
		state.Origin = binding.OriginManaged
		state.Owner = managed.Binding.Owner
		marker := *managed.Binding.Marker
		state.Marker = &marker
		if managed.Binding.RegionalOwnership != nil {
			regional := *managed.Binding.RegionalOwnership
			state.RegionalOwnership = &regional
		}
	} else if remote.Marker != nil || remote.RegionalEvidence != nil || input.Connection.ProjectManagement.UnmanagedProjects != sentryv1alpha1.UnmanagedProjectsStandby {
		return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
	}
	if _, err := e.Bindings.CreateBinding(ctx, state); err != nil {
		return Result{}, err
	}
	return Result{Origin: state.Origin, AccessMode: binding.AccessReadOnly, Owner: state.Owner, ProjectURL: remote.URL}, nil
}

func (e *Engine) findStandbyManagedSource(ctx context.Context, installation, organization, slug string, remote RemoteProject) (binding.BindingRecord, bool, error) {
	if remote.RegionalEvidence != nil {
		return e.Bindings.FindRegionalManaged(ctx, installation, organization, slug, remote.Identity, *remote.RegionalEvidence)
	}
	if remote.Marker != nil {
		return e.Bindings.FindManaged(ctx, installation, organization, slug, remote.Identity, *remote.Marker)
	}
	return binding.BindingRecord{}, false, nil
}

func (e *Engine) matchesStandbyManagedSource(ctx context.Context, candidate binding.ProjectBinding, remote RemoteProject) bool {
	managed, found, err := e.findStandbyManagedSource(ctx, candidate.Installation, candidate.Organization, candidate.Slug, remote)
	if err != nil || !found || candidate.Marker == nil || managed.Binding.Marker == nil {
		return false
	}
	return candidate.Owner == managed.Binding.Owner && candidate.Marker.Matches(*managed.Binding.Marker) && equalRegionalOwnership(candidate.RegionalOwnership, managed.Binding.RegionalOwnership)
}

// Delete applies Retain or guarded DeleteProject semantics. A caller removes the finalizer
// only when Complete is true.
func (e *Engine) Delete(ctx context.Context, input Input) (DeletionResult, error) {
	if e.Bindings == nil || e.Adapter == nil || input.Project == nil || input.Project.UID == "" {
		return DeletionResult{}, &Error{Reason: ReasonInvalidState}
	}
	if input.Project.Spec.Project.Create == nil {
		if input.Project.Spec.Lifecycle.OnDelete != sentryv1alpha1.OnDeleteDeleteProject {
			return e.releaseReadOnlyBinding(ctx, input)
		}
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}
	if e.Audit == nil {
		return DeletionResult{}, &Error{Reason: ReasonInvalidState, cause: ErrAuditUnavailable}
	}
	installation := input.CanonicalInstallation
	var err error
	if installation == "" {
		installation, err = binding.CanonicalInstallation(input.Connection.URL)
		if err != nil {
			return DeletionResult{}, &Error{Reason: ReasonInvalidState}
		}
	}
	state, err := e.Bindings.GetBinding(ctx, string(input.Project.UID))
	if errors.Is(err, binding.ErrNotFound) {
		return e.recoverMissingBinding(ctx, input, installation)
	}
	if err != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	if input.Project.Spec.Lifecycle.OnDelete != sentryv1alpha1.OnDeleteDeleteProject && state.Binding.AccessMode == binding.AccessReadOnly {
		return e.releaseReadOnlyBinding(ctx, input)
	}
	decision, err := policy.EvaluateProject(input.Project.Namespace, input.Project.Spec, input.Connection)
	if err != nil || decision.AccessMode != binding.AccessManage {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	decision, err = e.preserveBoundOwner(ctx, input, decision)
	if err != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	if input.Project.Spec.Lifecycle.OnDelete != sentryv1alpha1.OnDeleteDeleteProject {
		return e.releaseManaged(ctx, input, installation, decision, state)
	}
	slug := input.Project.Name
	marker := binding.Marker{Origin: "managed", Owner: decision.Owner, ManagedBy: e.ManagedBy}
	regional, _, regionalErr := desiredRegionalOwnership(input.Connection, decision.Owner, e.ManagedBy)
	if regionalErr != nil {
		return DeletionResult{}, regionalErr
	}
	if !matchesManagedContext(state.Binding, input, installation, slug, decision.Owner, marker, regional) || state.Binding.ExternalIdentity == "" {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}
	claim, err := e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, slug)
	if errors.Is(err, binding.ErrNotFound) && state.Binding.Pending != nil && state.Binding.Pending.Kind == "DeleteProject" && state.Binding.Pending.Phase == string(sentryapi.OperationSubmitted) {
		_, getErr := e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
		if sentryapi.IsKind(getErr, sentryapi.ErrorNotFound) {
			if auditErr := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeRecovered, audit.ReasonResultRecovered); auditErr != nil {
				return DeletionResult{}, auditErr
			}
			if deleteErr := e.Bindings.DeleteBinding(ctx, state); deleteErr != nil {
				return DeletionResult{}, deleteErr
			}
			return DeletionResult{Complete: true}, nil
		}
		if getErr != nil {
			return DeletionResult{}, getErr
		}
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: fmt.Errorf("manager claim missing while project still exists")}
	}
	if err != nil || claim.Claim.Manager.UID != string(input.Project.UID) || claim.Claim.ExternalIdentity != state.Binding.ExternalIdentity || claim.Claim.Handoff != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	remote, err := e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		if state.Binding.Pending != nil && state.Binding.Pending.Kind == "DeleteProject" && state.Binding.Pending.Phase == string(sentryapi.OperationSubmitted) {
			if auditErr := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeRecovered, audit.ReasonResultRecovered); auditErr != nil {
				return DeletionResult{}, auditErr
			}
		}
		return e.completeDeletion(ctx, state, claim)
	}
	if err != nil {
		return DeletionResult{}, err
	}
	if remote.Identity != state.Binding.ExternalIdentity || !matchesRemoteOwnership(remote, marker, regional) {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}

	// Re-read authorization immediately before DELETE.
	claim, err = e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, slug)
	if err != nil || claim.Claim.Manager.UID != string(input.Project.UID) || claim.Claim.ExternalIdentity != remote.Identity || claim.Claim.Handoff != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	next := state.Binding
	next.Pending = &binding.OperationState{Kind: "DeleteProject", Phase: string(sentryapi.OperationSubmitted), DesiredHash: hashValue(state.Binding.ExternalIdentity)}
	state, err = e.Bindings.UpdateBinding(ctx, state, next)
	if err != nil {
		return DeletionResult{}, err
	}
	if err := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeStarted, audit.ReasonJournalSubmitted); err != nil {
		return DeletionResult{}, err
	}
	if err := e.Adapter.Delete(ctx, input.Connection.Organization, slug); err != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeFailed, audit.ReasonApply); auditErr != nil {
			return DeletionResult{}, auditErr
		}
		return DeletionResult{}, err
	}
	remote, err = e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
	if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
		if auditErr := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeSucceeded, audit.ReasonMutationVerified); auditErr != nil {
			return DeletionResult{}, auditErr
		}
		latestClaim, claimErr := e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, slug)
		if claimErr != nil {
			return DeletionResult{}, claimErr
		}
		return e.completeDeletion(ctx, state, latestClaim)
	}
	if err != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeFailed, audit.ReasonVerification); auditErr != nil {
			return DeletionResult{}, auditErr
		}
		return DeletionResult{}, err
	}
	if remote.Identity != state.Binding.ExternalIdentity {
		if auditErr := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeFailed, audit.ReasonVerification); auditErr != nil {
			return DeletionResult{}, auditErr
		}
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}
	if auditErr := e.recordAudit(ctx, input, audit.ActionDelete, audit.OutcomeFailed, audit.ReasonVerification); auditErr != nil {
		return DeletionResult{}, auditErr
	}
	return DeletionResult{}, &Error{Reason: ReasonDeletionPending}
}

// recoverMissingBinding completes only local cleanup states that cannot represent an owned
// remote project. A same-project claim without a binding is possible before the first binding
// create, or after completeDeletion removed the binding and claim deletion failed. Any remote
// ownership evidence still fails closed because the missing binding is the authorization record.
func (e *Engine) recoverMissingBinding(ctx context.Context, input Input, installation string) (DeletionResult, error) {
	retain := input.Project.Spec.Lifecycle.OnDelete != sentryv1alpha1.OnDeleteDeleteProject
	claim, err := e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, input.Project.Name)
	if errors.Is(err, binding.ErrNotFound) {
		if retain {
			return DeletionResult{Complete: true, Retained: true}, nil
		}
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}
	if err != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	manager := claim.Claim.Manager
	if manager.UID != string(input.Project.UID) || manager.Namespace != input.Project.Namespace || manager.Name != input.Project.Name || claim.Claim.Handoff != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}

	remoteRetained := false
	if claim.Claim.ExternalIdentity != "" {
		remote, remoteErr := e.Adapter.GetBySlug(ctx, input.Connection.Organization, input.Project.Name)
		if remoteErr == nil {
			if remote.Identity != claim.Claim.ExternalIdentity || remote.Marker != nil || remote.RegionalEvidence != nil {
				return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
			}
			remoteRetained = true
		} else if !sentryapi.IsKind(remoteErr, sentryapi.ErrorNotFound) {
			return DeletionResult{}, remoteErr
		}
	}
	if err := e.Bindings.DeleteClaim(ctx, claim); err != nil && !errors.Is(err, binding.ErrNotFound) {
		return DeletionResult{}, err
	}
	return DeletionResult{Complete: true, Retained: retain || remoteRetained}, nil
}

func (e *Engine) releaseReadOnlyBinding(ctx context.Context, input Input) (DeletionResult, error) {
	record, err := e.Bindings.GetBinding(ctx, string(input.Project.UID))
	if errors.Is(err, binding.ErrNotFound) {
		return DeletionResult{Complete: true, Retained: true}, nil
	}
	if err != nil || record.Binding.AccessMode != binding.AccessReadOnly || record.Binding.Project.UID != string(input.Project.UID) {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	if err := e.Bindings.DeleteBinding(ctx, record); err != nil {
		return DeletionResult{}, err
	}
	return DeletionResult{Complete: true, Retained: true}, nil
}

func (e *Engine) releaseManaged(ctx context.Context, input Input, installation string, decision policy.Decision, state binding.BindingRecord) (DeletionResult, error) {
	project := input.Project
	marker := binding.Marker{Origin: "managed", Owner: decision.Owner, ManagedBy: e.ManagedBy}
	regional, _, err := desiredRegionalOwnership(input.Connection, decision.Owner, e.ManagedBy)
	if err != nil {
		return DeletionResult{}, err
	}
	if !matchesManagedContext(state.Binding, input, installation, project.Name, decision.Owner, marker, regional) || state.Binding.ExternalIdentity == "" {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}
	if len(state.Binding.Alerts) != 0 || len(state.Binding.Destinations) != 0 || len(state.Binding.GrantedTeams) != 0 || len(state.Binding.ClientKeys) != 0 {
		return DeletionResult{}, &Error{Reason: ReasonReleasePending}
	}
	claim, err := e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, project.Name)
	if err != nil || claim.Claim.Manager.UID != string(project.UID) || claim.Claim.ExternalIdentity != state.Binding.ExternalIdentity || claim.Claim.Handoff != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	remote, err := e.Adapter.GetBySlug(ctx, state.Binding.Organization, state.Binding.Slug)
	if err != nil {
		return DeletionResult{}, err
	}
	if remote.Identity != state.Binding.ExternalIdentity {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}
	if remote.Marker == nil && remote.RegionalEvidence == nil && state.Binding.Pending != nil && state.Binding.Pending.Kind == "ReleaseOwnership" && state.Binding.Pending.Phase == string(sentryapi.OperationSubmitted) {
		if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
			return DeletionResult{}, err
		}
		result, err := e.completeDeletion(ctx, state, claim)
		result.Retained = true
		return result, err
	}
	if !matchesRemoteOwnership(remote, marker, regional) {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost}
	}
	next := state.Binding
	next.Pending = &binding.OperationState{Kind: "ReleaseOwnership", Phase: string(sentryapi.OperationSubmitted), DesiredHash: hashValue(state.Binding.ExternalIdentity)}
	state, err = e.Bindings.UpdateBinding(ctx, state, next)
	if err != nil {
		return DeletionResult{}, err
	}
	if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeStarted, audit.ReasonJournalSubmitted); err != nil {
		return DeletionResult{}, err
	}
	claim, err = e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, project.Name)
	if err != nil || claim.Claim.Manager.UID != string(project.UID) || claim.Claim.ExternalIdentity != state.Binding.ExternalIdentity || claim.Claim.Handoff != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	releaser, ok := e.Adapter.(OwnershipReleaseAdapter)
	if !ok {
		return DeletionResult{}, &Error{Reason: ReasonInvalidState, cause: errors.New("project adapter does not support ownership release")}
	}
	if err := releaser.ReleaseOwnership(ctx, state.Binding.Organization, state.Binding.Slug); err != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeFailed, audit.ReasonApply); auditErr != nil {
			return DeletionResult{}, auditErr
		}
		return DeletionResult{}, err
	}
	remote, err = e.Adapter.GetBySlug(ctx, state.Binding.Organization, state.Binding.Slug)
	if err != nil || remote.Identity != state.Binding.ExternalIdentity || remote.Marker != nil || remote.RegionalEvidence != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeFailed, audit.ReasonVerification); auditErr != nil {
			return DeletionResult{}, auditErr
		}
		return DeletionResult{}, &Error{Reason: ReasonReleasePending, cause: err}
	}
	if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
		return DeletionResult{}, err
	}
	result, err := e.completeDeletion(ctx, state, claim)
	result.Retained = true
	return result, err
}

// preserveBoundOwner keeps pre-migration ownership evidence stable after the public connection
// access policy is removed. New projects use their primary team as owner; an exact existing
// project incarnation continues to use the owner already fenced in protected and remote state.
func (e *Engine) preserveBoundOwner(ctx context.Context, input Input, decision policy.Decision) (policy.Decision, error) {
	record, err := e.Bindings.GetBinding(ctx, string(input.Project.UID))
	if errors.Is(err, binding.ErrNotFound) {
		return decision, nil
	}
	if err != nil {
		return policy.Decision{}, err
	}
	state := record.Binding
	if state.Project.UID == string(input.Project.UID) && state.Project.Namespace == input.Project.Namespace &&
		state.Project.Name == input.Project.Name && state.Connection == input.ConnectionName &&
		state.Organization == input.Connection.Organization && state.Origin == binding.OriginManaged && state.Owner != "" {
		decision.Owner = state.Owner
	}
	return decision, nil
}

func (e *Engine) completeDeletion(ctx context.Context, state binding.BindingRecord, claim binding.ClaimRecord) (DeletionResult, error) {
	// Delete the UID-scoped binding first. If claim deletion then fails, Delete can safely
	// recover the same-UID claim after proving that no remote ownership evidence remains.
	if err := e.Bindings.DeleteBinding(ctx, state); err != nil && !errors.Is(err, binding.ErrNotFound) {
		return DeletionResult{}, err
	}
	if err := e.Bindings.DeleteClaim(ctx, claim); err != nil && !errors.Is(err, binding.ErrNotFound) {
		return DeletionResult{}, err
	}
	return DeletionResult{Complete: true}, nil
}

func (e *Engine) reconcileManaged(ctx context.Context, input Input, installation string, decision policy.Decision) (Result, error) {
	project := input.Project
	create := project.Spec.Project.Create
	slug := project.Name
	reference := projectReference(project)
	marker := binding.Marker{Origin: "managed", Owner: decision.Owner, ManagedBy: e.ManagedBy}
	regional, regionalEvidence, regionalErr := desiredRegionalOwnership(input.Connection, decision.Owner, e.ManagedBy)
	if regionalErr != nil {
		return Result{}, regionalErr
	}
	claim := binding.ManagerClaim{
		Version:      binding.CurrentVersion,
		Installation: installation,
		Organization: input.Connection.Organization,
		Slug:         slug,
		Owner:        decision.Owner,
		Manager:      reference,
	}
	bindingRecord, err := e.Bindings.GetBinding(ctx, string(project.UID))
	if errors.Is(err, binding.ErrNotFound) {
		preflight, preflightErr := e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
		if preflightErr == nil {
			if input.Connection.ProjectManagement.UnmanagedProjects == sentryv1alpha1.UnmanagedProjectsAdopt && preflight.Marker == nil && preflight.RegionalEvidence == nil {
				return e.reconcileAdoption(ctx, input, installation, decision, preflight, marker, regional, regionalEvidence)
			}
			claimRecord, claimErr := e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, slug)
			if errors.Is(claimErr, binding.ErrNotFound) {
				return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
			}
			if claimErr != nil {
				return Result{}, claimErr
			}
			if claimRecord.Claim.Manager.UID != string(project.UID) {
				return Result{}, &Error{Reason: ReasonOwnershipConflict}
			}
			if claimRecord.Claim.Owner != decision.Owner || e.Bindings.AuthorizeHandoff(claimRecord, reference, slug) != nil {
				return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
			}
			state := newManagedBinding(input, installation, slug, decision.Owner, marker, regional, claimRecord.Name, reference, create)
			bindingRecord, err = e.Bindings.CreateBinding(ctx, state)
			if err != nil {
				return Result{}, err
			}
			return e.handleExistingManaged(ctx, bindingRecord, claimRecord, preflight, reference, marker, regional)
		}
		if !sentryapi.IsKind(preflightErr, sentryapi.ErrorNotFound) {
			return Result{}, preflightErr
		}
		if err := validateManagedName(slug, marker, regionalEvidence); err != nil {
			return Result{}, &Error{Reason: ReasonInvalidState, cause: err}
		}

		claimRecord, _, claimErr := e.Bindings.AcquireClaim(ctx, claim)
		if errors.Is(claimErr, binding.ErrOwnershipConflict) {
			return Result{}, &Error{Reason: ReasonOwnershipConflict}
		}
		if claimErr != nil {
			return Result{}, claimErr
		}
		if claimRecord.Claim.Owner != decision.Owner {
			return Result{}, &Error{Reason: ReasonOwnershipLost}
		}
		state := newManagedBinding(input, installation, slug, decision.Owner, marker, regional, claimRecord.Name, reference, create)
		bindingRecord, err = e.Bindings.CreateBinding(ctx, state)
	}
	if err != nil {
		return Result{}, err
	}
	if bindingRecord.Binding.AccessMode == binding.AccessReadOnly {
		switch bindingRecord.Binding.Origin {
		case binding.OriginExternal:
			if input.Connection.ProjectManagement.UnmanagedProjects != sentryv1alpha1.UnmanagedProjectsAdopt || regional == nil {
				return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
			}
		case binding.OriginManaged:
			if bindingRecord.Binding.Owner != decision.Owner || bindingRecord.Binding.Marker == nil || !bindingRecord.Binding.Marker.Matches(marker) || !equalRegionalOwnership(bindingRecord.Binding.RegionalOwnership, regional) {
				return Result{}, &Error{Reason: ReasonOwnershipLost}
			}
		default:
			return Result{}, &Error{Reason: ReasonOwnershipLost}
		}
	}
	claimRecord, claimCreated, err := e.Bindings.AcquireClaim(ctx, claim)
	if errors.Is(err, binding.ErrOwnershipConflict) {
		return Result{}, &Error{Reason: ReasonOwnershipConflict}
	}
	if err != nil {
		return Result{}, err
	}
	if claimRecord.Claim.Owner != decision.Owner {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if bindingRecord.Binding.Origin == binding.OriginExternal && bindingRecord.Binding.AccessMode == binding.AccessReadOnly {
		return e.promoteExternalStandbyByAdoption(ctx, input, bindingRecord, claimRecord, decision, marker, regional, regionalEvidence)
	}
	if bindingRecord.Binding.Origin == binding.OriginManaged && bindingRecord.Binding.AccessMode == binding.AccessReadOnly {
		return e.promoteStandby(ctx, input, bindingRecord, claimRecord, claimCreated, marker, regional)
	}
	if !matchesStaticManagedContext(bindingRecord.Binding, input, installation, slug, decision.Owner, marker) {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if bindingRecord.Binding.Pending != nil && bindingRecord.Binding.Pending.Kind == "AdoptProject" {
		return e.continueAdoption(ctx, input, bindingRecord, marker, regional, regionalEvidence)
	}
	bindingRecord, err = e.reconcileRegionalOwnership(ctx, input, bindingRecord, regional)
	if err != nil {
		return Result{}, err
	}

	remote, getErr := e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
	if getErr == nil {
		return e.handleExistingManaged(ctx, bindingRecord, claimRecord, remote, reference, marker, regional)
	}
	if !sentryapi.IsKind(getErr, sentryapi.ErrorNotFound) {
		return Result{}, getErr
	}
	if bindingRecord.Binding.ExternalIdentity != "" {
		return Result{}, &Error{Reason: ReasonExternalDeleted}
	}
	if claimRecord.Claim.Handoff != nil || (bindingRecord.Binding.Pending != nil && bindingRecord.Binding.Pending.Phase == string(sentryapi.OperationSubmitted)) {
		return Result{}, &Error{Reason: ReasonCreationOutcomeUnknown}
	}
	// Validate the complete Sentry name before moving the durable create intent
	// to Submitted. FormatManagedName is deterministic and performs no I/O, so a
	// local validation error must never look like an uncertain external mutation.
	if err := validateManagedName(slug, marker, regionalEvidence); err != nil {
		return Result{}, &Error{Reason: ReasonInvalidState, cause: err}
	}

	// Re-read the claim immediately before the only external mutation.
	claimRecord, err = e.Bindings.GetClaim(ctx, installation, input.Connection.Organization, slug)
	if err != nil || claimRecord.Claim.Manager.UID != string(project.UID) || claimRecord.Claim.Owner != decision.Owner || claimRecord.Claim.Handoff != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	nextBinding := bindingRecord.Binding
	nextBinding.Pending.Phase = string(sentryapi.OperationSubmitted)
	bindingRecord, err = e.Bindings.UpdateBinding(ctx, bindingRecord, nextBinding)
	if err != nil {
		return Result{}, err
	}
	if err := e.recordAudit(ctx, input, audit.ActionCreate, audit.OutcomeStarted, audit.ReasonJournalSubmitted); err != nil {
		return Result{}, err
	}
	remote, err = e.Adapter.Create(ctx, CreateRequest{
		Organization:     input.Connection.Organization,
		Slug:             slug,
		DisplayName:      create.DisplayName,
		Team:             create.Team,
		Platform:         create.Platform,
		DefaultRules:     false,
		Marker:           marker,
		RegionalEvidence: regionalEvidence,
	})
	if err != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionCreate, audit.OutcomeFailed, audit.ReasonApply); auditErr != nil {
			return Result{}, auditErr
		}
		return Result{}, &Error{Reason: ReasonCreationOutcomeUnknown, cause: err}
	}
	if remote.Identity == "" || remote.Slug != slug || !matchesRemoteOwnership(remote, marker, regional) {
		if auditErr := e.recordAudit(ctx, input, audit.ActionCreate, audit.OutcomeFailed, audit.ReasonVerification); auditErr != nil {
			return Result{}, auditErr
		}
		return Result{}, &Error{Reason: ReasonCreationOutcomeUnknown}
	}
	if err := e.recordAudit(ctx, input, audit.ActionCreate, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
		return Result{}, err
	}
	return e.bindVerifiedRemote(ctx, bindingRecord, claimRecord, remote, marker, regional)
}

func (e *Engine) promoteExternalStandbyByAdoption(ctx context.Context, input Input, record binding.BindingRecord, claim binding.ClaimRecord, decision policy.Decision, marker binding.Marker, regional *ownership.Marker, evidence *ownership.Evidence) (Result, error) {
	state := record.Binding
	project := input.Project
	if input.Connection.ProjectManagement.UnmanagedProjects != sentryv1alpha1.UnmanagedProjectsAdopt || regional == nil || evidence == nil || state.Project.UID != string(project.UID) || state.Project.Namespace != project.Namespace || state.Project.Name != project.Name || state.Connection != input.ConnectionName || state.Organization != input.Connection.Organization || state.Slug != project.Name || state.Origin != binding.OriginExternal || state.AccessMode != binding.AccessReadOnly || state.ExternalIdentity == "" || claim.Claim.Manager.UID != string(project.UID) || claim.Claim.Owner != decision.Owner || claim.Claim.Handoff != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if claim.Claim.ExternalIdentity == "" {
		nextClaim := claim.Claim
		nextClaim.ExternalIdentity = state.ExternalIdentity
		var err error
		claim, err = e.Bindings.UpdateClaim(ctx, claim, nextClaim)
		if err != nil {
			return Result{}, err
		}
	} else if claim.Claim.ExternalIdentity != state.ExternalIdentity {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	remote, err := e.Adapter.GetBySlug(ctx, state.Organization, state.Slug)
	if err != nil {
		return Result{}, err
	}
	if remote.Identity != state.ExternalIdentity || remote.Slug != state.Slug || remote.Marker != nil || remote.RegionalEvidence != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	next := state
	next.Project = projectReference(project)
	next.Owner = decision.Owner
	next.Origin = binding.OriginManaged
	next.AccessMode = binding.AccessManage
	next.Marker = &marker
	next.RegionalOwnership = regional
	next.ManagerUID = string(project.UID)
	next.ClaimName = claim.Name
	next.Pending = &binding.OperationState{
		Kind: "AdoptProject", Phase: string(sentryapi.OperationPlanned),
		DesiredHash: hashValue("adopt\x00" + state.ExternalIdentity + "\x00" + state.Slug + "\x00" + regional.Region + "\x00" + fmt.Sprintf("%d", regional.Revision) + "\x00" + regional.Owner + "\x00" + regional.Installation),
	}
	updated, err := e.Bindings.UpdateBinding(ctx, record, next)
	if err != nil {
		return Result{}, err
	}
	return e.continueAdoption(ctx, input, updated, marker, regional, evidence)
}

func (e *Engine) promoteStandby(ctx context.Context, input Input, record binding.BindingRecord, claim binding.ClaimRecord, claimCreated bool, marker binding.Marker, regional *ownership.Marker) (Result, error) {
	project := input.Project
	state := record.Binding
	if state.Project.UID != string(project.UID) || state.Project.Namespace != project.Namespace || state.Project.Name != project.Name || state.Connection != input.ConnectionName || state.Organization != input.Connection.Organization || state.Slug != project.Name || state.Origin != binding.OriginManaged || state.AccessMode != binding.AccessReadOnly || state.ExternalIdentity == "" || state.Owner != marker.Owner || state.Marker == nil || !state.Marker.Matches(marker) || !equalRegionalOwnership(state.RegionalOwnership, regional) {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	reference := projectReference(project)
	if claim.Claim.Manager.UID != string(project.UID) || claim.Claim.Owner != state.Owner {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if claim.Claim.Handoff != nil {
		if claimCreated || e.Bindings.AuthorizeHandoff(claim, reference, state.Slug) != nil {
			return Result{}, &Error{Reason: ReasonOwnershipLost}
		}
	} else if !claimCreated && claim.Claim.ExternalIdentity != state.ExternalIdentity {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if claim.Claim.ExternalIdentity == "" {
		nextClaim := claim.Claim
		nextClaim.ExternalIdentity = state.ExternalIdentity
		var err error
		claim, err = e.Bindings.UpdateClaim(ctx, claim, nextClaim)
		if err != nil {
			return Result{}, err
		}
	}
	remote, err := e.Adapter.GetBySlug(ctx, state.Organization, state.Slug)
	if err != nil {
		return Result{}, err
	}
	if remote.Identity != state.ExternalIdentity || remote.Slug != state.Slug || !matchesRemoteOwnership(remote, marker, regional) {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	latestClaim, err := e.Bindings.GetClaim(ctx, state.Installation, state.Organization, state.Slug)
	if err != nil || latestClaim.Claim.Manager.UID != string(project.UID) || latestClaim.Claim.Owner != state.Owner || latestClaim.Claim.ExternalIdentity != state.ExternalIdentity {
		return Result{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	if latestClaim.Claim.Handoff != nil && e.Bindings.AuthorizeHandoff(latestClaim, reference, state.Slug) != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	next := state
	next.Project = reference
	next.AccessMode = binding.AccessManage
	next.ManagerUID = string(project.UID)
	next.ClaimName = latestClaim.Name
	next.Marker = &marker
	next.RegionalOwnership = regional
	updated, err := e.Bindings.UpdateBinding(ctx, record, next)
	if err != nil {
		return Result{}, err
	}
	if latestClaim.Claim.Handoff != nil {
		if _, err := e.Bindings.CompleteHandoff(ctx, latestClaim, reference, state.Slug); err != nil {
			return Result{}, err
		}
	}
	return Result{Origin: binding.OriginManaged, AccessMode: binding.AccessManage, Owner: updated.Binding.Owner, ProjectURL: remote.URL}, nil
}

func (e *Engine) reconcileAdoption(ctx context.Context, input Input, installation string, decision policy.Decision, remote RemoteProject, marker binding.Marker, regional *ownership.Marker, evidence *ownership.Evidence) (Result, error) {
	if regional == nil || evidence == nil || input.Connection.Ownership == nil || input.Connection.Ownership.Role != sentryv1alpha1.ConnectionRoleActive || remote.Identity == "" || remote.Slug != input.Project.Name {
		return Result{}, &Error{Reason: ReasonInvalidState}
	}
	reference := projectReference(input.Project)
	claim := binding.ManagerClaim{
		Version: binding.CurrentVersion, Installation: installation,
		Organization: input.Connection.Organization, Slug: input.Project.Name,
		Owner: decision.Owner, Manager: reference,
	}
	claimRecord, _, err := e.Bindings.AcquireClaim(ctx, claim)
	if errors.Is(err, binding.ErrOwnershipConflict) {
		return Result{}, &Error{Reason: ReasonOwnershipConflict}
	}
	if err != nil {
		return Result{}, err
	}
	if claimRecord.Claim.Owner != decision.Owner || claimRecord.Claim.Manager != reference || claimRecord.Claim.Handoff != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if claimRecord.Claim.ExternalIdentity == "" {
		nextClaim := claimRecord.Claim
		nextClaim.ExternalIdentity = remote.Identity
		claimRecord, err = e.Bindings.UpdateClaim(ctx, claimRecord, nextClaim)
		if err != nil {
			return Result{}, err
		}
	} else if claimRecord.Claim.ExternalIdentity != remote.Identity {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}

	state := newManagedBinding(input, installation, input.Project.Name, decision.Owner, marker, regional, claimRecord.Name, reference, input.Project.Spec.Project.Create)
	state.ExternalIdentity = remote.Identity
	state.Pending = &binding.OperationState{
		Kind: "AdoptProject", Phase: string(sentryapi.OperationPlanned),
		DesiredHash: hashValue("adopt\x00" + remote.Identity + "\x00" + input.Project.Name + "\x00" + regional.Region + "\x00" + fmt.Sprintf("%d", regional.Revision) + "\x00" + regional.Owner + "\x00" + regional.Installation),
	}
	bindingRecord, err := e.Bindings.CreateBinding(ctx, state)
	if err != nil {
		return Result{}, err
	}
	return e.continueAdoption(ctx, input, bindingRecord, marker, regional, evidence)
}

func (e *Engine) continueAdoption(ctx context.Context, input Input, record binding.BindingRecord, marker binding.Marker, regional *ownership.Marker, evidence *ownership.Evidence) (Result, error) {
	if regional == nil || evidence == nil || record.Binding.Pending == nil || record.Binding.Pending.Kind != "AdoptProject" || record.Binding.ExternalIdentity == "" || !matchesManagedContext(record.Binding, input, record.Binding.Installation, input.Project.Name, record.Binding.Owner, marker, regional) {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	claim, err := e.Bindings.GetClaim(ctx, record.Binding.Installation, record.Binding.Organization, record.Binding.Slug)
	if err != nil || claim.Claim.Manager.UID != string(input.Project.UID) || claim.Claim.Owner != record.Binding.Owner || claim.Claim.ExternalIdentity != record.Binding.ExternalIdentity || claim.Claim.Handoff != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	remote, err := e.Adapter.GetBySlug(ctx, record.Binding.Organization, record.Binding.Slug)
	if err != nil {
		return Result{}, err
	}
	if remote.Identity != record.Binding.ExternalIdentity || remote.Slug != record.Binding.Slug {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if remote.RegionalEvidence != nil && ownership.ValidateEvidence(ownership.Local{Marker: *regional, Active: true}, *remote.RegionalEvidence) == nil {
		if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
			return Result{}, err
		}
		return e.completeAdoption(ctx, record, remote)
	}
	if remote.Marker != nil || remote.RegionalEvidence != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	auditReason := audit.ReasonResumeRetry
	if record.Binding.Pending.Phase == string(sentryapi.OperationPlanned) {
		next := record.Binding
		next.Pending.Phase = string(sentryapi.OperationSubmitted)
		record, err = e.Bindings.UpdateBinding(ctx, record, next)
		if err != nil {
			return Result{}, err
		}
		auditReason = audit.ReasonJournalSubmitted
	}
	if record.Binding.Pending.Phase != string(sentryapi.OperationSubmitted) {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeStarted, auditReason); err != nil {
		return Result{}, err
	}
	claim, err = e.Bindings.GetClaim(ctx, record.Binding.Installation, record.Binding.Organization, record.Binding.Slug)
	if err != nil || claim.Claim.Manager.UID != string(input.Project.UID) || claim.Claim.ExternalIdentity != record.Binding.ExternalIdentity || claim.Claim.Handoff != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	remote, err = e.Adapter.GetBySlug(ctx, record.Binding.Organization, record.Binding.Slug)
	if err != nil || remote.Identity != record.Binding.ExternalIdentity || remote.Marker != nil || remote.RegionalEvidence != nil {
		return Result{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	updater, ok := e.Adapter.(RegionalOwnershipAdapter)
	if !ok {
		return Result{}, &Error{Reason: ReasonInvalidState, cause: errors.New("project adapter does not support adoption")}
	}
	if err := updater.UpdateRegionalEvidence(ctx, record.Binding.Organization, record.Binding.Slug, *evidence); err != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeFailed, audit.ReasonApply); auditErr != nil {
			return Result{}, auditErr
		}
		return Result{}, err
	}
	remote, err = e.Adapter.GetBySlug(ctx, record.Binding.Organization, record.Binding.Slug)
	if err != nil || remote.Identity != record.Binding.ExternalIdentity || remote.RegionalEvidence == nil || ownership.ValidateEvidence(ownership.Local{Marker: *regional, Active: true}, *remote.RegionalEvidence) != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeFailed, audit.ReasonVerification); auditErr != nil {
			return Result{}, auditErr
		}
		if err != nil {
			return Result{}, err
		}
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
		return Result{}, err
	}
	return e.completeAdoption(ctx, record, remote)
}

func (e *Engine) completeAdoption(ctx context.Context, record binding.BindingRecord, remote RemoteProject) (Result, error) {
	next := record.Binding
	next.Pending = nil
	updated, err := e.Bindings.UpdateBinding(ctx, record, next)
	if err != nil {
		return Result{}, err
	}
	return Result{Origin: binding.OriginManaged, AccessMode: binding.AccessManage, Owner: updated.Binding.Owner, ProjectURL: remote.URL}, nil
}

func validateManagedName(slug string, marker binding.Marker, regional *ownership.Evidence) error {
	if regional != nil {
		_, err := FormatRegionalManagedNameFromSlug(slug, *regional)
		return err
	}
	_, err := FormatManagedNameFromSlug(slug, marker)
	return err
}

func newManagedBinding(input Input, installation, slug, owner string, marker binding.Marker, regional *ownership.Marker, claimName string, reference binding.ProjectReference, create *sentryv1alpha1.ProjectCreate) binding.ProjectBinding {
	return binding.ProjectBinding{
		Version:           binding.CurrentVersion,
		Project:           reference,
		Connection:        input.ConnectionName,
		Installation:      installation,
		Organization:      input.Connection.Organization,
		Slug:              slug,
		Owner:             owner,
		Origin:            binding.OriginManaged,
		AccessMode:        binding.AccessManage,
		Marker:            &marker,
		RegionalOwnership: regional,
		ManagerUID:        string(input.Project.UID),
		ClaimName:         claimName,
		Pending: &binding.OperationState{
			Kind:        "CreateProject",
			Phase:       string(sentryapi.OperationPlanned),
			DesiredHash: createHash(input.Connection.Organization, slug, create.Team, marker, regional),
		},
	}
}

func (e *Engine) handleExistingManaged(ctx context.Context, bindingRecord binding.BindingRecord, claimRecord binding.ClaimRecord, remote RemoteProject, reference binding.ProjectReference, marker binding.Marker, regional *ownership.Marker) (Result, error) {
	if remote.Identity == "" || remote.Slug != bindingRecord.Binding.Slug {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if bindingRecord.Binding.ExternalIdentity != "" {
		if bindingRecord.Binding.ExternalIdentity != remote.Identity || !matchesRemoteOwnership(remote, marker, regional) || (claimRecord.Claim.ExternalIdentity != "" && claimRecord.Claim.ExternalIdentity != remote.Identity) {
			return Result{}, &Error{Reason: ReasonOwnershipLost}
		}
		if claimRecord.Claim.Handoff != nil {
			if err := e.Bindings.AuthorizeHandoff(claimRecord, reference, bindingRecord.Binding.Slug); err != nil {
				return Result{}, &Error{Reason: ReasonOwnershipLost}
			}
			if _, err := e.Bindings.CompleteHandoff(ctx, claimRecord, reference, bindingRecord.Binding.Slug); err != nil {
				return Result{}, err
			}
		}
		return Result{Origin: binding.OriginManaged, AccessMode: binding.AccessManage, Owner: bindingRecord.Binding.Owner, ProjectURL: remote.URL}, nil
	}

	if claimRecord.Claim.Handoff != nil {
		if err := e.Bindings.AuthorizeHandoff(claimRecord, reference, bindingRecord.Binding.Slug); err != nil || claimRecord.Claim.ExternalIdentity != remote.Identity || !matchesRemoteOwnership(remote, marker, regional) {
			return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
		}
		if _, found, err := e.Bindings.FindManaged(ctx, bindingRecord.Binding.Installation, bindingRecord.Binding.Organization, bindingRecord.Binding.Slug, remote.Identity, marker); err != nil || !found {
			return Result{}, &Error{Reason: ReasonOwnershipLost, cause: err}
		}
		result, err := e.bindVerifiedRemote(ctx, bindingRecord, claimRecord, remote, marker, regional)
		if err != nil {
			return Result{}, err
		}
		latestClaim, err := e.Bindings.GetClaim(ctx, bindingRecord.Binding.Installation, bindingRecord.Binding.Organization, bindingRecord.Binding.Slug)
		if err != nil {
			return Result{}, err
		}
		if _, err := e.Bindings.CompleteHandoff(ctx, latestClaim, reference, bindingRecord.Binding.Slug); err != nil {
			return Result{}, err
		}
		return result, nil
	}

	if bindingRecord.Binding.Pending != nil && bindingRecord.Binding.Pending.Phase == string(sentryapi.OperationSubmitted) && matchesRemoteOwnership(remote, marker, regional) && (claimRecord.Claim.ExternalIdentity == "" || claimRecord.Claim.ExternalIdentity == remote.Identity) {
		if err := e.recordAuditReference(ctx, reference, audit.ActionCreate, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
			return Result{}, err
		}
		return e.bindVerifiedRemote(ctx, bindingRecord, claimRecord, remote, marker, regional)
	}
	return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
}

func (e *Engine) bindVerifiedRemote(ctx context.Context, bindingRecord binding.BindingRecord, claimRecord binding.ClaimRecord, remote RemoteProject, marker binding.Marker, regional *ownership.Marker) (Result, error) {
	if claimRecord.Claim.ExternalIdentity == "" {
		nextClaim := claimRecord.Claim
		nextClaim.ExternalIdentity = remote.Identity
		var err error
		claimRecord, err = e.Bindings.UpdateClaim(ctx, claimRecord, nextClaim)
		if err != nil {
			return Result{}, err
		}
	} else if claimRecord.Claim.ExternalIdentity != remote.Identity {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	nextBinding := bindingRecord.Binding
	nextBinding.ExternalIdentity = remote.Identity
	nextBinding.Marker = &marker
	nextBinding.RegionalOwnership = regional
	nextBinding.Pending = nil
	updated, err := e.Bindings.UpdateBinding(ctx, bindingRecord, nextBinding)
	if err != nil {
		return Result{}, err
	}
	return Result{Origin: binding.OriginManaged, AccessMode: binding.AccessManage, Owner: updated.Binding.Owner, ProjectURL: remote.URL}, nil
}

func (e *Engine) reconcileReadOnly(ctx context.Context, input Input, installation string, decision policy.Decision) (Result, error) {
	project := input.Project
	slug := *project.Spec.Project.Use
	remote, err := e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
	if err != nil {
		if sentryapi.IsKind(err, sentryapi.ErrorNotFound) {
			if _, bindingErr := e.Bindings.GetBinding(ctx, string(project.UID)); bindingErr == nil {
				return Result{}, &Error{Reason: ReasonExternalDeleted}
			}
		}
		return Result{}, err
	}
	if remote.Identity == "" || remote.Slug != slug {
		return Result{}, &Error{Reason: ReasonInvalidState}
	}
	record, err := e.Bindings.GetBinding(ctx, string(project.UID))
	if err == nil {
		if record.Binding.Project.UID != string(project.UID) || record.Binding.Installation != installation || record.Binding.Organization != input.Connection.Organization || record.Binding.Slug != slug || record.Binding.ExternalIdentity != remote.Identity || record.Binding.AccessMode != binding.AccessReadOnly {
			return Result{}, &Error{Reason: ReasonOwnershipLost}
		}
		if record.Binding.Origin == binding.OriginManaged {
			if remote.Marker == nil || record.Binding.Marker == nil || !record.Binding.Marker.Matches(*remote.Marker) {
				return Result{}, &Error{Reason: ReasonOwnershipLost}
			}
			if _, found, findErr := e.Bindings.FindManaged(ctx, installation, input.Connection.Organization, slug, remote.Identity, *remote.Marker); findErr != nil || !found {
				return Result{}, &Error{Reason: ReasonOwnershipLost, cause: findErr}
			}
		}
		return Result{Origin: record.Binding.Origin, AccessMode: binding.AccessReadOnly, Owner: record.Binding.Owner, ProjectURL: remote.URL}, nil
	}
	if !errors.Is(err, binding.ErrNotFound) {
		return Result{}, err
	}

	state := binding.ProjectBinding{
		Version:          binding.CurrentVersion,
		Project:          projectReference(project),
		Connection:       input.ConnectionName,
		Installation:     installation,
		Organization:     input.Connection.Organization,
		Slug:             slug,
		Origin:           binding.OriginExternal,
		AccessMode:       binding.AccessReadOnly,
		ExternalIdentity: remote.Identity,
	}
	if remote.Marker != nil {
		managedRecord, found, findErr := e.Bindings.FindManaged(ctx, installation, input.Connection.Organization, slug, remote.Identity, *remote.Marker)
		if findErr != nil {
			return Result{}, findErr
		}
		if found {
			state.Origin = binding.OriginManaged
			state.Owner = managedRecord.Binding.Owner
			marker := *remote.Marker
			state.Marker = &marker
		}
	}
	if _, err := e.Bindings.CreateBinding(ctx, state); err != nil {
		return Result{}, err
	}
	return Result{Origin: state.Origin, AccessMode: binding.AccessReadOnly, Owner: state.Owner, ProjectURL: remote.URL}, nil
}

func projectReference(project *sentryv1alpha1.SentryProject) binding.ProjectReference {
	return binding.ProjectReference{Namespace: project.Namespace, Name: project.Name, UID: string(project.UID), Generation: project.Generation}
}

func (e *Engine) recordAudit(ctx context.Context, input Input, action audit.Action, outcome audit.Outcome, reason audit.Reason) error {
	return e.recordAuditReference(ctx, projectReference(input.Project), action, outcome, reason)
}

func (e *Engine) recordAuditReference(ctx context.Context, reference binding.ProjectReference, action audit.Action, outcome audit.Outcome, reason audit.Reason) error {
	event := audit.Event{
		Timestamp: time.Now().UTC().Round(0), Component: audit.ComponentProjects,
		Action: action, Outcome: outcome, Reason: reason,
		LocalResource: reference.Namespace + "/" + reference.Name, Revision: reference.Generation,
	}
	if e.Audit == nil || e.Audit.Record(ctx, event) != nil {
		return &Error{Reason: ReasonInvalidState, cause: ErrAuditUnavailable}
	}
	return nil
}

func (e *Engine) reconcileRegionalOwnership(ctx context.Context, input Input, record binding.BindingRecord, desired *ownership.Marker) (binding.BindingRecord, error) {
	pinned := record.Binding.RegionalOwnership
	if equalRegionalOwnership(pinned, desired) {
		return record, nil
	}
	if pinned == nil || desired == nil {
		return binding.BindingRecord{}, &Error{Reason: ReasonRevisionConflict, cause: ownership.ErrConflict}
	}
	local := ownership.Local{Marker: *desired, Active: true}
	if err := ownership.CanTakeOver(local, *pinned); err != nil {
		return binding.BindingRecord{}, regionalOwnershipError(err)
	}
	updater, ok := e.Adapter.(RegionalOwnershipAdapter)
	if !ok {
		return binding.BindingRecord{}, &Error{Reason: ReasonInvalidState, cause: errors.New("project adapter does not support regional ownership updates")}
	}
	store := &regionalMarkerStore{
		adapter: updater, reader: e.Adapter, organization: record.Binding.Organization,
		slug: record.Binding.Slug, externalIdentity: record.Binding.ExternalIdentity,
		previous: *pinned, desired: *desired,
	}
	observed, err := store.Read(ctx)
	if err != nil {
		return binding.BindingRecord{}, regionalOwnershipError(err)
	}
	if observed == *desired {
		if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
			return binding.BindingRecord{}, err
		}
		return e.updateRegionalBinding(ctx, record, desired)
	}
	claim, err := e.Bindings.GetClaim(ctx, record.Binding.Installation, record.Binding.Organization, record.Binding.Slug)
	if err != nil || claim.Claim.Manager.UID != string(input.Project.UID) || claim.Claim.ExternalIdentity != record.Binding.ExternalIdentity || claim.Claim.Handoff != nil {
		return binding.BindingRecord{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeStarted, audit.ReasonJournalSubmitted); err != nil {
		return binding.BindingRecord{}, err
	}
	clock := e.Clock
	if clock == nil {
		clock = contextClock{}
	}
	delay := e.FencingDelay
	if delay <= 0 {
		delay = defaultRevisionFencingDelay
	}
	if err := ownership.TakeOver(ctx, store, clock, delay, local); err != nil {
		reason := audit.ReasonApply
		if store.wrote {
			reason = audit.ReasonVerification
		}
		if auditErr := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeFailed, reason); auditErr != nil {
			return binding.BindingRecord{}, auditErr
		}
		return binding.BindingRecord{}, regionalOwnershipError(err)
	}
	if err := e.recordAudit(ctx, input, audit.ActionUpdate, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
		return binding.BindingRecord{}, err
	}
	return e.updateRegionalBinding(ctx, record, desired)
}

func (e *Engine) updateRegionalBinding(ctx context.Context, record binding.BindingRecord, desired *ownership.Marker) (binding.BindingRecord, error) {
	next := record.Binding
	marker := *desired
	next.RegionalOwnership = &marker
	updated, err := e.Bindings.UpdateBinding(ctx, record, next)
	if err != nil {
		return binding.BindingRecord{}, err
	}
	return updated, nil
}

type regionalMarkerStore struct {
	adapter          RegionalOwnershipAdapter
	reader           Adapter
	organization     string
	slug             string
	externalIdentity string
	previous         ownership.Marker
	desired          ownership.Marker
	wrote            bool
}

func (s *regionalMarkerStore) Read(ctx context.Context) (ownership.Marker, error) {
	remote, err := s.reader.GetBySlug(ctx, s.organization, s.slug)
	if err != nil {
		return ownership.Marker{}, err
	}
	if remote.Identity != s.externalIdentity || remote.RegionalEvidence == nil {
		return ownership.Marker{}, ownership.ErrConflict
	}
	if ownership.ValidateEvidence(ownership.Local{Marker: s.desired, Active: true}, *remote.RegionalEvidence) == nil {
		return s.desired, nil
	}
	if ownership.ValidateEvidence(ownership.Local{Marker: s.previous, Active: true}, *remote.RegionalEvidence) == nil {
		return s.previous, nil
	}
	return ownership.Marker{}, ownership.ErrConflict
}

func (s *regionalMarkerStore) Write(ctx context.Context, marker ownership.Marker) error {
	if marker != s.desired {
		return ownership.ErrConflict
	}
	evidence, err := ownership.EvidenceFor(marker)
	if err != nil {
		return err
	}
	if err := s.adapter.UpdateRegionalEvidence(ctx, s.organization, s.slug, evidence); err != nil {
		return err
	}
	s.wrote = true
	return nil
}

type contextClock struct{}

func (contextClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func regionalOwnershipError(err error) error {
	switch {
	case errors.Is(err, ownership.ErrFenced):
		return &Error{Reason: ReasonRevisionFenced, cause: err}
	case errors.Is(err, ownership.ErrConflict), errors.Is(err, ownership.ErrChanged):
		return &Error{Reason: ReasonRevisionConflict, cause: err}
	default:
		return err
	}
}

func matchesManagedContext(state binding.ProjectBinding, input Input, installation, slug, owner string, marker binding.Marker, regional *ownership.Marker) bool {
	return matchesStaticManagedContext(state, input, installation, slug, owner, marker) && equalRegionalOwnership(state.RegionalOwnership, regional)
}

func matchesStaticManagedContext(state binding.ProjectBinding, input Input, installation, slug, owner string, marker binding.Marker) bool {
	return state.Version == binding.CurrentVersion && state.Project.UID == string(input.Project.UID) && state.Project.Namespace == input.Project.Namespace && state.Project.Name == input.Project.Name && state.Connection == input.ConnectionName && state.Installation == installation && state.Organization == input.Connection.Organization && state.Slug == slug && state.Owner == owner && state.Origin == binding.OriginManaged && state.AccessMode == binding.AccessManage && state.ManagerUID == string(input.Project.UID) && state.Marker != nil && state.Marker.Matches(marker)
}

func desiredRegionalOwnership(connection sentryv1alpha1.SentryConnectionSpec, owner, installation string) (*ownership.Marker, *ownership.Evidence, error) {
	if connection.Ownership == nil {
		return nil, nil, nil
	}
	if connection.Ownership.Role != sentryv1alpha1.ConnectionRoleActive {
		return nil, nil, &Error{Reason: ReasonConnectionStandby, cause: ownership.ErrStandby}
	}
	marker := &ownership.Marker{
		Region: connection.Ownership.Region, Revision: connection.Ownership.Revision,
		Owner: owner, Installation: installation,
	}
	evidence, err := ownership.EvidenceFor(*marker)
	if err != nil {
		return nil, nil, &Error{Reason: ReasonInvalidState, cause: err}
	}
	return marker, &evidence, nil
}

func matchesRemoteOwnership(remote RemoteProject, marker binding.Marker, regional *ownership.Marker) bool {
	if regional == nil {
		return remote.Marker != nil && marker.Matches(*remote.Marker)
	}
	return remote.RegionalEvidence != nil && ownership.ValidateEvidence(ownership.Local{Marker: *regional, Active: true}, *remote.RegionalEvidence) == nil
}

func equalRegionalOwnership(left, right *ownership.Marker) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func createHash(organization, slug, team string, marker binding.Marker, regional ...*ownership.Marker) string {
	value := organization + "\x00" + slug + "\x00" + team + "\x00" + marker.Owner + "\x00" + marker.ManagedBy
	if len(regional) != 0 && regional[0] != nil {
		value += "\x00" + regional[0].Region + "\x00" + fmt.Sprintf("%d", regional[0].Revision) + "\x00" + regional[0].Owner + "\x00" + regional[0].Installation
	}
	return hashValue(value)
}

func hashValue(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
