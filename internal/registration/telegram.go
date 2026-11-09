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

// Package registration implements protected one-time Telegram registration state. The
// Kubernetes authentication/SAR and mTLS transport layers call this state machine but do
// not weaken its exact object/policy binding.
package registration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const (
	PhasePendingPublish      = "PendingPublish"
	PhasePendingFenceRelease = "PendingFenceRelease"
)

var (
	ErrRegistrationRequired = errors.New("telegram destination registration is not available")
	ErrChallengeInvalid     = errors.New("telegram registration challenge is invalid or expired")
	ErrLiveVerification     = errors.New("telegram registration live verification failed")
	ErrPublishPending       = errors.New("telegram routing publish remains pending")
	ErrAuditUnavailable     = errors.New("telegram registration audit is unavailable")
)

type IssueInput struct {
	Project               binding.ProjectReference
	Destination           string
	Owner                 string
	Connection            string
	PolicyGeneration      int64
	PolicyResourceVersion string
	TTL                   time.Duration
}

// Verify performs any transport-specific verification required before a redemption.
// Kubernetes live reads and ordered status fences belong to FenceManager.
type Verify func(context.Context, binding.TelegramChallenge) error

// FenceManager serializes a registration against live project and connection policy.
// Implementations must acquire Connection before Project and clear in reverse order.
type FenceManager interface {
	Begin(context.Context, binding.TelegramChallenge, string) error
	Clear(context.Context, binding.TelegramChallenge, string) error
}

type Publisher interface {
	PublishTelegramRoute(context.Context, string, binding.TelegramTarget) error
}

type RouteManager interface {
	Publisher
	RevokeTelegramRoute(context.Context, string, binding.TelegramTarget) error
}

type RouteObserver interface {
	GetTelegramRoute(context.Context, string) (binding.TelegramTarget, error)
}

type Service struct {
	Store           *binding.Store
	Index           *ChallengeIndexStore
	Publisher       Publisher
	Fences          FenceManager
	Audit           audit.Sink
	Now             func() time.Time
	Random          func([]byte) (int, error)
	OperationRandom func([]byte) (int, error)
}

func (s *Service) Issue(ctx context.Context, input IssueInput) (string, error) {
	if s.Store == nil || input.Project.UID == "" || input.Project.Generation <= 0 || input.Destination == "" || input.Owner == "" || input.Connection == "" || input.PolicyGeneration <= 0 || input.PolicyResourceVersion == "" || input.TTL <= 0 {
		return "", ErrRegistrationRequired
	}
	record, err := s.Store.GetBinding(ctx, input.Project.UID)
	if err != nil {
		return "", err
	}
	destination, found := record.Binding.Destinations[input.Destination]
	if !found || destination.Kind != "telegram" || destination.State != "RegistrationRequired" || destination.SelectorHash == "" || record.Binding.Project != input.Project || record.Binding.Owner != input.Owner || record.Binding.Connection != input.Connection {
		return "", ErrRegistrationRequired
	}
	if destination.Telegram != nil && (destination.Telegram.Target != nil || destination.Telegram.Phase != "" || destination.Telegram.OutboxKey != "" || destination.Telegram.FenceHash != "" || destination.Telegram.RouteKey != "") {
		return "", ErrRegistrationRequired
	}
	var previousIndex *ChallengeIndex
	if s.Index != nil && destination.Telegram != nil && destination.Telegram.Challenge != nil {
		old := destination.Telegram.Challenge
		previousIndex = &ChallengeIndex{
			Version: binding.CurrentVersion, CodeHash: old.CodeHash, ProjectUID: old.Project.UID,
			Destination: old.Destination, ExpiresAt: old.ExpiresAt,
		}
	}
	random := s.Random
	if random == nil {
		random = rand.Read
	}
	material := make([]byte, 32)
	if n, err := random(material); err != nil || n != len(material) {
		return "", ErrRegistrationRequired
	}
	code := base64.RawURLEncoding.EncodeToString(material)
	now := s.now()
	challenge := binding.TelegramChallenge{
		CodeHash: hash(code), Project: input.Project, Destination: input.Destination,
		SelectorHash: destination.SelectorHash, Owner: input.Owner, Connection: input.Connection,
		PolicyGeneration: input.PolicyGeneration, PolicyResourceVersion: input.PolicyResourceVersion,
		ExpiresAt: now.Add(input.TTL),
	}
	index := ChallengeIndex{
		Version: binding.CurrentVersion, CodeHash: challenge.CodeHash, ProjectUID: input.Project.UID,
		Destination: input.Destination, ExpiresAt: challenge.ExpiresAt,
	}
	if s.Index != nil {
		if err := s.Index.Create(ctx, index); err != nil {
			return "", err
		}
	}
	destination.Telegram = &binding.TelegramBinding{Challenge: &challenge}
	next := record.Binding
	next.Destinations = cloneDestinations(record.Binding.Destinations)
	next.Destinations[input.Destination] = destination
	if _, err := s.Store.UpdateBinding(ctx, record, next); err != nil {
		if s.Index != nil {
			return "", errors.Join(err, s.Index.DeleteExact(ctx, index))
		}
		return "", err
	}
	if previousIndex != nil && previousIndex.CodeHash != index.CodeHash {
		// The replaced code can no longer match the binding. Exact best-effort
		// cleanup prevents routine re-issue from accumulating stale indexes.
		_ = s.Index.DeleteExact(ctx, *previousIndex)
	}
	return code, nil
}

// RedeemCode resolves only hash(code) from protected state. This is the sole
// redemption entry point needed by the public receiver; project identity and
// destination never cross the public-to-internal boundary.
func (s *Service) RedeemCode(ctx context.Context, code string, target binding.TelegramTarget) error {
	decoded, err := base64.RawURLEncoding.DecodeString(code)
	if s.Index == nil || err != nil || len(decoded) != 32 || !validTelegramTarget(target) {
		return ErrChallengeInvalid
	}
	index, err := s.Index.Get(ctx, hash(code))
	if err != nil || s.now().After(index.ExpiresAt) {
		if err == nil {
			_ = s.Index.DeleteExact(ctx, index)
		}
		return ErrChallengeInvalid
	}

	// A receiver retry after a durable pending response resumes the same target
	// instead of starting a second fenced operation.
	record, getErr := s.Store.GetBinding(ctx, index.ProjectUID)
	if getErr == nil {
		destination, found := record.Binding.Destinations[index.Destination]
		if found && destination.Telegram != nil && destination.Telegram.Target != nil &&
			(destination.Telegram.Phase == PhasePendingPublish || destination.Telegram.Phase == PhasePendingFenceRelease) {
			if *destination.Telegram.Target != target {
				return ErrChallengeInvalid
			}
			err = s.Resume(ctx, index.ProjectUID, index.Destination)
			if err == nil {
				err = s.Index.DeleteExact(ctx, index)
			}
			return err
		}
		if found && destination.State == "Active" && destination.Telegram != nil && destination.Telegram.Target != nil &&
			destination.Telegram.Challenge == nil && *destination.Telegram.Target == target {
			// Publication already committed but the final exact index delete failed.
			// A retry may complete that cleanup and nothing else.
			return s.Index.DeleteExact(ctx, index)
		}
	}

	err = s.Redeem(ctx, index.ProjectUID, index.Destination, code, target, nil)
	if err == nil {
		err = s.Index.DeleteExact(ctx, index)
	}
	return err
}

func (s *Service) Redeem(ctx context.Context, projectUID, destinationName, code string, target binding.TelegramTarget, verify Verify) error {
	if s.Store == nil || s.Publisher == nil || (verify == nil && s.Fences == nil) || projectUID == "" || destinationName == "" || code == "" || target.ChatIdentity == "" {
		return ErrChallengeInvalid
	}
	record, err := s.Store.GetBinding(ctx, projectUID)
	if err != nil {
		return err
	}
	destination, challenge, err := challengeFrom(record.Binding, destinationName)
	if err != nil || s.now().After(challenge.ExpiresAt) || !sameHash(challenge.CodeHash, hash(code)) {
		return ErrChallengeInvalid
	}
	if verify != nil {
		if err := verify(ctx, challenge); err != nil {
			return ErrLiveVerification
		}
	}
	if err := s.recordAudit(ctx, challenge, audit.OutcomeStarted, audit.ReasonRegistrationAuthorized); err != nil {
		return err
	}
	outboxKey := hash(challenge.CodeHash + "\x00" + target.ChatIdentity + "\x00" + target.TopicIdentity)
	fenceHash := ""
	if s.Fences != nil {
		fenceHash, err = s.newOperationHash(outboxKey)
		if err != nil {
			return errors.Join(ErrLiveVerification, s.recordAudit(ctx, challenge, audit.OutcomeFailed, audit.ReasonVerification))
		}
		if err := s.Fences.Begin(ctx, challenge, fenceHash); err != nil {
			return errors.Join(ErrLiveVerification, s.recordAudit(ctx, challenge, audit.OutcomeFailed, audit.ReasonVerification))
		}
	}
	destination.Telegram = cloneTelegram(destination.Telegram)
	destination.Telegram.Target = &target
	destination.Telegram.Phase = PhasePendingPublish
	destination.Telegram.OutboxKey = outboxKey
	destination.Telegram.FenceHash = fenceHash
	next := record.Binding
	next.Destinations = cloneDestinations(record.Binding.Destinations)
	next.Destinations[destinationName] = destination
	if _, err := s.Store.UpdateBinding(ctx, record, next); err != nil {
		if s.Fences != nil {
			return errors.Join(err, s.Fences.Clear(ctx, challenge, fenceHash), s.recordAudit(ctx, challenge, audit.OutcomeFailed, audit.ReasonApply))
		}
		return errors.Join(err, s.recordAudit(ctx, challenge, audit.OutcomeFailed, audit.ReasonApply))
	}
	return s.resume(ctx, projectUID, destinationName, false)
}

func (s *Service) Resume(ctx context.Context, projectUID, destinationName string) error {
	return s.resume(ctx, projectUID, destinationName, true)
}

func (s *Service) resume(ctx context.Context, projectUID, destinationName string, recovered bool) error {
	if s.Store == nil || s.Publisher == nil {
		return ErrPublishPending
	}
	record, err := s.Store.GetBinding(ctx, projectUID)
	if err != nil {
		return err
	}
	destination, found := record.Binding.Destinations[destinationName]
	if !found || destination.Telegram == nil || destination.Telegram.Target == nil || destination.Telegram.Challenge == nil || destination.Telegram.OutboxKey == "" || (destination.Telegram.Phase != PhasePendingPublish && destination.Telegram.Phase != PhasePendingFenceRelease) {
		return ErrRegistrationRequired
	}
	challenge := *destination.Telegram.Challenge
	if recovered {
		if err := s.recordAudit(ctx, challenge, audit.OutcomeStarted, audit.ReasonResumeRetry); err != nil {
			return err
		}
	}
	if destination.Telegram.Phase == PhasePendingPublish {
		if err := s.Publisher.PublishTelegramRoute(ctx, destination.Telegram.OutboxKey, *destination.Telegram.Target); err != nil {
			return errors.Join(ErrPublishPending, s.recordAudit(ctx, challenge, audit.OutcomeFailed, audit.ReasonApply))
		}
		destination.Telegram = cloneTelegram(destination.Telegram)
		destination.Telegram.Phase = PhasePendingFenceRelease
		next := record.Binding
		next.Destinations = cloneDestinations(record.Binding.Destinations)
		next.Destinations[destinationName] = destination
		if _, err := s.Store.UpdateBinding(ctx, record, next); err != nil {
			return errors.Join(ErrPublishPending, s.recordAudit(ctx, challenge, audit.OutcomeFailed, audit.ReasonApply))
		}
		record, err = s.Store.GetBinding(ctx, projectUID)
		if err != nil {
			return err
		}
		destination = record.Binding.Destinations[destinationName]
	}
	if destination.Telegram.FenceHash != "" {
		if s.Fences == nil || s.Fences.Clear(ctx, *destination.Telegram.Challenge, destination.Telegram.FenceHash) != nil {
			return errors.Join(ErrPublishPending, s.recordAudit(ctx, challenge, audit.OutcomeFailed, audit.ReasonVerification))
		}
	}
	outcome := audit.OutcomeSucceeded
	reason := audit.ReasonMutationVerified
	if recovered {
		outcome = audit.OutcomeRecovered
		reason = audit.ReasonResultRecovered
	}
	if err := s.recordAudit(ctx, challenge, outcome, reason); err != nil {
		return err
	}
	destination.Telegram = cloneTelegram(destination.Telegram)
	destination.State = "Active"
	destination.Reason = "TelegramReady"
	destination.Telegram.RouteKey = destination.Telegram.OutboxKey
	destination.Telegram.RouteRevoked = false
	destination.Telegram.Challenge = nil
	destination.Telegram.Phase = ""
	destination.Telegram.OutboxKey = ""
	destination.Telegram.FenceHash = ""
	next := record.Binding
	next.Destinations = cloneDestinations(record.Binding.Destinations)
	next.Destinations[destinationName] = destination
	_, err = s.Store.UpdateBinding(ctx, record, next)
	return err
}

func (s *Service) recordAudit(ctx context.Context, challenge binding.TelegramChallenge, outcome audit.Outcome, reason audit.Reason) error {
	if s == nil || s.Audit == nil {
		return ErrAuditUnavailable
	}
	event := audit.Event{
		Timestamp: s.now(), Component: audit.ComponentRegistration, Action: audit.ActionRegister,
		Outcome: outcome, Reason: reason,
		LocalResource: challenge.Project.Namespace + "/" + challenge.Project.Name + "/" + challenge.Destination,
		Revision:      challenge.Project.Generation,
	}
	if err := s.Audit.Record(ctx, event); err != nil {
		return ErrAuditUnavailable
	}
	return nil
}

func (s *Service) newOperationHash(outboxKey string) (string, error) {
	random := s.OperationRandom
	if random == nil {
		random = rand.Read
	}
	material := make([]byte, 32)
	if n, err := random(material); err != nil || n != len(material) {
		return "", ErrLiveVerification
	}
	return hash(outboxKey + "\x00" + base64.RawURLEncoding.EncodeToString(material)), nil
}

func challengeFrom(project binding.ProjectBinding, destinationName string) (binding.DestinationBinding, binding.TelegramChallenge, error) {
	destination, found := project.Destinations[destinationName]
	if !found || destination.Kind != "telegram" || destination.State != "RegistrationRequired" || destination.Telegram == nil || destination.Telegram.Challenge == nil || destination.Telegram.Target != nil || destination.Telegram.Phase != "" || destination.Telegram.OutboxKey != "" || destination.Telegram.FenceHash != "" || destination.Telegram.RouteKey != "" || destination.Telegram.RouteRevoked || destination.Telegram.Challenge.Project.UID != project.Project.UID || destination.Telegram.Challenge.Destination != destinationName || destination.Telegram.Challenge.SelectorHash != destination.SelectorHash || destination.Telegram.Challenge.Owner != project.Owner || destination.Telegram.Challenge.Connection != project.Connection {
		return binding.DestinationBinding{}, binding.TelegramChallenge{}, ErrChallengeInvalid
	}
	return destination, *destination.Telegram.Challenge, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func hash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func sameHash(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func cloneDestinations(input map[string]binding.DestinationBinding) map[string]binding.DestinationBinding {
	result := make(map[string]binding.DestinationBinding, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func cloneTelegram(input *binding.TelegramBinding) *binding.TelegramBinding {
	if input == nil {
		return nil
	}
	result := *input
	if input.Challenge != nil {
		challenge := *input.Challenge
		result.Challenge = &challenge
	}
	if input.Target != nil {
		target := *input.Target
		result.Target = &target
	}
	return &result
}
