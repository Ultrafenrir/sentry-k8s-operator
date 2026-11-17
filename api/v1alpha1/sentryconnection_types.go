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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SecretKeyReference identifies one key in a Secret. The containing resource determines
// the lookup namespace: connection credentials use the fixed operator namespace, while
// namespaced project children use their SentryProject namespace. Namespace is never
// user-configurable.
type SecretKeyReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// ConnectionCredentials contains platform-owned Sentry API credentials.
type ConnectionCredentials struct {
	TokenSecretRef SecretKeyReference `json:"tokenSecretRef"`

	// +optional
	CABundleSecretRef *SecretKeyReference `json:"caBundleSecretRef,omitempty"`
}

// ConnectionTransport contains explicit development-only transport exceptions. Both
// options default to false so production connections remain HTTPS with certificate
// verification enabled.
type ConnectionTransport struct {
	// AllowInsecureHTTP permits a plain HTTP Sentry origin. This sends the API token
	// without transport encryption and should only be used on a trusted local network.
	// +kubebuilder:default=false
	// +optional
	AllowInsecureHTTP bool `json:"allowInsecureHTTP,omitempty"`

	// InsecureSkipTLSVerify keeps HTTPS encryption but disables server certificate and
	// hostname verification. It is mutually exclusive with caBundleSecretRef.
	// +kubebuilder:default=false
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// IntegrationCatalogEntry maps one stable local alias to one already-installed Sentry
// integration name. Declaring an entry never installs an integration.
type IntegrationCatalogEntry struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	SentryName string `json:"sentryName"`
}

// SlackIntegration declares readable aliases for already-installed workspaces.
type SlackIntegration struct {
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=name
	// +optional
	Workspaces []IntegrationCatalogEntry `json:"workspaces,omitempty"`
}

// PagerDutyIntegration declares readable aliases for already-installed accounts.
type PagerDutyIntegration struct {
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=name
	// +optional
	Accounts []IntegrationCatalogEntry `json:"accounts,omitempty"`
}

// SourceCodeRepositoryCatalogEntry maps one stable local alias to one repository already
// imported by a declared source-code integration. Numeric Sentry repository identities stay
// in protected state.
type SourceCodeRepositoryCatalogEntry struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	SentryName string `json:"sentryName"`
}

// SourceCodeProvider is a portable Sentry source-code integration provider.
// +kubebuilder:validation:Enum=github;github_enterprise;gitlab;bitbucket;vsts
type SourceCodeProvider string

// SourceCodeIntegrationCatalogEntry declares one installed source-code integration and its
// readable repository aliases. Declaring an entry never installs an integration or imports a
// repository.
type SourceCodeIntegrationCatalogEntry struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	Name string `json:"name"`

	Provider SourceCodeProvider `json:"provider"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	SentryName string `json:"sentryName"`

	// +kubebuilder:validation:MaxItems=200
	// +listType=map
	// +listMapKey=name
	// +optional
	Repositories []SourceCodeRepositoryCatalogEntry `json:"repositories,omitempty"`
}

// SourceCodeIntegrations declares stable aliases for installed integrations and repositories.
type SourceCodeIntegrations struct {
	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=name
	// +optional
	Installations []SourceCodeIntegrationCatalogEntry `json:"installations,omitempty"`
}

// ConnectionRole is the platform-controlled regional mutation role.
// +kubebuilder:validation:Enum=Active;Standby
type ConnectionRole string

const (
	ConnectionRoleActive  ConnectionRole = "Active"
	ConnectionRoleStandby ConnectionRole = "Standby"
)

// ConnectionOwnership configures connection-wide regional fencing.
type ConnectionOwnership struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	Region string `json:"region"`

	Role ConnectionRole `json:"role"`

	// +kubebuilder:validation:Minimum=1
	Revision int64 `json:"revision"`
}

// UnmanagedProjectPolicy controls existing projects with no recognized ownership marker.
// +kubebuilder:validation:Enum=Reject;Standby;Adopt
type UnmanagedProjectPolicy string

const (
	UnmanagedProjectsReject  UnmanagedProjectPolicy = "Reject"
	UnmanagedProjectsStandby UnmanagedProjectPolicy = "Standby"
	UnmanagedProjectsAdopt   UnmanagedProjectPolicy = "Adopt"
)

type ProjectManagement struct {
	// +kubebuilder:default=Reject
	UnmanagedProjects UnmanagedProjectPolicy `json:"unmanagedProjects,omitempty"`
}

// ConnectionIntegrations contains optional platform integration defaults.
type ConnectionIntegrations struct {
	// +optional
	Slack *SlackIntegration `json:"slack,omitempty"`

	// +optional
	PagerDuty *PagerDutyIntegration `json:"pagerDuty,omitempty"`

	// +optional
	SourceCode *SourceCodeIntegrations `json:"sourceCode,omitempty"`
}

// AlertCompatibilityProfile is an explicit platform assertion about the target Sentry
// alert API. The operator never infers this safety boundary from an unstable version route.
// +kubebuilder:validation:Enum=legacy-24.7.1;modern-26.10.1
type AlertCompatibilityProfile string

const (
	AlertCompatibilityLegacy24 AlertCompatibilityProfile = "legacy-24.7.1"
	AlertCompatibilityModern26 AlertCompatibilityProfile = "modern-26.10.1"
)

// ConnectionCompatibility pins version-sensitive API families selected by the platform
// administrator.
type ConnectionCompatibility struct {
	// +optional
	Alerts AlertCompatibilityProfile `json:"alerts,omitempty"`
}

// SentryConnectionSpec defines the desired state of SentryConnection.
type SentryConnectionSpec struct {
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://[^[:space:]]+$`
	URL string `json:"url"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Organization string `json:"organization"`

	Credentials ConnectionCredentials `json:"credentials"`

	// +optional
	Transport *ConnectionTransport `json:"transport,omitempty"`

	// +optional
	Integrations *ConnectionIntegrations `json:"integrations,omitempty"`

	// Compatibility is required before version-sensitive APIs such as alerts are mutated.
	// +optional
	Compatibility *ConnectionCompatibility `json:"compatibility,omitempty"`

	// Ownership declares the regional mutation role when regional fencing is enabled.
	// +optional
	Ownership *ConnectionOwnership `json:"ownership,omitempty"`

	// +kubebuilder:default={unmanagedProjects: Reject}
	// +optional
	ProjectManagement ProjectManagement `json:"projectManagement,omitempty"`
}

// SentryConnectionStatus defines the observed state of SentryConnection.
type SentryConnectionStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +kubebuilder:validation:MaxLength=64
	// +optional
	ServerVersion string `json:"serverVersion,omitempty"`

	// +kubebuilder:validation:MaxLength=64
	// +optional
	Organization string `json:"organization,omitempty"`

	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=64
	// +listType=set
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// +kubebuilder:validation:MaxLength=63
	// +optional
	Region string `json:"region,omitempty"`

	// +kubebuilder:validation:Enum=Active;Standby;Fenced;Conflict
	// +optional
	EffectiveRole string `json:"effectiveRole,omitempty"`

	// +optional
	ObservedRevision int64 `json:"observedRevision,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=".status.serverVersion"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SentryConnection is the Schema for the sentryconnections API
type SentryConnection struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SentryConnection
	// +required
	Spec SentryConnectionSpec `json:"spec"`

	// status defines the observed state of SentryConnection
	// +optional
	Status SentryConnectionStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SentryConnectionList contains a list of SentryConnection
type SentryConnectionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SentryConnection `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SentryConnection{}, &SentryConnectionList{})
		return nil
	})
}
