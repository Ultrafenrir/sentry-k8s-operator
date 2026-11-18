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

package alerts

import (
	"context"
	"errors"
	"time"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

var (
	ErrJournalConflict    = errors.New("alert operation journal conflicts with pending intent")
	ErrMutationUnverified = errors.New("alert mutation result could not be verified")
	ErrAuditUnavailable   = errors.New("alert mutation audit is unavailable")
)

type mutation struct {
	Key           string
	Kind          string
	LocalResource string
	Hash          string
	Audit         audit.Sink
	Action        audit.Action
	Authorize     Authorization
	Apply         func(context.Context) error
	Verify        func(context.Context) (bool, error)
}

// runMutation makes an idempotent exact-identity mutation crash safe. Submitted is written
// before Apply. A resumed Submitted operation verifies first, then re-applies only the same
// hash when verification is still false. Callers must provide idempotent Apply semantics.
func runMutation(ctx context.Context, journal sentryapi.OperationJournal, value mutation) error {
	if journal == nil || value.Key == "" || value.Kind == "" || value.LocalResource == "" || value.Hash == "" || value.Audit == nil || value.Action == "" || value.Authorize == nil || value.Apply == nil || value.Verify == nil {
		return ErrInvalidInput
	}
	current, found, err := journal.Load(ctx, value.Key)
	if err != nil {
		return err
	}
	if found && (current.Key != value.Key || current.Kind != value.Kind || current.LocalResource != value.LocalResource) {
		return ErrJournalConflict
	}
	if found && current.Phase == sentryapi.OperationSubmitted {
		if current.IdempotencyHash != value.Hash {
			// A user may revert desired state after an operation was submitted but before
			// it was authorized or verified. Close the stale intent only when exact
			// read-after-write verification proves that the newer desired state already
			// exists. Otherwise retain the fail-closed conflict.
			verified, verifyErr := value.Verify(ctx)
			if verifyErr != nil {
				return verifyErr
			}
			if !verified {
				return ErrJournalConflict
			}
			replacement := current
			replacement.IdempotencyHash = value.Hash
			replacement.Phase = sentryapi.OperationVerified
			replacement.Revision++
			if err := recordAudit(ctx, value, replacement, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
				return err
			}
			return journal.CompareAndSwap(ctx, current.Revision, replacement)
		}
		verified, verifyErr := value.Verify(ctx)
		if verifyErr != nil {
			return auditFailure(ctx, value, current, audit.ReasonVerification, verifyErr)
		}
		if verified {
			if err := recordAudit(ctx, value, current, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
				return err
			}
			return finishMutation(ctx, journal, current)
		}
		return submitMutation(ctx, journal, current, value, false)
	}
	if found && current.Phase == sentryapi.OperationVerified && current.IdempotencyHash == value.Hash {
		verified, verifyErr := value.Verify(ctx)
		if verifyErr != nil {
			return verifyErr
		}
		if verified {
			return nil
		}
	}

	previousRevision := int64(0)
	if found {
		previousRevision = current.Revision
	}
	planned := sentryapi.Operation{
		Key: value.Key, Kind: value.Kind, LocalResource: value.LocalResource,
		IdempotencyHash: value.Hash, Phase: sentryapi.OperationPlanned, Revision: previousRevision + 1,
	}
	if err := journal.CompareAndSwap(ctx, previousRevision, planned); err != nil {
		return err
	}
	return submitMutation(ctx, journal, planned, value, true)
}

func submitMutation(ctx context.Context, journal sentryapi.OperationJournal, current sentryapi.Operation, value mutation, advance bool) error {
	submitted := current
	if advance {
		submitted.Phase = sentryapi.OperationSubmitted
		submitted.Revision++
		if err := journal.CompareAndSwap(ctx, current.Revision, submitted); err != nil {
			return err
		}
	}
	reason := audit.ReasonResumeRetry
	if advance {
		reason = audit.ReasonJournalSubmitted
	}
	if err := recordAudit(ctx, value, submitted, audit.OutcomeStarted, reason); err != nil {
		return err
	}
	if err := value.Authorize(ctx); err != nil {
		return auditFailure(ctx, value, submitted, audit.ReasonAuthorization, err)
	}
	if err := value.Apply(ctx); err != nil {
		return auditFailure(ctx, value, submitted, audit.ReasonApply, err)
	}
	verified, err := value.Verify(ctx)
	if err != nil {
		return auditFailure(ctx, value, submitted, audit.ReasonVerification, err)
	}
	if !verified {
		return auditFailure(ctx, value, submitted, audit.ReasonVerification, ErrMutationUnverified)
	}
	if err := recordAudit(ctx, value, submitted, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
		return err
	}
	return finishMutation(ctx, journal, submitted)
}

func recordAudit(ctx context.Context, value mutation, operation sentryapi.Operation, outcome audit.Outcome, reason audit.Reason) error {
	event := audit.Event{
		Timestamp: time.Now().UTC().Round(0), Component: audit.ComponentAlerts,
		Action: value.Action, Outcome: outcome, Reason: reason,
		LocalResource: value.LocalResource, Revision: operation.Revision,
	}
	if err := value.Audit.Record(ctx, event); err != nil {
		return ErrAuditUnavailable
	}
	return nil
}

func auditFailure(ctx context.Context, value mutation, operation sentryapi.Operation, reason audit.Reason, operationErr error) error {
	if err := recordAudit(ctx, value, operation, audit.OutcomeFailed, reason); err != nil {
		return err
	}
	return operationErr
}

func finishMutation(ctx context.Context, journal sentryapi.OperationJournal, submitted sentryapi.Operation) error {
	verified := submitted
	verified.Phase = sentryapi.OperationVerified
	verified.Revision++
	return journal.CompareAndSwap(ctx, submitted.Revision, verified)
}

func pendingMutation(ctx context.Context, journal sentryapi.OperationJournal, key, hash string) (bool, error) {
	if journal == nil {
		return false, ErrInvalidInput
	}
	operation, found, err := journal.Load(ctx, key)
	if err != nil || !found {
		return false, err
	}
	if operation.Phase != sentryapi.OperationSubmitted {
		return false, nil
	}
	if operation.IdempotencyHash != hash {
		// Let runMutation perform exact verification before deciding whether the stale
		// submitted intent may be superseded. This helper only answers whether the
		// current desired hash itself is pending.
		return false, nil
	}
	return true, nil
}
