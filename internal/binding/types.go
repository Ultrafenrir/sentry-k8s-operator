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

import "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/ownership"

const CurrentVersion = "v2"

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

// DSNBinding pins a client key and the fixed namespace-local output Secret. It never
// stores the DSN value. PendingSecret remains for decoding pre-migration protected state.
type DSNBinding struct {
	KeyIdentity   string `json:"keyIdentity"`
	KeyName       string `json:"keyName"`
	SelectorHash  string `json:"selectorHash"`
	CurrentSecret string `json:"currentSecret"`
	PendingSecret string `json:"pendingSecret,omitempty"`
}

// ClientKeyRateLimit is the protected normalized rate limit for one managed client key.
type ClientKeyRateLimit struct {
	Count         int64 `json:"count"`
	WindowSeconds int32 `json:"windowSeconds"`
}

// ClientKeyBinding pins exactly one operator-owned Sentry client key. Public and secret key
// material and DSNs are deliberately absent. Replacement fields make rotation resumable.
type ClientKeyBinding struct {
	ExternalIdentity       string              `json:"externalIdentity"`
	RemoteName             string              `json:"remoteName"`
	RotationRevision       int64               `json:"rotationRevision"`
	Active                 bool                `json:"active"`
	RateLimit              *ClientKeyRateLimit `json:"rateLimit,omitempty"`
	DesiredHash            string              `json:"desiredHash"`
	ReplacementIdentity    string              `json:"replacementIdentity,omitempty"`
	ReplacementRemoteName  string              `json:"replacementRemoteName,omitempty"`
	ReplacementRevision    int64               `json:"replacementRevision,omitempty"`
	ReplacementDesiredHash string              `json:"replacementDesiredHash,omitempty"`
}

// SymbolSourceBinding pins one operator-owned custom symbol source without retaining any
// credential value. DesiredHash is a one-way digest used to detect Secret rotation.
type SymbolSourceBinding struct {
	ExternalIdentity string `json:"externalIdentity"`
	RemoteName       string `json:"remoteName"`
	Type             string `json:"type"`
	DesiredHash      string `json:"desiredHash"`
}

// CodeMappingBinding pins one repository path mapping and its optional imported CODEOWNERS
// child. Numeric Sentry identities and remote catalog selections remain protected.
type CodeMappingBinding struct {
	ExternalIdentity      string `json:"externalIdentity"`
	IntegrationIdentity   string `json:"integrationIdentity"`
	RepositoryIdentity    string `json:"repositoryIdentity"`
	IntegrationAlias      string `json:"integrationAlias"`
	RepositoryAlias       string `json:"repositoryAlias"`
	DesiredHash           string `json:"desiredHash"`
	CodeownersIdentity    string `json:"codeownersIdentity,omitempty"`
	CodeownersContentHash string `json:"codeownersContentHash,omitempty"`
}

// DestinationBinding pins external identities for one local destination key.
// SelectorHash prevents silent same-name rebinding.
type DestinationBinding struct {
	Owner               ChildOwner `json:"owner"`
	Kind                string     `json:"kind"`
	SelectorHash        string     `json:"selectorHash"`
	IntegrationIdentity string     `json:"integrationIdentity,omitempty"`
	IntegrationName     string     `json:"integrationName,omitempty"`
	ResourceIdentity    string     `json:"resourceIdentity,omitempty"`
	ResourceName        string     `json:"resourceName"`
	State               string     `json:"state"`
	Reason              string     `json:"reason,omitempty"`
}

// AlertBinding pins one operator-owned external alert child. The public API never exposes
// ExternalIdentity or adapter-native state; both remain in the protected project binding.
type AlertBinding struct {
	Owner            ChildOwner `json:"owner"`
	Adapter          string     `json:"adapter"`
	ExternalIdentity string     `json:"externalIdentity"`
	Trigger          string     `json:"trigger"`
	DesiredHash      string     `json:"desiredHash"`
	Enabled          bool       `json:"enabled"`
	Destinations     []string   `json:"destinations"`
	Marker           Marker     `json:"marker"`
}

const (
	ChildOwnerProject = "Project"
	ChildOwnerPolicy  = "Policy"
)

// ChildOwner is protected ownership of one alert or notification target. UID prevents a
// recreated policy with the same name from inheriting external objects.
type ChildOwner struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	UID  string `json:"uid"`
}

func (o ChildOwner) Valid() bool {
	return (o.Kind == ChildOwnerProject || o.Kind == ChildOwnerPolicy) && o.Name != "" && o.UID != ""
}

// ProjectBinding is the protected record for one SentryProject incarnation.
type ProjectBinding struct {
	Version           string                         `json:"version"`
	Project           ProjectReference               `json:"project"`
	Connection        string                         `json:"connection"`
	Installation      string                         `json:"installation"`
	Organization      string                         `json:"organization"`
	Slug              string                         `json:"slug"`
	Owner             string                         `json:"owner,omitempty"`
	Origin            Origin                         `json:"origin"`
	AccessMode        string                         `json:"accessMode"`
	ExternalIdentity  string                         `json:"externalIdentity,omitempty"`
	Marker            *Marker                        `json:"marker,omitempty"`
	RegionalOwnership *ownership.Marker              `json:"regionalOwnership,omitempty"`
	ManagerUID        string                         `json:"managerUID,omitempty"`
	ClaimName         string                         `json:"claimName,omitempty"`
	Pending           *OperationState                `json:"pending,omitempty"`
	AppliedHashes     map[string]string              `json:"appliedHashes,omitempty"`
	PinnedAdapters    map[string]string              `json:"pinnedAdapters,omitempty"`
	ChildIdentities   map[string]string              `json:"childIdentities,omitempty"`
	Tombstones        map[string]bool                `json:"tombstones,omitempty"`
	DSN               *DSNBinding                    `json:"dsn,omitempty"`
	ClientKeys        map[string]ClientKeyBinding    `json:"clientKeys,omitempty"`
	SymbolSources     map[string]SymbolSourceBinding `json:"symbolSources,omitempty"`
	CodeMappings      map[string]CodeMappingBinding  `json:"codeMappings,omitempty"`
	GrantedTeams      map[string]string              `json:"grantedTeams,omitempty"`
	Destinations      map[string]DestinationBinding  `json:"destinations,omitempty"`
	Alerts            map[string]AlertBinding        `json:"alerts,omitempty"`
	AlertPolicy       *AppliedAlertPolicy            `json:"alertPolicy,omitempty"`
}

type AppliedAlertPolicy struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
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
