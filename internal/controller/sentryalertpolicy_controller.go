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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

const sentryAlertPolicyFinalizer = "sentryconfiguration.io/alert-policy-protection"

// SentryAlertPolicyReconciler maintains bounded aggregate policy status. Policy changes
// enqueue projects; projects remain the only reconcilers that mutate Sentry children.
type SentryAlertPolicyReconciler struct {
	client.Client
	Bindings *binding.Store
}

// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryalertpolicies,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryalertpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryprojects,verbs=get;list;watch

func (r *SentryAlertPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	base := policy.DeepCopy()
	policy.Status.ObservedGeneration = policy.Generation
	projects := &sentryv1alpha1.SentryProjectList{}
	if err := r.List(ctx, projects); err != nil {
		return ctrl.Result{}, err
	}
	var desired, ready, failed, pending int32
	for i := range projects.Items {
		project := &projects.Items[i]
		if project.Spec.AlertPolicy != policy.Name {
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
		if record.Binding.AlertPolicy == nil || record.Binding.AlertPolicy.UID != string(policy.UID) || record.Binding.AlertPolicy.Generation != policy.Generation {
			pending++
			continue
		}
		condition := meta.FindStatusCondition(project.Status.Conditions, "AlertPolicyReady")
		switch {
		case condition == nil || condition.ObservedGeneration != project.Generation:
			pending++
		case condition.Status == metav1.ConditionTrue:
			ready++
		case condition.Status == metav1.ConditionFalse:
			failed++
		default:
			pending++
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
	return ctrl.Result{}, nil
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
	if !ok || project.Spec.AlertPolicy == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Name: project.Spec.AlertPolicy}}}
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
