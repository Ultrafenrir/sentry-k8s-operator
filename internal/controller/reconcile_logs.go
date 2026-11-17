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
	log.V(1).Info("SentryConnection reconciliation completed",
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
		"requeue", result.Requeue,
		"requeueAfter", durationLogValue(result.RequeueAfter),
		"error", reconcileErr != nil,
	)
}

func logProjectReconcile(log logr.Logger, req ctrl.Request, project *sentryv1alpha1.SentryProject, resourceFound bool, result ctrl.Result, reconcileErr error) {
	log.V(1).Info("SentryProject reconciliation completed",
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
