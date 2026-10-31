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

package project

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/ownership"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

func TestManagedCreateIsIdempotentAndAtomic(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	result, err := engine.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if result.Origin != binding.OriginManaged || result.AccessMode != binding.AccessManage || result.Owner != "payments" {
		t.Fatalf("result = %#v", result)
	}
	if adapter.createCalls != 1 || adapter.lastCreate.DefaultRules {
		t.Fatalf("createCalls=%d defaultRules=%v", adapter.createCalls, adapter.lastCreate.DefaultRules)
	}
	if adapter.lastCreate.Marker != (binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-op"}) {
		t.Fatalf("atomic marker = %#v", adapter.lastCreate.Marker)
	}
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if adapter.createCalls != 1 {
		t.Fatalf("repeat reconcile created %d projects", adapter.createCalls)
	}
}

func TestManagedCreatePublishesAndPinsRegionalRevisionEvidence(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}

	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatalf("regional reconcile: %v", err)
	}
	if adapter.lastCreate.RegionalEvidence == nil || adapter.projects["checkout"].RegionalEvidence == nil || adapter.projects["checkout"].Marker != nil {
		t.Fatalf("regional create=%#v remote=%#v", adapter.lastCreate, adapter.projects["checkout"])
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || record.Binding.RegionalOwnership == nil || record.Binding.RegionalOwnership.Region != "eu-west-1" || record.Binding.RegionalOwnership.Revision != 8 {
		t.Fatalf("regional binding=%#v err=%v", record.Binding, err)
	}
	if _, err := engine.Reconcile(context.Background(), input); err != nil || adapter.createCalls != 1 {
		t.Fatalf("regional no-op err=%v createCalls=%d", err, adapter.createCalls)
	}
}

func TestManagedCreateRejectsStandbyConnectionBeforeExternalMutation(t *testing.T) {
	t.Parallel()
	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleStandby, Revision: 8,
	}

	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonConnectionStandby) {
		t.Fatalf("standby error=%v", err)
	}
	if adapter.createCalls != 0 {
		t.Fatalf("standby made %d create calls", adapter.createCalls)
	}
}

func TestManagedCreateRejectsStandbyProjectBeforeExternalLookup(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	input.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby

	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonProjectStandby) {
		t.Fatalf("standby project error=%v", err)
	}
	if adapter.getCalls != 0 || adapter.createCalls != 0 {
		t.Fatalf("standby project made get=%d create=%d calls", adapter.getCalls, adapter.createCalls)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("standby project left binding: %v", err)
	}
}

func TestRegionalRevisionTakeoverUsesDelayReadbackAndPinsNewRevision(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	clock := &fakeOwnershipClock{}
	engine.Clock = clock
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Connection.Ownership.Revision = 9
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if adapter.updateEvidenceCalls != 1 || clock.calls != 1 {
		t.Fatalf("updates=%d sleeps=%d, want one each", adapter.updateEvidenceCalls, clock.calls)
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || record.Binding.RegionalOwnership == nil || record.Binding.RegionalOwnership.Revision != 9 {
		t.Fatalf("binding=%#v err=%v", record.Binding, err)
	}
	remote := adapter.projects["checkout"]
	if remote.RegionalEvidence == nil || ownership.ValidateEvidence(ownership.Local{Marker: *record.Binding.RegionalOwnership, Active: true}, *remote.RegionalEvidence) != nil {
		t.Fatalf("remote evidence=%#v", remote.RegionalEvidence)
	}
}

func TestRegionalRevisionTakeoverRecoversCompletedRemoteWrite(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Connection.Ownership.Revision = 9
	desired := ownership.Marker{Region: "eu-west-1", Revision: 9, Owner: "payments", Installation: "company-sentry-op"}
	evidence, err := ownership.EvidenceFor(desired)
	if err != nil {
		t.Fatal(err)
	}
	remote := adapter.projects["checkout"]
	remote.RegionalEvidence = &evidence
	adapter.projects["checkout"] = remote
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatalf("recover takeover: %v", err)
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || record.Binding.RegionalOwnership == nil || record.Binding.RegionalOwnership.Revision != 9 || adapter.updateEvidenceCalls != 0 {
		t.Fatalf("binding=%#v updates=%d err=%v", record.Binding, adapter.updateEvidenceCalls, err)
	}
}

func TestRegionalRevisionTakeoverRejectsLowerRevision(t *testing.T) {
	t.Parallel()
	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Connection.Ownership.Revision = 7
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonRevisionFenced) {
		t.Fatalf("lower revision error=%v", err)
	}
	if adapter.updateEvidenceCalls != 0 {
		t.Fatalf("lower revision made %d updates", adapter.updateEvidenceCalls)
	}
}

func TestRegionalRevisionTakeoverRejectsForeignPinnedEvidence(t *testing.T) {
	t.Parallel()
	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	foreign := ownership.Marker{Region: "eu-west-1", Revision: 8, Owner: "other", Installation: "other-operator"}
	evidence, err := ownership.EvidenceFor(foreign)
	if err != nil {
		t.Fatal(err)
	}
	remote := adapter.projects["checkout"]
	remote.RegionalEvidence = &evidence
	adapter.projects["checkout"] = remote
	input.Connection.Ownership.Revision = 9
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonRevisionConflict) {
		t.Fatalf("foreign evidence error=%v", err)
	}
	if adapter.updateEvidenceCalls != 0 {
		t.Fatalf("foreign evidence made %d updates", adapter.updateEvidenceCalls)
	}
}

func TestManagedCreateCapabilityGateRequiresBothContracts(t *testing.T) {
	t.Parallel()

	if ManagedCreateSupported(nil) || ManagedCreateSupported([]string{CapabilityAtomicDefaultRules}) || ManagedCreateSupported([]string{CapabilityAutomaticActiveKey}) {
		t.Fatal("partial or missing create capabilities were accepted")
	}
	if !ManagedCreateSupported([]string{CapabilityAutomaticActiveKey, CapabilityAtomicDefaultRules}) {
		t.Fatal("complete create capability set was rejected")
	}
}

func TestManagedCreateRejectsOversizedMarkerBeforeDurableIntent(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	engine.ManagedBy = strings.Repeat("m", 40)

	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonInvalidState) {
		t.Fatalf("oversized marked name error = %v", err)
	}
	if adapter.createCalls != 0 {
		t.Fatalf("oversized marked name made %d create calls", adapter.createCalls)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("oversized marked name left binding: %v", err)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("oversized marked name left claim: %v", err)
	}
}

func TestExistingSlugIsNotAdoptedAndDoesNotCreateClaim(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	adapter.projects["checkout"] = RemoteProject{Identity: "foreign-id", Slug: "checkout", URL: "https://sentry.example/projects/checkout"}
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonProjectAlreadyExists) {
		t.Fatalf("existing slug error = %v", err)
	}
	if adapter.createCalls != 0 {
		t.Fatal("existing slug triggered create")
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); err != binding.ErrNotFound {
		t.Fatalf("existing external project left manager claim: %v", err)
	}
}

func TestCompetingWritersAcrossAliasesGetOneClaimAndOnePOST(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	first := managedInput("checkout", "uid-one", "payments")
	first.ConnectionName = "alias-one"
	first.CanonicalInstallation = "https://sentry.example"
	second := managedInput("checkout", "uid-two", "payments-two")
	second.ConnectionName = "alias-two"
	second.Connection.URL = "https://alias.internal.example"
	second.CanonicalInstallation = "https://sentry.example"
	if _, err := engine.Reconcile(context.Background(), first); err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if _, err := engine.Reconcile(context.Background(), second); !IsReason(err, ReasonOwnershipConflict) {
		t.Fatalf("second writer error = %v", err)
	}
	if adapter.createCalls != 1 {
		t.Fatalf("competing writers made %d POSTs", adapter.createCalls)
	}
}

func TestLostPOSTResponseIsRecoveredByMarkerWithoutSecondPOST(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	adapter.loseCreateResponse = true
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonCreationOutcomeUnknown) {
		t.Fatalf("lost response error = %v", err)
	}
	adapter.loseCreateResponse = false
	result, err := engine.Reconcile(context.Background(), input)
	if err != nil || result.Origin != binding.OriginManaged {
		t.Fatalf("recovery result = %#v, %v", result, err)
	}
	if adapter.createCalls != 1 {
		t.Fatalf("lost response recovery made %d POSTs", adapter.createCalls)
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || record.Binding.ExternalIdentity == "" || record.Binding.Pending != nil {
		t.Fatalf("recovered binding = %#v, %v", record, err)
	}
	auditSink := engine.Audit.(*fakeAudit)
	if got := auditSink.outcomes(); !equalStrings(got, []string{"started/JournalSubmitted", "failed/ApplyFailed", "recovered/ResultRecovered"}) {
		t.Fatalf("unexpected create recovery audit: %v", got)
	}
	payload, marshalErr := json.Marshal(auditSink.events)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-op"}
	for _, forbidden := range []string{"project-checkout", createHash("company", "checkout", "payments", marker), "create-project"} {
		if forbidden != "" && strings.Contains(string(payload), forbidden) {
			t.Fatalf("project audit leaked protected value %q: %s", forbidden, payload)
		}
	}
}

func TestAuditFailureBeforeCreateBlocksPOSTAndKeepsSubmittedIntent(t *testing.T) {
	engine, adapter, store := testEngine(t)
	engine.Audit = &fakeAudit{failAt: 1}
	input := managedInput("checkout", "uid-one", "payments")

	_, err := engine.Reconcile(context.Background(), input)
	if !errors.Is(err, ErrAuditUnavailable) || adapter.createCalls != 0 {
		t.Fatalf("audit outage did not block create: err=%v calls=%d", err, adapter.createCalls)
	}
	record, getErr := store.GetBinding(context.Background(), "uid-one")
	if getErr != nil || record.Binding.Pending == nil || record.Binding.Pending.Phase != string(sentryapi.OperationSubmitted) {
		t.Fatalf("audit outage did not preserve Submitted intent: record=%#v err=%v", record, getErr)
	}
}

func TestAuditFailureAfterCreateResponseRecoversWithoutSecondPOST(t *testing.T) {
	engine, adapter, _ := testEngine(t)
	auditSink := &fakeAudit{failAt: 2}
	engine.Audit = auditSink
	input := managedInput("checkout", "uid-one", "payments")

	if _, err := engine.Reconcile(context.Background(), input); !errors.Is(err, ErrAuditUnavailable) || adapter.createCalls != 1 {
		t.Fatalf("post-create audit outage = %v calls=%d", err, adapter.createCalls)
	}
	auditSink.failAt = 0
	if _, err := engine.Reconcile(context.Background(), input); err != nil || adapter.createCalls != 1 {
		t.Fatalf("marker recovery after audit outage = %v calls=%d", err, adapter.createCalls)
	}
	if got := auditSink.outcomes(); !equalStrings(got, []string{"started/JournalSubmitted", "recovered/ResultRecovered"}) {
		t.Fatalf("unexpected post-create recovery audit: %v", got)
	}
}

func TestSubmittedCreateWithoutRecoverableObjectDoesNotRetry(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	adapter.dropCreate = true
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonCreationOutcomeUnknown) {
		t.Fatalf("first unknown outcome = %v", err)
	}
	adapter.dropCreate = false
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonCreationOutcomeUnknown) {
		t.Fatalf("second unknown outcome = %v", err)
	}
	if adapter.createCalls != 1 {
		t.Fatalf("unknown create outcome was retried: %d POSTs", adapter.createCalls)
	}
}

func TestSameSlugReplacementAndLostMarkerStopMutation(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	original := adapter.projects["checkout"]
	replacement := original
	replacement.Identity = "replacement-id"
	adapter.projects["checkout"] = replacement
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonOwnershipLost) {
		t.Fatalf("replacement error = %v", err)
	}
	adapter.projects["checkout"] = original
	withoutMarker := original
	withoutMarker.Marker = nil
	adapter.projects["checkout"] = withoutMarker
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonOwnershipLost) {
		t.Fatalf("lost marker error = %v", err)
	}
	if adapter.createCalls != 1 {
		t.Fatalf("ownership loss triggered create: %d", adapter.createCalls)
	}
}

func TestProjectUseIsReadOnlyAndMarkerAloneDoesNotProveManaged(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	marker := binding.Marker{Origin: "managed", Owner: "payments", ManagedBy: "company-sentry-op"}
	adapter.projects["shared"] = RemoteProject{Identity: "shared-id", Slug: "shared", URL: "https://sentry.example/projects/shared", Marker: &marker}
	input := useInput("reader", "reader-uid", "payments", "shared")
	result, err := engine.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatalf("external reader: %v", err)
	}
	if result.Origin != binding.OriginExternal || result.AccessMode != binding.AccessReadOnly || result.Owner != "" {
		t.Fatalf("marker-only reader result = %#v", result)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "shared"); err != binding.ErrNotFound {
		t.Fatalf("reader created manager claim: %v", err)
	}
}

func TestProjectUseRecognizesProtectedManagedEvidenceButRemainsReadOnly(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	managed := managedInput("shared", "manager-uid", "payments")
	if _, err := engine.Reconcile(context.Background(), managed); err != nil {
		t.Fatalf("manager create: %v", err)
	}
	reader := useInput("reader", "reader-uid", "payments-two", "shared")
	result, err := engine.Reconcile(context.Background(), reader)
	if err != nil {
		t.Fatalf("managed reader: %v", err)
	}
	if result.Origin != binding.OriginManaged || result.AccessMode != binding.AccessReadOnly || result.Owner != "payments" {
		t.Fatalf("managed reader result = %#v", result)
	}
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "shared")
	if err != nil || claim.Claim.Manager.UID != "manager-uid" {
		t.Fatalf("reader changed manager claim = %#v, %v", claim, err)
	}
	if adapter.createCalls != 1 {
		t.Fatalf("reader triggered create: %d", adapter.createCalls)
	}
}

func TestAdminHandoffBindsTargetWithoutPOSTAndRevokesSource(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	source := managedInput("shared", "source-uid", "payments")
	if _, err := engine.Reconcile(context.Background(), source); err != nil {
		t.Fatalf("source create: %v", err)
	}
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "shared")
	if err != nil {
		t.Fatal(err)
	}
	target := managedInput("shared", "target-uid", "payments")
	target.Project.Generation = 3
	targetRef := binding.ProjectReference{Namespace: "payments", Name: "shared", UID: "target-uid", Generation: 3}
	if _, err := store.Handoff(context.Background(), claim, "source-uid", targetRef); err != nil {
		t.Fatalf("admin handoff: %v", err)
	}
	result, err := engine.Reconcile(context.Background(), target)
	if err != nil {
		t.Fatalf("target reconcile: %v", err)
	}
	if result.AccessMode != binding.AccessManage || result.Origin != binding.OriginManaged || adapter.createCalls != 1 {
		t.Fatalf("target result=%#v createCalls=%d", result, adapter.createCalls)
	}
	claim, err = store.GetClaim(context.Background(), "https://sentry.example", "company", "shared")
	if err != nil || claim.Claim.Manager.UID != "target-uid" || claim.Claim.Handoff != nil {
		t.Fatalf("completed claim=%#v err=%v", claim, err)
	}
	if _, err := engine.Reconcile(context.Background(), source); !IsReason(err, ReasonOwnershipConflict) {
		t.Fatalf("revoked source error = %v", err)
	}
}

func TestReadOnlyBindingDetectsExternalDeletion(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	adapter.projects["shared"] = RemoteProject{Identity: "shared-id", Slug: "shared", URL: "https://sentry.example/projects/shared"}
	reader := useInput("reader", "reader-uid", "payments", "shared")
	if _, err := engine.Reconcile(context.Background(), reader); err != nil {
		t.Fatal(err)
	}
	delete(adapter.projects, "shared")
	if _, err := engine.Reconcile(context.Background(), reader); !IsReason(err, ReasonExternalDeleted) {
		t.Fatalf("external deletion error = %v", err)
	}
}

func TestDeleteProjectRequiresCurrentOwnershipAndCleansProtectedRecords(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteDeleteProject
	result, err := engine.Delete(context.Background(), input)
	if err != nil || !result.Complete || result.Retained || adapter.deleteCalls != 1 {
		t.Fatalf("Delete result=%#v calls=%d err=%v", result, adapter.deleteCalls, err)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); err != binding.ErrNotFound {
		t.Fatalf("binding remains after confirmed delete: %v", err)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); err != binding.ErrNotFound {
		t.Fatalf("claim remains after confirmed delete: %v", err)
	}
}

func TestAuditFailureAfterDeleteResponseRecoversWithoutSecondDelete(t *testing.T) {
	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteDeleteProject
	auditSink := &fakeAudit{failAt: 2}
	engine.Audit = auditSink

	if _, err := engine.Delete(context.Background(), input); !errors.Is(err, ErrAuditUnavailable) || adapter.deleteCalls != 1 {
		t.Fatalf("post-delete audit outage = %v calls=%d", err, adapter.deleteCalls)
	}
	auditSink.failAt = 0
	result, err := engine.Delete(context.Background(), input)
	if err != nil || !result.Complete || adapter.deleteCalls != 1 {
		t.Fatalf("delete recovery = %#v err=%v calls=%d", result, err, adapter.deleteCalls)
	}
	if got := auditSink.outcomes(); !equalStrings(got, []string{"started/JournalSubmitted", "recovered/ResultRecovered"}) {
		t.Fatalf("unexpected delete recovery audit: %v", got)
	}
}

func TestRetainAndReadOnlyNeverDeleteExternalProject(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	managed := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), managed); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Delete(context.Background(), managed)
	if err != nil || !result.Complete || !result.Retained || adapter.deleteCalls != 0 {
		t.Fatalf("Retain result=%#v calls=%d err=%v", result, adapter.deleteCalls, err)
	}

	adapter.projects["shared"] = RemoteProject{Identity: "shared-id", Slug: "shared"}
	reader := useInput("reader", "reader-uid", "payments", "shared")
	reader.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteDeleteProject
	if _, err := engine.Delete(context.Background(), reader); !IsReason(err, ReasonOwnershipLost) {
		t.Fatalf("read-only DeleteProject error = %v", err)
	}
	if adapter.deleteCalls != 0 {
		t.Fatalf("read-only project issued DELETE: %d", adapter.deleteCalls)
	}
}

func TestDeleteProjectRejectsSameSlugReplacement(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteDeleteProject
	replacement := adapter.projects["checkout"]
	replacement.Identity = "replacement-id"
	adapter.projects["checkout"] = replacement
	if _, err := engine.Delete(context.Background(), input); !IsReason(err, ReasonOwnershipLost) {
		t.Fatalf("replacement delete error = %v", err)
	}
	if adapter.deleteCalls != 0 {
		t.Fatalf("replacement received DELETE: %d", adapter.deleteCalls)
	}
}

type fakeAdapter struct {
	mu                  sync.Mutex
	projects            map[string]RemoteProject
	getCalls            int
	createCalls         int
	lastCreate          CreateRequest
	loseCreateResponse  bool
	dropCreate          bool
	deleteCalls         int
	deleteLeavesObject  bool
	updateEvidenceCalls int
}

func (a *fakeAdapter) UpdateRegionalEvidence(_ context.Context, _, slug string, evidence ownership.Evidence) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	project, found := a.projects[slug]
	if !found {
		return &sentryapi.APIError{Kind: sentryapi.ErrorNotFound, StatusCode: 404, Operation: "update-project-regional-evidence"}
	}
	a.updateEvidenceCalls++
	project.Marker = nil
	project.RegionalEvidence = &evidence
	a.projects[slug] = project
	return nil
}

func (a *fakeAdapter) GetBySlug(_ context.Context, _, slug string) (RemoteProject, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.getCalls++
	project, found := a.projects[slug]
	if !found {
		return RemoteProject{}, &sentryapi.APIError{Kind: sentryapi.ErrorNotFound, StatusCode: 404, Operation: "get-project"}
	}
	return cloneRemote(project), nil
}

func (a *fakeAdapter) Create(_ context.Context, request CreateRequest) (RemoteProject, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.createCalls++
	a.lastCreate = request
	if a.dropCreate {
		return RemoteProject{}, &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "create-project"}
	}
	marker := request.Marker
	project := RemoteProject{
		Identity: "project-" + request.Slug,
		Slug:     request.Slug,
		URL:      "https://sentry.example/projects/" + request.Slug,
	}
	if request.RegionalEvidence != nil {
		evidence := *request.RegionalEvidence
		project.RegionalEvidence = &evidence
	} else {
		project.Marker = &marker
	}
	a.projects[request.Slug] = project
	if a.loseCreateResponse {
		return RemoteProject{}, &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "create-project"}
	}
	return cloneRemote(project), nil
}

func (a *fakeAdapter) Delete(_ context.Context, _, slug string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.deleteCalls++
	if !a.deleteLeavesObject {
		delete(a.projects, slug)
	}
	return nil
}

func cloneRemote(project RemoteProject) RemoteProject {
	if project.Marker != nil {
		marker := *project.Marker
		project.Marker = &marker
	}
	if project.RegionalEvidence != nil {
		evidence := *project.RegionalEvidence
		project.RegionalEvidence = &evidence
	}
	return project
}

func testEngine(t *testing.T) (*Engine, *fakeAdapter, *binding.Store) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := &binding.Store{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "operator"}
	adapter := &fakeAdapter{projects: make(map[string]RemoteProject)}
	return &Engine{Bindings: store, Adapter: adapter, ManagedBy: "company-sentry-op", Audit: &fakeAudit{}}, adapter, store
}

type fakeAudit struct {
	events []audit.Event
	failAt int
}

type fakeOwnershipClock struct {
	calls int
}

func (c *fakeOwnershipClock) Sleep(context.Context, time.Duration) error {
	c.calls++
	return nil
}

func (a *fakeAudit) Record(_ context.Context, event audit.Event) error {
	if a.failAt > 0 && len(a.events)+1 == a.failAt {
		return errors.New("audit unavailable")
	}
	if err := audit.Validate(event); err != nil {
		return err
	}
	a.events = append(a.events, event)
	return nil
}

func (a *fakeAudit) outcomes() []string {
	result := make([]string, 0, len(a.events))
	for _, event := range a.events {
		result = append(result, string(event.Outcome)+"/"+string(event.Reason))
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func managedInput(name, uid, namespace string) Input {
	return Input{
		Project: &sentryv1alpha1.SentryProject{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: types.UID(uid), Generation: 1},
			Spec:       sentryv1alpha1.SentryProjectSpec{Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{Team: "payments"}}},
		},
		ConnectionName: "main",
		Connection:     testConnectionSpec(),
	}
}

func useInput(name, uid, namespace, slug string) Input {
	input := managedInput(name, uid, namespace)
	input.Project.Spec.Project = sentryv1alpha1.ProjectSelector{Use: &slug}
	return input
}

func testConnectionSpec() sentryv1alpha1.SentryConnectionSpec {
	return sentryv1alpha1.SentryConnectionSpec{
		URL:          "https://sentry.example/",
		Organization: "company",
		Access: []sentryv1alpha1.AccessRule{
			{
				Owner:       "payments",
				Namespaces:  sentryv1alpha1.NamespaceAccess{MatchNames: []string{"payments"}},
				Teams:       []string{"payments"},
				AllowCreate: true,
				ExistingProjects: &sentryv1alpha1.ExistingProjectAccess{
					Observe: []string{"shared", "checkout"},
				},
			},
			{
				Owner:       "payments-two",
				Namespaces:  sentryv1alpha1.NamespaceAccess{MatchNames: []string{"payments-two"}},
				Teams:       []string{"payments"},
				AllowCreate: true,
				ExistingProjects: &sentryv1alpha1.ExistingProjectAccess{
					Observe: []string{"shared", "checkout"},
				},
			},
		},
	}
}
