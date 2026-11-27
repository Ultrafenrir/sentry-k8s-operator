/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/monitor"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/monitorstate"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/validation"
)

const sentryUptimeMonitorFinalizer = "sentryconfiguration.io/uptime-monitor-protection"

type UptimeMonitorAdapter interface {
	ResolveTeam(context.Context, string, string) (string, bool, error)
	ListUptime(context.Context, string, string) ([]monitor.UptimeRemote, error)
	PreflightUptime(context.Context, string, monitor.UptimeRequest) error
	CreateUptime(context.Context, string, string, monitor.UptimeRequest) (monitor.UptimeRemote, error)
	GetUptime(context.Context, string, string, string) (monitor.UptimeRemote, bool, error)
	UpdateUptime(context.Context, string, string, string, monitor.UptimeRequest) (monitor.UptimeRemote, error)
	DeleteUptime(context.Context, string, string, string) error
}

type SentryUptimeMonitorReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Registry        *connectionruntime.Registry
	ProjectBindings *binding.Store
	MonitorBindings *monitorstate.Store
	SecretClient    client.Client
	NewAdapter      func(*sentryapi.Client) UptimeMonitorAdapter
	Recorder        events.EventRecorder
	Audit           audit.Sink
	Metrics         *Metrics
}

// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryuptimemonitors,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryuptimemonitors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects,verbs=get;list;watch

func (r *SentryUptimeMonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcileResult ctrl.Result, reconcileErr error) {
	started := time.Now()
	resource := &sentryv1alpha1.SentryUptimeMonitor{}
	resourceFound := false
	defer func() {
		r.Metrics.ObserveReconcile(controllerUptime, reconcileResult, reconcileErr, time.Since(started))
		logUptimeMonitorReconcile(logf.FromContext(ctx), req, resource, resourceFound, reconcileResult, reconcileErr)
	}()
	if err := r.Get(ctx, req.NamespacedName, resource); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	resourceFound = true
	if !resource.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, resource)
	}
	if err := validation.UptimeMonitor(resource.Spec); err != nil {
		return r.report(ctx, resource, false, "SpecInvalid", "The Uptime Monitor specification is invalid.", "")
	}
	if !controllerutil.ContainsFinalizer(resource, sentryUptimeMonitorFinalizer) {
		base := resource.DeepCopy()
		controllerutil.AddFinalizer(resource, sentryUptimeMonitorFinalizer)
		if err := r.Patch(ctx, resource, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	project, err := resolveMonitorProject(ctx, r.Client, r.Registry, r.ProjectBindings, resource.Namespace, resource.Spec.ProjectRef)
	if err != nil {
		if errors.Is(err, errMonitorConnectionRuntimeInitializing) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return r.report(ctx, resource, false, monitorReason(err), monitorProjectFailureMessage(err), "")
	}
	if project.Connection.Spec.Compatibility == nil || project.Connection.Spec.Compatibility.Alerts != sentryv1alpha1.AlertCompatibilityModern26 {
		return r.report(ctx, resource, false, "SpecIncompatible", "Uptime Monitors require the modern-26.10.1 compatibility profile.", "")
	}
	if err := r.ensureOwner(ctx, resource, project.Project); err != nil {
		return ctrl.Result{}, err
	}
	adapter := r.adapter(project.Client)
	request, err := r.uptimeRequest(ctx, adapter, project, resource)
	if err != nil {
		return r.report(ctx, resource, false, monitorReason(err), "Uptime Monitor inputs are unavailable or invalid.", "")
	}
	digest := desiredDigest(request)
	record, stateErr := r.MonitorBindings.Get(ctx, string(resource.UID))
	var remote monitor.UptimeRemote
	mutated := false
	action := audit.ActionUpdate
	if errors.Is(stateErr, monitorstate.ErrNotFound) {
		remote, err = r.recoverOrCreate(ctx, adapter, project, resource, request)
		if err == nil {
			mutated = true
			action = audit.ActionCreate
			_, err = r.MonitorBindings.Create(ctx, monitorstate.NewBinding("Uptime", resource.Namespace, resource.Name, string(resource.UID), string(project.Project.UID), project.Connection.Name, project.Binding.Binding.Organization, project.Binding.Binding.Slug, remote.Identity, request.Name, digest))
		}
	} else if stateErr != nil {
		err = stateErr
	} else if record.Binding.ProjectUID != string(project.Project.UID) || record.Binding.Connection != project.Connection.Name {
		err = monitorstate.ErrInvalid
	} else {
		remote, _, err = adapter.GetUptime(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity)
		needsUpdate := record.Binding.DesiredHash != digest || record.Binding.RemoteName != request.Name
		if err == nil && needsUpdate {
			if err = adapter.PreflightUptime(ctx, record.Binding.Organization, request); err == nil {
				remote, err = adapter.UpdateUptime(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity, request)
				mutated = err == nil
			}
		}
		if err == nil {
			if mismatch := uptimeMismatch(remote, request); mismatch != "" {
				err = fmt.Errorf("uptime monitor verification failed: %s", mismatch)
			}
		}
		if err == nil && needsUpdate {
			next := record.Binding
			next.DesiredHash = digest
			next.RemoteName = request.Name
			_, err = r.MonitorBindings.Update(ctx, record, next)
		}
	}
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Uptime Monitor reconciliation failed", "reason", monitorReason(err))
		return r.report(ctx, resource, false, monitorReason(err), "Uptime Monitor reconciliation is safely blocked.", "")
	}
	if mutated {
		if err := recordMonitorAudit(ctx, r.Audit, action, resource.Namespace+"/"+resource.Name, resource.Generation); err != nil {
			return ctrl.Result{}, err
		}
	}
	externalURL := project.Connection.Spec.URL + "/organizations/" + project.Binding.Binding.Organization + "/alerts/rules/uptime/" + project.Binding.Binding.Slug + "/" + remote.Identity + "/details/"
	return r.report(ctx, resource, true, "MonitorReady", "The Uptime Monitor matches the declared configuration.", externalURL)
}

func (r *SentryUptimeMonitorReconciler) uptimeRequest(ctx context.Context, adapter UptimeMonitorAdapter, project monitorProject, resource *sentryv1alpha1.SentryUptimeMonitor) (monitor.UptimeRequest, error) {
	status := "disabled"
	if resource.Spec.Enabled {
		status = "active"
	}
	method := string(resource.Spec.Method)
	if method == "" {
		method = "GET"
	}
	request := monitor.UptimeRequest{Name: monitorRemoteName(resource.Spec.DisplayName, resource.Name, resource.UID), Status: status, URL: resource.Spec.URL, Method: method, IntervalSeconds: resource.Spec.IntervalSeconds, TimeoutMilliseconds: resource.Spec.TimeoutMilliseconds, TraceSampling: resource.Spec.TraceSampling, ResponseCaptureEnabled: resource.Spec.ResponseCaptureEnabled, RecoveryThreshold: resource.Spec.RecoveryThreshold, DowntimeThreshold: resource.Spec.FailureThreshold, Headers: make([][2]string, 0, len(resource.Spec.Headers))}
	if request.RecoveryThreshold == 0 {
		request.RecoveryThreshold = 1
	}
	if request.DowntimeThreshold == 0 {
		request.DowntimeThreshold = 3
	}
	if resource.Spec.Environment != "" {
		value := resource.Spec.Environment
		request.Environment = &value
	}
	if resource.Spec.OwnerTeam != "" {
		id, found, err := adapter.ResolveTeam(ctx, project.Binding.Binding.Organization, resource.Spec.OwnerTeam)
		if err != nil {
			return request, err
		}
		if !found {
			return request, fmt.Errorf("owner team not found")
		}
		value := "team:" + id
		request.Owner = &value
	}
	if len(resource.Spec.Headers) > 0 || resource.Spec.BodyFrom != nil {
		secret := &corev1.Secret{}
		secretClient := r.SecretClient
		if secretClient == nil {
			secretClient = r.Client
		}
		if err := secretClient.Get(ctx, types.NamespacedName{Namespace: resource.Namespace, Name: "sentry-uptime-inputs"}, secret); err != nil {
			return request, err
		}
		for _, header := range resource.Spec.Headers {
			value, found := secret.Data[header.ValueFrom.Key]
			if !found {
				return request, fmt.Errorf("header input is unavailable")
			}
			request.Headers = append(request.Headers, [2]string{header.Name, string(value)})
		}
		if resource.Spec.BodyFrom != nil {
			value, found := secret.Data[resource.Spec.BodyFrom.Key]
			if !found {
				return request, fmt.Errorf("body input is unavailable")
			}
			body := string(value)
			request.Body = &body
		}
	}
	request.Assertion = serializeAssertions(resource.Spec.Assertions)
	return request, nil
}

func serializeAssertions(assertions []sentryv1alpha1.UptimeAssertion) any {
	if len(assertions) == 0 {
		return nil
	}
	children := make([]any, 0, len(assertions))
	for index, assertion := range assertions {
		cmp := map[string]string{"equals": "equals", "notEquals": "not_equal", "lessThan": "less_than", "greaterThan": "greater_than"}[string(assertion.Comparison)]
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index+2)
		switch assertion.Type {
		case "statusCode":
			code, _ := strconv.Atoi(assertion.Value)
			children = append(children, map[string]any{"id": id, "op": "status_code_check", "operator": map[string]string{"cmp": cmp}, "value": code})
		case "jsonPath":
			children = append(children, map[string]any{"id": id, "op": "json_path", "value": assertion.Key, "operator": map[string]string{"cmp": cmp}, "operand": map[string]any{"jsonpath_op": "literal", "value": assertion.Value}})
		case "responseHeader":
			children = append(children, map[string]any{"id": id, "op": "header_check", "key_op": map[string]string{"cmp": "equals"}, "key_operand": map[string]any{"header_op": "literal", "value": assertion.Key}, "value_op": map[string]string{"cmp": cmp}, "value_operand": map[string]any{"header_op": "literal", "value": assertion.Value}})
		}
	}
	return map[string]any{"root": map[string]any{"id": "00000000-0000-4000-8000-000000000001", "op": "and", "children": children}}
}

func (r *SentryUptimeMonitorReconciler) recoverOrCreate(ctx context.Context, adapter UptimeMonitorAdapter, project monitorProject, resource *sentryv1alpha1.SentryUptimeMonitor, request monitor.UptimeRequest) (monitor.UptimeRemote, error) {
	items, err := adapter.ListUptime(ctx, project.Binding.Binding.Organization, project.Binding.Binding.Slug)
	if err != nil {
		return monitor.UptimeRemote{}, err
	}
	matches := []monitor.UptimeRemote{}
	for _, item := range items {
		if item.Name == request.Name && item.ProjectSlug == project.Binding.Binding.Slug {
			matches = append(matches, item)
		}
	}
	if len(matches) > 1 {
		return monitor.UptimeRemote{}, fmt.Errorf("ambiguous Uptime Monitor ownership")
	}
	if err := adapter.PreflightUptime(ctx, project.Binding.Binding.Organization, request); err != nil {
		return monitor.UptimeRemote{}, err
	}
	if len(matches) == 1 {
		if !uptimeMatches(matches[0], request) {
			return adapter.UpdateUptime(ctx, project.Binding.Binding.Organization, project.Binding.Binding.Slug, matches[0].Identity, request)
		}
		return matches[0], nil
	}
	created, err := adapter.CreateUptime(ctx, project.Binding.Binding.Organization, project.Binding.Binding.Slug, request)
	if err != nil {
		return monitor.UptimeRemote{}, err
	}
	if created.Identity == "" || !uptimeMatches(created, request) {
		return monitor.UptimeRemote{}, fmt.Errorf("Uptime Monitor create verification failed")
	}
	return created, nil
}

func uptimeMatches(remote monitor.UptimeRemote, request monitor.UptimeRequest) bool {
	return uptimeMismatch(remote, request) == ""
}

func uptimeMismatch(remote monitor.UptimeRemote, request monitor.UptimeRequest) string {
	if remote.Identity == "" {
		return "identity"
	}
	owner := ""
	if remote.Owner != nil {
		owner = "team:" + remote.Owner.ID
	}
	desiredOwner := ""
	if request.Owner != nil {
		desiredOwner = *request.Owner
	}
	if owner != desiredOwner {
		return "owner"
	}
	checks := []struct {
		name  string
		match bool
	}{
		{"name", remote.Name == request.Name},
		{"status", remote.Status == request.Status},
		{"environment", desiredDigest(remote.Environment) == desiredDigest(request.Environment)},
		{"url", remote.URL == request.URL},
		{"interval", remote.IntervalSeconds == request.IntervalSeconds},
		{"timeout", remote.TimeoutMilliseconds == request.TimeoutMilliseconds},
		{"method", remote.Method == request.Method},
		{"headers", desiredDigest(remote.Headers) == desiredDigest(request.Headers)},
		{"body", desiredDigest(remote.Body) == desiredDigest(request.Body)},
		{"trace-sampling", remote.TraceSampling == request.TraceSampling},
		{"response-capture", remote.ResponseCaptureEnabled == request.ResponseCaptureEnabled},
		{"recovery-threshold", remote.RecoveryThreshold == request.RecoveryThreshold},
		{"downtime-threshold", remote.DowntimeThreshold == request.DowntimeThreshold},
		{"assertion", desiredDigest(remote.Assertion) == desiredDigest(request.Assertion)},
	}
	for _, check := range checks {
		if !check.match {
			return check.name
		}
	}
	return ""
}

func (r *SentryUptimeMonitorReconciler) reconcileDelete(ctx context.Context, resource *sentryv1alpha1.SentryUptimeMonitor) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(resource, sentryUptimeMonitorFinalizer) {
		return ctrl.Result{}, nil
	}
	record, err := r.MonitorBindings.Get(ctx, string(resource.UID))
	if errors.Is(err, monitorstate.ErrNotFound) {
		return ctrl.Result{}, r.removeFinalizer(ctx, resource)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if resource.Spec.OnDelete != sentryv1alpha1.MonitorRetain {
		apiClient, found := r.Registry.Get(record.Binding.Connection)
		if !found {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		adapter := r.adapter(apiClient)
		remote, found, err := adapter.GetUptime(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity)
		if err != nil {
			return ctrl.Result{}, err
		}
		if found && !monitorDeletionPending(remote.Identity, record.Binding.ExternalIdentity, remote.Status) {
			if remote.Name != record.Binding.RemoteName {
				return ctrl.Result{}, monitorstate.ErrInvalid
			}
			if err := adapter.DeleteUptime(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			if err := recordMonitorAudit(ctx, r.Audit, audit.ActionDelete, resource.Namespace+"/"+resource.Name, resource.Generation); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}
	if err := r.MonitorBindings.Delete(ctx, record); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, r.removeFinalizer(ctx, resource)
}
func (r *SentryUptimeMonitorReconciler) ensureOwner(ctx context.Context, resource *sentryv1alpha1.SentryUptimeMonitor, project *sentryv1alpha1.SentryProject) error {
	if metav1.IsControlledBy(resource, project) {
		return nil
	}
	base := resource.DeepCopy()
	if err := controllerutil.SetControllerReference(project, resource, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, resource, client.MergeFrom(base))
}
func (r *SentryUptimeMonitorReconciler) removeFinalizer(ctx context.Context, resource *sentryv1alpha1.SentryUptimeMonitor) error {
	base := resource.DeepCopy()
	controllerutil.RemoveFinalizer(resource, sentryUptimeMonitorFinalizer)
	return r.Patch(ctx, resource, client.MergeFrom(base))
}
func (r *SentryUptimeMonitorReconciler) adapter(client *sentryapi.Client) UptimeMonitorAdapter {
	if r.NewAdapter != nil {
		return r.NewAdapter(client)
	}
	return &monitor.HTTPAdapter{Client: client}
}
func (r *SentryUptimeMonitorReconciler) report(ctx context.Context, resource *sentryv1alpha1.SentryUptimeMonitor, ready bool, reason, message, externalURL string) (ctrl.Result, error) {
	base := resource.DeepCopy()
	resource.Status.ObservedGeneration = resource.Generation
	resource.Status.ExternalURL = externalURL
	meta.SetStatusCondition(&resource.Status.Conditions, readyCondition(resource.Generation, ready, reason, message))
	if err := r.Status().Patch(ctx, resource, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	emitConditionChanges(r.Recorder, resource, base.Status.Conditions, resource.Status.Conditions)
	if ready {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
func (r *SentryUptimeMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&sentryv1alpha1.SentryUptimeMonitor{}).Named("sentryuptimemonitor").Complete(r)
}
