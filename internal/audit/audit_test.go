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

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
)

func TestEventHasClosedIdentityFreeShape(t *testing.T) {
	event := Event{
		Timestamp: time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC),
		Component: ComponentAlerts, Action: ActionUpdate, Outcome: OutcomeSucceeded,
		Reason: ReasonMutationVerified, LocalResource: "checkout/rule", Revision: 3,
	}
	if err := Validate(event); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"externalIdentity", "desiredHash", "payload", "error"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("audit shape contains forbidden field %q: %s", forbidden, payload)
		}
	}
}

func TestLogSinkAcceptsOnlyValidatedEventsAndSuppliesTimestamp(t *testing.T) {
	sink := &LogSink{
		Logger: funcr.New(func(string, string) {}, funcr.Options{}),
		Now:    func() time.Time { return time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC) },
	}
	event := Event{
		Component: ComponentAlerts, Action: ActionDelete, Outcome: OutcomeStarted,
		Reason: ReasonJournalSubmitted, LocalResource: "checkout/rule", Revision: 2,
	}
	if err := sink.Record(context.Background(), event); err != nil {
		t.Fatalf("valid event rejected: %v", err)
	}
	event.LocalResource = "checkout/rule\nraw-response"
	if err := sink.Record(context.Background(), event); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unsafe event accepted: %v", err)
	}
	if err := (*LogSink)(nil).Record(context.Background(), Event{}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("nil sink accepted: %v", err)
	}
}

func TestEventRejectsOpenEnumsAndUnsafeLocalIdentity(t *testing.T) {
	base := Event{
		Timestamp: time.Now(), Component: ComponentAlerts, Action: ActionEnsure,
		Outcome: OutcomeStarted, Reason: ReasonJournalSubmitted, LocalResource: "checkout/rule", Revision: 2,
	}
	tests := []Event{base, base, base, base, base}
	tests[0].Component = Component("native-component")
	tests[1].Action = Action("raw-api-action")
	tests[2].Reason = Reason("upstream error body")
	tests[3].LocalResource = "checkout/rule\nexternal-id-SHOULD-NOT-LEAK"
	tests[4].Action = ActionDeliver
	for _, event := range tests {
		if err := Validate(event); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("unsafe event accepted: %#v error=%v", event, err)
		}
	}
}

func TestRegistrationAcceptsOnlyRegisterAction(t *testing.T) {
	t.Parallel()
	event := Event{
		Timestamp: time.Now(), Component: ComponentRegistration, Action: ActionRegister,
		Outcome: OutcomeStarted, Reason: ReasonRegistrationAuthorized,
		LocalResource: "payments/checkout/telegram", Revision: 7,
	}
	if err := Validate(event); err != nil {
		t.Fatal(err)
	}
	event.Action = ActionDeliver
	if err := Validate(event); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("registration delivery action accepted: %v", err)
	}
}

func TestProjectsAcceptAccessUpdateAction(t *testing.T) {
	t.Parallel()
	event := Event{
		Timestamp: time.Now(), Component: ComponentProjects, Action: ActionUpdate,
		Outcome: OutcomeSucceeded, Reason: ReasonMutationVerified,
		LocalResource: "payments/checkout", Revision: 3,
	}
	if err := Validate(event); err != nil {
		t.Fatal(err)
	}
}

func TestDeliveryAcceptsRetentionDelete(t *testing.T) {
	t.Parallel()
	event := Event{
		Timestamp: time.Now(), Component: ComponentDelivery, Action: ActionDelete,
		Outcome: OutcomeStarted, Reason: ReasonRetentionExpired,
		LocalResource: "delivery/queue-retention", Revision: 3,
	}
	if err := Validate(event); err != nil {
		t.Fatal(err)
	}
}

func TestDestinationsAcceptOnlyCleanupDelete(t *testing.T) {
	t.Parallel()
	event := Event{
		Timestamp: time.Now(), Component: ComponentDestinations, Action: ActionDelete,
		Outcome: OutcomeStarted, Reason: ReasonCleanupAuthorized,
		LocalResource: "payments/checkout/telegram", Revision: 7,
	}
	if err := Validate(event); err != nil {
		t.Fatal(err)
	}
	for _, action := range []Action{ActionEnsure, ActionUpdate, ActionRegister, ActionDeliver} {
		event.Action = action
		if err := Validate(event); !errors.Is(err, ErrInvalidEvent) {
			t.Fatalf("destination action %q accepted: %v", action, err)
		}
	}
}
