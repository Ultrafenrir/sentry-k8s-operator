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
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/alerts"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/destination"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/validation"
)

const (
	sentryAlertPolicyFinalizer   = "sentryconfiguration.io/alert-policy-protection"
	policyRolloutRequeueInterval = 2 * time.Second
)

// SentryAlertPolicyReconciler maintains bounded aggregate policy status. Policy changes
// enqueue projects; projects remain the only reconcilers that mutate Sentry children.
type SentryAlertPolicyReconciler struct {
	client.Client
	Bindings *binding.Store
	Recorder events.EventRecorder
	Metrics  *Metrics
}

// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryalertpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryalertpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects,verbs=get;list;watch

func (r *SentryAlertPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcileResult ctrl.Result, reconcileErr error) {
	started := time.Now()
	defer func() {
		r.Metrics.ObserveReconcile(controllerAlertPolicy, reconcileResult, reconcileErr, time.Since(started))
	}()
	policy := &sentryv1alpha1.SentryAlertPolicy{}
	if err := r.Get(ctx, req.NamespacedName, policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if policy.DeletionTimestamp.IsZero() {
		if !containsString(policy.Finalizers, sentryAlertPolicyFinalizer) {
			base := policy.DeepCopy()
			policy.Finalizers = append(policy.Finalizers, sentryAlertPolicyFinalizer)
			if err := r.Patch(ctx, policy, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}
	} else {
		if !containsString(policy.Finalizers, sentryAlertPolicyFinalizer) {
			return ctrl.Result{}, nil
		}
		if r.Bindings == nil {
			return ctrl.Result{}, binding.ErrInvalidRecord
		}
		hasChildren, err := r.Bindings.HasPolicyChildren(ctx, string(policy.UID))
		if err != nil {
			return ctrl.Result{}, err
		}
		if hasChildren {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		base := policy.DeepCopy()
		policy.Finalizers = removeString(policy.Finalizers, sentryAlertPolicyFinalizer)
		return ctrl.Result{}, r.Patch(ctx, policy, client.MergeFrom(base))
	}
	if err := validation.AlertPolicy(policy.Spec); err != nil {
		base := policy.DeepCopy()
		policy.Status.ObservedGeneration = policy.Generation
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: policy.Generation, Reason: "SpecInvalid", Message: "The SentryAlertPolicy specification is invalid: " + err.Error()})
		if statusErr := r.Status().Patch(ctx, policy, client.MergeFrom(base)); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		emitConditionChangesObserved(r.Recorder, r.Metrics, controllerAlertPolicy, policy, base.Status.Conditions, policy.Status.Conditions)
		return ctrl.Result{}, nil
	}
	base := policy.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	projects := &sentryv1alpha1.SentryProjectList{}
	if err := r.List(ctx, projects); err != nil {
		return ctrl.Result{}, err
	}
	var desired, ready, failed, pending int32
	for i := range projects.Items {
		project := &projects.Items[i]
		if !projectReferencesPolicy(project.Spec, policy.Name) {
			continue
		}
		desired++
		record, bindingErr := r.Bindings.GetBinding(ctx, string(project.UID))
		if bindingErr != nil {
			if bindingErr == binding.ErrNotFound {
				pending++
				continue
			}
			return ctrl.Result{}, bindingErr
		}
		if !bindingContainsPolicy(record.Binding, string(policy.UID), policy.Generation) {
			pending++
			continue
		}
		switch policyConsumerRolloutStatus(policy, project) {
		case metav1.ConditionUnknown:
			pending++
		case metav1.ConditionTrue:
			ready++
		case metav1.ConditionFalse:
			failed++
		}
	}
	policy.Status.Rollout = sentryv1alpha1.AlertPolicyRolloutStatus{DesiredProjects: desired, ReadyProjects: ready, FailedProjects: failed, PendingProjects: pending}
	connection := &sentryv1alpha1.SentryConnection{}
	status, reason, message := metav1.ConditionTrue, "PolicyValid", "The policy is valid and available for project opt-in."
	if err := r.Get(ctx, client.ObjectKey{Name: policy.Spec.Sentry}, connection); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		status, reason, message = metav1.ConditionFalse, "SentryNotFound", "The selected SentryConnection does not exist."
	}
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{Type: "Ready", Status: status, ObservedGeneration: policy.Generation, Reason: reason, Message: message})
	rolloutStatus, rolloutReason := metav1.ConditionTrue, "RolloutComplete"
	if pending > 0 || failed > 0 {
		rolloutStatus, rolloutReason = metav1.ConditionFalse, "RolloutIncomplete"
	}
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{Type: "RolloutComplete", Status: rolloutStatus, ObservedGeneration: policy.Generation, Reason: rolloutReason, Message: "Aggregate project rollout status is current."})
	if err := r.Status().Patch(ctx, policy, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	emitConditionChangesObserved(r.Recorder, r.Metrics, controllerAlertPolicy, policy, base.Status.Conditions, policy.Status.Conditions)
	return alertPolicyRolloutResult(pending), nil
}

func alertPolicyRolloutResult(pending int32) ctrl.Result {
	if pending > 0 {
		// A project can update its protected binding without changing its public status.
		// Recheck pending rollouts so the aggregate status observes that durable update.
		return ctrl.Result{RequeueAfter: policyRolloutRequeueInterval}
	}
	return ctrl.Result{}
}

func policyConsumerRolloutStatus(policy *sentryv1alpha1.SentryAlertPolicy, project *sentryv1alpha1.SentryProject) metav1.ConditionStatus {
	condition := meta.FindStatusCondition(project.Status.Conditions, "AlertPolicyReady")
	if condition == nil || condition.ObservedGeneration != project.Generation || condition.Status == metav1.ConditionUnknown {
		return metav1.ConditionUnknown
	}
	if condition.Status != metav1.ConditionTrue {
		return metav1.ConditionFalse
	}
	targets, err := policy.Spec.NotificationTargets.Flatten()
	if err != nil {
		return metav1.ConditionFalse
	}
	for key := range targets {
		status, found := project.Status.NotificationTargets[key]
		if !found {
			return metav1.ConditionUnknown
		}
		if status.State != destination.StateActive {
			return metav1.ConditionFalse
		}
	}
	for key := range policy.Spec.Alerts {
		status, found := project.Status.Alerts[key]
		if !found {
			return metav1.ConditionUnknown
		}
		if status.State != alerts.StateReady {
			return metav1.ConditionFalse
		}
	}
	return metav1.ConditionTrue
}

func (r *SentryAlertPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sentryv1alpha1.SentryAlertPolicy{}).
		Watches(&sentryv1alpha1.SentryProject{}, handler.EnqueueRequestsFromMapFunc(r.policyForProject)).
		Watches(&sentryv1alpha1.SentryConnection{}, handler.EnqueueRequestsFromMapFunc(r.policiesForConnection)).
		Named("sentryalertpolicy").
		Complete(r)
}

func (r *SentryAlertPolicyReconciler) policyForProject(_ context.Context, object client.Object) []reconcile.Request {
	project, ok := object.(*sentryv1alpha1.SentryProject)
	if !ok {
		return nil
	}
	names := project.Spec.AlertPolicies
	if len(names) == 0 && project.Spec.AlertPolicy != "" {
		names = []string{project.Spec.AlertPolicy}
	}
	requests := make([]reconcile.Request, 0, len(names))
	for _, name := range names {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: name}})
	}
	return requests
}

func projectReferencesPolicy(spec sentryv1alpha1.SentryProjectSpec, name string) bool {
	if spec.AlertPolicy == name {
		return true
	}
	return slices.Contains(spec.AlertPolicies, name)
}
func bindingContainsPolicy(state binding.ProjectBinding, uid string, generation int64) bool {
	if state.AlertPolicy != nil && state.AlertPolicy.UID == uid && state.AlertPolicy.Generation == generation {
		return true
	}
	for _, policy := range state.AlertPolicies {
		if policy.UID == uid && policy.Generation == generation {
			return true
		}
	}
	return false
}

func (r *SentryAlertPolicyReconciler) policiesForConnection(ctx context.Context, object client.Object) []reconcile.Request {
	policies := &sentryv1alpha1.SentryAlertPolicyList{}
	if err := r.List(ctx, policies); err != nil {
		return nil
	}
	result := make([]reconcile.Request, 0)
	for i := range policies.Items {
		if policies.Items[i].Spec.Sentry == object.GetName() {
			result = append(result, reconcile.Request{NamespacedName: client.ObjectKey{Name: policies.Items[i].Name}})
		}
	}
	return result
}
