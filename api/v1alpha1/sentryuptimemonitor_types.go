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

// UptimeHTTPMethod is the method vocabulary supported by the bounded API.
// +kubebuilder:validation:Enum=GET;HEAD;POST;PUT;PATCH;DELETE;OPTIONS
type UptimeHTTPMethod string

type SecretValueReference struct {
	// Name must be sentry-uptime-inputs in the monitor namespace.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

type UptimeHeader struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	Name      string               `json:"name"`
	ValueFrom SecretValueReference `json:"valueFrom"`
}

// UptimeAssertionType selects one bounded response assertion.
// +kubebuilder:validation:Enum=statusCode;responseHeader;jsonPath
type UptimeAssertionType string

// UptimeAssertionComparison selects a non-executable comparison.
// +kubebuilder:validation:Enum=equals;notEquals;lessThan;greaterThan
type UptimeAssertionComparison string

type UptimeAssertion struct {
	Type UptimeAssertionType `json:"type"`
	// Key is a response-header name or a bounded JSON path. It is absent for statusCode.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Key        string                    `json:"key,omitempty"`
	Comparison UptimeAssertionComparison `json:"comparison"`
	// Value is intentionally public desired state. Do not place credentials here.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Value string `json:"value"`
}

type SentryUptimeMonitorSpec struct {
	// ProjectRef names a SentryProject in the same namespace.
	ProjectRef string `json:"projectRef"`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DisplayName string `json:"displayName,omitempty"`
	// URL must use HTTP or HTTPS and pass the operator's destination safety policy.
	// +kubebuilder:validation:MaxLength=255
	URL string `json:"url"`
	// +kubebuilder:default=GET
	// +optional
	Method UptimeHTTPMethod `json:"method,omitempty"`
	// Sentry currently accepts 60, 300, 600, 1200, 1800, or 3600 seconds.
	// +kubebuilder:validation:Enum=60;300;600;1200;1800;3600
	IntervalSeconds int32 `json:"intervalSeconds"`
	// +kubebuilder:validation:Minimum=1000
	// +kubebuilder:validation:Maximum=60000
	TimeoutMilliseconds int32 `json:"timeoutMilliseconds"`
	// +optional
	TraceSampling bool `json:"traceSampling,omitempty"`
	// +kubebuilder:default=true
	// +optional
	ResponseCaptureEnabled bool `json:"responseCaptureEnabled,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1000
	// +optional
	RecoveryThreshold int32 `json:"recoveryThreshold,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Environment string `json:"environment,omitempty"`
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	// +optional
	OwnerTeam string `json:"ownerTeam,omitempty"`
	// +kubebuilder:validation:MaxItems=20
	// +listType=map
	// +listMapKey=name
	// +optional
	Headers []UptimeHeader `json:"headers,omitempty"`
	// +optional
	BodyFrom *SecretValueReference `json:"bodyFrom,omitempty"`
	// Assertions are combined with logical AND in declared order.
	// +kubebuilder:validation:MaxItems=10
	// +listType=atomic
	// +optional
	Assertions []UptimeAssertion `json:"assertions,omitempty"`
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled,omitempty"`
	// +kubebuilder:default=Delete
	// +optional
	OnDelete MonitorDeletionPolicy `json:"onDelete,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=".spec.projectRef"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type SentryUptimeMonitor struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`
	Spec              SentryUptimeMonitorSpec `json:"spec"`
	// +optional
	Status SentryMonitorStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true
type SentryUptimeMonitorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SentryUptimeMonitor `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SentryUptimeMonitor{}, &SentryUptimeMonitorList{})
		return nil
	})
}
