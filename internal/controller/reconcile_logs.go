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
	"sort"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
)

func logConnectionReconcile(log logr.Logger, req ctrl.Request, connection *sentryv1alpha1.SentryConnection, resourceFound bool, registry *connectionruntime.Registry, result ctrl.Result, reconcileErr error) {
	clientLoaded := false
	if registry != nil && connection.Name != "" {
		_, clientLoaded = registry.Get(connection.Name)
	}
	ready := meta.FindStatusCondition(connection.Status.Conditions, "Ready")
	log.Info("SentryConnection reconciliation completed",
		"connection", req.Name,
		"resourceFound", resourceFound,
		"generation", connection.Generation,
		"observedGeneration", connection.Status.ObservedGeneration,
		"organization", connection.Spec.Organization,
		"remoteOrganizationDiscovered", meta.IsStatusConditionTrue(connection.Status.Conditions, "Ready"),
		"serverVersion", connection.Status.ServerVersion,
		"capabilities", connection.Status.Capabilities,
		"apiClientLoaded", clientLoaded,
		"ownershipConfigured", connection.Spec.Ownership != nil,
		"effectiveRole", connection.Status.EffectiveRole,
		"conditions", conditionLogSummary(connection.Status.Conditions),
		"readyReason", conditionReason(ready),
		"readyMessage", conditionMessage(ready),
		"requeue", result.Requeue,
		"requeueAfter", durationLogValue(result.RequeueAfter),
		"error", reconcileErr != nil,
	)
}

func logProjectReconcile(log logr.Logger, req ctrl.Request, project *sentryv1alpha1.SentryProject, resourceFound bool, result ctrl.Result, reconcileErr error) {
	ready := meta.FindStatusCondition(project.Status.Conditions, "Ready")
	log.Info("SentryProject reconciliation completed",
		"project", req.NamespacedName.String(),
		"resourceFound", resourceFound,
		"generation", project.Generation,
		"observedGeneration", project.Status.ObservedGeneration,
		"sentryConnection", project.Spec.Sentry,
		"deleting", !project.DeletionTimestamp.IsZero(),
		"paused", project.Spec.Paused,
		"remoteProjectDiscovered", meta.IsStatusConditionTrue(project.Status.Conditions, "ProjectReady"),
		"origin", project.Status.Origin,
		"accessMode", project.Status.AccessMode,
		"requestedRole", project.Status.RequestedRole,
		"effectiveRole", project.Status.EffectiveRole,
		"conditions", conditionLogSummary(project.Status.Conditions),
		"readyReason", conditionReason(ready),
		"readyMessage", conditionMessage(ready),
		"clientKeys", projectClientKeyStateCounts(project.Status.ClientKeys),
		"symbolSources", projectSymbolSourceStateCounts(project.Status.SymbolSources),
		"codeMappings", projectCodeMappingStateCounts(project.Status.CodeMappings),
		"notificationTargets", projectDestinationStateCounts(project.Status.NotificationTargets),
		"alerts", projectAlertStateCounts(project.Status.Alerts),
		"requeue", result.Requeue,
		"requeueAfter", durationLogValue(result.RequeueAfter),
		"error", reconcileErr != nil,
	)
}

func logAlertPolicyReconcile(log logr.Logger, req ctrl.Request, policy *sentryv1alpha1.SentryAlertPolicy, resourceFound bool, result ctrl.Result, reconcileErr error) {
	ready := meta.FindStatusCondition(policy.Status.Conditions, "Ready")
	rollout := meta.FindStatusCondition(policy.Status.Conditions, "RolloutComplete")
	log.Info("SentryAlertPolicy reconciliation completed",
		"policy", req.Name,
		"resourceFound", resourceFound,
		"generation", policy.Generation,
		"observedGeneration", policy.Status.ObservedGeneration,
		"sentryConnection", policy.Spec.Sentry,
		"deleting", !policy.DeletionTimestamp.IsZero(),
		"desiredProjects", policy.Status.Rollout.DesiredProjects,
		"readyProjects", policy.Status.Rollout.ReadyProjects,
		"failedProjects", policy.Status.Rollout.FailedProjects,
		"pendingProjects", policy.Status.Rollout.PendingProjects,
		"conditions", conditionLogSummary(policy.Status.Conditions),
		"readyReason", conditionReason(ready),
		"rolloutReason", conditionReason(rollout),
		"requeue", result.Requeue,
		"requeueAfter", durationLogValue(result.RequeueAfter),
		"error", reconcileErr != nil,
	)
}

func logCronMonitorReconcile(log logr.Logger, req ctrl.Request, monitor *sentryv1alpha1.SentryCronMonitor, resourceFound bool, result ctrl.Result, reconcileErr error) {
	ready := meta.FindStatusCondition(monitor.Status.Conditions, "Ready")
	log.Info("SentryCronMonitor reconciliation completed",
		"monitor", req.NamespacedName.String(),
		"resourceFound", resourceFound,
		"generation", monitor.Generation,
		"observedGeneration", monitor.Status.ObservedGeneration,
		"projectRef", monitor.Spec.ProjectRef,
		"deleting", !monitor.DeletionTimestamp.IsZero(),
		"conditions", conditionLogSummary(monitor.Status.Conditions),
		"readyReason", conditionReason(ready),
		"readyMessage", conditionMessage(ready),
		"requeue", result.Requeue,
		"requeueAfter", durationLogValue(result.RequeueAfter),
		"error", reconcileErr != nil,
	)
}

func logUptimeMonitorReconcile(log logr.Logger, req ctrl.Request, monitor *sentryv1alpha1.SentryUptimeMonitor, resourceFound bool, result ctrl.Result, reconcileErr error) {
	ready := meta.FindStatusCondition(monitor.Status.Conditions, "Ready")
	log.Info("SentryUptimeMonitor reconciliation completed",
		"monitor", req.NamespacedName.String(),
		"resourceFound", resourceFound,
		"generation", monitor.Generation,
		"observedGeneration", monitor.Status.ObservedGeneration,
		"projectRef", monitor.Spec.ProjectRef,
		"deleting", !monitor.DeletionTimestamp.IsZero(),
		"conditions", conditionLogSummary(monitor.Status.Conditions),
		"readyReason", conditionReason(ready),
		"readyMessage", conditionMessage(ready),
		"requeue", result.Requeue,
		"requeueAfter", durationLogValue(result.RequeueAfter),
		"error", reconcileErr != nil,
	)
}

func conditionReason(condition *metav1.Condition) string {
	if condition == nil {
		return ""
	}
	return condition.Reason
}

func conditionMessage(condition *metav1.Condition) string {
	if condition == nil {
		return ""
	}
	return condition.Message
}

func conditionLogSummary(conditions []metav1.Condition) []string {
	result := make([]string, 0, len(conditions))
	for _, condition := range conditions {
		if suppressConditionEvent(condition) {
			continue
		}
		result = append(result, condition.Type+"="+string(condition.Status)+"/"+condition.Reason)
	}
	sort.Strings(result)
	return result
}

func durationLogValue(duration time.Duration) string {
	if duration <= 0 {
		return "0s"
	}
	return duration.Round(time.Millisecond).String()
}

func projectClientKeyStateCounts(statuses map[string]sentryv1alpha1.ClientKeyStatus) map[string]int {
	return countProjectStates(statuses, func(status sentryv1alpha1.ClientKeyStatus) string { return status.State })
}

func projectSymbolSourceStateCounts(statuses map[string]sentryv1alpha1.SymbolSourceStatus) map[string]int {
	return countProjectStates(statuses, func(status sentryv1alpha1.SymbolSourceStatus) string { return status.State })
}

func projectCodeMappingStateCounts(statuses map[string]sentryv1alpha1.CodeMappingStatus) map[string]int {
	return countProjectStates(statuses, func(status sentryv1alpha1.CodeMappingStatus) string { return status.State })
}

func projectDestinationStateCounts(statuses map[string]sentryv1alpha1.DestinationStatus) map[string]int {
	return countProjectStates(statuses, func(status sentryv1alpha1.DestinationStatus) string { return status.State })
}

func projectAlertStateCounts(statuses map[string]sentryv1alpha1.AlertStatus) map[string]int {
	return countProjectStates(statuses, func(status sentryv1alpha1.AlertStatus) string { return status.State })
}

func countProjectStates[T any](statuses map[string]T, state func(T) string) map[string]int {
	result := map[string]int{}
	for _, status := range statuses {
		result[state(status)]++
	}
	return result
}
