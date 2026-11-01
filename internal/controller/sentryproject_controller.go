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

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	alertpolicyruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/alertpolicy"
	alertsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/alerts"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	clientkeyruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/clientkey"
	codemappingruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/codemapping"
	connectionruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/connection"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/dsn"
	issueownershipruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/issueownership"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/ownership"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/policy"
	projectruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/project"
	projectaccessruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/projectaccess"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/resolver"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
	settingsruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/settings"
	symbolsourceruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/symbolsource"
)

const sentryProjectFinalizer = "sentryconfiguration.io/project-protection"

type AdapterFactory func(*sentryapi.Client) projectruntime.Adapter

type ClientKeyLister func(context.Context, *sentryapi.Client, string, string) ([]dsn.ClientKey, error)

type ClientKeyAdapterFactory func(*sentryapi.Client) clientkeyruntime.Adapter

type SettingsAdapterFactory func(*sentryapi.Client) settingsruntime.Adapter

type SymbolSourceAdapterFactory func(*sentryapi.Client) symbolsourceruntime.Adapter

type CodeMappingAdapterFactory func(*sentryapi.Client) codemappingruntime.Adapter

type IssueOwnershipAdapterFactory func(*sentryapi.Client) issueownershipruntime.Adapter

type ProjectAccessAdapterFactory func(*sentryapi.Client) projectaccessruntime.Adapter

type DestinationCatalogFactory func(*sentryapi.Client) destination.Catalog

type AlertAdapterFactory func(*sentryapi.Client, map[string]binding.DestinationBinding) alertsruntime.Adapter

type ServiceHookAdapterFactory func(*sentryapi.Client) sentryhook.ServiceHookAdapter

// SentryProjectReconciler reconciles a SentryProject object.
type SentryProjectReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Registry *connectionruntime.Registry
	Bindings *binding.Store
	// OutputSecrets bypasses the Secret cache, which is deliberately limited to
	// the distinct platform-credential and protected-state namespaces.
	OutputSecrets            client.Client
	ManagedBy                string
	NewAdapter               AdapterFactory
	ListKeys                 ClientKeyLister
	NewClientKeyAdapter      ClientKeyAdapterFactory
	NewSettingsAdapter       SettingsAdapterFactory
	NewSymbolSourceAdapter   SymbolSourceAdapterFactory
	NewCodeMappingAdapter    CodeMappingAdapterFactory
	NewIssueOwnershipAdapter IssueOwnershipAdapterFactory
	NewProjectAccessAdapter  ProjectAccessAdapterFactory
	NewDestinationCatalog    DestinationCatalogFactory
	NewAlertAdapter          AlertAdapterFactory
	NewServiceHookAdapter    ServiceHookAdapterFactory
	HookSources              sentryhook.SourcePublicationStore
	HookVerifier             sentryhook.LifecycleVerifierStore
	// RegistrationRecovery uses a direct uncached client. It completes a durable
	// Telegram publish/fence release before any Sentry API dependency is needed.
	RegistrationRecovery *registration.Service
	RegistrationRoutes   registration.RouteManager
	RegistrationIndexes  *registration.ChallengeIndexStore
	Recorder             events.EventRecorder
	Metrics              *Metrics
	Audit                audit.Sink
	DestinationAudit     audit.Sink
}

const projectDriftReconcileInterval = 30 * time.Second

// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *SentryProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcileResult ctrl.Result, reconcileErr error) {
	started := time.Now()
	defer func() {
		r.Metrics.ObserveReconcile(controllerProject, reconcileResult, reconcileErr, time.Since(started))
	}()
	resource := &sentryconfigurationv1alpha1.SentryProject{}
	if err := r.Get(ctx, req.NamespacedName, resource); err != nil {
		if apierrors.IsNotFound(err) {
			r.Metrics.ForgetProjectSynchronization(req.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if r.Registry == nil || r.Bindings == nil || r.ManagedBy == "" {
		return ctrl.Result{}, fmt.Errorf("project runtime is not configured")
	}
	if attempted, recoveryErr := r.resumePendingRegistration(ctx, resource); attempted {
		if recoveryErr != nil {
			return ctrl.Result{RequeueAfter: jitterDuration(5 * time.Second)}, nil
		}
		// Let the Secret informer observe the direct-client CAS before ordinary
		// destination reconciliation reads or writes the same binding.
		return ctrl.Result{Requeue: true}, nil
	}

	if !resource.DeletionTimestamp.IsZero() {
		if !containsString(resource.Finalizers, sentryProjectFinalizer) {
			return ctrl.Result{}, nil
		}
		return r.reconcileDelete(ctx, resource)
	}
	if !containsString(resource.Finalizers, sentryProjectFinalizer) {
		base := resource.DeepCopy()
		resource.Finalizers = append(resource.Finalizers, sentryProjectFinalizer)
		if err := r.Patch(ctx, resource, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if resource.Spec.Paused {
		return r.reportFailure(ctx, resource, "Paused", "Reconciliation is paused; no Sentry API operation was attempted.")
	}

	connection, err := r.resolveConnection(ctx, resource)
	if err != nil {
		dsnRevoked, revokeErr := r.revokeDSNIfDenied(ctx, resource)
		if revokeErr != nil {
			return r.reportFailure(ctx, resource, reasonForError(revokeErr), "Connection policy could not be verified; an existing working DSN Secret was preserved.")
		}
		destinationResult, destinationsRevoked, destinationErr := r.revokeDestinationsIfDenied(ctx, resource)
		if destinationErr != nil {
			return r.reportFailure(ctx, resource, reasonForError(destinationErr), "Connection policy revoke state could not be recorded; existing destination identities were preserved.")
		}
		if dsnRevoked || destinationsRevoked {
			return r.reportAccessRevoked(ctx, resource, dsnRevoked, destinationResult, reasonForError(err))
		}
		return r.reportFailure(ctx, resource, reasonForError(err), "Connection or access policy could not be resolved.")
	}
	statusBase := resource.DeepCopy()
	setProjectOwnershipStatus(resource, connection)
	standbyCandidate := resource.Spec.Project.Create != nil && resource.Spec.Role == sentryconfigurationv1alpha1.ProjectRoleStandby
	if resource.Spec.Project.Create != nil && !standbyCandidate {
		if reason, message := connectionMutationFence(connection); reason != "" {
			return r.reportFailure(ctx, resource, reason, message)
		}
	}
	if resource.Spec.Project.Create != nil && !standbyCandidate && !projectruntime.ManagedCreateSupported(connection.Status.Capabilities) {
		return r.reportFailure(ctx, resource, "ManagedCreateUnsupported", "The connection has not proven atomic default-rule suppression and an automatic active client key.")
	}
	apiClient, found := r.Registry.Get(connection.Name)
	if !found {
		return r.reportFailure(ctx, resource, "ConnectionNotReady", "The selected SentryConnection has not loaded its credentials yet.")
	}
	effectiveResource, appliedPolicy, application, err := r.applyAlertPolicy(ctx, resource, connection)
	if err != nil {
		return r.reportAlertPolicyFailure(ctx, resource, reasonForAlertPolicyError(err))
	}
	result, err := r.engine(apiClient).Reconcile(ctx, projectruntime.Input{
		Project:        resource,
		ConnectionName: connection.Name,
		Connection:     connection.Spec,
	})
	if err != nil {
		return r.reportFailure(ctx, resource, reasonForError(err), "Project reconciliation is safely blocked; no unverified mutation was performed.")
	}
	if result.AccessMode == binding.AccessManage && resource.Spec.Project.Create != nil && resource.Spec.Role != sentryconfigurationv1alpha1.ProjectRoleStandby && connection.Spec.Ownership != nil && connection.Spec.Ownership.Role == sentryconfigurationv1alpha1.ConnectionRoleActive {
		resource.Status.EffectiveRole = string(sentryconfigurationv1alpha1.ProjectRoleActive)
	}
	projectAccessResult := r.reconcileProjectAccess(ctx, resource, connection, apiClient, result)
	projectAccessErr := projectAccessResult.Err
	clientKeyResult := r.reconcileClientKeys(ctx, resource, connection, apiClient, result)
	clientKeyErr := clientKeyResult.Err()
	symbolSourceResult := r.reconcileSymbolSources(ctx, resource, connection, apiClient, result)
	symbolSourceErr := symbolSourceResult.Err()
	codeMappingResult := r.reconcileCodeMappings(ctx, resource, connection, apiClient, result)
	codeMappingErr := codeMappingResult.Err()
	dsnResult, err := r.reconcileDSN(ctx, resource, connection, apiClient, result, clientKeyResult)
	dsnErr := err
	settingsResult := r.reconcileSettings(ctx, resource, connection, apiClient, result)
	settingsErr := settingsResult.Err()
	issueOwnershipResult := r.reconcileIssueOwnership(ctx, resource, connection, apiClient, result, projectAccessErr)
	issueOwnershipErr := issueOwnershipResult.Err
	destinationResult := r.reconcileDestinations(ctx, effectiveResource, connection, apiClient, result, application)
	alertResult := r.reconcileAlerts(ctx, effectiveResource, connection, apiClient, result, destinationCleanupKeys(destinationResult), application)
	if err := r.finalizeDestinationCleanup(ctx, resource, connection, &destinationResult, alertResult.ReleasableDestinations); err != nil {
		destinationResult.PolicyErr = errors.Join(destinationResult.PolicyErr, err)
	}
	destinationErr := destinationResult.Err()
	hookErr := r.reconcileServiceHook(ctx, effectiveResource, connection, apiClient)
	alertErr := errors.Join(alertResult.Err(), hookErr)
	base := statusBase
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.Origin = string(result.Origin)
	resource.Status.AccessMode = result.AccessMode
	resource.Status.ProjectURL = result.ProjectURL
	resource.Status.DSNSecret = dsnResult.Secret
	resource.Status.DSNClientKey = dsnResult.ClientKey
	resource.Status.ClientKeys = safeClientKeyStatuses(clientKeyResult.Statuses)
	resource.Status.SymbolSources = safeSymbolSourceStatuses(symbolSourceResult.Statuses)
	resource.Status.CodeMappings = safeCodeMappingStatuses(codeMappingResult.Statuses)
	resource.Status.NotificationTargets = safeDestinationStatuses(destinationResult.Statuses, resource.Status.NotificationTargets)
	resource.Status.Alerts = safeAlertStatuses(alertResult.Statuses)
	policyStatus, policyReason, policyMessage := metav1.ConditionTrue, "NotConfigured", "No reusable alert policy is configured."
	if application.PolicyDeleting {
		policyStatus, policyReason, policyMessage = metav1.ConditionFalse, "AlertPolicyDeleting", "The referenced alert policy is deleting; policy-owned children are being removed safely."
	} else if appliedPolicy != nil {
		policyReason, policyMessage = "AlertPolicyApplied", "The referenced alert policy was merged without ownership conflicts."
	}
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{Type: "AlertPolicyReady", Status: policyStatus, ObservedGeneration: resource.Generation, Reason: policyReason, Message: policyMessage})
	dsnApplicable := result.AccessMode == binding.AccessManage || resource.Spec.DSN != nil
	dsnStatus, dsnReason, dsnMessage := dsnCondition(dsnApplicable, dsnResult.Phase, dsnResult.ExternalNameChanged, dsnErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "DSNReady",
		Status:             dsnStatus,
		ObservedGeneration: resource.Generation,
		Reason:             dsnReason,
		Message:            dsnMessage,
	})
	clientKeysApplicable := resource.Spec.ClientKeys != nil
	clientKeysStatus, clientKeysReason, clientKeysMessage := clientKeysCondition(clientKeysApplicable, clientKeyResult, clientKeyErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "ClientKeysReady",
		Status:             clientKeysStatus,
		ObservedGeneration: resource.Generation,
		Reason:             clientKeysReason,
		Message:            clientKeysMessage,
	})
	symbolSourcesApplicable := resource.Spec.SymbolSources != nil
	symbolSourcesStatus, symbolSourcesReason, symbolSourcesMessage := symbolSourcesCondition(symbolSourcesApplicable, symbolSourceResult, symbolSourceErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "SymbolSourcesReady",
		Status:             symbolSourcesStatus,
		ObservedGeneration: resource.Generation,
		Reason:             symbolSourcesReason,
		Message:            symbolSourcesMessage,
	})
	codeMappingsApplicable := resource.Spec.CodeMappings != nil
	codeMappingsStatus, codeMappingsReason, codeMappingsMessage := codeMappingsCondition(codeMappingsApplicable, codeMappingResult, codeMappingErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "CodeMappingsReady",
		Status:             codeMappingsStatus,
		ObservedGeneration: resource.Generation,
		Reason:             codeMappingsReason,
		Message:            codeMappingsMessage,
	})
	settingsApplicable := resource.Spec.Settings != nil
	settingsStatus, settingsReason, settingsMessage := settingsCondition(settingsApplicable, settingsResult, settingsErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "SettingsReady",
		Status:             settingsStatus,
		ObservedGeneration: resource.Generation,
		Reason:             settingsReason,
		Message:            settingsMessage,
	})
	issueOwnershipApplicable := resource.Spec.IssueOwnership != nil
	issueOwnershipStatus, issueOwnershipReason, issueOwnershipMessage := issueOwnershipCondition(issueOwnershipApplicable, issueOwnershipErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "IssueOwnershipReady",
		Status:             issueOwnershipStatus,
		ObservedGeneration: resource.Generation,
		Reason:             issueOwnershipReason,
		Message:            issueOwnershipMessage,
	})
	projectAccessApplicable := resource.Spec.GrantAccess != nil || len(projectAccessResult.Bindings) > 0
	projectAccessStatus, projectAccessReason, projectAccessMessage := projectAccessCondition(projectAccessApplicable, projectAccessErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "GrantAccessReady",
		Status:             projectAccessStatus,
		ObservedGeneration: resource.Generation,
		Reason:             projectAccessReason,
		Message:            projectAccessMessage,
	})
	destinationApplicable := effectiveResource.Spec.NotificationTargets != nil || len(destinationResult.Statuses) > 0
	destinationStatus, destinationReason, destinationMessage := destinationCondition(destinationApplicable, destinationResult, destinationErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "NotificationTargetsReady",
		Status:             destinationStatus,
		ObservedGeneration: resource.Generation,
		Reason:             destinationReason,
		Message:            destinationMessage,
	})
	alertsApplicable := effectiveResource.Spec.Alerts != nil
	alertsStatus, alertsReason, alertsMessage := alertCondition(alertsApplicable, alertResult, alertErr)
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "AlertsReady",
		Status:             alertsStatus,
		ObservedGeneration: resource.Generation,
		Reason:             alertsReason,
		Message:            alertsMessage,
	})
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "ProjectReady",
		Status:             metav1.ConditionTrue,
		ObservedGeneration: resource.Generation,
		Reason:             "ProjectVerified",
		Message:            "Project identity, access mode and ownership evidence are verified.",
	})
	ready := policyStatus == metav1.ConditionTrue && (!dsnApplicable || dsnStatus == metav1.ConditionTrue) && (!clientKeysApplicable || clientKeysStatus == metav1.ConditionTrue) && (!symbolSourcesApplicable || symbolSourcesStatus == metav1.ConditionTrue) && (!codeMappingsApplicable || codeMappingsStatus == metav1.ConditionTrue) && (!settingsApplicable || settingsStatus == metav1.ConditionTrue) && (!issueOwnershipApplicable || issueOwnershipStatus == metav1.ConditionTrue) && (!projectAccessApplicable || projectAccessStatus == metav1.ConditionTrue) && (!destinationApplicable || destinationStatus == metav1.ConditionTrue) && (!alertsApplicable || alertsStatus == metav1.ConditionTrue)
	readyStatus := metav1.ConditionTrue
	readyReason := "ProjectVerified"
	readyMessage := "Project identity, access mode and all applicable components are verified."
	if !ready {
		readyStatus = metav1.ConditionFalse
		readyReason = "ComponentNotReady"
		readyMessage = "One or more applicable components are not ready; verified components were preserved."
	}
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             readyStatus,
		ObservedGeneration: resource.Generation,
		Reason:             readyReason,
		Message:            readyMessage,
	})
	if err := r.Status().Patch(ctx, resource, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	emitConditionChangesObserved(r.Recorder, r.Metrics, controllerProject, resource, base.Status.Conditions, resource.Status.Conditions)
	if !ready {
		return ctrl.Result{RequeueAfter: jitterDuration(30 * time.Second)}, nil
	}
	return ctrl.Result{RequeueAfter: jitterDuration(projectDriftReconcileInterval)}, nil
}

func (r *SentryProjectReconciler) resumePendingRegistration(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (bool, error) {
	service := r.RegistrationRecovery
	if service == nil || service.Store == nil || resource == nil || resource.UID == "" {
		return false, nil
	}
	record, err := service.Store.GetBinding(ctx, string(resource.UID))
	if errors.Is(err, binding.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	attempted := false
	for name, destinationBinding := range record.Binding.Destinations {
		if destinationBinding.Telegram == nil ||
			(destinationBinding.Telegram.Phase != registration.PhasePendingPublish && destinationBinding.Telegram.Phase != registration.PhasePendingFenceRelease) {
			continue
		}
		attempted = true
		if err := service.Resume(ctx, string(resource.UID), name); err != nil {
			return true, err
		}
	}
	return attempted, nil
}

func dsnCondition(applicable bool, phase dsn.Phase, externalNameChanged bool, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "DSN publication was not requested for this read-only project."
	}
	if err != nil {
		if errors.Is(err, dsn.ErrPinnedKeyLost) {
			return metav1.ConditionFalse, "DSNKeyRevoked", "The output Secret was removed after the pinned client key was proven missing or inactive."
		}
		return metav1.ConditionFalse, reasonForError(err), "DSN reconciliation is safely blocked; an existing working Secret was preserved unless revoke was proven."
	}
	if phase == dsn.PhaseCutoverPending {
		return metav1.ConditionFalse, "DSNCutoverPending", "The new Secret is ready; confirm the cutover annotation before the old Secret is removed."
	}
	if externalNameChanged {
		return metav1.ConditionTrue, "ExternalNameChanged", "The pinned client key was renamed in Sentry; its verified identity and output Secret remain current."
	}
	return metav1.ConditionTrue, "DSNReady", "The managed output Secret is current."
}

func clientKeysCondition(applicable bool, result clientkeyruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No managed client keys are configured."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil && status.State == clientkeyruntime.StateReady {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "ClientKeysPartiallyApplied", "At least one managed client key failed; independently verified keys were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Managed client keys could not be safely reconciled; protected identities were preserved."
	}
	for _, status := range result.Statuses {
		if status.State == clientkeyruntime.StateRotationPending {
			return metav1.ConditionFalse, "ClientKeyCutoverPending", "A replacement key is verified and waiting for public DSN cutover before guarded old-key deletion."
		}
	}
	return metav1.ConditionTrue, "ClientKeysReady", "All managed client keys match the requested lifecycle state."
}

func symbolSourcesCondition(applicable bool, result symbolsourceruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No custom symbol sources are configured."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil && status.State == symbolsourceruntime.StateReady {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "SymbolSourcesPartiallyApplied", "At least one custom symbol source failed; independently verified sources were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Custom symbol sources could not be safely reconciled; protected identities were preserved."
	}
	return metav1.ConditionTrue, "SymbolSourcesReady", "All managed custom symbol sources match the requested lifecycle state."
}

func codeMappingsCondition(applicable bool, result codemappingruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No repository code mappings are configured."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil && status.State == codemappingruntime.StateReady {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "CodeMappingsPartiallyApplied", "At least one code mapping failed; independently verified mappings were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Repository code mappings could not be safely reconciled; protected identities were preserved."
	}
	return metav1.ConditionTrue, "CodeMappingsReady", "All managed repository code mappings and requested CODEOWNERS imports match desired state."
}

func settingsCondition(applicable bool, result settingsruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No typed project settings are managed."
	}
	if err != nil {
		if (result.Filters.Managed && result.Filters.Err == nil) || (result.PII.Managed && result.PII.Err == nil) || (result.Behavior.Managed && result.Behavior.Err == nil) || (result.Grouping.Managed && result.Grouping.Err == nil) || (result.Symbolication.Managed && result.Symbolication.Err == nil) {
			return metav1.ConditionFalse, "SettingsPartiallyApplied", "At least one independent settings component failed; successful components were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Typed project settings could not be fully reconciled."
	}
	return metav1.ConditionTrue, "SettingsReady", "All requested typed project settings match desired state."
}

func issueOwnershipCondition(applicable bool, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No issue ownership configuration is managed."
	}
	if err != nil {
		return metav1.ConditionFalse, reasonForError(err), "Issue ownership configuration could not be safely reconciled."
	}
	return metav1.ConditionTrue, "IssueOwnershipReady", "Team-only issue ownership rules and assignment behavior match the requested state."
}

func projectAccessCondition(applicable bool, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No additional project team access is managed."
	}
	if err != nil {
		return metav1.ConditionFalse, reasonForError(err), "Additional project team access could not be fully reconciled."
	}
	return metav1.ConditionTrue, "GrantAccessReady", "All requested additional teams have project access."
}

func destinationCondition(applicable bool, result destination.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No notification destinations are configured."
	}
	if err != nil {
		if result.RevokeOnly {
			return metav1.ConditionFalse, "DestinationAccessRevoked", "Destination access was revoked; only protected cleanup state remains."
		}
		return metav1.ConditionFalse, reasonForError(err), "One or more destinations could not be safely resolved; pinned identities were preserved."
	}
	for _, status := range result.Statuses {
		if status.State == destination.StateRegistrationRequired {
			return metav1.ConditionFalse, "TelegramRegistrationRequired", "Telegram destination registration is required."
		}
		if status.State == destination.StateCleanupPending {
			return metav1.ConditionFalse, "DestinationCleanupPending", "Removed destination routing cleanup is pending."
		}
	}
	return metav1.ConditionTrue, "NotificationTargetsReady", "All configured notification targets have verified pinned identities."
}

func alertCondition(applicable bool, result alertsruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "Alert management was not requested."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "AlertsPartiallyApplied", "At least one alert failed; independently verified alerts were preserved."
		}
		return metav1.ConditionFalse, "AlertsNotReady", "Alerts could not be safely reconciled; protected identities were preserved."
	}
	return metav1.ConditionTrue, "AlertsReady", "All managed alerts match the requested state."
}

type dsnReconcileResult struct {
	Secret              string
	ClientKey           string
	Phase               dsn.Phase
	ExternalNameChanged bool
}

func (r *SentryProjectReconciler) reconcileProjectAccess(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) projectaccessruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return projectaccessruntime.Result{Err: err}
	}
	if resource.Spec.GrantAccess == nil && len(record.Binding.GrantedTeams) == 0 {
		return projectaccessruntime.Result{Bindings: map[string]string{}}
	}
	if projectResult.AccessMode != binding.AccessManage || resource.Spec.Project.Create == nil {
		return projectaccessruntime.Result{Bindings: record.Binding.GrantedTeams, Err: projectaccessruntime.ErrInvalidInput}
	}
	var adapter projectaccessruntime.Adapter
	if r.NewProjectAccessAdapter != nil {
		adapter = r.NewProjectAccessAdapter(apiClient)
	} else {
		adapter = &projectaccessruntime.HTTPAdapter{Client: apiClient}
	}
	authorizedDesired, policyErr := policy.AuthorizedProjectAccessTeams(resource.Namespace, resource.Spec, connection.Spec)
	result := (&projectaccessruntime.Engine{Adapter: adapter, Audit: r.Audit}).Reconcile(ctx, projectaccessruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug,
		PrimaryTeam: resource.Spec.Project.Create.Team, Desired: authorizedDesired, Current: record.Binding.GrantedTeams,
		LocalResource: resource.Namespace + "/" + resource.Name, Generation: resource.Generation,
		Authorize: r.projectAccessAuthorizer(resource, connection, record.Binding, authorizedDesired),
	})
	result.Err = errors.Join(result.Err, policyErr)
	if reflect.DeepEqual(record.Binding.GrantedTeams, result.Bindings) {
		return result
	}
	next := record.Binding
	next.GrantedTeams = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Err = errors.Join(result.Err, err)
	}
	return result
}

func (r *SentryProjectReconciler) projectAccessAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding, expectedTeams []string) projectaccessruntime.Authorization {
	return func(ctx context.Context) error {
		if err := r.dsnAuthorizer(original, connection, expected)(ctx); err != nil {
			return err
		}
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latest); err != nil {
			return err
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		if !original.DeletionTimestamp.IsZero() {
			if latest.DeletionTimestamp.IsZero() || original.Spec.GrantAccess == nil || len(original.Spec.GrantAccess.Teams) != 0 || len(expectedTeams) != 0 {
				return projectaccessruntime.ErrAuthorization
			}
		} else {
			if !reflect.DeepEqual(latest.Spec.GrantAccess, original.Spec.GrantAccess) {
				return projectaccessruntime.ErrAuthorization
			}
			latestTeams, policyErr := policy.AuthorizedProjectAccessTeams(latest.Namespace, latest.Spec, latestConnection.Spec)
			if !reflect.DeepEqual(latestTeams, expectedTeams) || (policyErr != nil && !policy.IsKind(policyErr, policy.ErrorAccessDenied)) {
				return projectaccessruntime.ErrAuthorization
			}
		}
		state, err := r.Bindings.GetBinding(ctx, string(original.UID))
		if err != nil || !reflect.DeepEqual(state.Binding.GrantedTeams, expected.GrantedTeams) {
			return projectaccessruntime.ErrAuthorization
		}
		return nil
	}
}

func (r *SentryProjectReconciler) reconcileClientKeys(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) clientkeyruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return clientkeyruntime.Result{Statuses: map[string]clientkeyruntime.Status{"": {State: clientkeyruntime.StateError, Reason: "ClientKeyStateUnavailable", Err: err}}}
	}
	if resource.Spec.ClientKeys == nil {
		return clientkeyruntime.Result{Bindings: record.Binding.ClientKeys, Statuses: map[string]clientkeyruntime.Status{}, PublicDSNs: map[string]string{}}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return clientkeyruntime.Result{Bindings: record.Binding.ClientKeys, Statuses: map[string]clientkeyruntime.Status{"": {State: clientkeyruntime.StateError, Reason: "InvalidClientKeyInput", Err: clientkeyruntime.ErrInvalidInput}}, PublicDSNs: map[string]string{}}
	}
	desired := make(map[string]clientkeyruntime.Desired, len(*resource.Spec.ClientKeys))
	for name, spec := range *resource.Spec.ClientKeys {
		revision := spec.RotationRevision
		if revision == 0 {
			revision = 1
		}
		value := clientkeyruntime.Desired{Active: spec.Active, RotationRevision: revision}
		if spec.RateLimit != nil {
			value.RateLimit = &clientkeyruntime.RateLimit{Count: spec.RateLimit.Count, Window: spec.RateLimit.WindowSeconds}
		}
		desired[name] = value
	}
	promote := make(map[string]bool, len(desired))
	selected := ""
	if resource.Spec.DSN != nil {
		selected = resource.Spec.DSN.ManagedClientKey
	}
	for name, current := range record.Binding.ClientKeys {
		if current.ReplacementIdentity == "" {
			continue
		}
		promote[name] = name != selected || record.Binding.DSN != nil && record.Binding.DSN.KeyIdentity == current.ReplacementIdentity
	}
	adapter := clientkeyruntime.Adapter(&clientkeyruntime.HTTPAdapter{Client: apiClient})
	if r.NewClientKeyAdapter != nil {
		adapter = r.NewClientKeyAdapter(apiClient)
	}
	result := (&clientkeyruntime.Engine{Adapter: adapter, Journal: r.Bindings}).Reconcile(ctx, clientkeyruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug, ProjectUID: string(resource.UID),
		Desired: desired, Current: record.Binding.ClientKeys, Promote: promote,
		Authorize: clientkeyruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
	if reflect.DeepEqual(record.Binding.ClientKeys, result.Bindings) {
		return result
	}
	next := record.Binding
	next.ClientKeys = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = clientkeyruntime.Status{State: clientkeyruntime.StateError, Reason: "ClientKeyStatePersistFailed", Err: err}
	}
	return result
}

var errSymbolSourceSecret = errors.New("symbol-source Secret input is unavailable")

func (r *SentryProjectReconciler) reconcileSymbolSources(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) symbolsourceruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return symbolsourceruntime.Result{Bindings: map[string]binding.SymbolSourceBinding{}, Statuses: map[string]symbolsourceruntime.Status{"": {State: symbolsourceruntime.StateError, Reason: "SymbolSourceStateUnavailable", Err: err}}}
	}
	if resource.Spec.SymbolSources == nil {
		return symbolsourceruntime.Result{Bindings: record.Binding.SymbolSources, Statuses: map[string]symbolsourceruntime.Status{}}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return symbolsourceruntime.Result{Bindings: record.Binding.SymbolSources, Statuses: map[string]symbolsourceruntime.Status{"": {State: symbolsourceruntime.StateError, Reason: "InvalidSymbolSource", Err: symbolsourceruntime.ErrInvalidInput}}}
	}
	desired, err := r.loadSymbolSourceDesired(ctx, resource)
	if err != nil {
		return symbolsourceruntime.Result{Bindings: record.Binding.SymbolSources, Statuses: map[string]symbolsourceruntime.Status{"": {State: symbolsourceruntime.StateError, Reason: "SymbolSourceSecretUnavailable", Err: err}}}
	}
	adapter := symbolsourceruntime.Adapter(&symbolsourceruntime.HTTPAdapter{Client: apiClient})
	if r.NewSymbolSourceAdapter != nil {
		adapter = r.NewSymbolSourceAdapter(apiClient)
	}
	result := (&symbolsourceruntime.Engine{Adapter: adapter}).Reconcile(ctx, symbolsourceruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug, ProjectUID: string(resource.UID),
		Desired: desired, Current: record.Binding.SymbolSources,
		Authorize: r.dsnAuthorizer(resource, connection, record.Binding),
	})
	if reflect.DeepEqual(record.Binding.SymbolSources, result.Bindings) {
		return result
	}
	next := record.Binding
	next.SymbolSources = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = symbolsourceruntime.Status{State: symbolsourceruntime.StateError, Reason: "SymbolSourceStatePersistFailed", Err: err}
	}
	return result
}

func (r *SentryProjectReconciler) reconcileCodeMappings(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) codemappingruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return codemappingruntime.Result{Bindings: map[string]binding.CodeMappingBinding{}, Statuses: map[string]codemappingruntime.Status{"": {State: codemappingruntime.StateError, Reason: "CodeMappingStateUnavailable", Err: err}}}
	}
	if resource.Spec.CodeMappings == nil {
		return codemappingruntime.Result{Bindings: record.Binding.CodeMappings, Statuses: map[string]codemappingruntime.Status{}}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return codemappingruntime.Result{Bindings: record.Binding.CodeMappings, Statuses: map[string]codemappingruntime.Status{"": {State: codemappingruntime.StateError, Reason: "InvalidCodeMapping", Err: codemappingruntime.ErrInvalidInput}}}
	}
	desired, err := codeMappingDesired(*resource.Spec.CodeMappings, connection.Spec.Integrations)
	if err != nil {
		return codemappingruntime.Result{Bindings: record.Binding.CodeMappings, Statuses: map[string]codemappingruntime.Status{"": {State: codemappingruntime.StateError, Reason: "SourceCodeAliasUnresolved", Err: err}}}
	}
	adapter := codemappingruntime.Adapter(&codemappingruntime.HTTPAdapter{Client: apiClient})
	if r.NewCodeMappingAdapter != nil {
		adapter = r.NewCodeMappingAdapter(apiClient)
	}
	result := (&codemappingruntime.Engine{Adapter: adapter, Journal: r.Bindings}).Reconcile(ctx, codemappingruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug, ProjectIdentity: record.Binding.ExternalIdentity, ProjectUID: string(resource.UID),
		Desired: desired, Current: record.Binding.CodeMappings, Authorize: codemappingruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
	if reflect.DeepEqual(record.Binding.CodeMappings, result.Bindings) {
		return result
	}
	next := record.Binding
	next.CodeMappings = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = codemappingruntime.Status{State: codemappingruntime.StateError, Reason: "CodeMappingStatePersistFailed", Err: err}
	}
	return result
}

func codeMappingDesired(specs map[string]sentryconfigurationv1alpha1.CodeMappingSpec, integrations *sentryconfigurationv1alpha1.ConnectionIntegrations) (map[string]codemappingruntime.Desired, error) {
	if len(specs) > 0 && (integrations == nil || integrations.SourceCode == nil) {
		return nil, codemappingruntime.ErrCatalogMissing
	}
	result := make(map[string]codemappingruntime.Desired, len(specs))
	for name, spec := range specs {
		var installation *sentryconfigurationv1alpha1.SourceCodeIntegrationCatalogEntry
		for index := range integrations.SourceCode.Installations {
			candidate := &integrations.SourceCode.Installations[index]
			if candidate.Name == spec.Integration {
				if installation != nil {
					return nil, codemappingruntime.ErrCatalogMissing
				}
				installation = candidate
			}
		}
		if installation == nil {
			return nil, codemappingruntime.ErrCatalogMissing
		}
		repositoryName := ""
		for _, repository := range installation.Repositories {
			if repository.Name == spec.Repository {
				if repositoryName != "" {
					return nil, codemappingruntime.ErrCatalogMissing
				}
				repositoryName = repository.SentryName
			}
		}
		if repositoryName == "" {
			return nil, codemappingruntime.ErrCatalogMissing
		}
		result[name] = codemappingruntime.Desired{IntegrationAlias: spec.Integration, Provider: string(installation.Provider), IntegrationName: installation.SentryName, RepositoryAlias: spec.Repository, RepositoryName: repositoryName, StackRoot: spec.StackRoot, SourceRoot: spec.SourceRoot, DefaultBranch: spec.DefaultBranch, ImportCodeowners: spec.ImportCodeowners}
	}
	return result, nil
}

func (r *SentryProjectReconciler) loadSymbolSourceDesired(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (map[string]symbolsourceruntime.Desired, error) {
	result := make(map[string]symbolsourceruntime.Desired, len(*resource.Spec.SymbolSources))
	for name, spec := range *resource.Spec.SymbolSources {
		desired := symbolsourceruntime.Desired{Layout: symbolsourceruntime.Layout{Type: string(spec.Layout.Type), Casing: string(spec.Layout.Casing)}}
		types := 0
		if spec.HTTP != nil {
			types++
			desired.Type = "http"
			desired.URL = spec.HTTP.URL
			desired.Username = spec.HTTP.Username
			if spec.HTTP.PasswordSecretRef != nil {
				value, err := r.loadProjectSecretKey(ctx, resource.Namespace, *spec.HTTP.PasswordSecretRef)
				if err != nil {
					return nil, err
				}
				desired.Password = value
			}
		}
		if spec.S3 != nil {
			types++
			desired.Type = "s3"
			desired.Bucket = spec.S3.Bucket
			desired.Region = spec.S3.Region
			desired.Prefix = spec.S3.Prefix
			accessKey, err := r.loadProjectSecretKey(ctx, resource.Namespace, spec.S3.AccessKeySecretRef)
			if err != nil {
				return nil, err
			}
			secretKey, err := r.loadProjectSecretKey(ctx, resource.Namespace, spec.S3.SecretKeySecretRef)
			if err != nil {
				return nil, err
			}
			desired.AccessKey = accessKey
			desired.SecretKey = secretKey
		}
		if spec.GCS != nil {
			types++
			desired.Type = "gcs"
			desired.Bucket = spec.GCS.Bucket
			desired.ClientEmail = spec.GCS.ClientEmail
			desired.Prefix = spec.GCS.Prefix
			privateKey, err := r.loadProjectSecretKey(ctx, resource.Namespace, spec.GCS.PrivateKeySecretRef)
			if err != nil {
				return nil, err
			}
			desired.PrivateKey = privateKey
		}
		if types != 1 {
			return nil, symbolsourceruntime.ErrInvalidInput
		}
		result[name] = desired
	}
	return result, nil
}

func (r *SentryProjectReconciler) loadProjectSecretKey(ctx context.Context, namespace string, ref sentryconfigurationv1alpha1.SecretKeyReference) (string, error) {
	if namespace == "" || ref.Name == "" || ref.Key == "" || strings.ContainsAny(ref.Name+ref.Key, "\r\n\x00") {
		return "", errSymbolSourceSecret
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return "", errSymbolSourceSecret
	}
	value, found := secret.Data[ref.Key]
	if !found || len(value) == 0 {
		return "", errSymbolSourceSecret
	}
	return string(value), nil
}

func (r *SentryProjectReconciler) reconcileDSN(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, clientKeys clientkeyruntime.Result) (dsnReconcileResult, error) {
	current := dsnReconcileResult{Secret: resource.Status.DSNSecret, ClientKey: resource.Status.DSNClientKey}
	if projectResult.AccessMode == binding.AccessReadOnly && resource.Spec.DSN == nil {
		return dsnReconcileResult{}, nil
	}
	decision, err := policy.EvaluateProject(resource.Namespace, resource.Spec, connection.Spec)
	if err != nil {
		return current, err
	}
	if projectResult.AccessMode == binding.AccessReadOnly && !decision.ConsumeDSN {
		return current, dsn.ErrAuthorization
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return current, err
	}
	current.Secret = existingSecret(record.Binding.DSN)
	selector := ""
	desiredSecret := ""
	if resource.Spec.DSN != nil {
		selector = resource.Spec.DSN.ClientKey
		desiredSecret = resource.Spec.DSN.Secret
	}
	allowInsecureHTTP := connection.Spec.Transport != nil && connection.Spec.Transport.AllowInsecureHTTP
	var selection dsn.Selection
	managedSelector := ""
	if resource.Spec.DSN != nil {
		managedSelector = resource.Spec.DSN.ManagedClientKey
	}
	if managedSelector != "" {
		status, found := clientKeys.Statuses[managedSelector]
		if !found || status.Err != nil {
			return current, clientKeys.Err()
		}
		managed, found := record.Binding.ClientKeys[managedSelector]
		if !found {
			return current, clientkeyruntime.ErrExternalMissing
		}
		identity := managed.ExternalIdentity
		if managed.ReplacementIdentity != "" {
			identity = managed.ReplacementIdentity
		}
		publicDSN := clientKeys.PublicDSNs[managedSelector]
		key := dsn.ClientKey{ID: identity, Name: managedSelector, Label: managedSelector, IsActive: true}
		key.DSN.Public = publicDSN
		selection, err = dsn.SelectManaged(key, managedSelector, record.Binding.DSN, allowInsecureHTTP)
	} else {
		listKeys := r.ListKeys
		if listKeys == nil {
			listKeys = func(ctx context.Context, client *sentryapi.Client, organization, projectSlug string) ([]dsn.ClientKey, error) {
				return (&dsn.KeyReader{Client: client}).List(ctx, organization, projectSlug)
			}
		}
		var keys []dsn.ClientKey
		keys, err = listKeys(ctx, apiClient, connection.Spec.Organization, record.Binding.Slug)
		if err == nil {
			selection, err = dsn.SelectWithTransport(keys, selector, record.Binding.DSN, allowInsecureHTTP)
		}
	}
	if err != nil {
		if errors.Is(err, dsn.ErrPinnedKeyLost) && record.Binding.DSN != nil {
			expected := *record.Binding.DSN
			if revokeErr := (&dsn.Publisher{Client: r.outputSecrets()}).Revoke(ctx, resource, expected, r.dsnAuthorizer(resource, connection, record.Binding)); revokeErr != nil {
				return current, revokeErr
			}
			next := record.Binding
			nextDSN := expected
			nextDSN.CurrentSecret = ""
			nextDSN.PendingSecret = ""
			next.DSN = &nextDSN
			if _, updateErr := r.Bindings.UpdateBinding(ctx, record, next); updateErr != nil {
				return current, updateErr
			}
			return dsnReconcileResult{}, dsn.ErrPinnedKeyLost
		}
		return current, err
	}
	nextDSN := selection.Binding
	if record.Binding.DSN != nil {
		nextDSN.CurrentSecret = record.Binding.DSN.CurrentSecret
		nextDSN.PendingSecret = record.Binding.DSN.PendingSecret
	}
	authorize := r.dsnAuthorizer(resource, connection, record.Binding)
	nextDSN, phase, err := (&dsn.Publisher{Client: r.outputSecrets(), AllowInsecureHTTP: allowInsecureHTTP}).Reconcile(ctx, resource, decision.Owner, desiredSecret, selection.Key.DSN.Public, nextDSN, authorize)
	if err != nil {
		return current, err
	}
	nextBinding := record.Binding
	nextBinding.DSN = &nextDSN
	if _, err := r.Bindings.UpdateBinding(ctx, record, nextBinding); err != nil {
		return current, err
	}
	return dsnReconcileResult{
		Secret: nextDSN.CurrentSecret, ClientKey: selection.ReadableName, Phase: phase,
		ExternalNameChanged: selection.ExternalRenamed,
	}, nil
}

func (r *SentryProjectReconciler) dsnAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding) dsn.Authorization {
	return func(ctx context.Context) error {
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latest); err != nil {
			return err
		}
		deletionCleanup := !original.DeletionTimestamp.IsZero()
		if latest.UID != original.UID || latest.Generation != original.Generation || latest.DeletionTimestamp.IsZero() != original.DeletionTimestamp.IsZero() || (!deletionCleanup && latest.Spec.Paused) {
			return dsn.ErrAuthorization
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		decision, err := policy.EvaluateProject(latest.Namespace, latest.Spec, latestConnection.Spec)
		if err != nil {
			return err
		}
		state, err := r.Bindings.GetBinding(ctx, string(latest.UID))
		if err != nil || state.Binding.ExternalIdentity != expected.ExternalIdentity || state.Binding.Connection != connection.Name || state.Binding.AccessMode != expected.AccessMode {
			return dsn.ErrAuthorization
		}
		if err := r.validateRegionalRevision(ctx, latestConnection, state.Binding); err != nil {
			return err
		}
		if latest.Spec.Project.Use != nil {
			if latest.Spec.DSN == nil || decision.AccessMode != binding.AccessReadOnly || !decision.ConsumeDSN {
				return dsn.ErrAuthorization
			}
			return nil
		}
		if latest.Spec.Role == sentryconfigurationv1alpha1.ProjectRoleStandby && state.Binding.AccessMode == binding.AccessReadOnly && decision.AccessMode == binding.AccessManage {
			return r.validateStandbyObservation(ctx, latestConnection, state.Binding)
		}
		claim, err := r.Bindings.GetClaim(ctx, expected.Installation, expected.Organization, expected.Slug)
		if err != nil || decision.AccessMode != binding.AccessManage || claim.Claim.Manager.UID != string(latest.UID) || claim.Claim.ExternalIdentity != expected.ExternalIdentity || claim.Claim.Handoff != nil {
			return dsn.ErrAuthorization
		}
		return nil
	}
}

func (r *SentryProjectReconciler) reconcileSettings(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) settingsruntime.Result {
	if resource.Spec.Settings == nil {
		return settingsruntime.Result{}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return failedSettingsResult(resource.Spec.Settings, settingsruntime.ErrInvalidDesiredState)
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return failedSettingsResult(resource.Spec.Settings, err)
	}
	factory := r.NewSettingsAdapter
	if factory == nil {
		factory = func(client *sentryapi.Client) settingsruntime.Adapter {
			return &settingsruntime.HTTPAdapter{Client: client}
		}
	}
	return (&settingsruntime.Engine{Adapter: factory(apiClient)}).Reconcile(ctx, settingsruntime.Input{
		Organization: connection.Spec.Organization,
		ProjectSlug:  record.Binding.Slug,
		Desired:      resource.Spec.Settings,
		Authorize:    settingsruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
}

func failedSettingsResult(desired *sentryconfigurationv1alpha1.ProjectSettings, err error) settingsruntime.Result {
	failed := func(managed bool) settingsruntime.Component {
		if !managed {
			return settingsruntime.Component{}
		}
		return settingsruntime.Component{Managed: true, Err: err}
	}
	return settingsruntime.Result{
		Filters:       failed(desired.Filters != nil),
		PII:           failed(desired.PII != nil),
		Behavior:      failed(desired.Behavior != nil),
		Grouping:      failed(desired.Grouping != nil),
		Symbolication: failed(desired.Symbolication != nil),
	}
}

func (r *SentryProjectReconciler) reconcileIssueOwnership(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, projectAccessErr error) issueownershipruntime.Result {
	if resource.Spec.IssueOwnership == nil {
		return issueownershipruntime.Result{}
	}
	if projectResult.AccessMode != binding.AccessManage || resource.Spec.Project.Create == nil {
		return issueownershipruntime.Result{Managed: true, Err: issueownershipruntime.ErrInvalidDesiredState}
	}
	if projectAccessErr != nil {
		return issueownershipruntime.Result{Managed: true, Err: errors.Join(issueownershipruntime.ErrAuthorization, projectAccessErr)}
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return issueownershipruntime.Result{Managed: true, Err: err}
	}
	allowedTeams, policyErr := policy.AuthorizedIssueOwnershipTeams(resource.Namespace, resource.Spec, connection.Spec)
	if policyErr != nil {
		return issueownershipruntime.Result{Managed: true, Err: errors.Join(issueownershipruntime.ErrAuthorization, policyErr)}
	}
	adapter := issueownershipruntime.Adapter(&issueownershipruntime.HTTPAdapter{Client: apiClient})
	if r.NewIssueOwnershipAdapter != nil {
		adapter = r.NewIssueOwnershipAdapter(apiClient)
	}
	return (&issueownershipruntime.Engine{Adapter: adapter}).Reconcile(ctx, issueownershipruntime.Input{
		Organization: connection.Spec.Organization,
		ProjectSlug:  record.Binding.Slug,
		Desired:      resource.Spec.IssueOwnership,
		AllowedTeams: allowedTeams,
		Authorize:    issueownershipruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
}

func (r *SentryProjectReconciler) reconcileDestinations(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, application alertpolicyruntime.Result) destination.Result {
	if !canManageChildren(resource, projectResult) {
		return destination.Result{}
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return destination.Result{PolicyErr: err}
	}
	if resource.Spec.NotificationTargets == nil && len(record.Binding.Destinations) == 0 {
		return destination.Result{}
	}
	var catalog destination.Catalog
	if r.NewDestinationCatalog != nil {
		catalog = r.NewDestinationCatalog(apiClient)
	} else {
		catalog = &destination.HTTPCatalog{Client: apiClient}
	}
	catalog = &destination.AliasCatalog{Catalog: catalog, Integrations: connection.Spec.Integrations}
	result := (&destination.Engine{Catalog: catalog}).Reconcile(ctx, destination.Input{
		Namespace:        resource.Namespace,
		Project:          resource.Spec,
		Connection:       connection.Spec,
		Current:          record.Binding.Destinations,
		Owners:           application.TargetOwners,
		AllowedOwnerUIDs: allowedChildOwners(resource, record.Binding, application),
	})
	if reflect.DeepEqual(record.Binding.Destinations, result.Bindings) && reflect.DeepEqual(record.Binding.AlertPolicy, appliedPolicyBinding(application)) {
		return result
	}
	if err := r.destinationAuthorizer(resource, connection, record.Binding, result)(ctx); err != nil {
		result.PolicyErr = err
		return result
	}
	if err := r.reconcileTelegramRoutes(ctx, resource, record.Binding.Destinations, &result, nil); err != nil {
		result.PolicyErr = err
		return result
	}
	next := record.Binding
	next.Destinations = result.Bindings
	next.AlertPolicy = appliedPolicyBinding(application)
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.PolicyErr = err
	}
	return result
}

func (r *SentryProjectReconciler) reconcileAlerts(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, destinationCleanup []string, application alertpolicyruntime.Result) alertsruntime.Result {
	if !canManageChildren(resource, projectResult) {
		if resource.Spec.Alerts == nil {
			return alertsruntime.Result{}
		}
		return alertsruntime.Result{Statuses: map[string]alertsruntime.Status{"": {State: alertsruntime.StateError, Reason: "InvalidAlertInput", Err: alertsruntime.ErrInvalidInput}}}
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return alertsruntime.Result{Statuses: map[string]alertsruntime.Status{"": {State: alertsruntime.StateError, Reason: "AlertStateUnavailable", Err: err}}}
	}
	if resource.Spec.Alerts == nil && len(record.Binding.Alerts) == 0 {
		return alertsruntime.Result{}
	}
	desiredCount := 0
	if resource.Spec.Alerts != nil {
		desiredCount = len(*resource.Spec.Alerts)
	}
	desired := make(map[string]alertsruntime.Desired, desiredCount)
	if resource.Spec.Alerts != nil {
		for name, alert := range *resource.Spec.Alerts {
			enabled := true
			if alert.Enabled != nil {
				enabled = *alert.Enabled
			}
			value := alertsruntime.Desired{Owner: application.AlertOwners[name], Trigger: alertsruntime.Trigger(alert.Trigger), Enabled: enabled, Notify: append([]string(nil), alert.Notify...)}
			if alert.WindowMinutes != nil {
				value.WindowMinutes = int(*alert.WindowMinutes)
			}
			if alert.Threshold != nil {
				value.Threshold = *alert.Threshold
			}
			desired[name] = value
		}
	}

	legacy := &alertsruntime.LegacyRouterAdapter{Legacy: &alertsruntime.LegacyHTTPAdapter{Client: apiClient, Destinations: record.Binding.Destinations}}
	modern := &alertsruntime.ModernHTTPAdapter{Client: apiClient, Destinations: record.Binding.Destinations}
	var adapter alertsruntime.Adapter = &alertsruntime.MuxAdapter{Adapters: map[string]alertsruntime.Adapter{
		alertsruntime.LegacyCombinedAdapter:    legacy,
		alertsruntime.ModernMetricIssueAdapter: modern,
	}}
	if r.NewAlertAdapter != nil {
		adapter = r.NewAlertAdapter(apiClient, record.Binding.Destinations)
	}
	capabilities := alertCapabilities(connection.Spec)
	marker := binding.Marker{}
	if record.Binding.Marker != nil {
		marker = *record.Binding.Marker
	}
	result := (&alertsruntime.Engine{Adapter: adapter, Journal: r.Bindings, Audit: r.Audit}).Reconcile(ctx, alertsruntime.Input{
		Organization:       connection.Spec.Organization,
		ProjectSlug:        record.Binding.Slug,
		Manage:             resource.Spec.Alerts != nil,
		Desired:            desired,
		Current:            record.Binding.Alerts,
		Tombstones:         record.Binding.Tombstones,
		Destinations:       record.Binding.Destinations,
		DestinationCleanup: destinationCleanup,
		PinnedAdapter: func() string {
			if record.Binding.PinnedAdapters == nil {
				return ""
			}
			return record.Binding.PinnedAdapters["alerts"]
		}(),
		Capabilities:     capabilities,
		Marker:           marker,
		Authorize:        r.alertAuthorizer(resource, connection, record.Binding),
		AllowedOwnerUIDs: allowedChildOwners(resource, record.Binding, application),
	})
	if resource.Spec.Alerts == nil {
		return result
	}
	next := record.Binding
	next.Alerts = result.Bindings
	next.Tombstones = result.Tombstones
	next.PinnedAdapters = cloneStringMap(record.Binding.PinnedAdapters)
	if result.PinnedAdapter != "" {
		next.PinnedAdapters["alerts"] = result.PinnedAdapter
	}
	next.AlertPolicy = appliedPolicyBinding(application)
	if reflect.DeepEqual(record.Binding.Alerts, next.Alerts) && reflect.DeepEqual(record.Binding.Tombstones, next.Tombstones) && reflect.DeepEqual(record.Binding.PinnedAdapters, next.PinnedAdapters) && reflect.DeepEqual(record.Binding.AlertPolicy, next.AlertPolicy) {
		return result
	}
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = alertsruntime.Status{State: alertsruntime.StateError, Reason: "AlertStatePersistFailed", Err: err}
	}
	return result
}

func canManageChildren(resource *sentryconfigurationv1alpha1.SentryProject, result projectruntime.Result) bool {
	return result.AccessMode == binding.AccessManage || (resource != nil && resource.Spec.Role == sentryconfigurationv1alpha1.ProjectRoleStandby && result.AccessMode == binding.AccessReadOnly && result.Origin == binding.OriginManaged)
}

func appliedPolicyBinding(application alertpolicyruntime.Result) *binding.AppliedAlertPolicy {
	if application.PolicyUID == "" {
		return nil
	}
	return &binding.AppliedAlertPolicy{Name: application.PolicyName, UID: application.PolicyUID, Generation: application.Generation}
}

func allowedChildOwners(resource *sentryconfigurationv1alpha1.SentryProject, current binding.ProjectBinding, application alertpolicyruntime.Result) map[string]bool {
	result := map[string]bool{string(resource.UID): true}
	if application.PolicyUID != "" {
		result[application.PolicyUID] = true
	}
	if current.AlertPolicy != nil {
		result[current.AlertPolicy.UID] = true
	}
	return result
}

func alertCapabilities(spec sentryconfigurationv1alpha1.SentryConnectionSpec) []alertsruntime.Capability {
	if spec.Compatibility == nil {
		return nil
	}
	switch spec.Compatibility.Alerts {
	case sentryconfigurationv1alpha1.AlertCompatibilityLegacy24:
		return []alertsruntime.Capability{{
			Name: alertsruntime.LegacyCombinedAdapter,
			Triggers: map[alertsruntime.Trigger]bool{
				alertsruntime.TriggerFirstSeen: true, alertsruntime.TriggerRegression: true, alertsruntime.TriggerEventCount: true, alertsruntime.TriggerUserCount: true,
			},
			SupportsUpdate: true, SupportsToggle: false,
		}}
	case sentryconfigurationv1alpha1.AlertCompatibilityModern26:
		return []alertsruntime.Capability{{
			Name: alertsruntime.ModernMetricIssueAdapter,
			Triggers: map[alertsruntime.Trigger]bool{
				alertsruntime.TriggerEventCount: true, alertsruntime.TriggerUserCount: true,
			},
			SupportsUpdate: true, SupportsToggle: true,
		}}
	default:
		return nil
	}
}

func (r *SentryProjectReconciler) alertAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding) alertsruntime.Authorization {
	return func(ctx context.Context) error {
		if err := r.dsnAuthorizer(original, connection, expected)(ctx); err != nil {
			return err
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		if !reflect.DeepEqual(latestConnection.Spec.Compatibility, connection.Spec.Compatibility) {
			return dsn.ErrAuthorization
		}
		latestProject := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latestProject); err != nil {
			return err
		}
		if !original.DeletionTimestamp.IsZero() {
			targets, flattenErr := original.Spec.NotificationTargets.Flatten()
			if latestProject.DeletionTimestamp.IsZero() || original.Spec.Alerts == nil || len(*original.Spec.Alerts) != 0 || flattenErr != nil || len(targets) != 0 {
				return dsn.ErrAuthorization
			}
		} else {
			effectiveLatest, _, _, err := r.applyAlertPolicy(ctx, latestProject, latestConnection)
			if err != nil || !reflect.DeepEqual(effectiveLatest.Spec.Alerts, original.Spec.Alerts) || !reflect.DeepEqual(effectiveLatest.Spec.NotificationTargets, original.Spec.NotificationTargets) {
				return dsn.ErrAuthorization
			}
		}
		state, err := r.Bindings.GetBinding(ctx, string(original.UID))
		if err != nil || !reflect.DeepEqual(state.Binding.Alerts, expected.Alerts) || !reflect.DeepEqual(state.Binding.Destinations, expected.Destinations) || !reflect.DeepEqual(state.Binding.PinnedAdapters, expected.PinnedAdapters) {
			return dsn.ErrAuthorization
		}
		if err := r.validateChildMutationOwnership(ctx, latestConnection, state.Binding); err != nil {
			return err
		}
		return nil
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input)+1)
	for key, value := range input {
		result[key] = value
	}
	return result
}

func destinationCleanupKeys(result destination.Result) []string {
	keys := make([]string, 0)
	for name, current := range result.Bindings {
		if current.State == destination.StateCleanupPending {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
}

// finalizeDestinationCleanup releases protected destination identities only after
// the alert engine has proven that no desired or still-bound Sentry alert refers
// to them. Telegram additionally requires a verified route revoke before the
// protected binding can be removed.
func (r *SentryProjectReconciler) finalizeDestinationCleanup(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, result *destination.Result, releasable []string) error {
	if result == nil || len(releasable) == 0 {
		return nil
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(releasable))
	for _, name := range releasable {
		current, found := result.Bindings[name]
		if !found || current.State != destination.StateCleanupPending {
			return destination.ErrInvalidDestination
		}
		allowed[name] = true
	}
	if err := r.destinationAuthorizer(resource, connection, record.Binding, *result)(ctx); err != nil {
		return err
	}
	if err := r.reconcileTelegramRoutes(ctx, resource, record.Binding.Destinations, result, allowed); err != nil {
		return err
	}

	nextDestinations := make(map[string]binding.DestinationBinding, len(result.Bindings))
	for name, current := range result.Bindings {
		nextDestinations[name] = current
	}
	for _, name := range releasable {
		current := nextDestinations[name]
		if current.Kind == "telegram" && (current.Telegram == nil || !current.Telegram.RouteRevoked) {
			return registration.ErrPublishPending
		}
		delete(nextDestinations, name)
	}
	next := record.Binding
	next.Destinations = nextDestinations
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		return err
	}
	for _, name := range releasable {
		delete(result.Bindings, name)
		delete(result.Statuses, name)
	}
	return nil
}

func (r *SentryProjectReconciler) reconcileTelegramRoutes(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, current map[string]binding.DestinationBinding, result *destination.Result, cleanupAllowed map[string]bool) error {
	if result == nil {
		return nil
	}
	for name, previous := range current {
		if previous.Kind != "telegram" || previous.Telegram == nil || previous.Telegram.Challenge == nil {
			continue
		}
		next, found := result.Bindings[name]
		if found && next.Telegram != nil && next.Telegram.Challenge != nil && next.Telegram.Challenge.CodeHash == previous.Telegram.Challenge.CodeHash && next.State == destination.StateRegistrationRequired {
			continue
		}
		if r.RegistrationIndexes == nil {
			return registration.ErrChallengeIndexInvalid
		}
		challenge := previous.Telegram.Challenge
		index := registration.ChallengeIndex{
			Version: binding.CurrentVersion, CodeHash: challenge.CodeHash, ProjectUID: challenge.Project.UID,
			Destination: challenge.Destination, ExpiresAt: challenge.ExpiresAt,
		}
		if err := r.RegistrationIndexes.DeleteExact(ctx, index); err != nil {
			return err
		}
		if found && next.Telegram != nil && next.Telegram.Challenge != nil && next.Telegram.Challenge.CodeHash == challenge.CodeHash {
			telegram := cloneTelegramBinding(next.Telegram)
			telegram.Challenge = nil
			next.Telegram = telegram
			result.Bindings[name] = next
		}
	}
	for name, next := range result.Bindings {
		if next.Kind != "telegram" || next.Telegram == nil {
			continue
		}
		telegram := cloneTelegramBinding(next.Telegram)
		switch next.State {
		case destination.StateActive:
			if telegram.RouteKey == "" || telegram.Target == nil || telegram.RouteRevoked || r.RegistrationRoutes == nil {
				return registration.ErrRouteIdentityLost
			}
			// Idempotent verification repairs the crash window where the route
			// mutation succeeded but the protected binding CAS did not.
			if err := r.RegistrationRoutes.PublishTelegramRoute(ctx, telegram.RouteKey, *telegram.Target); err != nil {
				return err
			}
		case destination.StateRevoked, destination.StateCleanupPending:
			if next.State == destination.StateCleanupPending && !cleanupAllowed[name] {
				continue
			}
			if telegram.RouteRevoked {
				continue
			}
			if telegram.RouteKey == "" {
				if telegram.Target != nil {
					return registration.ErrRouteIdentityLost
				}
				continue
			}
			if r.RegistrationRoutes == nil {
				return registration.ErrPublishPending
			}
			if telegram.Target == nil {
				return registration.ErrRouteIdentityLost
			}
			if observer, ok := r.RegistrationRoutes.(registration.RouteObserver); ok {
				observed, err := observer.GetTelegramRoute(ctx, telegram.RouteKey)
				if apierrors.IsNotFound(err) {
					if err := r.recordDestinationCleanupAudit(ctx, resource, name, audit.OutcomeRecovered, audit.ReasonResultRecovered); err != nil {
						return err
					}
					telegram.RouteRevoked = true
					next.Telegram = telegram
					result.Bindings[name] = next
					continue
				}
				if err != nil {
					return err
				}
				if observed != *telegram.Target {
					return registration.ErrRouteConflict
				}
			}
			if err := r.recordDestinationCleanupAudit(ctx, resource, name, audit.OutcomeStarted, audit.ReasonCleanupAuthorized); err != nil {
				return err
			}
			if err := r.RegistrationRoutes.RevokeTelegramRoute(ctx, telegram.RouteKey, *telegram.Target); err != nil {
				return errors.Join(err, r.recordDestinationCleanupAudit(ctx, resource, name, audit.OutcomeFailed, audit.ReasonApply))
			}
			if err := r.recordDestinationCleanupAudit(ctx, resource, name, audit.OutcomeSucceeded, audit.ReasonMutationVerified); err != nil {
				return err
			}
			telegram.RouteRevoked = true
			next.Telegram = telegram
			result.Bindings[name] = next
		case destination.StateRestorePending:
			if telegram.RouteKey == "" || telegram.Target == nil || !telegram.RouteRevoked || r.RegistrationRoutes == nil {
				return registration.ErrPublishPending
			}
			if err := r.RegistrationRoutes.PublishTelegramRoute(ctx, telegram.RouteKey, *telegram.Target); err != nil {
				return err
			}
			telegram.RouteRevoked = false
			next.Telegram = telegram
			next.State = destination.StateActive
			next.Reason = "TelegramReady"
			result.Bindings[name] = next
			result.Statuses[name] = destination.Status{Kind: "telegram", State: destination.StateActive, Reason: "TelegramReady"}
		}
	}
	return nil
}

var errDestinationAuditUnavailable = errors.New("destination cleanup audit is unavailable")

func (r *SentryProjectReconciler) recordDestinationCleanupAudit(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, destinationName string, outcome audit.Outcome, reason audit.Reason) error {
	if r == nil || r.DestinationAudit == nil || resource == nil || resource.Namespace == "" || resource.Name == "" || resource.Generation <= 0 || destinationName == "" {
		return errDestinationAuditUnavailable
	}
	event := audit.Event{
		Component: audit.ComponentDestinations, Action: audit.ActionDelete,
		Outcome: outcome, Reason: reason,
		LocalResource: resource.Namespace + "/" + resource.Name + "/" + destinationName,
		Revision:      resource.Generation,
	}
	if err := r.DestinationAudit.Record(ctx, event); err != nil {
		return errDestinationAuditUnavailable
	}
	return nil
}

func cloneTelegramBinding(input *binding.TelegramBinding) *binding.TelegramBinding {
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

func (r *SentryProjectReconciler) destinationAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding, result destination.Result) dsn.Authorization {
	return func(ctx context.Context) error {
		if err := r.dsnAuthorizer(original, connection, expected)(ctx); err != nil {
			return err
		}
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latest); err != nil {
			return err
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		if !original.DeletionTimestamp.IsZero() {
			if latest.DeletionTimestamp.IsZero() {
				return dsn.ErrAuthorization
			}
			latest = original
		} else {
			effectiveLatest, _, _, err := r.applyAlertPolicy(ctx, latest, latestConnection)
			if err != nil {
				return dsn.ErrAuthorization
			}
			latest = effectiveLatest
		}
		desiredTargets, flattenErr := latest.Spec.NotificationTargets.Flatten()
		if flattenErr != nil {
			return dsn.ErrAuthorization
		}
		for name, desired := range desiredTargets {
			_, policyErr := policy.EvaluateDestination(latest.Namespace, latest.Spec, latestConnection.Spec, desired)
			status, statusFound := result.Statuses[name]
			next, nextFound := result.Bindings[name]
			if policy.IsKind(policyErr, policy.ErrorDestinationAccessDenied) {
				if !statusFound || status.Reason != "DestinationAccessRevoked" || (nextFound && next.State != destination.StateRevoked) {
					return dsn.ErrAuthorization
				}
				continue
			}
			if policyErr != nil || (statusFound && status.Reason == "DestinationAccessRevoked") {
				return dsn.ErrAuthorization
			}
		}
		state, err := r.Bindings.GetBinding(ctx, string(latest.UID))
		if err != nil || state.Binding.ExternalIdentity != expected.ExternalIdentity || state.Binding.Connection != expected.Connection || !reflect.DeepEqual(state.Binding.Destinations, expected.Destinations) {
			return dsn.ErrAuthorization
		}
		if err := r.validateChildMutationOwnership(ctx, latestConnection, state.Binding); err != nil {
			return err
		}
		return nil
	}
}

func (r *SentryProjectReconciler) applyAlertPolicy(ctx context.Context, project *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection) (*sentryconfigurationv1alpha1.SentryProject, *sentryconfigurationv1alpha1.SentryAlertPolicy, alertpolicyruntime.Result, error) {
	effective := project.DeepCopy()
	if project.Spec.AlertPolicy == "" {
		merged, err := alertpolicyruntime.Merge(effective, nil)
		return effective, nil, merged, err
	}
	policyResource := &sentryconfigurationv1alpha1.SentryAlertPolicy{}
	if err := r.Get(ctx, client.ObjectKey{Name: project.Spec.AlertPolicy}, policyResource); err != nil {
		return nil, nil, alertpolicyruntime.Result{}, err
	}
	if !policyResource.DeletionTimestamp.IsZero() {
		merged, err := alertpolicyruntime.Merge(effective, nil)
		if effective.Spec.Alerts == nil {
			emptyAlerts := map[string]sentryconfigurationv1alpha1.AlertSpec{}
			effective.Spec.Alerts = &emptyAlerts
			merged.Alerts = &emptyAlerts
		}
		merged.PolicyDeleting = true
		merged.PolicyName = policyResource.Name
		return effective, nil, merged, err
	}
	merged, err := alertpolicyruntime.Merge(effective, policyResource)
	if err != nil {
		return nil, nil, alertpolicyruntime.Result{}, err
	}
	effective.Spec.NotificationTargets = merged.Targets
	effective.Spec.Alerts = merged.Alerts
	return effective, policyResource, merged, nil
}

func reasonForAlertPolicyError(err error) string {
	switch {
	case apierrors.IsNotFound(err):
		return "AlertPolicyNotFound"
	case errors.Is(err, alertpolicyruntime.ErrSentryMismatch):
		return "AlertPolicySentryMismatch"
	case errors.Is(err, alertpolicyruntime.ErrPolicyLocalConflict):
		return "PolicyLocalConflict"
	case errors.Is(err, alertpolicyruntime.ErrTargetNotFound):
		return "AlertPolicyTargetNotFound"
	default:
		return "AlertPolicyInvalid"
	}
}

func (r *SentryProjectReconciler) reportAlertPolicyFailure(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, reason string) (ctrl.Result, error) {
	base := resource.DeepCopy()
	resource.Status.ObservedGeneration = resource.Generation
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{Type: "AlertPolicyReady", Status: metav1.ConditionFalse, ObservedGeneration: resource.Generation, Reason: reason, Message: "The referenced alert policy could not be merged safely; no alert or notification target mutation was attempted."})
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: resource.Generation, Reason: reason, Message: "The referenced alert policy is not ready."})
	if err := r.Status().Patch(ctx, resource, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func safeDestinationStatuses(statuses map[string]destination.Status, existing map[string]sentryconfigurationv1alpha1.DestinationStatus) map[string]sentryconfigurationv1alpha1.DestinationStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.DestinationStatus, len(statuses))
	for name, status := range statuses {
		reason := status.Reason
		if status.Err != nil {
			reason = reasonForError(status.Err)
		}
		next := sentryconfigurationv1alpha1.DestinationStatus{Type: status.Kind, State: status.State, Reason: reason}
		if current, found := existing[name]; found && current.RegistrationCommit != nil {
			next.RegistrationCommit = current.RegistrationCommit.DeepCopy()
		}
		result[name] = next
	}
	return result
}

func safeAlertStatuses(statuses map[string]alertsruntime.Status) map[string]sentryconfigurationv1alpha1.AlertStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.AlertStatus, len(statuses))
	for name, status := range statuses {
		if name == "" {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.AlertStatus{State: status.State, Reason: status.Reason}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func safeClientKeyStatuses(statuses map[string]clientkeyruntime.Status) map[string]sentryconfigurationv1alpha1.ClientKeyStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.ClientKeyStatus, len(statuses))
	for name, status := range statuses {
		if name == "" {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.ClientKeyStatus{State: status.State, Reason: status.Reason, RotationRevision: status.RotationRevision}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func safeSymbolSourceStatuses(statuses map[string]symbolsourceruntime.Status) map[string]sentryconfigurationv1alpha1.SymbolSourceStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.SymbolSourceStatus, len(statuses))
	for name, status := range statuses {
		if name == "" || status.State == symbolsourceruntime.StateDeleted {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.SymbolSourceStatus{State: status.State, Reason: status.Reason}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func safeCodeMappingStatuses(statuses map[string]codemappingruntime.Status) map[string]sentryconfigurationv1alpha1.CodeMappingStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.CodeMappingStatus, len(statuses))
	for name, status := range statuses {
		if name == "" || status.State == codemappingruntime.StateDeleted {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.CodeMappingStatus{State: status.State, Reason: status.Reason}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func existingSecret(state *binding.DSNBinding) string {
	if state == nil {
		return ""
	}
	return state.CurrentSecret
}

// revokeDSNIfDenied removes only the Secrets named in protected state, and only
// after the current policy has been re-read and proven to deny project access.
func (r *SentryProjectReconciler) revokeDSNIfDenied(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (bool, error) {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if record.Binding.DSN == nil || record.Binding.Project.UID != string(resource.UID) || record.Binding.Project.Namespace != resource.Namespace || record.Binding.Project.Name != resource.Name {
		return false, nil
	}
	connection := &sentryconfigurationv1alpha1.SentryConnection{}
	if err := r.Get(ctx, client.ObjectKey{Name: record.Binding.Connection}, connection); err != nil {
		return false, err
	}
	if _, err := policy.EvaluateProject(resource.Namespace, resource.Spec, connection.Spec); !policy.IsKind(err, policy.ErrorAccessDenied) {
		return false, nil
	}
	expected := *record.Binding.DSN
	authorize := func(ctx context.Context) error {
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(resource), latest); err != nil {
			return err
		}
		if latest.UID != resource.UID || latest.Generation != resource.Generation || !latest.DeletionTimestamp.IsZero() || latest.Spec.Paused {
			return dsn.ErrAuthorization
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: record.Binding.Connection}, latestConnection); err != nil {
			return err
		}
		if _, err := policy.EvaluateProject(latest.Namespace, latest.Spec, latestConnection.Spec); !policy.IsKind(err, policy.ErrorAccessDenied) {
			return dsn.ErrAuthorization
		}
		latestRecord, err := r.Bindings.GetBinding(ctx, string(latest.UID))
		if err != nil || latestRecord.Binding.ExternalIdentity != record.Binding.ExternalIdentity || latestRecord.Binding.Connection != record.Binding.Connection || !sameDSNBinding(latestRecord.Binding.DSN, &expected) {
			return dsn.ErrAuthorization
		}
		return nil
	}
	if err := (&dsn.Publisher{Client: r.outputSecrets()}).Revoke(ctx, resource, expected, authorize); err != nil {
		return false, err
	}
	next := record.Binding
	next.DSN = nil
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		return false, err
	}
	return true, nil
}

func sameDSNBinding(left, right *binding.DSNBinding) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r *SentryProjectReconciler) revokeDestinationsIfDenied(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (destination.Result, bool, error) {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if errors.Is(err, binding.ErrNotFound) {
		return destination.Result{}, false, nil
	}
	if err != nil {
		return destination.Result{}, false, err
	}
	if len(record.Binding.Destinations) == 0 {
		return destination.Result{}, false, nil
	}
	connection := &sentryconfigurationv1alpha1.SentryConnection{}
	if err := r.Get(ctx, client.ObjectKey{Name: record.Binding.Connection}, connection); err != nil {
		return destination.Result{}, false, err
	}
	_, policyErr := policy.Evaluate(resource.Namespace, resource.Spec, connection.Spec)
	if !policy.IsKind(policyErr, policy.ErrorAccessDenied) && !policy.IsKind(policyErr, policy.ErrorDestinationAccessDenied) {
		return destination.Result{}, false, nil
	}
	result := (&destination.Engine{}).Reconcile(ctx, destination.Input{
		Namespace: resource.Namespace, Project: resource.Spec, Connection: connection.Spec, Current: record.Binding.Destinations,
	})
	if !result.RevokeOnly {
		return destination.Result{}, false, nil
	}
	expected := record.Binding.Destinations
	latest := &sentryconfigurationv1alpha1.SentryProject{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(resource), latest); err != nil {
		return destination.Result{}, false, err
	}
	latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
	if err := r.Get(ctx, client.ObjectKey{Name: record.Binding.Connection}, latestConnection); err != nil {
		return destination.Result{}, false, err
	}
	_, latestPolicyErr := policy.Evaluate(latest.Namespace, latest.Spec, latestConnection.Spec)
	if latest.UID != resource.UID || latest.Generation != resource.Generation || !latest.DeletionTimestamp.IsZero() || latest.Spec.Paused || (!policy.IsKind(latestPolicyErr, policy.ErrorAccessDenied) && !policy.IsKind(latestPolicyErr, policy.ErrorDestinationAccessDenied)) {
		return destination.Result{}, false, dsn.ErrAuthorization
	}
	latestRecord, err := r.Bindings.GetBinding(ctx, string(latest.UID))
	if err != nil || latestRecord.Binding.ExternalIdentity != record.Binding.ExternalIdentity || latestRecord.Binding.Connection != record.Binding.Connection || !reflect.DeepEqual(latestRecord.Binding.Destinations, expected) {
		return destination.Result{}, false, dsn.ErrAuthorization
	}
	if err := r.reconcileTelegramRoutes(ctx, latest, latestRecord.Binding.Destinations, &result, nil); err != nil {
		return destination.Result{}, false, err
	}
	next := latestRecord.Binding
	next.Destinations = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, latestRecord, next); err != nil {
		return destination.Result{}, false, err
	}
	return result, true, nil
}

func (r *SentryProjectReconciler) reportAccessRevoked(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, dsnRevoked bool, destinationResult destination.Result, readyReason string) (ctrl.Result, error) {
	base := resource.DeepCopy()
	resource.Status.ObservedGeneration = resource.Generation
	if dsnRevoked {
		resource.Status.DSNSecret = ""
		resource.Status.DSNClientKey = ""
		meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
			Type: "DSNReady", Status: metav1.ConditionFalse, ObservedGeneration: resource.Generation,
			Reason: "DSNAccessRevoked", Message: "The managed DSN Secret was removed after current policy denial was verified.",
		})
	}
	if len(destinationResult.Statuses) > 0 {
		resource.Status.NotificationTargets = safeDestinationStatuses(destinationResult.Statuses, resource.Status.NotificationTargets)
		meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
			Type: "NotificationTargetsReady", Status: metav1.ConditionFalse, ObservedGeneration: resource.Generation,
			Reason: "DestinationAccessRevoked", Message: "New destination operations are denied; protected cleanup tombstones were recorded.",
		})
	}
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: resource.Generation,
		Reason: readyReason, Message: "Current connection policy denies one or more previously authorized operations.",
	})
	if err := r.Status().Patch(ctx, resource, client.MergeFrom(base)); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else {
		emitConditionChangesObserved(r.Recorder, r.Metrics, controllerProject, resource, base.Status.Conditions, resource.Status.Conditions)
	}
	return ctrl.Result{RequeueAfter: jitterDuration(30 * time.Second)}, nil
}

func (r *SentryProjectReconciler) outputSecrets() client.Client {
	if r.OutputSecrets != nil {
		return r.OutputSecrets
	}
	return r.Client
}

func (r *SentryProjectReconciler) reconcileDelete(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (ctrl.Result, error) {
	connection, err := r.resolveConnection(ctx, resource)
	if err != nil {
		return r.reportFailure(ctx, resource, reasonForError(err), "DeleteProject is blocked because current authorization could not be proven.")
	}
	setProjectOwnershipStatus(resource, connection)
	apiClient, found := r.Registry.Get(connection.Name)
	if !found {
		return r.reportFailure(ctx, resource, "ConnectionNotReady", "Project release is blocked until the selected connection is ready.")
	}
	if resource.Spec.Lifecycle.OnDelete != sentryconfigurationv1alpha1.OnDeleteDeleteProject {
		if err := r.reconcileRetainCleanup(ctx, resource, connection, apiClient); err != nil {
			return r.reportFailure(ctx, resource, reasonForError(err), "Retain release is waiting for exact cleanup of operator-owned child resources.")
		}
	}
	result, err := r.engine(apiClient).Delete(ctx, projectruntime.Input{
		Project:        resource,
		ConnectionName: connection.Name,
		Connection:     connection.Spec,
	})
	if err != nil {
		return r.reportFailure(ctx, resource, reasonForError(err), "Project deletion or retention release is blocked until ownership can be verified.")
	}
	if !result.Complete {
		return ctrl.Result{RequeueAfter: jitterDuration(10 * time.Second)}, nil
	}
	return ctrl.Result{}, r.removeFinalizer(ctx, resource)
}

func (r *SentryProjectReconciler) reconcileRetainCleanup(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client) error {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if errors.Is(err, binding.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	managedStandby := record.Binding.AccessMode == binding.AccessReadOnly &&
		record.Binding.Origin == binding.OriginManaged &&
		resource.Spec.Role == sentryconfigurationv1alpha1.ProjectRoleStandby
	if record.Binding.AccessMode == binding.AccessReadOnly && !managedStandby {
		return nil
	}
	if err := r.removeServiceHook(ctx, resource, connection, apiClient, record); err != nil {
		return err
	}
	cleanup := resource.DeepCopy()
	emptyAlerts := map[string]sentryconfigurationv1alpha1.AlertSpec{}
	cleanup.Spec.Alerts = &emptyAlerts
	cleanup.Spec.AlertPolicy = ""
	cleanup.Spec.NotificationTargets = &sentryconfigurationv1alpha1.NotificationTargets{}
	projectResult := projectruntime.Result{Origin: record.Binding.Origin, AccessMode: record.Binding.AccessMode, Owner: record.Binding.Owner, ProjectURL: resource.Status.ProjectURL}
	var accessErr, clientKeyErr, symbolSourceErr error
	codeMappingResult := codemappingruntime.Result{Bindings: record.Binding.CodeMappings, Statuses: map[string]codemappingruntime.Status{}}
	if !managedStandby {
		cleanup.Spec.GrantAccess = &sentryconfigurationv1alpha1.ProjectGrantAccess{}
		emptyClientKeys := map[string]sentryconfigurationv1alpha1.ManagedClientKeySpec{}
		cleanup.Spec.ClientKeys = &emptyClientKeys
		emptySymbolSources := map[string]sentryconfigurationv1alpha1.CustomSymbolSourceSpec{}
		cleanup.Spec.SymbolSources = &emptySymbolSources
		emptyCodeMappings := map[string]sentryconfigurationv1alpha1.CodeMappingSpec{}
		if len(record.Binding.CodeMappings) > 0 {
			cleanup.Spec.CodeMappings = &emptyCodeMappings
		}
		accessErr = r.reconcileProjectAccess(ctx, cleanup, connection, apiClient, projectResult).Err
		clientKeyErr = r.reconcileClientKeys(ctx, cleanup, connection, apiClient, projectResult).Err()
		symbolSourceErr = r.reconcileSymbolSources(ctx, cleanup, connection, apiClient, projectResult).Err()
		if cleanup.Spec.CodeMappings != nil {
			codeMappingResult = r.reconcileCodeMappings(ctx, cleanup, connection, apiClient, projectResult)
		}
	}
	destinationResult := r.reconcileDestinations(ctx, cleanup, connection, apiClient, projectResult, alertpolicyruntime.Result{})
	alertResult := r.reconcileAlerts(ctx, cleanup, connection, apiClient, projectResult, destinationCleanupKeys(destinationResult), alertpolicyruntime.Result{})
	if err := r.finalizeDestinationCleanup(ctx, cleanup, connection, &destinationResult, alertResult.ReleasableDestinations); err != nil {
		destinationResult.PolicyErr = errors.Join(destinationResult.PolicyErr, err)
	}
	return errors.Join(accessErr, clientKeyErr, symbolSourceErr, codeMappingResult.Err(), destinationResult.Err(), alertResult.Err())
}

func (r *SentryProjectReconciler) reconcileServiceHook(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client) error {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return err
	}
	deliveries, err := telegramHookDeliveries(resource.Name, record.Binding)
	if err != nil {
		return err
	}
	if len(deliveries) == 0 {
		if record.Binding.ServiceHook == nil {
			return nil
		}
		return r.removeServiceHook(ctx, resource, connection, apiClient, record)
	}
	if connection.Spec.Integrations == nil || connection.Spec.Integrations.Telegram == nil || r.HookSources == nil || r.HookVerifier == nil {
		return sentryhook.ErrServiceHookLifecycle
	}
	return r.ensureServiceHook(ctx, resource, connection, apiClient, record, connection.Spec.Integrations.Telegram.ReceiverURL, deliveries)
}

func (r *SentryProjectReconciler) ensureServiceHook(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, record binding.BindingRecord, receiverBase string, deliveries []sentryhook.TelegramDelivery) error {
	adapter := sentryhook.ServiceHookAdapter(&sentryhook.HTTPServiceHookAdapter{Client: apiClient})
	if r.NewServiceHookAdapter != nil {
		adapter = r.NewServiceHookAdapter(apiClient)
	}
	_, err := (&sentryhook.LifecycleEngine{Bindings: r.Bindings, Adapter: adapter, Sources: r.HookSources, Verifier: r.HookVerifier}).Ensure(ctx, sentryhook.LifecycleInput{
		Binding: record, ConnectionUID: string(connection.UID), ReceiverBase: receiverBase,
		Organization: record.Binding.Organization, ProjectSlug: record.Binding.Slug, Revision: effectiveServiceHookRevision(connection.Spec.Integrations.Telegram.ServiceHookRevision), Deliveries: deliveries,
		Authorize: r.dsnAuthorizer(resource, connection, record.Binding),
	})
	return err
}

func effectiveServiceHookRevision(revision int64) int64 {
	if revision < 1 {
		return 1
	}
	return revision
}

func (r *SentryProjectReconciler) removeServiceHook(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, record binding.BindingRecord) error {
	if record.Binding.ServiceHook == nil {
		return nil
	}
	if r.HookSources == nil || r.HookVerifier == nil {
		return sentryhook.ErrServiceHookLifecycle
	}
	receiverBase := strings.TrimSuffix(record.Binding.ServiceHook.EndpointURL, "/"+record.Binding.ServiceHook.SourceBindingKey)
	adapter := sentryhook.ServiceHookAdapter(&sentryhook.HTTPServiceHookAdapter{Client: apiClient})
	if r.NewServiceHookAdapter != nil {
		adapter = r.NewServiceHookAdapter(apiClient)
	}
	_, err := (&sentryhook.LifecycleEngine{Bindings: r.Bindings, Adapter: adapter, Sources: r.HookSources, Verifier: r.HookVerifier}).Remove(ctx, sentryhook.LifecycleInput{
		Binding: record, ConnectionUID: string(connection.UID), ReceiverBase: receiverBase,
		Organization: record.Binding.Organization, ProjectSlug: record.Binding.Slug, Revision: record.Binding.ServiceHook.Revision,
		Authorize: r.dsnAuthorizer(resource, connection, record.Binding),
	})
	return err
}

func telegramHookDeliveries(projectName string, protected binding.ProjectBinding) ([]sentryhook.TelegramDelivery, error) {
	alertNames := make([]string, 0, len(protected.Alerts))
	for name := range protected.Alerts {
		alertNames = append(alertNames, name)
	}
	sort.Strings(alertNames)
	routes := map[string]bool{}
	result := make([]sentryhook.TelegramDelivery, 0)
	for _, alertName := range alertNames {
		alert := protected.Alerts[alertName]
		if !alert.Enabled {
			continue
		}
		for _, destinationName := range alert.Destinations {
			target, found := protected.Destinations[destinationName]
			if !found || target.Kind != "telegram" || target.State != destination.StateActive || target.Telegram == nil || target.Telegram.RouteKey == "" || target.Telegram.Target == nil || target.Telegram.RouteRevoked {
				continue
			}
			if routes[target.Telegram.RouteKey] {
				return nil, sentryhook.ErrServiceHookLifecycle
			}
			routes[target.Telegram.RouteKey] = true
			result = append(result, sentryhook.TelegramDelivery{RouteKey: target.Telegram.RouteKey, Render: sentryhook.RenderInput{ProjectName: projectName, AlertName: alertName, Severity: hookSeverity(alert.Trigger)}})
		}
	}
	return result, nil
}

func hookSeverity(trigger string) sentryhook.Severity {
	switch trigger {
	case string(sentryconfigurationv1alpha1.AlertTriggerEventCount), string(sentryconfigurationv1alpha1.AlertTriggerUserCount):
		return sentryhook.SeverityCritical
	case string(sentryconfigurationv1alpha1.AlertTriggerRegression):
		return sentryhook.SeverityWarning
	default:
		return sentryhook.SeverityInfo
	}
}

func (r *SentryProjectReconciler) engine(apiClient *sentryapi.Client) *projectruntime.Engine {
	return &projectruntime.Engine{Bindings: r.Bindings, Adapter: r.projectAdapter(apiClient), ManagedBy: r.ManagedBy, Audit: r.Audit}
}

func (r *SentryProjectReconciler) projectAdapter(apiClient *sentryapi.Client) projectruntime.Adapter {
	factory := r.NewAdapter
	if factory == nil {
		factory = func(client *sentryapi.Client) projectruntime.Adapter {
			return &projectruntime.HTTPAdapter{Client: client}
		}
	}
	return factory(apiClient)
}

func (r *SentryProjectReconciler) validateRegionalRevision(ctx context.Context, connection *sentryconfigurationv1alpha1.SentryConnection, protected binding.ProjectBinding) error {
	if protected.AccessMode != binding.AccessManage {
		return nil
	}
	return r.validateManagedRemoteOwnership(ctx, connection, protected)
}

func (r *SentryProjectReconciler) validateChildMutationOwnership(ctx context.Context, connection *sentryconfigurationv1alpha1.SentryConnection, protected binding.ProjectBinding) error {
	if protected.Origin != binding.OriginManaged || protected.Marker == nil {
		return dsn.ErrAuthorization
	}
	return r.validateManagedRemoteOwnership(ctx, connection, protected)
}

func (r *SentryProjectReconciler) validateStandbyObservation(ctx context.Context, connection *sentryconfigurationv1alpha1.SentryConnection, protected binding.ProjectBinding) error {
	apiClient, found := r.Registry.Get(connection.Name)
	if !found {
		return dsn.ErrAuthorization
	}
	remote, err := r.projectAdapter(apiClient).GetBySlug(ctx, protected.Organization, protected.Slug)
	if err != nil {
		return err
	}
	if remote.Identity != protected.ExternalIdentity {
		return dsn.ErrAuthorization
	}
	if protected.Origin == binding.OriginExternal {
		if connection.Spec.ProjectManagement.UnmanagedProjects != sentryconfigurationv1alpha1.UnmanagedProjectsStandby || remote.Marker != nil || remote.RegionalEvidence != nil {
			return dsn.ErrAuthorization
		}
		return nil
	}
	if protected.Origin != binding.OriginManaged || protected.Marker == nil {
		return dsn.ErrAuthorization
	}
	if protected.RegionalOwnership == nil {
		if remote.Marker == nil || !protected.Marker.Matches(*remote.Marker) {
			return dsn.ErrAuthorization
		}
		return nil
	}
	if remote.RegionalEvidence == nil || ownership.ValidateEvidence(ownership.Local{Marker: *protected.RegionalOwnership, Active: true}, *remote.RegionalEvidence) != nil {
		return dsn.ErrAuthorization
	}
	return nil
}

func (r *SentryProjectReconciler) validateManagedRemoteOwnership(ctx context.Context, connection *sentryconfigurationv1alpha1.SentryConnection, protected binding.ProjectBinding) error {
	configured := connection.Spec.Ownership
	pinned := protected.RegionalOwnership
	if protected.Marker == nil || (configured == nil) != (pinned == nil) {
		return dsn.ErrAuthorization
	}
	apiClient, found := r.Registry.Get(connection.Name)
	if !found {
		return dsn.ErrAuthorization
	}
	remote, err := r.projectAdapter(apiClient).GetBySlug(ctx, protected.Organization, protected.Slug)
	if err != nil {
		return err
	}
	if remote.Identity != protected.ExternalIdentity {
		return dsn.ErrAuthorization
	}
	if configured == nil {
		if remote.Marker == nil || !protected.Marker.Matches(*remote.Marker) {
			return dsn.ErrAuthorization
		}
		return nil
	}
	if configured.Role != sentryconfigurationv1alpha1.ConnectionRoleActive || pinned.Region != configured.Region || pinned.Revision != configured.Revision || pinned.Owner != protected.Owner || pinned.Installation != protected.Marker.ManagedBy || remote.RegionalEvidence == nil {
		return dsn.ErrAuthorization
	}
	if err := ownership.ValidateEvidence(ownership.Local{Marker: *pinned, Active: true}, *remote.RegionalEvidence); err != nil {
		return dsn.ErrAuthorization
	}
	return nil
}

func (r *SentryProjectReconciler) resolveConnection(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (*sentryconfigurationv1alpha1.SentryConnection, error) {
	connection := &sentryconfigurationv1alpha1.SentryConnection{}
	if err := r.Get(ctx, client.ObjectKey{Name: resource.Spec.Sentry}, connection); err != nil {
		return nil, err
	}
	if _, err := policy.EvaluateProject(resource.Namespace, resource.Spec, connection.Spec); err != nil {
		return nil, err
	}
	return connection, nil
}

func connectionMutationFence(connection *sentryconfigurationv1alpha1.SentryConnection) (string, string) {
	if connection == nil || connection.Spec.Ownership == nil {
		return "", ""
	}
	if connection.Spec.Ownership.Role != sentryconfigurationv1alpha1.ConnectionRoleActive {
		return "ConnectionStandby", "The selected connection is Standby; no external Sentry mutation is authorized."
	}
	return "", ""
}

func setProjectOwnershipStatus(project *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection) {
	requested := project.Spec.Role
	if requested == "" {
		requested = sentryconfigurationv1alpha1.ProjectRoleActive
	}
	project.Status.RequestedRole = string(requested)
	project.Status.EffectiveRole = string(sentryconfigurationv1alpha1.ProjectRoleStandby)
	if requested == sentryconfigurationv1alpha1.ProjectRoleActive && connection != nil && (connection.Spec.Ownership == nil || connection.Status.EffectiveRole == string(sentryconfigurationv1alpha1.ConnectionRoleActive)) {
		project.Status.EffectiveRole = string(sentryconfigurationv1alpha1.ProjectRoleActive)
	}
}

func (r *SentryProjectReconciler) reportFailure(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, reason, message string) (ctrl.Result, error) {
	base := resource.DeepCopy()
	resource.Status.ObservedGeneration = resource.Generation
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "ProjectReady",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: resource.Generation,
		Reason:             reason,
		Message:            message,
	})
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		ObservedGeneration: resource.Generation,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Patch(ctx, resource, client.MergeFrom(base)); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else {
		emitConditionChangesObserved(r.Recorder, r.Metrics, controllerProject, resource, base.Status.Conditions, resource.Status.Conditions)
	}
	return ctrl.Result{RequeueAfter: jitterDuration(30 * time.Second)}, nil
}

func (r *SentryProjectReconciler) removeFinalizer(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) error {
	base := resource.DeepCopy()
	resource.Finalizers = removeString(resource.Finalizers, sentryProjectFinalizer)
	return r.Patch(ctx, resource, client.MergeFrom(base))
}

func (r *SentryProjectReconciler) projectsForConnection(ctx context.Context, _ client.Object) []reconcile.Request {
	projects := &sentryconfigurationv1alpha1.SentryProjectList{}
	if err := r.List(ctx, projects); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(projects.Items))
	for _, project := range projects.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&project)})
	}
	return requests
}

func (r *SentryProjectReconciler) projectsForAlertPolicy(ctx context.Context, object client.Object) []reconcile.Request {
	projects := &sentryconfigurationv1alpha1.SentryProjectList{}
	if err := r.List(ctx, projects); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range projects.Items {
		if projects.Items[i].Spec.AlertPolicy == object.GetName() {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&projects.Items[i])})
		}
	}
	return requests
}

func (r *SentryProjectReconciler) projectsForSymbolSourceSecret(ctx context.Context, object client.Object) []reconcile.Request {
	secret, ok := object.(*corev1.Secret)
	if !ok || secret.Namespace == "" || secret.Name == "" {
		return nil
	}
	projects := &sentryconfigurationv1alpha1.SentryProjectList{}
	if err := r.List(ctx, projects, client.InNamespace(secret.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range projects.Items {
		if projectReferencesSymbolSourceSecret(&projects.Items[i], secret.Name) {
			requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&projects.Items[i])})
		}
	}
	return requests
}

func projectReferencesSymbolSourceSecret(project *sentryconfigurationv1alpha1.SentryProject, secretName string) bool {
	if project == nil || project.Spec.SymbolSources == nil || secretName == "" {
		return false
	}
	for _, source := range *project.Spec.SymbolSources {
		if source.HTTP != nil && source.HTTP.PasswordSecretRef != nil && source.HTTP.PasswordSecretRef.Name == secretName {
			return true
		}
		if source.S3 != nil && (source.S3.AccessKeySecretRef.Name == secretName || source.S3.SecretKeySecretRef.Name == secretName) {
			return true
		}
		if source.GCS != nil && source.GCS.PrivateKeySecretRef.Name == secretName {
			return true
		}
	}
	return false
}

func (r *SentryProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Registry == nil || r.Bindings == nil || r.ManagedBy == "" {
		return fmt.Errorf("project runtime is required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sentryconfigurationv1alpha1.SentryProject{}).
		Watches(&sentryconfigurationv1alpha1.SentryConnection{}, handler.EnqueueRequestsFromMapFunc(r.projectsForConnection)).
		Watches(&sentryconfigurationv1alpha1.SentryAlertPolicy{}, handler.EnqueueRequestsFromMapFunc(r.projectsForAlertPolicy)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.projectsForSymbolSourceSecret)).
		Named("sentryproject").
		Complete(r)
}

func reasonForError(err error) string {
	if errors.Is(err, errSymbolSourceSecret) {
		return "SymbolSourceSecretUnavailable"
	}
	if errors.Is(err, symbolsourceruntime.ErrInvalidInput) {
		return "SymbolSourceInvalid"
	}
	if errors.Is(err, symbolsourceruntime.ErrOwnershipLost) {
		return "SymbolSourceOwnershipLost"
	}
	if errors.Is(err, symbolsourceruntime.ErrExternalMissing) {
		return "SymbolSourceMissing"
	}
	if errors.Is(err, symbolsourceruntime.ErrVerification) {
		return "SymbolSourceVerificationFailed"
	}
	if errors.Is(err, clientkeyruntime.ErrInvalidInput) {
		return "ClientKeyInvalid"
	}
	if errors.Is(err, clientkeyruntime.ErrOwnershipLost) {
		return "ClientKeyOwnershipLost"
	}
	if errors.Is(err, clientkeyruntime.ErrExternalMissing) {
		return "ClientKeyMissing"
	}
	if errors.Is(err, clientkeyruntime.ErrRecoveryPending) {
		return "ClientKeyRecoveryPending"
	}
	if errors.Is(err, clientkeyruntime.ErrRotationRollback) {
		return "ClientKeyRotationRollback"
	}
	if errors.Is(err, clientkeyruntime.ErrVerification) {
		return "ClientKeyVerificationFailed"
	}
	if errors.Is(err, dsn.ErrKeyAmbiguous) {
		return "DSNKeyAmbiguous"
	}
	if errors.Is(err, dsn.ErrKeyNotFound) {
		return "DSNKeyNotFound"
	}
	if errors.Is(err, dsn.ErrInvalidKeyName) {
		return "DSNKeyNameInvalid"
	}
	if errors.Is(err, dsn.ErrPinnedKeyLost) {
		return "DSNKeyRevoked"
	}
	if errors.Is(err, dsn.ErrSecretConflict) {
		return "DSNSecretConflict"
	}
	if errors.Is(err, dsn.ErrAuthorization) {
		return "AuthorizationChanged"
	}
	if errors.Is(err, settingsruntime.ErrInvalidDesiredState) {
		return "SettingsInvalid"
	}
	if errors.Is(err, settingsruntime.ErrUnsupportedFilter) {
		return "SettingsUnsupported"
	}
	if errors.Is(err, issueownershipruntime.ErrInvalidDesiredState) {
		return "IssueOwnershipInvalid"
	}
	if errors.Is(err, issueownershipruntime.ErrAuthorization) {
		return "AuthorizationChanged"
	}
	if errors.Is(err, issueownershipruntime.ErrVerification) || errors.Is(err, issueownershipruntime.ErrInvalidResponse) {
		return "IssueOwnershipVerificationFailed"
	}
	if errors.Is(err, projectaccessruntime.ErrAuthorization) {
		return "AuthorizationChanged"
	}
	if errors.Is(err, projectaccessruntime.ErrIdentityChange) {
		return "TeamIdentityChanged"
	}
	if errors.Is(err, projectaccessruntime.ErrInvalidInput) {
		return "GrantAccessInvalid"
	}
	if errors.Is(err, projectaccessruntime.ErrVerification) {
		return "GrantAccessVerificationFailed"
	}
	if errors.Is(err, destination.ErrExternalObjectReplaced) {
		return "ExternalObjectReplaced"
	}
	if errors.Is(err, destination.ErrExternalNameChanged) {
		return "ExternalNameChanged"
	}
	if errors.Is(err, destination.ErrInvalidDestination) {
		return "DestinationUnsupported"
	}
	if errors.Is(err, destination.ErrCatalogUnsupported) {
		return "DestinationUnsupported"
	}
	var projectErr *projectruntime.Error
	if errors.As(err, &projectErr) {
		return string(projectErr.Reason)
	}
	var policyErr *policy.Error
	if errors.As(err, &policyErr) {
		return string(policyErr.Kind)
	}
	var resolveErr *resolver.Error
	if errors.As(err, &resolveErr) {
		return string(resolveErr.Kind)
	}
	var apiErr *sentryapi.APIError
	if errors.As(err, &apiErr) {
		return string(apiErr.Kind)
	}
	if apierrors.IsNotFound(err) {
		return "ReferenceNotFound"
	}
	return "TemporaryFailure"
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func removeString(values []string, unwanted string) []string {
	result := values[:0]
	for _, value := range values {
		if value != unwanted {
			result = append(result, value)
		}
	}
	return result
}
