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
	"errors"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var ErrAmbiguousNotificationTarget = errors.New("notification target name is ambiguous within its provider")

const (
	// DefaultDSNSecretSuffix is appended to metadata.name when a custom Secret is not requested.
	DefaultDSNSecretSuffix = "-sentry"
	// DefaultDSNSecretKey is the stable key written to every managed DSN output Secret.
	DefaultDSNSecretKey = "SENTRY_DSN"
)

// ProjectCreate identifies a new project entirely through readable values.
type ProjectCreate struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	Team string `json:"team"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Platform string `json:"platform,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="has(self.create) != has(self.use)",message="exactly one of project.create or project.use is required"
// ProjectSelector chooses either a new managed project or an existing read-only project.
type ProjectSelector struct {
	// +optional
	Create *ProjectCreate `json:"create,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Use *string `json:"use,omitempty"`
}

// DSNSpec customizes the public DSN output without exposing external IDs.
type DSNSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Secret string `json:"secret,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	// +optional
	ClientKey string `json:"clientKey,omitempty"`
}

// FilterSettings contains the source-confirmed inbound filters.
type FilterSettings struct {
	// +optional
	BrowserExtensions *bool `json:"browserExtensions,omitempty"`

	// +optional
	Localhost *bool `json:"localhost,omitempty"`

	// +optional
	WebCrawlers *bool `json:"webCrawlers,omitempty"`
}

// PIISettings contains the source-confirmed, adapter-neutral data scrubbing fields.
type PIISettings struct {
	// +optional
	ScrubData *bool `json:"scrubData,omitempty"`

	// +optional
	ScrubDefaults *bool `json:"scrubDefaults,omitempty"`

	// +optional
	ScrubIPAddresses *bool `json:"scrubIPAddresses,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=128
	// +optional
	SensitiveFields *[]string `json:"sensitiveFields,omitempty"`

	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=128
	// +optional
	SafeFields *[]string `json:"safeFields,omitempty"`
}

// ProjectSettings is deliberately limited to the T00 source-confirmed alpha surface.
type ProjectSettings struct {
	// +optional
	Filters *FilterSettings `json:"filters,omitempty"`

	// +optional
	PII *PIISettings `json:"pii,omitempty"`
}

// SlackDestination selects a Slack channel by readable names.
type SlackDestination struct {
	// +kubebuilder:validation:MaxLength=100
	// +optional
	Workspace string `json:"workspace,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	Channel string `json:"channel"`
}

// PagerDutyDestination selects a PagerDuty service by readable names.
type PagerDutyDestination struct {
	// +kubebuilder:validation:MaxLength=100
	// +optional
	Account string `json:"account,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	Service string `json:"service"`
}

// TelegramDestination selects a logical Telegram target; numeric IDs stay internal.
type TelegramDestination struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=100
	Target string `json:"target"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.slack) && !has(self.pagerDuty) && !has(self.telegram)) || (!has(self.slack) && has(self.pagerDuty) && !has(self.telegram)) || (!has(self.slack) && !has(self.pagerDuty) && has(self.telegram))",message="exactly one destination type is required"
// DestinationSpec contains exactly one adapter-neutral destination selector.
type DestinationSpec struct {
	// +optional
	Slack *SlackDestination `json:"slack,omitempty"`

	// +optional
	PagerDuty *PagerDutyDestination `json:"pagerDuty,omitempty"`

	// +optional
	Telegram *TelegramDestination `json:"telegram,omitempty"`
}

// NotificationTargets selects readable resources inside platform-declared integrations.
// Channel and service names must be unique within their provider because alerts reference
// them as slack/<channel> and pagerDuty/<service>.
type NotificationTargets struct {
	// Slack maps one workspace catalog name to visible channel names.
	// +kubebuilder:validation:MaxProperties=20
	// +optional
	Slack map[string][]string `json:"slack,omitempty"`

	// PagerDuty maps one account catalog name to visible service names.
	// +kubebuilder:validation:MaxProperties=20
	// +optional
	PagerDuty map[string][]string `json:"pagerDuty,omitempty"`

	// Telegram contains registered readable targets.
	// +kubebuilder:validation:MaxItems=100
	// +listType=set
	// +optional
	Telegram []string `json:"telegram,omitempty"`
}

// Flatten returns canonical readable references without inventing local identifiers.
func (t *NotificationTargets) Flatten() (map[string]DestinationSpec, error) {
	result := map[string]DestinationSpec{}
	if t == nil {
		return result, nil
	}
	seenSlack := map[string]bool{}
	for workspace, channels := range t.Slack {
		for _, channel := range channels {
			channel = strings.TrimPrefix(strings.TrimSpace(channel), "#")
			if channel == "" || seenSlack[channel] {
				return nil, ErrAmbiguousNotificationTarget
			}
			seenSlack[channel] = true
			result["slack/"+channel] = DestinationSpec{Slack: &SlackDestination{Workspace: workspace, Channel: channel}}
		}
	}
	seenPagerDuty := map[string]bool{}
	for account, services := range t.PagerDuty {
		for _, service := range services {
			service = strings.TrimSpace(service)
			if service == "" || seenPagerDuty[service] {
				return nil, ErrAmbiguousNotificationTarget
			}
			seenPagerDuty[service] = true
			result["pagerDuty/"+service] = DestinationSpec{PagerDuty: &PagerDutyDestination{Account: account, Service: service}}
		}
	}
	seenTelegram := map[string]bool{}
	for _, target := range t.Telegram {
		target = strings.TrimSpace(target)
		if target == "" || seenTelegram[target] {
			return nil, ErrAmbiguousNotificationTarget
		}
		seenTelegram[target] = true
		result["telegram/"+target] = DestinationSpec{Telegram: &TelegramDestination{Target: target}}
	}
	return result, nil
}

// ProjectRole is a request to acquire management or remain a non-owning candidate. Active
// never authorizes stealing an existing claim.
// +kubebuilder:validation:Enum=Active;Standby
type ProjectRole string

const (
	ProjectRoleActive  ProjectRole = "Active"
	ProjectRoleStandby ProjectRole = "Standby"
)

// AlertTrigger is the portable alert condition exposed by the operator.
// +kubebuilder:validation:Enum=first-seen;regression;event-count
type AlertTrigger string

const (
	AlertTriggerFirstSeen  AlertTrigger = "first-seen"
	AlertTriggerRegression AlertTrigger = "regression"
	AlertTriggerEventCount AlertTrigger = "event-count"
)

// +kubebuilder:validation:XValidation:rule="self.trigger == 'event-count' ? (has(self.windowMinutes) && has(self.threshold)) : (!has(self.windowMinutes) && !has(self.threshold))",message="event-count requires windowMinutes and threshold; issue triggers forbid them"
// AlertSpec is adapter-neutral and references notification destinations only by local key.
type AlertSpec struct {
	Trigger AlertTrigger `json:"trigger"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1440
	// +optional
	WindowMinutes *int32 `json:"windowMinutes,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +optional
	Threshold *int64 `json:"threshold,omitempty"`

	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=20
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=211
	// +kubebuilder:validation:items:Pattern=`^(slack|pagerDuty|telegram)/[^/[:space:]][^/]*$`
	// +listType=set
	Notify []string `json:"notify"`
}

// OnDeletePolicy controls whether the external project may be deleted with the CR.
// +kubebuilder:validation:Enum=Retain;DeleteProject
type OnDeletePolicy string

const (
	// OnDeleteRetain leaves the external project intact.
	OnDeleteRetain OnDeletePolicy = "Retain"
	// OnDeleteDeleteProject requests guarded deletion of a managed project.
	OnDeleteDeleteProject OnDeletePolicy = "DeleteProject"
)

// LifecycleSpec defines deletion behavior.
type LifecycleSpec struct {
	// +kubebuilder:default=Retain
	OnDelete OnDeletePolicy `json:"onDelete,omitempty"`
}

// ProjectGrantAccess declares additional Sentry teams that may access a managed project.
// Team slugs are canonical; user membership remains outside this API.
type ProjectGrantAccess struct {
	// +kubebuilder:validation:MaxItems=50
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	// +listType=set
	// +optional
	Teams []string `json:"teams,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.project.use) || (!has(self.settings) && !has(self.notificationTargets) && !has(self.alerts) && !has(self.grantAccess))",message="project.use forbids settings, notification targets, alerts and grantAccess"
// +kubebuilder:validation:XValidation:rule="!has(self.project.use) || !has(self.alertPolicy)",message="project.use forbids alertPolicy"
// +kubebuilder:validation:XValidation:rule="!has(self.project.use) || self.lifecycle.onDelete != 'DeleteProject'",message="project.use forbids lifecycle.onDelete DeleteProject"
// +kubebuilder:validation:XValidation:rule="self.role != 'Standby' || (!has(self.settings) && !has(self.grantAccess) && self.lifecycle.onDelete != 'DeleteProject')",message="Standby forbids settings, grantAccess and DeleteProject lifecycle"
// +kubebuilder:validation:XValidation:rule="!has(self.alerts) || self.alerts.all(k, k.matches('^[a-z](?:[a-z0-9-]*[a-z0-9])?$'))",message="alert names must be lowercase DNS labels"
// +kubebuilder:validation:XValidation:rule="!has(self.grantAccess) || !has(self.project.create) || !has(self.grantAccess.teams) || !self.grantAccess.teams.exists(t, t == self.project.create.team)",message="grantAccess.teams must not repeat project.create.team"
// SentryProjectSpec defines the desired state of SentryProject.
type SentryProjectSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Sentry string `json:"sentry"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	AlertPolicy string `json:"alertPolicy,omitempty"`

	Project ProjectSelector `json:"project"`

	// +kubebuilder:default=Active
	// +optional
	Role ProjectRole `json:"role,omitempty"`

	// +optional
	DSN *DSNSpec `json:"dsn,omitempty"`

	// +optional
	Settings *ProjectSettings `json:"settings,omitempty"`

	// GrantAccess manages additional team access for a managed project. The connection access
	// rule must authorize every team slug.
	// +optional
	GrantAccess *ProjectGrantAccess `json:"grantAccess,omitempty"`

	// +optional
	NotificationTargets *NotificationTargets `json:"notificationTargets,omitempty"`

	// An absent alerts field leaves existing alert children unmanaged. An explicit empty map
	// deletes all protected operator-owned alerts.
	// +kubebuilder:validation:MaxProperties=100
	// +optional
	Alerts *map[string]AlertSpec `json:"alerts,omitempty"`

	// +kubebuilder:default=false
	// +optional
	Paused bool `json:"paused,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Lifecycle LifecycleSpec `json:"lifecycle,omitempty"`
}

// SentryProjectStatus defines the observed state of SentryProject.
type SentryProjectStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +kubebuilder:validation:Enum=Managed;External
	// +optional
	Origin string `json:"origin,omitempty"`

	// +kubebuilder:validation:Enum=Manage;ReadOnly;Deny
	// +optional
	AccessMode string `json:"accessMode,omitempty"`

	// +kubebuilder:validation:Enum=Active;Standby
	// +optional
	RequestedRole string `json:"requestedRole,omitempty"`

	// +kubebuilder:validation:Enum=Active;Standby;Fenced;Conflict
	// +optional
	EffectiveRole string `json:"effectiveRole,omitempty"`

	// +kubebuilder:validation:MaxLength=2048
	// +optional
	ProjectURL string `json:"projectURL,omitempty"`

	// +kubebuilder:validation:MaxLength=253
	// +optional
	DSNSecret string `json:"dsnSecret,omitempty"`

	// DSNClientKey is the current readable Sentry client-key name or label. The
	// protected binding, not this display value, pins the external identity.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	DSNClientKey string `json:"dsnClientKey,omitempty"`

	// NotificationTargets exposes only local names and safe lifecycle state. External
	// integration, channel, service and Telegram identities remain protected.
	// +mapType=granular
	// +optional
	NotificationTargets map[string]DestinationStatus `json:"notificationTargets,omitempty"`

	// Alerts exposes only local names and safe lifecycle state. Adapter and external IDs stay
	// in the protected project binding.
	// +mapType=granular
	// +optional
	Alerts map[string]AlertStatus `json:"alerts,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// DestinationStatus is the safe status for one canonical notification target key.
type DestinationStatus struct {
	// +kubebuilder:validation:Enum=slack;pagerduty;telegram
	Type string `json:"type"`

	// +kubebuilder:validation:MaxLength=64
	State string `json:"state"`

	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`

	// RegistrationCommit is a short-lived, non-secret fence used by the
	// authenticated Telegram registration writer.
	// +optional
	RegistrationCommit *RegistrationCommit `json:"registrationCommit,omitempty"`
}

// AlertStatus is the safe status of one local alert key.
type AlertStatus struct {
	// +kubebuilder:validation:MaxLength=64
	State string `json:"state"`

	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Origin",type=string,JSONPath=".status.origin"
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=".status.accessMode"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// SentryProject is the Schema for the sentryprojects API
type SentryProject struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SentryProject
	// +required
	Spec SentryProjectSpec `json:"spec"`

	// status defines the observed state of SentryProject
	// +optional
	Status SentryProjectStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SentryProjectList contains a list of SentryProject
type SentryProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SentryProject `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SentryProject{}, &SentryProjectList{})
		return nil
	})
}
