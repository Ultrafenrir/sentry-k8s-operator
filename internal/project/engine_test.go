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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/ownership"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
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

func TestManagedProjectPreservesLegacyBoundOwner(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-legacy-owner", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetBinding(context.Background(), "uid-legacy-owner")
	if err != nil {
		t.Fatal(err)
	}
	next := record.Binding
	next.Owner = "legacy-platform-owner"
	next.Marker.Owner = next.Owner
	if _, err := store.UpdateBinding(context.Background(), record, next); err != nil {
		t.Fatal(err)
	}
	claim, err := store.GetClaim(context.Background(), record.Binding.Installation, record.Binding.Organization, record.Binding.Slug)
	if err != nil {
		t.Fatal(err)
	}
	nextClaim := claim.Claim
	nextClaim.Owner = next.Owner
	if _, err := store.UpdateClaim(context.Background(), claim, nextClaim); err != nil {
		t.Fatal(err)
	}
	remote := adapter.projects["checkout"]
	remote.Marker.Owner = next.Owner
	adapter.projects["checkout"] = remote

	result, err := engine.Reconcile(context.Background(), input)
	if err != nil || result.Owner != next.Owner || adapter.createCalls != 1 {
		t.Fatalf("result=%#v err=%v creates=%d", result, err, adapter.createCalls)
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

func TestStandbyProjectObservesUnmanagedProjectWithoutClaim(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	adapter.projects["checkout"] = RemoteProject{Identity: "external-id", Slug: "checkout", URL: "https://sentry.example/projects/checkout"}
	input := managedInput("checkout", "uid-one", "payments")
	input.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
	input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsStandby

	result, err := engine.Reconcile(context.Background(), input)
	if err != nil || result.Origin != binding.OriginExternal || result.AccessMode != binding.AccessReadOnly {
		t.Fatalf("standby result=%#v error=%v", result, err)
	}
	if adapter.getCalls == 0 || adapter.createCalls != 0 {
		t.Fatalf("standby project made get=%d create=%d calls", adapter.getCalls, adapter.createCalls)
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || record.Binding.Origin != binding.OriginExternal || record.Binding.AccessMode != binding.AccessReadOnly || record.Binding.ExternalIdentity != "external-id" {
		t.Fatalf("standby binding=%#v error=%v", record.Binding, err)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("standby project created claim: %v", err)
	}
}

func TestStandbyProjectContributesToProtectedManagedProjectWithoutTakingClaim(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	manager := managedInput("checkout", "manager-uid", "payments")
	manager.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8}
	if _, err := engine.Reconcile(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	candidate := managedInput("checkout", "candidate-uid", "payments-two")
	candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
	candidate.Connection.Ownership = manager.Connection.Ownership.DeepCopy()
	result, err := engine.Reconcile(context.Background(), candidate)
	if err != nil || result.Origin != binding.OriginManaged || result.AccessMode != binding.AccessReadOnly || result.Owner != "payments" {
		t.Fatalf("candidate result=%#v error=%v", result, err)
	}
	record, err := store.GetBinding(context.Background(), "candidate-uid")
	if err != nil || record.Binding.Marker == nil || record.Binding.RegionalOwnership == nil || record.Binding.ManagerUID != "" || record.Binding.ClaimName != "" {
		t.Fatalf("candidate binding=%#v error=%v", record.Binding, err)
	}
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil || claim.Claim.Manager.UID != "manager-uid" {
		t.Fatalf("manager claim=%#v error=%v", claim.Claim, err)
	}
	if adapter.createCalls != 1 || adapter.updateEvidenceCalls != 0 {
		t.Fatalf("candidate create=%d update=%d", adapter.createCalls, adapter.updateEvidenceCalls)
	}
}

func TestExactHandoffPromotesStandbyWithoutExternalMutation(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	manager := managedInput("checkout", "manager-uid", "payments")
	manager.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8}
	if _, err := engine.Reconcile(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	candidate := managedInput("checkout", "candidate-uid", "payments-two")
	candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
	candidate.Connection.Ownership = manager.Connection.Ownership.DeepCopy()
	if _, err := engine.Reconcile(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleActive
	if _, err := engine.Reconcile(context.Background(), candidate); !IsReason(err, ReasonOwnershipConflict) {
		t.Fatalf("active request stole manager claim: %v", err)
	}
	candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	candidate.Project.Generation = 2
	target := projectReference(candidate.Project)
	if _, err := store.Handoff(context.Background(), claim, "manager-uid", target); err != nil {
		t.Fatal(err)
	}
	candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleActive
	result, err := engine.Reconcile(context.Background(), candidate)
	if err != nil || result.AccessMode != binding.AccessManage || result.Owner != "payments" {
		t.Fatalf("promotion result=%#v error=%v", result, err)
	}
	updated, err := store.GetBinding(context.Background(), "candidate-uid")
	if err != nil || updated.Binding.AccessMode != binding.AccessManage || updated.Binding.ManagerUID != "candidate-uid" || updated.Binding.Project.Generation != 2 {
		t.Fatalf("promoted binding=%#v error=%v", updated.Binding, err)
	}
	claim, err = store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil || claim.Claim.Manager.UID != "candidate-uid" || claim.Claim.Handoff != nil {
		t.Fatalf("promoted claim=%#v error=%v", claim.Claim, err)
	}
	if adapter.createCalls != 1 || adapter.updateEvidenceCalls != 0 {
		t.Fatalf("promotion create=%d update=%d", adapter.createCalls, adapter.updateEvidenceCalls)
	}
}

func TestUnclaimedManagedProjectPromotesStandbyWithoutStealing(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	manager := managedInput("checkout", "manager-uid", "payments")
	manager.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8}
	if _, err := engine.Reconcile(context.Background(), manager); err != nil {
		t.Fatal(err)
	}
	candidate := managedInput("checkout", "candidate-uid", "payments-two")
	candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
	candidate.Connection.Ownership = manager.Connection.Ownership.DeepCopy()
	if _, err := engine.Reconcile(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil || store.DeleteClaim(context.Background(), claim) != nil {
		t.Fatalf("release claim=%#v error=%v", claim.Claim, err)
	}
	candidate.Project.Spec.Role = sentryv1alpha1.ProjectRoleActive
	candidate.Project.Generation = 2
	result, err := engine.Reconcile(context.Background(), candidate)
	if err != nil || result.AccessMode != binding.AccessManage {
		t.Fatalf("unclaimed promotion result=%#v error=%v", result, err)
	}
	claim, err = store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil || claim.Claim.Manager.UID != "candidate-uid" || claim.Claim.ExternalIdentity == "" {
		t.Fatalf("acquired claim=%#v error=%v", claim.Claim, err)
	}
	if adapter.createCalls != 1 || adapter.updateEvidenceCalls != 0 {
		t.Fatalf("unclaimed promotion create=%d update=%d", adapter.createCalls, adapter.updateEvidenceCalls)
	}
}

func TestExternalStandbyPromotesOnlyThroughExplicitAdoption(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	adapter.projects["checkout"] = RemoteProject{Identity: "external-id", Slug: "checkout", URL: "https://sentry.example/projects/checkout"}
	input := managedInput("checkout", "candidate-uid", "payments")
	input.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
	input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsStandby
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Project.Spec.Role = sentryv1alpha1.ProjectRoleActive
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonProjectAlreadyExists) {
		t.Fatalf("standby policy unexpectedly promoted external project: %v", err)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("denied promotion left claim: %v", err)
	}
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8}
	input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsAdopt
	input.Project.Generation = 2
	result, err := engine.Reconcile(context.Background(), input)
	if err != nil || result.Origin != binding.OriginManaged || result.AccessMode != binding.AccessManage {
		t.Fatalf("external promotion result=%#v error=%v", result, err)
	}
	record, err := store.GetBinding(context.Background(), "candidate-uid")
	if err != nil || record.Binding.Origin != binding.OriginManaged || record.Binding.AccessMode != binding.AccessManage || record.Binding.Pending != nil || record.Binding.RegionalOwnership == nil {
		t.Fatalf("adopted standby binding=%#v error=%v", record.Binding, err)
	}
	if adapter.createCalls != 0 || adapter.updateEvidenceCalls != 1 {
		t.Fatalf("external promotion create=%d update=%d", adapter.createCalls, adapter.updateEvidenceCalls)
	}
}

func TestStandbyProjectRejectsUnmanagedProjectByDefault(t *testing.T) {
	t.Parallel()
	engine, adapter, store := testEngine(t)
	adapter.projects["checkout"] = RemoteProject{Identity: "external-id", Slug: "checkout"}
	input := managedInput("checkout", "uid-one", "payments")
	input.Project.Spec.Role = sentryv1alpha1.ProjectRoleStandby
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonProjectAlreadyExists) {
		t.Fatalf("default standby policy error=%v", err)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("rejected standby left binding: %v", err)
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

func TestExistingUnmarkedProjectIsAdoptedOnlyByActivePolicy(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	adapter.projects["checkout"] = RemoteProject{Identity: "existing-id", Slug: "checkout", URL: "https://sentry.example/projects/checkout"}
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}
	input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsAdopt

	result, err := engine.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatalf("adopt existing project: %v", err)
	}
	if result.Origin != binding.OriginManaged || result.AccessMode != binding.AccessManage || result.Owner != "payments" {
		t.Fatalf("adoption result = %#v", result)
	}
	if adapter.createCalls != 0 || adapter.updateEvidenceCalls != 1 {
		t.Fatalf("adoption create=%d update=%d", adapter.createCalls, adapter.updateEvidenceCalls)
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || record.Binding.ExternalIdentity != "existing-id" || record.Binding.Pending != nil || record.Binding.RegionalOwnership == nil || record.Binding.RegionalOwnership.Revision != 8 {
		t.Fatalf("adopted binding=%#v err=%v", record.Binding, err)
	}
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil || claim.Claim.ExternalIdentity != "existing-id" || claim.Claim.Manager.UID != "uid-one" {
		t.Fatalf("adoption claim=%#v err=%v", claim.Claim, err)
	}
	remote := adapter.projects["checkout"]
	if remote.RegionalEvidence == nil || ownership.ValidateEvidence(ownership.Local{Marker: *record.Binding.RegionalOwnership, Active: true}, *remote.RegionalEvidence) != nil {
		t.Fatalf("adopted remote=%#v", remote)
	}
	if got := engine.Audit.(*fakeAudit).outcomes(); !equalStrings(got, []string{"started/JournalSubmitted", "succeeded/MutationVerified"}) {
		t.Fatalf("adoption audit = %v", got)
	}
}

func TestAdoptionRecoversAppliedMarkerAfterLostUpdateResponse(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	adapter.projects["checkout"] = RemoteProject{Identity: "existing-id", Slug: "checkout", URL: "https://sentry.example/projects/checkout"}
	adapter.loseUpdateResponse = true
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}
	input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsAdopt

	if _, err := engine.Reconcile(context.Background(), input); err == nil {
		t.Fatal("lost adoption response unexpectedly succeeded")
	}
	adapter.loseUpdateResponse = false
	result, err := engine.Reconcile(context.Background(), input)
	if err != nil || result.Origin != binding.OriginManaged {
		t.Fatalf("recover adoption: result=%#v err=%v", result, err)
	}
	if adapter.updateEvidenceCalls != 1 || adapter.createCalls != 0 {
		t.Fatalf("recovered adoption create=%d update=%d", adapter.createCalls, adapter.updateEvidenceCalls)
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil || record.Binding.Pending != nil {
		t.Fatalf("recovered binding=%#v err=%v", record.Binding, err)
	}
	if got := engine.Audit.(*fakeAudit).outcomes(); !equalStrings(got, []string{"started/JournalSubmitted", "failed/ApplyFailed", "recovered/ResultRecovered"}) {
		t.Fatalf("recovered adoption audit = %v", got)
	}
}

func TestAdoptionDoesNotTakeRecognizedOrStandbyProject(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	marker := binding.Marker{Origin: "managed", Owner: "another-owner", ManagedBy: "another-operator"}
	adapter.projects["checkout"] = RemoteProject{Identity: "existing-id", Slug: "checkout", Marker: &marker}
	input := managedInput("checkout", "uid-one", "payments")
	input.Connection.Ownership = &sentryv1alpha1.ConnectionOwnership{
		Region: "eu-west-1", Role: sentryv1alpha1.ConnectionRoleActive, Revision: 8,
	}
	input.Connection.ProjectManagement.UnmanagedProjects = sentryv1alpha1.UnmanagedProjectsAdopt

	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonProjectAlreadyExists) {
		t.Fatalf("recognized project adoption error=%v", err)
	}
	delete(adapter.projects, "checkout")
	adapter.projects["checkout"] = RemoteProject{Identity: "existing-id", Slug: "checkout"}
	input.Connection.Ownership.Role = sentryv1alpha1.ConnectionRoleStandby
	if _, err := engine.Reconcile(context.Background(), input); !IsReason(err, ReasonConnectionStandby) {
		t.Fatalf("standby adoption error=%v", err)
	}
	if adapter.updateEvidenceCalls != 0 {
		t.Fatalf("blocked adoption made %d updates", adapter.updateEvidenceCalls)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("blocked adoption left claim: %v", err)
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

	engine, adapter, store := testEngine(t)
	managed := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), managed); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Delete(context.Background(), managed)
	if err != nil || !result.Complete || !result.Retained || adapter.deleteCalls != 0 || adapter.releaseCalls != 1 {
		t.Fatalf("Retain result=%#v delete=%d release=%d err=%v", result, adapter.deleteCalls, adapter.releaseCalls, err)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); err != binding.ErrNotFound {
		t.Fatalf("binding remains after Retain release: %v", err)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); err != binding.ErrNotFound {
		t.Fatalf("claim remains after Retain release: %v", err)
	}
	if remote := adapter.projects["checkout"]; remote.Marker != nil || remote.RegionalEvidence != nil {
		t.Fatalf("remote ownership evidence remains after Retain release: %#v", remote)
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

func TestRetainWaitsForOwnedChildCleanup(t *testing.T) {
	t.Parallel()

	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetBinding(context.Background(), "uid-one")
	if err != nil {
		t.Fatal(err)
	}
	next := record.Binding
	next.GrantedTeams = map[string]string{"backend": "team-id"}
	if _, err := store.UpdateBinding(context.Background(), record, next); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Delete(context.Background(), input); !IsReason(err, ReasonReleasePending) {
		t.Fatalf("Retain with owned children error = %v", err)
	}
	if adapter.releaseCalls != 0 {
		t.Fatalf("Retain released ownership before child cleanup: %d", adapter.releaseCalls)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); err != nil {
		t.Fatalf("binding was removed before child cleanup: %v", err)
	}
}

func TestRetainCompletesWhenManagedCreateNeverEstablishedProtectedState(t *testing.T) {
	t.Parallel()

	engine, adapter, _ := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	result, err := engine.Delete(context.Background(), input)
	if err != nil || !result.Complete || !result.Retained {
		t.Fatalf("unbound Retain result=%#v err=%v", result, err)
	}
	if adapter.releaseCalls != 0 || adapter.deleteCalls != 0 {
		t.Fatalf("unbound Retain mutated Sentry: release=%d delete=%d", adapter.releaseCalls, adapter.deleteCalls)
	}
}

func TestRetainRecoversLostReleaseResponseWithoutSecondMutation(t *testing.T) {
	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	adapter.loseReleaseResponse = true
	if _, err := engine.Delete(context.Background(), input); !sentryapi.IsKind(err, sentryapi.ErrorTimeout) || adapter.releaseCalls != 1 {
		t.Fatalf("lost release response error=%v calls=%d", err, adapter.releaseCalls)
	}
	adapter.loseReleaseResponse = false
	result, err := engine.Delete(context.Background(), input)
	if err != nil || !result.Complete || !result.Retained || adapter.releaseCalls != 1 {
		t.Fatalf("release recovery result=%#v err=%v calls=%d", result, err, adapter.releaseCalls)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); err != binding.ErrNotFound {
		t.Fatalf("binding remains after release recovery: %v", err)
	}
}

func TestRetainRecoversClaimDeleteFailureAfterBindingDelete(t *testing.T) {
	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	store.Client = &failOnceDeleteClient{Client: store.Client, name: claim.Name}

	if _, err := engine.Delete(context.Background(), input); err == nil || adapter.releaseCalls != 1 {
		t.Fatalf("first delete error=%v releaseCalls=%d", err, adapter.releaseCalls)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("binding remains after first cleanup: %v", err)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); err != nil {
		t.Fatalf("claim should remain after injected failure: %v", err)
	}

	result, err := engine.Delete(context.Background(), input)
	if err != nil || !result.Complete || !result.Retained || adapter.releaseCalls != 1 {
		t.Fatalf("recovered result=%#v err=%v releaseCalls=%d", result, err, adapter.releaseCalls)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("claim remains after recovery: %v", err)
	}
}

func TestDeleteProjectRecoversClaimDeleteFailureAfterBindingDelete(t *testing.T) {
	engine, adapter, store := testEngine(t)
	input := managedInput("checkout", "uid-one", "payments")
	if _, err := engine.Reconcile(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Project.Spec.Lifecycle.OnDelete = sentryv1alpha1.OnDeleteDeleteProject
	claim, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout")
	if err != nil {
		t.Fatal(err)
	}
	store.Client = &failOnceDeleteClient{Client: store.Client, name: claim.Name}

	if _, err := engine.Delete(context.Background(), input); err == nil || adapter.deleteCalls != 1 {
		t.Fatalf("first delete error=%v deleteCalls=%d", err, adapter.deleteCalls)
	}
	if _, err := store.GetBinding(context.Background(), "uid-one"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("binding remains after first cleanup: %v", err)
	}

	result, err := engine.Delete(context.Background(), input)
	if err != nil || !result.Complete || result.Retained || adapter.deleteCalls != 1 {
		t.Fatalf("recovered result=%#v err=%v deleteCalls=%d", result, err, adapter.deleteCalls)
	}
	if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); !errors.Is(err, binding.ErrNotFound) {
		t.Fatalf("claim remains after recovery: %v", err)
	}
}

func TestDeleteCleansClaimWhenBindingWasNeverCreated(t *testing.T) {
	for _, onDelete := range []sentryv1alpha1.OnDeletePolicy{sentryv1alpha1.OnDeleteRetain, sentryv1alpha1.OnDeleteDeleteProject} {
		t.Run(string(onDelete), func(t *testing.T) {
			engine, _, store := testEngine(t)
			input := managedInput("checkout", "uid-one", "payments")
			input.Project.Spec.Lifecycle.OnDelete = onDelete
			_, _, err := store.AcquireClaim(context.Background(), binding.ManagerClaim{
				Version: binding.CurrentVersion, Installation: "https://sentry.example", Organization: "company", Slug: "checkout",
				Owner: "payments", Manager: projectReference(input.Project),
			})
			if err != nil {
				t.Fatal(err)
			}

			result, err := engine.Delete(context.Background(), input)
			if err != nil || !result.Complete {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if _, err := store.GetClaim(context.Background(), "https://sentry.example", "company", "checkout"); !errors.Is(err, binding.ErrNotFound) {
				t.Fatalf("claim remains after recovery: %v", err)
			}
		})
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
	loseUpdateResponse  bool
	releaseCalls        int
	loseReleaseResponse bool
}

type failOnceDeleteClient struct {
	client.Client
	name   string
	failed bool
}

func (c *failOnceDeleteClient) Delete(ctx context.Context, object client.Object, options ...client.DeleteOption) error {
	if object.GetName() == c.name && !c.failed {
		c.failed = true
		return errors.New("injected delete failure")
	}
	return c.Client.Delete(ctx, object, options...)
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
	if a.loseUpdateResponse {
		return &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "update-project-regional-evidence"}
	}
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

func (a *fakeAdapter) ReleaseOwnership(_ context.Context, _, slug string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	project, found := a.projects[slug]
	if !found {
		return &sentryapi.APIError{Kind: sentryapi.ErrorNotFound, StatusCode: 404, Operation: "release-project-ownership"}
	}
	a.releaseCalls++
	project.Marker = nil
	project.RegionalEvidence = nil
	a.projects[slug] = project
	if a.loseReleaseResponse {
		return &sentryapi.APIError{Kind: sentryapi.ErrorTimeout, Operation: "release-project-ownership"}
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
	}
}
