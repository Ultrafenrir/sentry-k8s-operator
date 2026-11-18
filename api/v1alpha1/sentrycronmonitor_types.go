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

// MonitorDeletionPolicy controls whether the independently owned Sentry object is removed.
// +kubebuilder:validation:Enum=Delete;Retain
type MonitorDeletionPolicy string

const (
	MonitorDelete MonitorDeletionPolicy = "Delete"
	MonitorRetain MonitorDeletionPolicy = "Retain"
)

// CronIntervalUnit is the bounded interval vocabulary accepted by Sentry.
// +kubebuilder:validation:Enum=minute;hour;day;week;month;year
type CronIntervalUnit string

type CronSchedule struct {
	// Expression is a standard five-field crontab expression.
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=128
	Expression string `json:"expression"`

	// Timezone is an IANA timezone name.
	// +kubebuilder:default=UTC
	// +optional
	Timezone string `json:"timezone,omitempty"`
}

type IntervalSchedule struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10000
	Value int32            `json:"value"`
	Unit  CronIntervalUnit `json:"unit"`
}

type SentryCronMonitorSpec struct {
	// ProjectRef names a SentryProject in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ProjectRef string `json:"projectRef"`

	// DisplayName defaults to the Kubernetes resource name.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Exactly one schedule form must be set.
	// +optional
	Crontab *CronSchedule `json:"crontab,omitempty"`
	// +optional
	Interval *IntervalSchedule `json:"interval,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10080
	// +optional
	CheckInMarginMinutes *int32 `json:"checkInMarginMinutes,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=43200
	// +optional
	MaxRuntimeMinutes *int32 `json:"maxRuntimeMinutes,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	FailureThreshold *int32 `json:"failureThreshold,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	RecoveryThreshold *int32 `json:"recoveryThreshold,omitempty"`

	// OwnerTeam is a Sentry team slug. The controller resolves it to a remote actor ID.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	// +optional
	OwnerTeam string `json:"ownerTeam,omitempty"`

	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default=Delete
	// +optional
	OnDelete MonitorDeletionPolicy `json:"onDelete,omitempty"`
}

type SentryMonitorStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ExternalURL is safe to expose; remote numeric identities remain protected.
	ExternalURL string `json:"externalURL,omitempty"`
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=".spec.projectRef"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type SentryCronMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              SentryCronMonitorSpec `json:"spec"`
	// +optional
	Status SentryMonitorStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type SentryCronMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SentryCronMonitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SentryCronMonitor{}, &SentryCronMonitorList{})
		return nil
	})
}
