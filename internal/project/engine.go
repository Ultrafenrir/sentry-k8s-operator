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

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/ownership"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/policy"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

type ErrorReason string

const (
	ReasonProjectAlreadyExists   ErrorReason = "ProjectAlreadyExists"
	ReasonOwnershipConflict      ErrorReason = "OwnershipConflict"
	ReasonOwnershipLost          ErrorReason = "OwnershipLost"
	ReasonExternalDeleted        ErrorReason = "ExternalDeleted"
	ReasonCreationOutcomeUnknown ErrorReason = "CreationOutcomeUnknown"
	ReasonDeletionPending        ErrorReason = "DeletionPending"
	ReasonInvalidState           ErrorReason = "InvalidState"
)

const (
	CapabilityAtomicDefaultRules = "project.create.atomic-default-rules"
	CapabilityAutomaticActiveKey = "project.create.automatic-active-key"
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
		if e.Audit == nil {
			return Result{}, &Error{Reason: ReasonInvalidState, cause: ErrAuditUnavailable}
		}
		return e.reconcileManaged(ctx, input, installation, decision)
	}
	return e.reconcileReadOnly(ctx, input, installation, decision)
}

// Delete applies Retain or guarded DeleteProject semantics. A caller removes the finalizer
// only when Complete is true.
func (e *Engine) Delete(ctx context.Context, input Input) (DeletionResult, error) {
	if e.Bindings == nil || e.Adapter == nil || input.Project == nil || input.Project.UID == "" {
		return DeletionResult{}, &Error{Reason: ReasonInvalidState}
	}
	if input.Project.Spec.Lifecycle.OnDelete != sentryv1alpha1.OnDeleteDeleteProject {
		return DeletionResult{Complete: true, Retained: true}, nil
	}
	if input.Project.Spec.Project.Create == nil {
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
	decision, err := policy.EvaluateProject(input.Project.Namespace, input.Project.Spec, input.Connection)
	if err != nil || decision.AccessMode != binding.AccessManage {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	state, err := e.Bindings.GetBinding(ctx, string(input.Project.UID))
	if err != nil {
		return DeletionResult{}, &Error{Reason: ReasonOwnershipLost, cause: err}
	}
	slug := input.Project.Name
	marker := binding.Marker{Origin: "managed", Owner: decision.Owner, ManagedBy: e.ManagedBy}
	if !matchesManagedContext(state.Binding, input, installation, slug, decision.Owner, marker) || state.Binding.ExternalIdentity == "" {
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
	if remote.Identity != state.Binding.ExternalIdentity || remote.Marker == nil || !marker.Matches(*remote.Marker) {
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

func (e *Engine) completeDeletion(ctx context.Context, state binding.BindingRecord, claim binding.ClaimRecord) (DeletionResult, error) {
	if err := e.Bindings.DeleteClaim(ctx, claim); err != nil && !errors.Is(err, binding.ErrNotFound) {
		return DeletionResult{}, err
	}
	if err := e.Bindings.DeleteBinding(ctx, state); err != nil && !errors.Is(err, binding.ErrNotFound) {
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
			state := newManagedBinding(input, installation, slug, decision.Owner, marker, claimRecord.Name, reference, create)
			bindingRecord, err = e.Bindings.CreateBinding(ctx, state)
			if err != nil {
				return Result{}, err
			}
			return e.handleExistingManaged(ctx, bindingRecord, claimRecord, preflight, reference, marker)
		}
		if !sentryapi.IsKind(preflightErr, sentryapi.ErrorNotFound) {
			return Result{}, preflightErr
		}
		if err := validateManagedName(slug, marker); err != nil {
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
		state := newManagedBinding(input, installation, slug, decision.Owner, marker, claimRecord.Name, reference, create)
		bindingRecord, err = e.Bindings.CreateBinding(ctx, state)
	}
	if err != nil {
		return Result{}, err
	}
	claimRecord, _, err := e.Bindings.AcquireClaim(ctx, claim)
	if errors.Is(err, binding.ErrOwnershipConflict) {
		return Result{}, &Error{Reason: ReasonOwnershipConflict}
	}
	if err != nil {
		return Result{}, err
	}
	if claimRecord.Claim.Owner != decision.Owner {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if !matchesManagedContext(bindingRecord.Binding, input, installation, slug, decision.Owner, marker) {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}

	remote, getErr := e.Adapter.GetBySlug(ctx, input.Connection.Organization, slug)
	if getErr == nil {
		return e.handleExistingManaged(ctx, bindingRecord, claimRecord, remote, reference, marker)
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
	if err := validateManagedName(slug, marker); err != nil {
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
		Organization: input.Connection.Organization,
		Slug:         slug,
		Team:         create.Team,
		Platform:     create.Platform,
		DefaultRules: false,
		Marker:       marker,
	})
	if err != nil {
		if auditErr := e.recordAudit(ctx, input, audit.ActionCreate, audit.OutcomeFailed, audit.ReasonApply); auditErr != nil {
			return Result{}, auditErr
		}
		return Result{}, &Error{Reason: ReasonCreationOutcomeUnknown, cause: err}
	}
	if remote.Identity == "" || remote.Slug != slug || remote.Marker == nil || !marker.Matches(*remote.Marker) {
		if auditErr := e.recordAudit(ctx, input, audit.ActionCreate, audit.OutcomeFailed, audit.ReasonVerification); auditErr != nil {
			return Result{}, auditErr
		}
		return Result{}, &Error{Reason: ReasonCreationOutcomeUnknown}
	}
	if err := e.recordAudit(ctx, input, audit.ActionCreate, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
		return Result{}, err
	}
	return e.bindVerifiedRemote(ctx, bindingRecord, claimRecord, remote, marker)
}

func validateManagedName(slug string, marker binding.Marker) error {
	_, err := FormatManagedNameFromSlug(slug, marker)
	return err
}

func newManagedBinding(input Input, installation, slug, owner string, marker binding.Marker, claimName string, reference binding.ProjectReference, create *sentryv1alpha1.ProjectCreate) binding.ProjectBinding {
	return binding.ProjectBinding{
		Version:      binding.CurrentVersion,
		Project:      reference,
		Connection:   input.ConnectionName,
		Installation: installation,
		Organization: input.Connection.Organization,
		Slug:         slug,
		Owner:        owner,
		Origin:       binding.OriginManaged,
		AccessMode:   binding.AccessManage,
		Marker:       &marker,
		ManagerUID:   string(input.Project.UID),
		ClaimName:    claimName,
		Pending: &binding.OperationState{
			Kind:        "CreateProject",
			Phase:       string(sentryapi.OperationPlanned),
			DesiredHash: createHash(input.Connection.Organization, slug, create.Team, marker),
		},
	}
}

func (e *Engine) handleExistingManaged(ctx context.Context, bindingRecord binding.BindingRecord, claimRecord binding.ClaimRecord, remote RemoteProject, reference binding.ProjectReference, marker binding.Marker) (Result, error) {
	if remote.Identity == "" || remote.Slug != bindingRecord.Binding.Slug {
		return Result{}, &Error{Reason: ReasonOwnershipLost}
	}
	if bindingRecord.Binding.ExternalIdentity != "" {
		if bindingRecord.Binding.ExternalIdentity != remote.Identity || remote.Marker == nil || !marker.Matches(*remote.Marker) || (claimRecord.Claim.ExternalIdentity != "" && claimRecord.Claim.ExternalIdentity != remote.Identity) {
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
		if err := e.Bindings.AuthorizeHandoff(claimRecord, reference, bindingRecord.Binding.Slug); err != nil || claimRecord.Claim.ExternalIdentity != remote.Identity || remote.Marker == nil || !marker.Matches(*remote.Marker) {
			return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
		}
		if _, found, err := e.Bindings.FindManaged(ctx, bindingRecord.Binding.Installation, bindingRecord.Binding.Organization, bindingRecord.Binding.Slug, remote.Identity, marker); err != nil || !found {
			return Result{}, &Error{Reason: ReasonOwnershipLost, cause: err}
		}
		result, err := e.bindVerifiedRemote(ctx, bindingRecord, claimRecord, remote, marker)
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

	if bindingRecord.Binding.Pending != nil && bindingRecord.Binding.Pending.Phase == string(sentryapi.OperationSubmitted) && remote.Marker != nil && marker.Matches(*remote.Marker) && (claimRecord.Claim.ExternalIdentity == "" || claimRecord.Claim.ExternalIdentity == remote.Identity) {
		if err := e.recordAuditReference(ctx, reference, audit.ActionCreate, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
			return Result{}, err
		}
		return e.bindVerifiedRemote(ctx, bindingRecord, claimRecord, remote, marker)
	}
	return Result{}, &Error{Reason: ReasonProjectAlreadyExists}
}

func (e *Engine) bindVerifiedRemote(ctx context.Context, bindingRecord binding.BindingRecord, claimRecord binding.ClaimRecord, remote RemoteProject, marker binding.Marker) (Result, error) {
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

func matchesManagedContext(state binding.ProjectBinding, input Input, installation, slug, owner string, marker binding.Marker) bool {
	return state.Version == binding.CurrentVersion && state.Project.UID == string(input.Project.UID) && state.Project.Namespace == input.Project.Namespace && state.Project.Name == input.Project.Name && state.Connection == input.ConnectionName && state.Installation == installation && state.Organization == input.Connection.Organization && state.Slug == slug && state.Owner == owner && state.Origin == binding.OriginManaged && state.AccessMode == binding.AccessManage && state.ManagerUID == string(input.Project.UID) && state.Marker != nil && state.Marker.Matches(marker)
}

func createHash(organization, slug, team string, marker binding.Marker) string {
	return hashValue(organization + "\x00" + slug + "\x00" + team + "\x00" + marker.Owner + "\x00" + marker.ManagedBy)
}

func hashValue(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
