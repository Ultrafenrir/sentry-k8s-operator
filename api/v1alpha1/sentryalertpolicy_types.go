/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:validation:XValidation:rule="!has(self.alerts) || self.alerts.all(k, k.matches('^[a-z](?:[a-z0-9-]*[a-z0-9])?$'))",message="alert names must be lowercase DNS labels"
// SentryAlertPolicySpec is a reusable cluster-scoped alert configuration. Creating a
// policy alone never mutates Sentry; projects opt in by name.
type SentryAlertPolicySpec struct {
	// Sentry names one cluster-scoped SentryConnection.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Sentry string `json:"sentry"`

	// +optional
	NotificationTargets *NotificationTargets `json:"notificationTargets,omitempty"`

	// +kubebuilder:validation:MaxProperties=100
	// +optional
	Alerts map[string]AlertSpec `json:"alerts,omitempty"`
}

type AlertPolicyRolloutStatus struct {
	DesiredProjects int32 `json:"desiredProjects,omitempty"`
	ReadyProjects   int32 `json:"readyProjects,omitempty"`
	FailedProjects  int32 `json:"failedProjects,omitempty"`
	PendingProjects int32 `json:"pendingProjects,omitempty"`
}

// SentryAlertPolicyStatus is bounded and never lists consumer projects or protected IDs.
type SentryAlertPolicyStatus struct {
	ObservedGeneration int64                    `json:"observedGeneration,omitempty"`
	Rollout            AlertPolicyRolloutStatus `json:"rollout,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Sentry",type=string,JSONPath=".spec.sentry"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type SentryAlertPolicy struct {
	metav1.TypeMeta `json:",inline"`
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`
	// +required
	Spec SentryAlertPolicySpec `json:"spec"`
	// +optional
	Status SentryAlertPolicyStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type SentryAlertPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SentryAlertPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SentryAlertPolicy{}, &SentryAlertPolicyList{})
		return nil
	})
}
