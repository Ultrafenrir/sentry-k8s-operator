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
	"net/url"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/monitor"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/monitorstate"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/validation"
)

const sentryCronMonitorFinalizer = "sentryconfiguration.io/cron-monitor-protection"

type CronMonitorAdapter interface {
	ResolveTeam(context.Context, string, string) (string, bool, error)
	ListCron(context.Context, string, string) ([]monitor.CronRemote, error)
	CreateCron(context.Context, string, monitor.CronRequest) (monitor.CronRemote, error)
	GetCron(context.Context, string, string, string) (monitor.CronRemote, bool, error)
	UpdateCron(context.Context, string, string, string, monitor.CronRequest) (monitor.CronRemote, error)
	DeleteCron(context.Context, string, string, string) error
}

type SentryCronMonitorReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Registry        *connectionruntime.Registry
	ProjectBindings *binding.Store
	MonitorBindings *monitorstate.Store
	NewAdapter      func(*sentryapi.Client) CronMonitorAdapter
	Recorder        events.EventRecorder
	Audit           audit.Sink
}

// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentrycronmonitors,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentrycronmonitors/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects,verbs=get;list;watch

func (r *SentryCronMonitorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	resource := &sentryv1alpha1.SentryCronMonitor{}
	if err := r.Get(ctx, req.NamespacedName, resource); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !resource.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, resource)
	}
	if err := validation.CronMonitor(resource.Spec); err != nil {
		return r.report(ctx, resource, false, "SpecInvalid", "The Cron Monitor specification is invalid.", "")
	}
	if !controllerutil.ContainsFinalizer(resource, sentryCronMonitorFinalizer) {
		base := resource.DeepCopy()
		controllerutil.AddFinalizer(resource, sentryCronMonitorFinalizer)
		if err := r.Patch(ctx, resource, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	project, err := resolveMonitorProject(ctx, r.Client, r.Registry, r.ProjectBindings, resource.Namespace, resource.Spec.ProjectRef)
	if err != nil {
		return r.report(ctx, resource, false, monitorReason(err), "The referenced managed project is not ready.", "")
	}
	if err := r.ensureOwner(ctx, resource, project.Project); err != nil {
		return ctrl.Result{}, err
	}
	adapter := r.adapter(project.Client)
	request, err := r.cronRequest(ctx, adapter, project, resource)
	if err != nil {
		return r.report(ctx, resource, false, monitorReason(err), "The Cron Monitor owner could not be resolved.", "")
	}
	digest := desiredDigest(request)
	record, stateErr := r.MonitorBindings.Get(ctx, string(resource.UID))
	var remote monitor.CronRemote
	mutated := false
	action := audit.ActionUpdate
	if errors.Is(stateErr, monitorstate.ErrNotFound) {
		remote, err = r.recoverOrCreate(ctx, adapter, project, resource, request)
		if err == nil {
			mutated = true
			action = audit.ActionCreate
			_, err = r.MonitorBindings.Create(ctx, monitorstate.NewBinding("Cron", resource.Namespace, resource.Name, string(resource.UID), string(project.Project.UID), project.Connection.Name, project.Binding.Binding.Organization, project.Binding.Binding.Slug, remote.Identity, remote.Slug, digest))
		}
	} else if stateErr != nil {
		err = stateErr
	} else if record.Binding.ProjectUID != string(project.Project.UID) || record.Binding.Connection != project.Connection.Name || record.Binding.RemoteName != request.Slug {
		err = monitorstate.ErrInvalid
	} else {
		remote, _, err = adapter.GetCron(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity)
		if err == nil && !cronMatches(remote, request) {
			remote, err = adapter.UpdateCron(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity, request)
			mutated = err == nil
		}
		if err == nil {
			if mismatch := cronMismatch(remote, request); mismatch != "" {
				err = fmt.Errorf("cron monitor verification failed: %s", mismatch)
			}
		}
		if err == nil && record.Binding.DesiredHash != digest {
			next := record.Binding
			next.DesiredHash = digest
			_, err = r.MonitorBindings.Update(ctx, record, next)
		}
	}
	if err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "Cron Monitor reconciliation failed", "reason", monitorReason(err))
		return r.report(ctx, resource, false, monitorReason(err), "Cron Monitor reconciliation is safely blocked.", "")
	}
	if mutated {
		if err := recordMonitorAudit(ctx, r.Audit, action, resource.Namespace+"/"+resource.Name, resource.Generation); err != nil {
			return ctrl.Result{}, err
		}
	}
	externalURL := cronMonitorExternalURL(project.Connection.Spec.URL, project.Binding.Binding.Organization, project.Binding.Binding.Slug, remote.Slug, project.Connection.Spec.Compatibility)
	return r.report(ctx, resource, true, "MonitorReady", "The Cron Monitor matches the declared configuration.", externalURL)
}

func cronMonitorExternalURL(baseURL, organization, project, monitorSlug string, compatibility *sentryv1alpha1.ConnectionCompatibility) string {
	base := strings.TrimRight(baseURL, "/")
	organization = url.PathEscape(organization)
	project = url.PathEscape(project)
	monitorSlug = url.PathEscape(monitorSlug)
	if compatibility != nil && compatibility.Alerts == sentryv1alpha1.AlertCompatibilityLegacy24 {
		return fmt.Sprintf("%s/organizations/%s/crons/%s/%s/", base, organization, project, monitorSlug)
	}
	return fmt.Sprintf("%s/organizations/%s/issues/alerts/rules/crons/%s/%s/details/", base, organization, project, monitorSlug)
}

func (r *SentryCronMonitorReconciler) cronRequest(ctx context.Context, adapter CronMonitorAdapter, project monitorProject, resource *sentryv1alpha1.SentryCronMonitor) (monitor.CronRequest, error) {
	status := "disabled"
	if resource.Spec.Enabled {
		status = "active"
	}
	request := monitor.CronRequest{Project: project.Binding.Binding.Slug, Type: "cron_job", Name: resource.Spec.DisplayName, Slug: "k8s-" + monitorMarker(resource.UID), Status: status}
	if request.Name == "" {
		request.Name = resource.Name
	}
	if resource.Spec.Crontab != nil {
		zone := resource.Spec.Crontab.Timezone
		if zone == "" {
			zone = "UTC"
		}
		request.Config = monitor.CronConfig{ScheduleType: "crontab", Schedule: resource.Spec.Crontab.Expression, Timezone: zone}
	} else {
		request.Config = monitor.CronConfig{ScheduleType: "interval", Schedule: []any{resource.Spec.Interval.Value, string(resource.Spec.Interval.Unit)}}
	}
	request.Config.CheckInMargin = resource.Spec.CheckInMarginMinutes
	request.Config.MaxRuntime = resource.Spec.MaxRuntimeMinutes
	request.Config.FailureThreshold = resource.Spec.FailureThreshold
	request.Config.RecoveryThreshold = resource.Spec.RecoveryThreshold
	if resource.Spec.OwnerTeam != "" {
		id, found, err := adapter.ResolveTeam(ctx, project.Binding.Binding.Organization, resource.Spec.OwnerTeam)
		if err != nil {
			return request, err
		}
		if !found {
			return request, fmt.Errorf("owner team not found")
		}
		owner := "team:" + id
		request.Owner = &owner
	}
	return request, nil
}

func (r *SentryCronMonitorReconciler) recoverOrCreate(ctx context.Context, adapter CronMonitorAdapter, project monitorProject, resource *sentryv1alpha1.SentryCronMonitor, request monitor.CronRequest) (monitor.CronRemote, error) {
	items, err := adapter.ListCron(ctx, project.Binding.Binding.Organization, project.Binding.Binding.ExternalIdentity)
	if err != nil {
		return monitor.CronRemote{}, err
	}
	matches := []monitor.CronRemote{}
	for _, item := range items {
		if item.Slug == request.Slug {
			matches = append(matches, item)
		}
	}
	if len(matches) > 1 {
		return monitor.CronRemote{}, fmt.Errorf("ambiguous Cron Monitor ownership")
	}
	if len(matches) == 1 {
		if !cronMatches(matches[0], request) {
			return adapter.UpdateCron(ctx, project.Binding.Binding.Organization, project.Binding.Binding.Slug, matches[0].Identity, request)
		}
		return matches[0], nil
	}
	created, err := adapter.CreateCron(ctx, project.Binding.Binding.Organization, request)
	if err != nil {
		return monitor.CronRemote{}, err
	}
	if created.Identity == "" || created.Slug != request.Slug {
		return monitor.CronRemote{}, fmt.Errorf("Cron Monitor create verification failed")
	}
	return created, nil
}

func cronMatches(remote monitor.CronRemote, request monitor.CronRequest) bool {
	return cronMismatch(remote, request) == ""
}

func cronMismatch(remote monitor.CronRemote, request monitor.CronRequest) string {
	owner := ""
	if remote.Owner != nil {
		owner = "team:" + remote.Owner.ID
	}
	desiredOwner := ""
	if request.Owner != nil {
		desiredOwner = *request.Owner
	}
	checks := []struct {
		name  string
		match bool
	}{
		{"identity", remote.Identity != ""},
		{"name", remote.Name == request.Name},
		{"slug", remote.Slug == request.Slug},
		{"status", remote.Status == request.Status},
		{"project", remote.Project.Slug == request.Project},
		{"owner", owner == desiredOwner},
		{"schedule-type", remote.Config.ScheduleType == request.Config.ScheduleType},
		{"schedule", desiredDigest(remote.Config.Schedule) == desiredDigest(request.Config.Schedule)},
		{"check-in-margin", desiredDigest(remote.Config.CheckInMargin) == desiredDigest(request.Config.CheckInMargin)},
		{"max-runtime", desiredDigest(remote.Config.MaxRuntime) == desiredDigest(request.Config.MaxRuntime)},
		{"failure-threshold", desiredDigest(remote.Config.FailureThreshold) == desiredDigest(request.Config.FailureThreshold)},
		{"recovery-threshold", desiredDigest(remote.Config.RecoveryThreshold) == desiredDigest(request.Config.RecoveryThreshold)},
	}
	if request.Config.ScheduleType == "crontab" {
		checks = append(checks, struct {
			name  string
			match bool
		}{"timezone", remote.Config.Timezone == request.Config.Timezone})
	}
	for _, check := range checks {
		if !check.match {
			return check.name
		}
	}
	return ""
}

func (r *SentryCronMonitorReconciler) reconcileDelete(ctx context.Context, resource *sentryv1alpha1.SentryCronMonitor) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(resource, sentryCronMonitorFinalizer) {
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
		remote, found, err := adapter.GetCron(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity)
		if err != nil {
			return ctrl.Result{}, err
		}
		if found && !monitorDeletionPending(remote.Identity, record.Binding.ExternalIdentity, remote.Status) {
			// Sentry 24 can retain a hidden pending_deletion tombstone indefinitely.
			// A tombstone with the pinned ID is verified deletion evidence; only an
			// active remote must still pass the ownership check before DELETE.
			if remote.Slug != record.Binding.RemoteName {
				return ctrl.Result{}, monitorstate.ErrInvalid
			}
			if err := adapter.DeleteCron(ctx, record.Binding.Organization, record.Binding.ProjectSlug, record.Binding.ExternalIdentity); err != nil && !apierrors.IsNotFound(err) {
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

func (r *SentryCronMonitorReconciler) ensureOwner(ctx context.Context, resource *sentryv1alpha1.SentryCronMonitor, project *sentryv1alpha1.SentryProject) error {
	if metav1.IsControlledBy(resource, project) {
		return nil
	}
	base := resource.DeepCopy()
	if err := controllerutil.SetControllerReference(project, resource, r.Scheme); err != nil {
		return err
	}
	return r.Patch(ctx, resource, client.MergeFrom(base))
}

func (r *SentryCronMonitorReconciler) removeFinalizer(ctx context.Context, resource *sentryv1alpha1.SentryCronMonitor) error {
	base := resource.DeepCopy()
	controllerutil.RemoveFinalizer(resource, sentryCronMonitorFinalizer)
	return r.Patch(ctx, resource, client.MergeFrom(base))
}
func (r *SentryCronMonitorReconciler) adapter(client *sentryapi.Client) CronMonitorAdapter {
	if r.NewAdapter != nil {
		return r.NewAdapter(client)
	}
	return &monitor.HTTPAdapter{Client: client}
}
func (r *SentryCronMonitorReconciler) report(ctx context.Context, resource *sentryv1alpha1.SentryCronMonitor, ready bool, reason, message, externalURL string) (ctrl.Result, error) {
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
func (r *SentryCronMonitorReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&sentryv1alpha1.SentryCronMonitor{}).Named("sentrycronmonitor").Complete(r)
}
