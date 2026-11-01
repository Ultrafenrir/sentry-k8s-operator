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

// Package audit emits a deliberately small, identity-free mutation trail. Events contain
// local Kubernetes identity and fixed enums only; external IDs, hashes, payloads and raw
// errors are not representable.
package audit

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

type Component string
type Action string
type Outcome string
type Reason string

const (
	ComponentAlerts       Component = "alerts"
	ComponentDelivery     Component = "delivery"
	ComponentDestinations Component = "destinations"
	ComponentProjects     Component = "projects"
	ComponentRegistration Component = "registration"

	ActionCreate   Action = "create"
	ActionEnsure   Action = "ensure"
	ActionUpdate   Action = "update"
	ActionToggle   Action = "toggle"
	ActionDelete   Action = "delete"
	ActionDeliver  Action = "deliver"
	ActionRegister Action = "register"

	OutcomeStarted   Outcome = "started"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeRecovered Outcome = "recovered"
	OutcomeFailed    Outcome = "failed"

	ReasonJournalSubmitted       Reason = "JournalSubmitted"
	ReasonResumeRetry            Reason = "ResumeRetry"
	ReasonResultRecovered        Reason = "ResultRecovered"
	ReasonMutationVerified       Reason = "MutationVerified"
	ReasonAuthorization          Reason = "AuthorizationDenied"
	ReasonApply                  Reason = "ApplyFailed"
	ReasonVerification           Reason = "VerificationFailed"
	ReasonDeliveryClaimed        Reason = "DeliveryClaimed"
	ReasonDeliveryAccepted       Reason = "DeliveryAccepted"
	ReasonDeliveryFailed         Reason = "DeliveryFailed"
	ReasonRetentionExpired       Reason = "RetentionExpired"
	ReasonRouteUnavailable       Reason = "RouteUnavailable"
	ReasonRegistrationAuthorized Reason = "RegistrationAuthorized"
	ReasonCleanupAuthorized      Reason = "CleanupAuthorized"
)

var ErrInvalidEvent = errors.New("audit event is invalid")

type Event struct {
	Timestamp     time.Time `json:"timestamp"`
	Component     Component `json:"component"`
	Action        Action    `json:"action"`
	Outcome       Outcome   `json:"outcome"`
	Reason        Reason    `json:"reason"`
	LocalResource string    `json:"localResource"`
	Revision      int64     `json:"revision"`
}

type Sink interface {
	Record(context.Context, Event) error
}

type LogSink struct {
	Logger logr.Logger
	Now    func() time.Time
}

func (s *LogSink) Record(_ context.Context, event Event) error {
	if event.Timestamp.IsZero() {
		if s != nil && s.Now != nil {
			event.Timestamp = s.Now().UTC().Round(0)
		} else {
			event.Timestamp = time.Now().UTC().Round(0)
		}
	}
	if err := Validate(event); err != nil || s == nil || s.Logger.GetSink() == nil {
		return ErrInvalidEvent
	}
	logEvent(s.Logger, event)
	return nil
}

func logEvent(logger logr.Logger, event Event) {
	logger.Info("operator mutation audit",
		"timestamp", event.Timestamp.Format(time.RFC3339Nano),
		"component", string(event.Component),
		"action", string(event.Action),
		"outcome", string(event.Outcome),
		"reason", string(event.Reason),
		"localResource", event.LocalResource,
		"revision", event.Revision,
	)
}

func Validate(event Event) error {
	if event.Timestamp.IsZero() || event.Revision <= 0 || event.LocalResource == "" || len(event.LocalResource) > 512 || strings.ContainsAny(event.LocalResource, "\r\n\x00") || !validComponent(event.Component) || !validAction(event.Action) || !validComponentAction(event.Component, event.Action) || !validOutcome(event.Outcome) || !validReason(event.Reason) {
		return ErrInvalidEvent
	}
	return nil
}

func validComponentAction(component Component, action Action) bool {
	switch component {
	case ComponentAlerts:
		return action == ActionEnsure || action == ActionUpdate || action == ActionToggle || action == ActionDelete
	case ComponentDelivery:
		return action == ActionDeliver || action == ActionDelete
	case ComponentDestinations:
		return action == ActionDelete
	case ComponentProjects:
		return action == ActionCreate || action == ActionUpdate || action == ActionDelete
	case ComponentRegistration:
		return action == ActionRegister
	default:
		return false
	}
}

func validComponent(value Component) bool {
	return value == ComponentAlerts || value == ComponentDelivery || value == ComponentDestinations || value == ComponentProjects || value == ComponentRegistration
}

func validAction(value Action) bool {
	switch value {
	case ActionCreate, ActionEnsure, ActionUpdate, ActionToggle, ActionDelete, ActionDeliver, ActionRegister:
		return true
	default:
		return false
	}
}

func validOutcome(value Outcome) bool {
	switch value {
	case OutcomeStarted, OutcomeSucceeded, OutcomeRecovered, OutcomeFailed:
		return true
	default:
		return false
	}
}

func validReason(value Reason) bool {
	switch value {
	case ReasonJournalSubmitted, ReasonResumeRetry, ReasonResultRecovered, ReasonMutationVerified, ReasonAuthorization, ReasonApply, ReasonVerification,
		ReasonDeliveryClaimed, ReasonDeliveryAccepted, ReasonDeliveryFailed, ReasonRetentionExpired, ReasonRouteUnavailable, ReasonRegistrationAuthorized, ReasonCleanupAuthorized:
		return true
	default:
		return false
	}
}
