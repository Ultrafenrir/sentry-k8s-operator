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
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryconfigurationv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	alertpolicyruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/alertpolicy"
	alertsruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/alerts"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	clientkeyruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/clientkey"
	codemappingruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/codemapping"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/destination"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/dsn"
	issueownershipruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/issueownership"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/ownership"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/policy"
	projectruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/project"
	projectaccessruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/projectaccess"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/resolver"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
	settingsruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/settings"
	symbolsourceruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/symbolsource"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/validation"
)

const (
	sentryProjectFinalizer = "sentryconfiguration.io/project-protection"
	projectConnectionIndex = "spec.sentry"
	projectPolicyIndex     = "spec.alertPolicy"
)

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
	Recorder                 events.EventRecorder
	Metrics                  *Metrics
	Audit                    audit.Sink
}

const projectDriftReconcileInterval = 30 * time.Second

// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentrycronmonitors;sentryuptimemonitors,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

func (r *SentryProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcileResult ctrl.Result, reconcileErr error) {
	started := time.Now()
	resource := &sentryconfigurationv1alpha1.SentryProject{}
	resourceFound := false
	defer func() {
		r.Metrics.ObserveReconcile(controllerProject, reconcileResult, reconcileErr, time.Since(started))
		logProjectReconcile(logf.FromContext(ctx), req, resource, resourceFound, reconcileResult, reconcileErr)
	}()
	if err := r.Get(ctx, req.NamespacedName, resource); err != nil {
		if apierrors.IsNotFound(err) {
			r.Metrics.ForgetProjectSynchronization(req.String())
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	resourceFound = true
	if r.Registry == nil || r.Bindings == nil || r.ManagedBy == "" {
		return ctrl.Result{}, fmt.Errorf("project runtime is not configured")
	}
	if !resource.DeletionTimestamp.IsZero() {
		if !containsString(resource.Finalizers, sentryProjectFinalizer) {
			return ctrl.Result{}, nil
		}
		return r.reconcileDelete(ctx, resource)
	}
	if err := validation.Project(resource.Spec); err != nil {
		_, statusErr := r.reportFailure(ctx, resource, "SpecInvalid", "The SentryProject specification is invalid: "+err.Error())
		return ctrl.Result{}, statusErr
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
		return r.reportFailure(ctx, resource, reasonForError(err), "The selected SentryConnection could not be resolved.")
	}
	profile := sentryconfigurationv1alpha1.AlertCompatibilityProfile("")
	if connection.Spec.Compatibility != nil {
		profile = connection.Spec.Compatibility.Alerts
	}
	if err := validation.ProjectCompatibility(resource.Spec, profile); err != nil {
		return r.reportFailure(ctx, resource, "SpecIncompatible", "The SentryProject specification is incompatible with the selected connection: "+err.Error())
	}
	statusBase := resource.DeepCopy()
	setProjectOwnershipStatus(resource, connection)
	apiClient, found := r.Registry.Get(connection.Name)
	if !found {
		// On manager startup the project and connection caches can enqueue in either
		// order. A previously verified connection may therefore be visible before its
		// in-memory API client has been rebuilt. Preserve the last verified project
		// status and retry quickly instead of publishing a false outage during every
		// controller restart.
		if meta.IsStatusConditionTrue(connection.Status.Conditions, "Ready") {
			return ctrl.Result{RequeueAfter: jitterDuration(time.Second)}, nil
		}
		return r.reportFailure(ctx, resource, "ConnectionNotReady", "The selected SentryConnection has not loaded its credentials yet.")
	}
	effectiveResource, appliedPolicy, application, err := r.applyAlertPolicy(ctx, resource, connection)
	if err != nil {
		return r.reportAlertPolicyFailure(ctx, resource, reasonForAlertPolicyError(err))
	}
	if err := validation.ProjectCompatibility(effectiveResource.Spec, profile); err != nil {
		return r.reportFailure(ctx, resource, "AlertPolicyIncompatible", "The effective alert policy is incompatible with the selected connection: "+err.Error())
	}
	result, err := r.engine(apiClient).Reconcile(ctx, projectruntime.Input{
		Project:        resource,
		ConnectionName: connection.Name,
		Connection:     connection.Spec,
	})
	if err != nil {
		return r.reportFailure(ctx, resource, reasonForError(err), "Project reconciliation is safely blocked; no unverified mutation was performed.")
	}
	if result.AccessMode == binding.AccessManage && (connection.Spec.Ownership == nil || connection.Spec.Ownership.Role == sentryconfigurationv1alpha1.ConnectionRoleActive) {
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
	alertErr := alertResult.Err()
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
	dsnApplicable := result.AccessMode == binding.AccessManage || resource.Spec.ExistingClientKey != nil
	dsnStatus, dsnReason, dsnMessage := dsnCondition(dsnApplicable, dsnResult.ExternalNameChanged, dsnErr)
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

func (r *SentryProjectReconciler) reconcileDelete(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (ctrl.Result, error) {
	pendingMonitors, err := r.deleteDependentMonitors(ctx, resource)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pendingMonitors {
		return ctrl.Result{RequeueAfter: jitterDuration(time.Second)}, nil
	}
	connection, err := r.resolveConnection(ctx, resource)
	if err != nil {
		return r.reportFailure(ctx, resource, reasonForError(err), "Project release is blocked because the selected connection is unavailable.")
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
	if err := r.revokeDSNForDeletion(ctx, resource, connection); err != nil {
		return r.reportFailure(ctx, resource, reasonForError(err), "Project release is waiting for exact removal of its DSN output key.")
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

func (r *SentryProjectReconciler) deleteDependentMonitors(ctx context.Context, project *sentryconfigurationv1alpha1.SentryProject) (bool, error) {
	pending := false
	cronMonitors := &sentryconfigurationv1alpha1.SentryCronMonitorList{}
	if err := r.List(ctx, cronMonitors, client.InNamespace(project.Namespace)); err != nil {
		return false, err
	}
	for i := range cronMonitors.Items {
		monitor := &cronMonitors.Items[i]
		if monitor.Spec.ProjectRef != project.Name {
			continue
		}
		pending = true
		if monitor.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, monitor); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	uptimeMonitors := &sentryconfigurationv1alpha1.SentryUptimeMonitorList{}
	if err := r.List(ctx, uptimeMonitors, client.InNamespace(project.Namespace)); err != nil {
		return false, err
	}
	for i := range uptimeMonitors.Items {
		monitor := &uptimeMonitors.Items[i]
		if monitor.Spec.ProjectRef != project.Name {
			continue
		}
		pending = true
		if monitor.DeletionTimestamp.IsZero() {
			if err := r.Delete(ctx, monitor); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
	}
	return pending, nil
}

func (r *SentryProjectReconciler) revokeDSNForDeletion(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection) error {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if errors.Is(err, binding.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Binding.DSN == nil {
		return nil
	}
	expected := *record.Binding.DSN
	authorize := func(ctx context.Context) error {
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(resource), latest); err != nil {
			return err
		}
		if latest.UID != resource.UID || latest.Generation != resource.Generation || latest.DeletionTimestamp.IsZero() {
			return dsn.ErrAuthorization
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		if _, err := policy.EvaluateProject(latest.Namespace, latest.Spec, latestConnection.Spec); err != nil {
			return dsn.ErrAuthorization
		}
		latestRecord, err := r.Bindings.GetBinding(ctx, string(latest.UID))
		if err != nil || latestRecord.Binding.ExternalIdentity != record.Binding.ExternalIdentity || latestRecord.Binding.Connection != record.Binding.Connection || !sameDSNBinding(latestRecord.Binding.DSN, &expected) {
			return dsn.ErrAuthorization
		}
		return nil
	}
	if err := (&dsn.Publisher{Client: r.outputSecrets()}).Revoke(ctx, resource, expected, authorize); err != nil {
		return err
	}
	next := record.Binding
	next.DSN = nil
	_, err = r.Bindings.UpdateBinding(ctx, record, next)
	return err
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
		record.Binding.Origin == binding.OriginManaged
	if record.Binding.AccessMode == binding.AccessReadOnly && !managedStandby {
		return nil
	}
	cleanup := resource.DeepCopy()
	emptyAlerts := map[string]sentryconfigurationv1alpha1.AlertSpec{}
	cleanup.Spec.Alerts = &emptyAlerts
	cleanup.Spec.AlertPolicy = ""
	cleanup.Spec.AlertPolicies = nil
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
	requested := sentryconfigurationv1alpha1.ProjectRoleActive
	if connection != nil && connection.Spec.Ownership != nil && connection.Spec.Ownership.Role == sentryconfigurationv1alpha1.ConnectionRoleStandby {
		requested = sentryconfigurationv1alpha1.ProjectRoleStandby
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

func (r *SentryProjectReconciler) projectsForConnection(ctx context.Context, object client.Object) []reconcile.Request {
	projects := &sentryconfigurationv1alpha1.SentryProjectList{}
	if err := r.List(ctx, projects, client.MatchingFields{projectConnectionIndex: object.GetName()}); err != nil {
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
	if err := r.List(ctx, projects, client.MatchingFields{projectPolicyIndex: object.GetName()}); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range projects.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&projects.Items[i])})
	}
	return requests
}

func (r *SentryProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Registry == nil || r.Bindings == nil || r.ManagedBy == "" {
		return fmt.Errorf("project runtime is required")
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &sentryconfigurationv1alpha1.SentryProject{}, projectConnectionIndex, func(object client.Object) []string {
		project := object.(*sentryconfigurationv1alpha1.SentryProject)
		return []string{project.Spec.Sentry}
	}); err != nil {
		return fmt.Errorf("index projects by connection: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &sentryconfigurationv1alpha1.SentryProject{}, projectPolicyIndex, func(object client.Object) []string {
		project := object.(*sentryconfigurationv1alpha1.SentryProject)
		if len(project.Spec.AlertPolicies) > 0 {
			return slices.Clone(project.Spec.AlertPolicies)
		}
		if project.Spec.AlertPolicy != "" {
			return []string{project.Spec.AlertPolicy}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("index projects by alert policy: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sentryconfigurationv1alpha1.SentryProject{}).
		Watches(&sentryconfigurationv1alpha1.SentryConnection{}, handler.EnqueueRequestsFromMapFunc(r.projectsForConnection)).
		Watches(&sentryconfigurationv1alpha1.SentryAlertPolicy{}, handler.EnqueueRequestsFromMapFunc(r.projectsForAlertPolicy)).
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
