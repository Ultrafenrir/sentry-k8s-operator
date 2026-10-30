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

// Package binding stores protected external identity and manager claims.
package binding

import "time"

const CurrentVersion = "v1"

// Origin is the evidence-derived origin of an external object.
type Origin string

const (
	OriginManaged  Origin = "Managed"
	OriginExternal Origin = "External"
	AccessManage          = "Manage"
	AccessReadOnly        = "ReadOnly"
)

// ProjectReference binds state to one exact Kubernetes object incarnation.
type ProjectReference struct {
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

// Marker is the readable ownership evidence stored atomically on a Sentry object.
type Marker struct {
	Origin    string `json:"origin"`
	Owner     string `json:"operatorOwner"`
	ManagedBy string `json:"managedBy"`
}

// Matches requires every reserved marker component to match exactly.
func (m Marker) Matches(other Marker) bool {
	return m.Origin == "managed" && m == other
}

// OperationState is durable intent around one non-idempotent mutation.
type OperationState struct {
	Kind        string `json:"kind"`
	Phase       string `json:"phase"`
	DesiredHash string `json:"desiredHash"`
}

// DSNBinding pins a client key and tracks a two-phase output Secret cutover.
// It never stores the DSN value.
type DSNBinding struct {
	KeyIdentity   string `json:"keyIdentity"`
	KeyName       string `json:"keyName"`
	SelectorHash  string `json:"selectorHash"`
	CurrentSecret string `json:"currentSecret"`
	PendingSecret string `json:"pendingSecret,omitempty"`
}

// DestinationBinding pins external identities for one local destination key.
// Numeric Telegram identities are stored by the registration subsystem, never here as
// public status. SelectorHash prevents silent same-name rebinding.
type DestinationBinding struct {
	Kind                string           `json:"kind"`
	SelectorHash        string           `json:"selectorHash"`
	IntegrationIdentity string           `json:"integrationIdentity,omitempty"`
	IntegrationName     string           `json:"integrationName,omitempty"`
	ResourceIdentity    string           `json:"resourceIdentity,omitempty"`
	ResourceName        string           `json:"resourceName"`
	State               string           `json:"state"`
	Reason              string           `json:"reason,omitempty"`
	Telegram            *TelegramBinding `json:"telegram,omitempty"`
}

// AlertBinding pins one operator-owned external alert child. The public API never exposes
// ExternalIdentity or adapter-native state; both remain in the protected project binding.
type AlertBinding struct {
	Adapter          string   `json:"adapter"`
	ExternalIdentity string   `json:"externalIdentity"`
	Trigger          string   `json:"trigger"`
	DesiredHash      string   `json:"desiredHash"`
	Enabled          bool     `json:"enabled"`
	Destinations     []string `json:"destinations"`
	Marker           Marker   `json:"marker"`
}

// TelegramBinding is protected registration/outbox state. Numeric identities and
// challenge hashes must never be copied to public API status.
type TelegramBinding struct {
	Challenge    *TelegramChallenge `json:"challenge,omitempty"`
	Target       *TelegramTarget    `json:"target,omitempty"`
	Phase        string             `json:"phase,omitempty"`
	OutboxKey    string             `json:"outboxKey,omitempty"`
	FenceHash    string             `json:"fenceHash,omitempty"`
	RouteKey     string             `json:"routeKey,omitempty"`
	RouteRevoked bool               `json:"routeRevoked,omitempty"`
}

type TelegramChallenge struct {
	CodeHash              string           `json:"codeHash"`
	Project               ProjectReference `json:"project"`
	Destination           string           `json:"destination"`
	SelectorHash          string           `json:"selectorHash"`
	Owner                 string           `json:"owner"`
	Connection            string           `json:"connection"`
	PolicyGeneration      int64            `json:"policyGeneration"`
	PolicyResourceVersion string           `json:"policyResourceVersion"`
	ExpiresAt             time.Time        `json:"expiresAt"`
}

type TelegramTarget struct {
	ChatIdentity  string `json:"chatIdentity"`
	TopicIdentity string `json:"topicIdentity,omitempty"`
}

// ProjectBinding is the protected record for one SentryProject incarnation.
type ProjectBinding struct {
	Version          string                        `json:"version"`
	Project          ProjectReference              `json:"project"`
	Connection       string                        `json:"connection"`
	Installation     string                        `json:"installation"`
	Organization     string                        `json:"organization"`
	Slug             string                        `json:"slug"`
	Owner            string                        `json:"owner,omitempty"`
	Origin           Origin                        `json:"origin"`
	AccessMode       string                        `json:"accessMode"`
	ExternalIdentity string                        `json:"externalIdentity,omitempty"`
	Marker           *Marker                       `json:"marker,omitempty"`
	ManagerUID       string                        `json:"managerUID,omitempty"`
	ClaimName        string                        `json:"claimName,omitempty"`
	Pending          *OperationState               `json:"pending,omitempty"`
	AppliedHashes    map[string]string             `json:"appliedHashes,omitempty"`
	PinnedAdapters   map[string]string             `json:"pinnedAdapters,omitempty"`
	ChildIdentities  map[string]string             `json:"childIdentities,omitempty"`
	Tombstones       map[string]bool               `json:"tombstones,omitempty"`
	DSN              *DSNBinding                   `json:"dsn,omitempty"`
	Destinations     map[string]DestinationBinding `json:"destinations,omitempty"`
	Alerts           map[string]AlertBinding       `json:"alerts,omitempty"`
}

// HandoffAuthorization is bound to one exact target UID, generation and slug.
type HandoffAuthorization struct {
	Target ProjectReference `json:"target"`
}

// ManagerClaim serializes writers across connection aliases for one project slug.
type ManagerClaim struct {
	Version          string                `json:"version"`
	Installation     string                `json:"installation"`
	Organization     string                `json:"organization"`
	Slug             string                `json:"slug"`
	Owner            string                `json:"owner"`
	Manager          ProjectReference      `json:"manager"`
	ExternalIdentity string                `json:"externalIdentity,omitempty"`
	Handoff          *HandoffAuthorization `json:"handoff,omitempty"`
}

type BindingRecord struct {
	Name            string
	ResourceVersion string
	Binding         ProjectBinding
}

type ClaimRecord struct {
	Name            string
	ResourceVersion string
	Claim           ManagerClaim
}
