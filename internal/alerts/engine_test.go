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
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

var testMarker = binding.Marker{Origin: "managed", Owner: "team-a", ManagedBy: "operator-a"}

func TestAbsentAlertsDoesNotManageExistingChildren(t *testing.T) {
	current := map[string]binding.AlertBinding{"existing": {ExternalIdentity: "alert-1"}}
	result := (&Engine{}).Reconcile(context.Background(), Input{Manage: false, Current: current, DestinationCleanup: []string{"chat"}})
	if !reflect.DeepEqual(result.Bindings, current) || len(result.Statuses) != 0 || len(result.ReleasableDestinations) != 0 {
		t.Fatalf("absent alerts block must not manage existing state: %#v", result)
	}
}

func TestManagedOwnershipMetadataIsRequired(t *testing.T) {
	adapter := newFakeAdapter()
	input := baseInput(adapter, func(context.Context) error { return nil })
	input.AllowedOwnerUIDs = map[string]bool{"project-uid": true}
	input.Desired = map[string]Desired{"rule": {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}}

	result := newTestEngine(adapter).Reconcile(context.Background(), input)
	if !errors.Is(result.Statuses["rule"].Err, ErrInvalidInput) || len(adapter.operations) != 0 {
		t.Fatalf("missing protected owner did not fail closed: result=%#v operations=%v", result, adapter.operations)
	}
}

func TestLifecyclePinsAdapterOrdersDeleteBeforeCreateAndTombstones(t *testing.T) {
	adapter := newFakeAdapter()
	engine := newTestEngine(adapter)
	authorizations := 0
	input := baseInput(adapter, func(context.Context) error { authorizations++; return nil })
	input.Desired = map[string]Desired{
		"old-alert":  {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}},
		"regression": {Trigger: TriggerRegression, Enabled: true, Notify: []string{"chat"}},
	}
	input.Capabilities = []Capability{
		{Name: "modern-z", Triggers: allTriggers(), SupportsUpdate: true, SupportsToggle: true},
		{Name: "legacy-a", Triggers: allTriggers(), SupportsUpdate: true, SupportsToggle: true},
	}

	created := engine.Reconcile(context.Background(), input)
	if err := created.Err(); err != nil {
		t.Fatalf("create lifecycle failed: %v", err)
	}
	if created.PinnedAdapter != "legacy-a" || len(created.Bindings) != 2 || authorizations != 2 {
		t.Fatalf("unexpected create result: %#v authorizations=%d", created, authorizations)
	}

	adapter.operations = nil
	authorizations = 0
	input.Current = created.Bindings
	input.PinnedAdapter = created.PinnedAdapter
	input.Desired = map[string]Desired{
		"new-alert":  {Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 10, Enabled: true, Notify: []string{"chat"}},
		"regression": {Trigger: TriggerRegression, Enabled: false, Notify: []string{"chat"}},
	}
	changed := engine.Reconcile(context.Background(), input)
	if err := changed.Err(); err != nil {
		t.Fatalf("update lifecycle failed: %v", err)
	}
	wantOperations := []string{"delete:old-alert", "ensure:new-alert", "toggle:regression:false"}
	if !reflect.DeepEqual(adapter.operations, wantOperations) {
		t.Fatalf("deletion must precede create/toggle: got %v want %v", adapter.operations, wantOperations)
	}
	if !changed.Tombstones["old-alert"] || changed.Bindings["regression"].Enabled || authorizations != 3 {
		t.Fatalf("unexpected changed result: %#v authorizations=%d", changed, authorizations)
	}

	adapter.operations = nil
	input.Current = changed.Bindings
	input.Tombstones = changed.Tombstones
	input.Desired = map[string]Desired{}
	input.DestinationCleanup = []string{"chat", "chat"}
	empty := engine.Reconcile(context.Background(), input)
	if err := empty.Err(); err != nil {
		t.Fatalf("empty managed map must delete managed children: %v", err)
	}
	if len(empty.Bindings) != 0 || !empty.Tombstones["new-alert"] || !empty.Tombstones["regression"] {
		t.Fatalf("managed empty map did not tombstone all protected children: %#v", empty)
	}
	if _, manualExists := adapter.objects["manual-unbound-alert"]; !manualExists {
		t.Fatal("unbound manual alert was modified")
	}
	if !reflect.DeepEqual(empty.ReleasableDestinations, []string{"chat"}) {
		t.Fatalf("destination cleanup was not released after all bound alerts were deleted: %v", empty.ReleasableDestinations)
	}
}

func TestFailedAlertDeleteBlocksDestinationCleanup(t *testing.T) {
	adapter := newFakeAdapter()
	current := boundAlert("rule", "legacy-a", Desired{Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}, testMarker)
	adapter.objects[current.ExternalIdentity] = observedFromBinding(current)
	input := baseInput(adapter, func(context.Context) error { return errors.New("policy changed") })
	input.Current = map[string]binding.AlertBinding{"rule": current}
	input.Desired = map[string]Desired{}
	input.DestinationCleanup = []string{"chat"}

	result := newTestEngine(adapter).Reconcile(context.Background(), input)
	if result.Statuses["rule"].Err == nil || len(result.ReleasableDestinations) != 0 {
		t.Fatalf("failed alert deletion released destination cleanup: %#v", result)
	}
}

func TestInvalidDestinationIsIsolatedFromOtherAlert(t *testing.T) {
	adapter := newFakeAdapter()
	input := baseInput(adapter, func(context.Context) error { return nil })
	input.Desired = map[string]Desired{
		"good":        {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}},
		"bad":         {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"revoked"}},
		"unsupported": {Trigger: Trigger("native-query"), Enabled: true, Notify: []string{"chat"}},
	}
	input.Destinations["revoked"] = binding.DestinationBinding{State: "Revoked"}

	result := newTestEngine(adapter).Reconcile(context.Background(), input)
	if result.Statuses["good"].State != StateReady || !errors.Is(result.Statuses["bad"].Err, ErrInvalidInput) || !errors.Is(result.Statuses["unsupported"].Err, ErrInvalidInput) {
		t.Fatalf("per-alert isolation failed: %#v", result.Statuses)
	}
	if len(result.Bindings) != 1 || result.Bindings["good"].ExternalIdentity == "" {
		t.Fatalf("valid alert was not reconciled: %#v", result.Bindings)
	}
}

func TestPinnedAdapterAndOwnershipFailClosed(t *testing.T) {
	adapter := newFakeAdapter()
	input := baseInput(adapter, func(context.Context) error { return nil })
	input.PinnedAdapter = "missing-adapter"
	input.Desired = map[string]Desired{"rule": {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}}
	result := newTestEngine(adapter).Reconcile(context.Background(), input)
	if !errors.Is(result.Statuses["rule"].Err, ErrAdapterUnavailable) || len(adapter.operations) != 0 {
		t.Fatalf("missing pin must fail without mutation: %#v operations=%v", result, adapter.operations)
	}

	adapter = newFakeAdapter()
	input = baseInput(adapter, func(context.Context) error { return nil })
	desired := Desired{Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}
	foreign := testMarker
	foreign.Owner = "foreign"
	current := binding.AlertBinding{
		Adapter: "legacy-a", ExternalIdentity: "bound", Trigger: string(TriggerFirstSeen),
		DesiredHash: desiredHash(normalize(desired)), Enabled: true, Destinations: []string{"chat"}, Marker: foreign,
	}
	input.Current = map[string]binding.AlertBinding{"rule": current}
	input.Desired = map[string]Desired{"rule": desired}
	result = newTestEngine(adapter).Reconcile(context.Background(), input)
	if !errors.Is(result.Statuses["rule"].Err, ErrOwnershipLost) || len(adapter.operations) != 0 {
		t.Fatalf("foreign binding must fail closed: %#v operations=%v", result, adapter.operations)
	}
}

func TestCapabilityWithoutToggleAcceptsOnlyEnabledDesiredState(t *testing.T) {
	t.Parallel()
	adapter := newFakeAdapter()
	input := baseInput(adapter, func(context.Context) error { return nil })
	input.Capabilities = []Capability{{Name: "legacy", Triggers: allTriggers(), SupportsUpdate: true, SupportsToggle: false}}
	input.Desired = map[string]Desired{"rule": {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}}
	result := newTestEngine(adapter).Reconcile(context.Background(), input)
	if result.Err() != nil || result.PinnedAdapter != "legacy" || result.Statuses["rule"].State != StateReady {
		t.Fatalf("enabled legacy result=%#v", result)
	}
	input.Current = nil
	input.PinnedAdapter = ""
	input.Desired["rule"] = Desired{Trigger: TriggerFirstSeen, Enabled: false, Notify: []string{"chat"}}
	result = newTestEngine(adapter).Reconcile(context.Background(), input)
	if !errors.Is(result.Statuses["rule"].Err, ErrAdapterUnavailable) || len(result.Bindings) != 0 {
		t.Fatalf("disabled legacy result=%#v", result)
	}
}

func TestUnsupportedUpdateDoesNotRecreate(t *testing.T) {
	adapter := newFakeAdapter()
	input := baseInput(adapter, func(context.Context) error { return nil })
	input.Capabilities[0].SupportsUpdate = false
	oldDesired := Desired{Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 10, Enabled: true, Notify: []string{"chat"}}
	current := boundAlert("rule", "legacy-a", oldDesired, testMarker)
	adapter.objects[current.ExternalIdentity] = observedFromBinding(current)
	input.Current = map[string]binding.AlertBinding{"rule": current}
	input.PinnedAdapter = "legacy-a"
	input.Desired = map[string]Desired{"rule": {Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 20, Enabled: true, Notify: []string{"chat"}}}

	result := newTestEngine(adapter).Reconcile(context.Background(), input)
	if !errors.Is(result.Statuses["rule"].Err, ErrUnsupportedUpdate) {
		t.Fatalf("unsupported update was not reported: %#v", result.Statuses["rule"])
	}
	if len(adapter.operations) != 0 || !reflect.DeepEqual(result.Bindings["rule"], current) {
		t.Fatalf("unsupported update must preserve binding without recreate: %#v operations=%v", result.Bindings, adapter.operations)
	}
}

func TestUpdateAndToggleReauthorizeEveryMutation(t *testing.T) {
	adapter := newFakeAdapter()
	authorizations := 0
	input := baseInput(adapter, func(context.Context) error { authorizations++; return nil })
	oldDesired := Desired{Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 10, Enabled: true, Notify: []string{"chat"}}
	current := boundAlert("rule", "legacy-a", oldDesired, testMarker)
	adapter.objects[current.ExternalIdentity] = observedFromBinding(current)
	input.Current = map[string]binding.AlertBinding{"rule": current}
	input.PinnedAdapter = "legacy-a"
	input.Desired = map[string]Desired{"rule": {Trigger: TriggerEventCount, WindowMinutes: 5, Threshold: 20, Enabled: false, Notify: []string{"chat"}}}

	result := newTestEngine(adapter).Reconcile(context.Background(), input)
	if err := result.Err(); err != nil {
		t.Fatalf("update/toggle failed: %v", err)
	}
	if authorizations != 2 || !reflect.DeepEqual(adapter.operations, []string{"update:rule", "toggle:rule:false"}) {
		t.Fatalf("each mutation must reauthorize: count=%d operations=%v", authorizations, adapter.operations)
	}
}

func TestCanonicalHashIgnoresNotifyOrderAndRejectsDuplicates(t *testing.T) {
	left := normalize(Desired{Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"pager", "chat"}})
	right := normalize(Desired{Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat", "pager"}})
	if desiredHash(left) != desiredHash(right) {
		t.Fatal("canonical hash changed with destination order")
	}
	destinations := map[string]binding.DestinationBinding{"chat": {State: "Active"}}
	if !errors.Is(validateDesired("rule", Desired{Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat", "chat"}}, destinations, false), ErrInvalidInput) {
		t.Fatal("duplicate destination reference was accepted")
	}
	if err := validateDesired("affected-users", Desired{Trigger: TriggerUserCount, WindowMinutes: 10, Threshold: 50, Enabled: true, Notify: []string{"chat"}}, destinations, false); err != nil {
		t.Fatalf("valid user-count desired state rejected: %v", err)
	}
	if !errors.Is(validateDesired("affected-users", Desired{Trigger: TriggerUserCount, Enabled: true, Notify: []string{"chat"}}, destinations, false), ErrInvalidInput) {
		t.Fatal("user-count without metric bounds was accepted")
	}
}

func TestSubmittedEnsureRecoversLostResponseWithoutSecondChild(t *testing.T) {
	adapter := newFakeAdapter()
	adapter.loseEnsureResponseOnce = true
	journal := newFakeJournal()
	auditSink := &fakeAudit{}
	engine := &Engine{Adapter: adapter, Journal: journal, Audit: auditSink}
	authorizations := 0
	input := baseInput(adapter, func(context.Context) error { authorizations++; return nil })
	input.Desired = map[string]Desired{"rule": {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}}

	first := engine.Reconcile(context.Background(), input)
	if first.Statuses["rule"].Err == nil || len(first.Bindings) != 0 {
		t.Fatalf("lost response must leave submitted intent without binding: %#v", first)
	}
	operation := journal.operations[operationKey("ensure", input, "rule")]
	if operation.Phase != sentryapi.OperationSubmitted {
		t.Fatalf("lost response phase = %s, want Submitted", operation.Phase)
	}

	second := engine.Reconcile(context.Background(), input)
	if err := second.Err(); err != nil || second.Bindings["rule"].ExternalIdentity != "id-rule" {
		t.Fatalf("submitted ensure did not recover: result=%#v err=%v", second, err)
	}
	operation = journal.operations[operationKey("ensure", input, "rule")]
	if operation.Phase != sentryapi.OperationVerified || authorizations != 2 {
		t.Fatalf("recovered operation = %#v authorizations=%d", operation, authorizations)
	}
	if _, exists := adapter.objects["id-rule"]; !exists || len(adapter.objects) != 2 {
		t.Fatalf("ensure created a duplicate child: %#v", adapter.objects)
	}
	if got := auditSink.outcomes(); !reflect.DeepEqual(got, []string{
		"started/JournalSubmitted", "failed/ApplyFailed",
		"started/ResumeRetry", "succeeded/MutationVerified",
	}) {
		t.Fatalf("unexpected recovery audit sequence: %v", got)
	}
	for _, event := range auditSink.events {
		payload, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		for _, forbidden := range []string{"id-rule", operation.IdempotencyHash, "lost response"} {
			if forbidden != "" && strings.Contains(string(payload), forbidden) {
				t.Fatalf("audit event leaked protected data %q: %s", forbidden, payload)
			}
		}
	}
}

func TestSubmittedVerifiedResultRecordsRecoveryWithoutReapply(t *testing.T) {
	journal := newFakeJournal()
	journal.operations["op"] = sentryapi.Operation{
		Key: "op", Kind: "UpdateAlert", LocalResource: "checkout/rule",
		IdempotencyHash: "same", Phase: sentryapi.OperationSubmitted, Revision: 2,
	}
	auditSink := &fakeAudit{}
	authorized := false
	applied := false
	err := runMutation(context.Background(), journal, mutation{
		Key: "op", Kind: "UpdateAlert", LocalResource: "checkout/rule", Hash: "same",
		Audit: auditSink, Action: audit.ActionUpdate,
		Authorize: func(context.Context) error { authorized = true; return nil },
		Apply:     func(context.Context) error { applied = true; return nil },
		Verify:    func(context.Context) (bool, error) { return true, nil },
	})
	if err != nil || authorized || applied {
		t.Fatalf("verified result was re-applied: err=%v authorized=%v applied=%v", err, authorized, applied)
	}
	if operation := journal.operations["op"]; operation.Phase != sentryapi.OperationVerified || operation.Revision != 3 {
		t.Fatalf("recovered operation was not closed: %#v", operation)
	}
	if got := auditSink.outcomes(); !reflect.DeepEqual(got, []string{"recovered/ResultRecovered"}) {
		t.Fatalf("unexpected recovered audit event: %v", got)
	}
}

func TestAuditFailureBeforeApplyBlocksExternalMutation(t *testing.T) {
	adapter := newFakeAdapter()
	journal := newFakeJournal()
	engine := &Engine{Adapter: adapter, Journal: journal, Audit: &fakeAudit{failAt: 1}}
	input := baseInput(adapter, func(context.Context) error { return nil })
	input.Desired = map[string]Desired{"rule": {Trigger: TriggerFirstSeen, Enabled: true, Notify: []string{"chat"}}}

	result := engine.Reconcile(context.Background(), input)
	if !errors.Is(result.Statuses["rule"].Err, ErrAuditUnavailable) || len(adapter.operations) != 0 {
		t.Fatalf("audit outage did not block mutation: result=%#v operations=%v", result, adapter.operations)
	}
	operation := journal.operations[operationKey("ensure", input, "rule")]
	if operation.Phase != sentryapi.OperationSubmitted || operation.Revision != 2 {
		t.Fatalf("audit outage did not preserve submitted recovery intent: %#v", operation)
	}
}

func TestSubmittedDifferentIntentFailsClosed(t *testing.T) {
	journal := newFakeJournal()
	journal.operations["op"] = sentryapi.Operation{
		Key: "op", Kind: "UpdateAlert", LocalResource: "project/rule",
		IdempotencyHash: "old", Phase: sentryapi.OperationSubmitted, Revision: 2,
	}
	applied := false
	err := runMutation(context.Background(), journal, mutation{
		Key: "op", Kind: "UpdateAlert", LocalResource: "project/rule", Hash: "new",
		Audit: &fakeAudit{}, Action: audit.ActionUpdate,
		Authorize: func(context.Context) error { return nil },
		Apply:     func(context.Context) error { applied = true; return nil },
		Verify:    func(context.Context) (bool, error) { return false, nil },
	})
	if !errors.Is(err, ErrJournalConflict) || applied {
		t.Fatalf("different intent did not fail closed: err=%v applied=%v", err, applied)
	}
}

func baseInput(adapter *fakeAdapter, authorize Authorization) Input {
	return Input{
		Organization: "example", ProjectSlug: "checkout", Manage: true,
		Destinations: map[string]binding.DestinationBinding{"chat": {State: "Active"}},
		Capabilities: []Capability{{Name: "legacy-a", Triggers: allTriggers(), SupportsUpdate: true, SupportsToggle: true}},
		Marker:       testMarker, Authorize: authorize,
	}
}

func newTestEngine(adapter Adapter) *Engine {
	return &Engine{Adapter: adapter, Journal: newFakeJournal(), Audit: &fakeAudit{}}
}

func allTriggers() map[Trigger]bool {
	return map[Trigger]bool{TriggerFirstSeen: true, TriggerRegression: true, TriggerEventCount: true, TriggerUserCount: true}
}

func boundAlert(key, adapter string, desired Desired, marker binding.Marker) binding.AlertBinding {
	desired = normalize(desired)
	return binding.AlertBinding{
		Adapter: adapter, ExternalIdentity: "id-" + key, Trigger: string(desired.Trigger),
		DesiredHash: desiredHash(desired), Enabled: desired.Enabled,
		Destinations: append([]string(nil), desired.Notify...), Marker: marker,
	}
}

func observedFromBinding(value binding.AlertBinding) Observed {
	return Observed{Identity: value.ExternalIdentity, Adapter: value.Adapter, Marker: value.Marker, DesiredHash: value.DesiredHash, Enabled: value.Enabled}
}

type fakeAdapter struct {
	objects                map[string]Observed
	operations             []string
	loseEnsureResponseOnce bool
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{objects: map[string]Observed{
		"manual-unbound-alert": {Identity: "manual-unbound-alert", Adapter: "manual", Marker: binding.Marker{Origin: "foreign"}},
	}}
}

func (f *fakeAdapter) Ensure(_ context.Context, adapter, _, _, key string, desired Desired, marker binding.Marker) (Observed, error) {
	f.operations = append(f.operations, "ensure:"+key)
	identity := "id-" + key
	if existing, ok := f.objects[identity]; ok {
		return existing, nil
	}
	observed := Observed{Identity: identity, Adapter: adapter, Marker: marker, DesiredHash: desiredHash(normalize(desired)), Enabled: desired.Enabled}
	f.objects[identity] = observed
	if f.loseEnsureResponseOnce {
		f.loseEnsureResponseOnce = false
		return Observed{}, errors.New("lost response")
	}
	return observed, nil
}

func (f *fakeAdapter) Observe(_ context.Context, _, _, _, identity string) (Observed, bool, error) {
	observed, found := f.objects[identity]
	return observed, found, nil
}

func (f *fakeAdapter) Update(_ context.Context, _, _, _, identity string, desired Desired) (Observed, error) {
	key := identity[len("id-"):]
	f.operations = append(f.operations, "update:"+key)
	observed := f.objects[identity]
	observed.DesiredHash = desiredHash(normalize(desired))
	f.objects[identity] = observed
	return observed, nil
}

func (f *fakeAdapter) SetEnabled(_ context.Context, _, _, _, identity string, enabled bool) (Observed, error) {
	key := identity[len("id-"):]
	value := "false"
	if enabled {
		value = "true"
	}
	f.operations = append(f.operations, "toggle:"+key+":"+value)
	observed := f.objects[identity]
	observed.Enabled = enabled
	f.objects[identity] = observed
	return observed, nil
}

func (f *fakeAdapter) Delete(_ context.Context, _, _, _, identity string, _ binding.Marker) error {
	key := identity[len("id-"):]
	f.operations = append(f.operations, "delete:"+key)
	delete(f.objects, identity)
	return nil
}

type fakeJournal struct {
	operations map[string]sentryapi.Operation
}

type fakeAudit struct {
	events []audit.Event
	failAt int
}

func (f *fakeAudit) Record(_ context.Context, event audit.Event) error {
	if f.failAt > 0 && len(f.events)+1 == f.failAt {
		return errors.New("audit unavailable")
	}
	if err := audit.Validate(event); err != nil {
		return err
	}
	f.events = append(f.events, event)
	return nil
}

func (f *fakeAudit) outcomes() []string {
	result := make([]string, 0, len(f.events))
	for _, event := range f.events {
		result = append(result, string(event.Outcome)+"/"+string(event.Reason))
	}
	return result
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{operations: make(map[string]sentryapi.Operation)}
}

func (f *fakeJournal) Load(_ context.Context, key string) (sentryapi.Operation, bool, error) {
	operation, found := f.operations[key]
	return operation, found, nil
}

func (f *fakeJournal) CompareAndSwap(_ context.Context, previousRevision int64, next sentryapi.Operation) error {
	current, found := f.operations[next.Key]
	if previousRevision == 0 {
		if found || next.Revision != 1 {
			return errors.New("journal create conflict")
		}
	} else if !found || current.Revision != previousRevision || next.Revision != previousRevision+1 {
		return errors.New("journal update conflict")
	}
	f.operations[next.Key] = next
	return nil
}
