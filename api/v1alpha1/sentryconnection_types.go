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

// SecretKeyReference identifies one key in a Secret from the fixed operator namespace.
// Namespace is intentionally not user-configurable.
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

// NamespaceAccess selects namespaces by exact, human-readable name.
type NamespaceAccess struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +listType=set
	MatchNames []string `json:"matchNames"`
}

// ExistingProjectAccess grants read-only visibility and optional DSN consumption by slug.
type ExistingProjectAccess struct {
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +listType=set
	// +optional
	Observe []string `json:"observe,omitempty"`

	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +listType=set
	// +optional
	ConsumeDSN []string `json:"consumeDSN,omitempty"`
}

// SlackDestinationGrant scopes allowed Slack channels by optional workspace name.
type SlackDestinationGrant struct {
	// +kubebuilder:validation:MaxLength=100
	// +optional
	Workspace string `json:"workspace,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=100
	// +listType=set
	Channels []string `json:"channels"`
}

// PagerDutyDestinationGrant scopes allowed PagerDuty services by optional account name.
type PagerDutyDestinationGrant struct {
	// +kubebuilder:validation:MaxLength=100
	// +optional
	Account string `json:"account,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=100
	// +listType=set
	Services []string `json:"services"`
}

// TelegramDestinationGrant scopes allowed logical Telegram targets.
type TelegramDestinationGrant struct {
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=100
	// +listType=set
	Targets []string `json:"targets"`
}

// DestinationAccess contains all platform-approved destination names.
type DestinationAccess struct {
	// +kubebuilder:validation:MaxItems=20
	// +optional
	Slack []SlackDestinationGrant `json:"slack,omitempty"`

	// +kubebuilder:validation:MaxItems=20
	// +optional
	PagerDuty []PagerDutyDestinationGrant `json:"pagerDuty,omitempty"`

	// +optional
	Telegram *TelegramDestinationGrant `json:"telegram,omitempty"`
}

// AccessRule binds one trusted owner to namespace, project and destination policy.
type AccessRule struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9.-]*[a-z0-9])?$`
	Owner string `json:"owner"`

	Namespaces NamespaceAccess `json:"namespaces"`

	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +listType=set
	// +optional
	Teams []string `json:"teams,omitempty"`

	// +optional
	AllowCreate bool `json:"allowCreate,omitempty"`

	// +optional
	ExistingProjects *ExistingProjectAccess `json:"existingProjects,omitempty"`

	// +optional
	Destinations *DestinationAccess `json:"destinations,omitempty"`
}

// SlackIntegration selects the default workspace by readable name.
type SlackIntegration struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	DefaultWorkspace string `json:"defaultWorkspace"`
}

// PagerDutyIntegration selects the default account by readable name.
type PagerDutyIntegration struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	DefaultAccount string `json:"defaultAccount"`
}

// TelegramIntegration configures the platform-owned Telegram delivery route.
type TelegramIntegration struct {
	BotTokenSecretRef SecretKeyReference `json:"botTokenSecretRef"`

	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^[:space:]]+$`
	ReceiverURL string `json:"receiverURL"`

	WebhookAuthSecretRef SecretKeyReference `json:"webhookAuthSecretRef"`
}

// ConnectionIntegrations contains optional platform integration defaults.
type ConnectionIntegrations struct {
	// +optional
	Slack *SlackIntegration `json:"slack,omitempty"`

	// +optional
	PagerDuty *PagerDutyIntegration `json:"pagerDuty,omitempty"`

	// +optional
	Telegram *TelegramIntegration `json:"telegram,omitempty"`
}

// SentryConnectionSpec defines the desired state of SentryConnection.
type SentryConnectionSpec struct {
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^[:space:]]+$`
	URL string `json:"url"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Organization string `json:"organization"`

	Credentials ConnectionCredentials `json:"credentials"`

	// +kubebuilder:default=false
	// +optional
	Default bool `json:"default,omitempty"`

	// +kubebuilder:validation:MaxItems=100
	// +listType=map
	// +listMapKey=owner
	// +optional
	Access []AccessRule `json:"access,omitempty"`

	// +optional
	Integrations *ConnectionIntegrations `json:"integrations,omitempty"`
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

	// +optional
	RegistrationCommit *RegistrationCommit `json:"registrationCommit,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RegistrationCommit is a non-secret, short-lived policy fence written by the system.
type RegistrationCommit struct {
	// +kubebuilder:validation:MinLength=64
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{64}$`
	OperationHash string `json:"operationHash"`

	PolicyGeneration int64 `json:"policyGeneration"`

	ExpiresAt metav1.Time `json:"expiresAt"`
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
