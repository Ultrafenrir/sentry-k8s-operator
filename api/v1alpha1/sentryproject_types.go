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

// +kubebuilder:validation:XValidation:rule="!(has(self.clientKey) && has(self.managedClientKey))",message="clientKey and managedClientKey are mutually exclusive"
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

	// ManagedClientKey selects one operator-managed client key by its local map key. This
	// avoids exposing the ownership-bearing remote Sentry label.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	// +optional
	ManagedClientKey string `json:"managedClientKey,omitempty"`
}

// ClientKeyRateLimit caps accepted errors for one managed client key. A missing rateLimit
// explicitly disables the per-key limit because the complete managed child is owned.
type ClientKeyRateLimit struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	Count int64 `json:"count"`

	// WindowSeconds is bounded by the portable limit shared by both supported Sentry
	// versions.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=86400
	WindowSeconds int32 `json:"windowSeconds"`
}

// ManagedClientKeySpec owns one Sentry project client key without accepting public or
// secret key material. Increasing rotationRevision creates a server-generated replacement.
type ManagedClientKeySpec struct {
	// +kubebuilder:default=true
	// +optional
	Active bool `json:"active,omitempty"`

	// +optional
	RateLimit *ClientKeyRateLimit `json:"rateLimit,omitempty"`

	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	// +kubebuilder:default=1
	// +optional
	RotationRevision int64 `json:"rotationRevision,omitempty"`
}

// LegacyBrowserFilter selects one standard Sentry legacy-browser family.
// +kubebuilder:validation:Enum=ie;edge;safari;firefox;chrome;opera;android;opera_mini
type LegacyBrowserFilter string

const (
	LegacyBrowserIE        LegacyBrowserFilter = "ie"
	LegacyBrowserEdge      LegacyBrowserFilter = "edge"
	LegacyBrowserSafari    LegacyBrowserFilter = "safari"
	LegacyBrowserFirefox   LegacyBrowserFilter = "firefox"
	LegacyBrowserChrome    LegacyBrowserFilter = "chrome"
	LegacyBrowserOpera     LegacyBrowserFilter = "opera"
	LegacyBrowserAndroid   LegacyBrowserFilter = "android"
	LegacyBrowserOperaMini LegacyBrowserFilter = "opera_mini"
)

// FilterSettings contains the source-confirmed inbound filters. A nil field is unmanaged;
// an explicitly empty legacyBrowsers set disables every managed browser subfilter.
type FilterSettings struct {
	// +optional
	BrowserExtensions *bool `json:"browserExtensions,omitempty"`

	// +optional
	Localhost *bool `json:"localhost,omitempty"`

	// +optional
	WebCrawlers *bool `json:"webCrawlers,omitempty"`

	// +optional
	FilteredTransactions *bool `json:"filteredTransactions,omitempty"`

	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	// +optional
	LegacyBrowsers *[]LegacyBrowserFilter `json:"legacyBrowsers,omitempty"`
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

	// AdvancedRules owns the complete ordered Relay PII rule set. An explicit empty list
	// clears project-level advanced rules. Raw Relay JSON, regular expressions, and selector
	// grammar are deliberately not exposed.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	AdvancedRules *[]AdvancedPIIRule `json:"advancedRules,omitempty"`
}

// AdvancedPIIRuleType selects one Relay built-in PII detector shared by both supported
// Sentry compatibility versions.
// +kubebuilder:validation:Enum=anything;creditcard;password;ip;imei;email;uuid;pemkey;url_auth;us_ssn;userpath;mac
type AdvancedPIIRuleType string

// AdvancedPIIRedaction selects the bounded redaction applied after a detector matches.
// +kubebuilder:validation:Enum=mask;remove;hash;replace
type AdvancedPIIRedaction string

// AdvancedPIISelectorRoot selects a bounded event subtree without exposing Relay selector
// grammar.
// +kubebuilder:validation:Enum=message;extra;request;user;contexts;tags;breadcrumbs
type AdvancedPIISelectorRoot string

// +kubebuilder:validation:XValidation:rule="self.root == 'message' ? !has(self.path) || size(self.path) == 0 : has(self.path) && size(self.path) > 0",message="message selectors cannot have a path; all other selector roots require one"
// AdvancedPIISelector identifies one exact event field. Path segments are serialized into a
// canonical Relay selector by the settings adapter.
type AdvancedPIISelector struct {
	Root AdvancedPIISelectorRoot `json:"root"`

	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_-]+$`
	// +listType=atomic
	// +optional
	Path []string `json:"path,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self.redaction == 'replace' ? has(self.replacement) : !has(self.replacement)",message="replacement is required only for replace redaction"
// AdvancedPIIRule applies one built-in detector and redaction to one or more exact fields.
type AdvancedPIIRule struct {
	Type AdvancedPIIRuleType `json:"type"`

	Redaction AdvancedPIIRedaction `json:"redaction"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[^\n\r\f]*$`
	// +optional
	Replacement string `json:"replacement,omitempty"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=16
	// +listType=atomic
	Applications []AdvancedPIISelector `json:"applications"`
}

// GroupingEnhancementMatcher selects one portable stack-frame field.
// +kubebuilder:validation:Enum=path;module;function;package
type GroupingEnhancementMatcher string

// GroupingEnhancementAction selects one portable frame classification or contribution
// action.
// +kubebuilder:validation:Enum=MarkInApp;MarkOutOfApp;Include;Exclude
type GroupingEnhancementAction string

// GroupingEnhancementRule is serialized into one canonical Sentry stack-trace rule.
type GroupingEnhancementRule struct {
	Matcher GroupingEnhancementMatcher `json:"matcher"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\n\r\f\"]+$`
	Pattern string `json:"pattern"`

	// +optional
	Negate bool `json:"negate,omitempty"`

	Action GroupingEnhancementAction `json:"action"`
}

// FingerprintMatcher selects one event field supported by both grouping engines.
// +kubebuilder:validation:Enum=error.type;error.value;message;logger;level;stack.module;stack.abs_path;stack.package;stack.function
type FingerprintMatcher string

// +kubebuilder:validation:XValidation:rule="(has(self.includeDefault) && self.includeDefault) || (has(self.values) && size(self.values) > 0)",message="at least one custom value or includeDefault is required"
// FingerprintRule is serialized into one canonical ordered Sentry fingerprinting rule.
type FingerprintRule struct {
	Matcher FingerprintMatcher `json:"matcher"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^\n\r\f\"]+$`
	Pattern string `json:"pattern"`

	// +optional
	Negate bool `json:"negate,omitempty"`

	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9_.:/-]+$`
	// +listType=atomic
	// +optional
	Values []string `json:"values,omitempty"`

	// IncludeDefault appends Sentry's original fingerprint to the custom values.
	// +optional
	IncludeDefault bool `json:"includeDefault,omitempty"`
}

// GroupingSettings owns the portable ordered grouping enhancement and fingerprinting rule
// lists. Explicit empty lists clear the respective rule text.
type GroupingSettings struct {
	// +kubebuilder:validation:MaxItems=100
	// +optional
	Enhancements *[]GroupingEnhancementRule `json:"enhancements,omitempty"`

	// +kubebuilder:validation:MaxItems=100
	// +optional
	Fingerprints *[]FingerprintRule `json:"fingerprints,omitempty"`
}

// BuiltinSymbolSource selects one public symbol server configured in both supported Sentry
// compatibility versions. Version-specific, private, and console-gated sources are excluded.
// +kubebuilder:validation:Enum=amd;autodesk;chromium;citrix;electron;intel;microsoft;mozilla;nuget;nvidia;ubuntu;unity
type BuiltinSymbolSource string

// SymbolicationSettings owns portable native symbolication controls. An explicit empty
// builtinSources set disables every built-in source for the project.
type SymbolicationSettings struct {
	// +kubebuilder:validation:MaxItems=12
	// +listType=set
	// +optional
	BuiltinSources *[]BuiltinSymbolSource `json:"builtinSources,omitempty"`
}

// SymbolSourceLayoutType selects a directory convention accepted by both supported Sentry
// versions.
// +kubebuilder:validation:Enum=native;symstore;symstore_index2;ssqp;unified;debuginfod
type SymbolSourceLayoutType string

// SymbolSourceCasing controls case conversion while resolving symbol paths.
// +kubebuilder:validation:Enum=default;uppercase;lowercase
type SymbolSourceCasing string

// SymbolSourceLayout is the portable layout contract for custom HTTP and cloud sources.
type SymbolSourceLayout struct {
	Type   SymbolSourceLayoutType `json:"type"`
	Casing SymbolSourceCasing     `json:"casing"`
}

// HTTPSymbolSource fetches symbols over HTTPS. Basic-auth credentials are optional, but a
// password can only be supplied through a Secret in the SentryProject namespace.
type HTTPSymbolSource struct {
	// +kubebuilder:validation:MinLength=9
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https://[^[:space:]]+$`
	URL string `json:"url"`

	// +kubebuilder:validation:MaxLength=256
	// +optional
	Username string `json:"username,omitempty"`

	// +optional
	PasswordSecretRef *SecretKeyReference `json:"passwordSecretRef,omitempty"`
}

// S3SymbolSource fetches symbols from one AWS S3 bucket. Both credential values remain in
// namespace-local Secrets and never enter status or protected binding state.
type S3SymbolSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Bucket string `json:"bucket"`

	// +kubebuilder:validation:Enum=us-east-2;us-east-1;us-west-1;us-west-2;ap-east-1;ap-south-1;ap-northeast-2;ap-southeast-1;ap-southeast-2;ap-northeast-1;ca-central-1;cn-north-1;cn-northwest-1;eu-central-1;eu-west-1;eu-west-2;eu-west-3;eu-north-1;sa-east-1;us-gov-east-1;us-gov-west-1
	Region string `json:"region"`

	// +kubebuilder:validation:MaxLength=512
	// +optional
	Prefix string `json:"prefix,omitempty"`

	AccessKeySecretRef SecretKeyReference `json:"accessKeySecretRef"`
	SecretKeySecretRef SecretKeyReference `json:"secretKeySecretRef"`
}

// GCSSymbolSource fetches symbols from one Google Cloud Storage bucket using a service
// account private key held in a namespace-local Secret.
type GCSSymbolSource struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=222
	Bucket string `json:"bucket"`

	// +kubebuilder:validation:MinLength=3
	// +kubebuilder:validation:MaxLength=320
	ClientEmail string `json:"clientEmail"`

	// +kubebuilder:validation:MaxLength=512
	// +optional
	Prefix string `json:"prefix,omitempty"`

	PrivateKeySecretRef SecretKeyReference `json:"privateKeySecretRef"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.http) ? 1 : 0) + (has(self.s3) ? 1 : 0) + (has(self.gcs) ? 1 : 0) == 1",message="exactly one of http, s3 or gcs is required"
// CustomSymbolSourceSpec owns one portable custom source child. The map key is its stable
// local alias; generated Sentry identities remain protected.
type CustomSymbolSourceSpec struct {
	Layout SymbolSourceLayout `json:"layout"`

	// +optional
	HTTP *HTTPSymbolSource `json:"http,omitempty"`

	// +optional
	S3 *S3SymbolSource `json:"s3,omitempty"`

	// +optional
	GCS *GCSSymbolSource `json:"gcs,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="!has(self.allowedDomains) || !(self.allowedDomains.exists(d, d == '*') && size(self.allowedDomains) > 1)",message="allowedDomains wildcard must be the only entry"
// BehaviorSettings contains bounded project behavior shared by both supported Sentry
// compatibility versions.
type BehaviorSettings struct {
	// AutoResolveHours automatically resolves an issue after this many unseen hours. Zero
	// disables automatic resolution.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2147483647
	// +optional
	AutoResolveHours *int64 `json:"autoResolveHours,omitempty"`

	// DefaultEnvironment selects the environment shown by default. An empty value clears the
	// selection without deleting any Sentry environment.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[^\n\r\f/]*$`
	// +optional
	DefaultEnvironment *string `json:"defaultEnvironment,omitempty"`

	// CrashReportLimit controls stored native crash-report attachments per issue. Minus one
	// is unlimited, zero disables storage, and positive values set an explicit limit.
	// +kubebuilder:validation:Minimum=-1
	// +kubebuilder:validation:Maximum=100
	// +optional
	CrashReportLimit *int32 `json:"crashReportLimit,omitempty"`

	// ScrapeJavaScript permits Sentry to fetch JavaScript source files and source maps.
	// +optional
	ScrapeJavaScript *bool `json:"scrapeJavaScript,omitempty"`

	// AllowedDomains owns the non-empty set of browser origins allowed to submit events.
	// Each entry is one DNS hostname or HTTP(S) origin with an optional leading wildcard
	// subdomain and fixed numeric port. A sole `*` entry allows every origin.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^(\*|((https?://)?(\*\.)?[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?)(:[0-9]{1,5})?)$`
	// +listType=set
	// +optional
	AllowedDomains *[]string `json:"allowedDomains,omitempty"`
}

// IssueOwnershipMatcherType selects one portable Sentry ownership-rule matcher.
// +kubebuilder:validation:Enum=path;module;url;tag
type IssueOwnershipMatcherType string

const (
	IssueOwnershipMatcherPath   IssueOwnershipMatcherType = "path"
	IssueOwnershipMatcherModule IssueOwnershipMatcherType = "module"
	IssueOwnershipMatcherURL    IssueOwnershipMatcherType = "url"
	IssueOwnershipMatcherTag    IssueOwnershipMatcherType = "tag"
)

// +kubebuilder:validation:XValidation:rule="self.type == 'tag' ? has(self.tagKey) : !has(self.tagKey)",message="tagKey is required only for tag matchers"
// IssueOwnershipMatcher is a bounded, structured ownership-rule matcher. Whitespace,
// comments, quoted identifiers, and raw Sentry grammar stay outside the alpha API.
type IssueOwnershipMatcher struct {
	Type IssueOwnershipMatcherType `json:"type"`

	// TagKey is the event tag name without Sentry's tags. prefix.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_.-]+$`
	// +optional
	TagKey string `json:"tagKey,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^[^[:space:]#\"]+$`
	Pattern string `json:"pattern"`
}

// IssueOwnershipRule maps one ordered matcher to readable Sentry team slugs. Direct users,
// email addresses, and raw external identities are deliberately excluded.
type IssueOwnershipRule struct {
	Matcher IssueOwnershipMatcher `json:"matcher"`

	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	// +listType=set
	Teams []string `json:"teams"`
}

// IssueOwnershipFallthrough controls unmatched issues.
// +kubebuilder:validation:Enum=AllMembers;NoOne
type IssueOwnershipFallthrough string

const (
	IssueOwnershipFallthroughAllMembers IssueOwnershipFallthrough = "AllMembers"
	IssueOwnershipFallthroughNoOne      IssueOwnershipFallthrough = "NoOne"
)

// IssueOwnershipAutoAssignment controls automatic issue assignment.
// +kubebuilder:validation:Enum=IssueOwner;SuspectCommits;Disabled
type IssueOwnershipAutoAssignment string

const (
	IssueOwnershipAutoAssignIssueOwner     IssueOwnershipAutoAssignment = "IssueOwner"
	IssueOwnershipAutoAssignSuspectCommits IssueOwnershipAutoAssignment = "SuspectCommits"
	IssueOwnershipAutoAssignDisabled       IssueOwnershipAutoAssignment = "Disabled"
)

// IssueOwnershipSpec owns the complete custom ownership-rule text and the portable
// assignment controls exposed by Sentry's public project ownership endpoint.
type IssueOwnershipSpec struct {
	// Rules are evaluated in order. An explicit empty list clears custom ownership rules.
	// +kubebuilder:validation:MaxItems=100
	// +optional
	Rules []IssueOwnershipRule `json:"rules,omitempty"`

	Fallthrough IssueOwnershipFallthrough `json:"fallthrough"`

	AutoAssignment IssueOwnershipAutoAssignment `json:"autoAssignment"`

	// CodeownersAutoSync controls synchronization of already imported CODEOWNERS data. It
	// does not import a repository or create code mappings.
	CodeownersAutoSync bool `json:"codeownersAutoSync"`
}

// CodeMappingSpec maps stack-trace paths to one connection-owned repository alias. Sentry,
// integration, repository, and mapping identities never enter the public resource.
// +kubebuilder:validation:XValidation:rule="!self.importCodeowners || size(self.defaultBranch) > 0",message="CODEOWNERS import requires defaultBranch"
type CodeMappingSpec struct {
	// Integration selects a sourceCode installation alias on the SentryConnection.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	Integration string `json:"integration"`

	// Repository selects a repository alias nested under the integration.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z](?:[a-z0-9-]*[a-z0-9])?$`
	Repository string `json:"repository"`

	// +kubebuilder:validation:MaxLength=512
	// +optional
	StackRoot string `json:"stackRoot,omitempty"`

	// +kubebuilder:validation:MaxLength=512
	// +optional
	SourceRoot string `json:"sourceRoot,omitempty"`

	// +kubebuilder:validation:MaxLength=255
	// +optional
	DefaultBranch string `json:"defaultBranch,omitempty"`

	// ImportCodeowners fetches the repository CODEOWNERS file through Sentry and owns the
	// corresponding Sentry ProjectCodeOwners child for this mapping.
	// +optional
	ImportCodeowners bool `json:"importCodeowners,omitempty"`
}

// ProjectSettings is deliberately limited to the T00 source-confirmed alpha surface.
type ProjectSettings struct {
	// +optional
	Filters *FilterSettings `json:"filters,omitempty"`

	// +optional
	PII *PIISettings `json:"pii,omitempty"`

	// +optional
	Behavior *BehaviorSettings `json:"behavior,omitempty"`

	// +optional
	Grouping *GroupingSettings `json:"grouping,omitempty"`

	// +optional
	Symbolication *SymbolicationSettings `json:"symbolication,omitempty"`
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

// +kubebuilder:validation:XValidation:rule="(has(self.slack) && !has(self.pagerDuty)) || (!has(self.slack) && has(self.pagerDuty))",message="exactly one destination type is required"
// DestinationSpec contains exactly one adapter-neutral destination selector.
type DestinationSpec struct {
	// +optional
	Slack *SlackDestination `json:"slack,omitempty"`

	// +optional
	PagerDuty *PagerDutyDestination `json:"pagerDuty,omitempty"`
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
// +kubebuilder:validation:Enum=first-seen;regression;event-count;user-count
type AlertTrigger string

const (
	AlertTriggerFirstSeen  AlertTrigger = "first-seen"
	AlertTriggerRegression AlertTrigger = "regression"
	AlertTriggerEventCount AlertTrigger = "event-count"
	AlertTriggerUserCount  AlertTrigger = "user-count"
)

// +kubebuilder:validation:XValidation:rule="self.trigger in ['event-count', 'user-count'] ? (has(self.windowMinutes) && has(self.threshold)) : (!has(self.windowMinutes) && !has(self.threshold))",message="metric triggers require windowMinutes and threshold; issue triggers forbid them"
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
	// +kubebuilder:validation:items:Pattern=`^(slack|pagerDuty)/[^/[:space:]][^/]*$`
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

// +kubebuilder:validation:XValidation:rule="!has(self.project.use) || (!has(self.settings) && !has(self.issueOwnership) && !has(self.codeMappings) && !has(self.clientKeys) && !has(self.symbolSources) && !has(self.notificationTargets) && !has(self.alerts) && !has(self.grantAccess))",message="project.use forbids settings, issueOwnership, codeMappings, clientKeys, symbolSources, notification targets, alerts and grantAccess"
// +kubebuilder:validation:XValidation:rule="!has(self.project.use) || !has(self.alertPolicy)",message="project.use forbids alertPolicy"
// +kubebuilder:validation:XValidation:rule="!has(self.project.use) || self.lifecycle.onDelete != 'DeleteProject'",message="project.use forbids lifecycle.onDelete DeleteProject"
// +kubebuilder:validation:XValidation:rule="self.role != 'Standby' || (!has(self.settings) && !has(self.issueOwnership) && !has(self.codeMappings) && !has(self.clientKeys) && !has(self.symbolSources) && !has(self.grantAccess) && self.lifecycle.onDelete != 'DeleteProject')",message="Standby forbids settings, issueOwnership, codeMappings, clientKeys, symbolSources, grantAccess and DeleteProject lifecycle"
// +kubebuilder:validation:XValidation:rule="!has(self.alerts) || self.alerts.all(k, k.matches('^[a-z](?:[a-z0-9-]*[a-z0-9])?$'))",message="alert names must be lowercase DNS labels"
// +kubebuilder:validation:XValidation:rule="!has(self.grantAccess) || !has(self.project.create) || !has(self.grantAccess.teams) || !self.grantAccess.teams.exists(t, t == self.project.create.team)",message="grantAccess.teams must not repeat project.create.team"
// +kubebuilder:validation:XValidation:rule="!has(self.issueOwnership) || (has(self.project.create) && self.issueOwnership.rules.all(r, r.teams.all(t, t == self.project.create.team || (has(self.grantAccess) && t in self.grantAccess.teams))))",message="issueOwnership teams must be the primary project team or appear in grantAccess.teams"
// +kubebuilder:validation:XValidation:rule="!has(self.dsn) || !has(self.dsn.managedClientKey) || (has(self.clientKeys) && self.dsn.managedClientKey in self.clientKeys)",message="dsn.managedClientKey must reference a declared clientKeys entry"
// +kubebuilder:validation:XValidation:rule="!has(self.dsn) || !has(self.dsn.managedClientKey) || self.clientKeys[self.dsn.managedClientKey].active",message="dsn.managedClientKey must reference an active managed key"
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

	// IssueOwnership manages team-only issue ownership rules and assignment behavior.
	// Every referenced team must be the primary project team or appear in grantAccess.
	// +optional
	IssueOwnership *IssueOwnershipSpec `json:"issueOwnership,omitempty"`

	// CodeMappings owns repository path mappings keyed by stable local aliases. An absent
	// field leaves mappings unmanaged; an explicit empty map removes owned mappings and their
	// imported CODEOWNERS children.
	// +kubebuilder:validation:MaxProperties=50
	// +optional
	CodeMappings *map[string]CodeMappingSpec `json:"codeMappings,omitempty"`

	// ClientKeys owns bounded server-generated client-key children. An absent field leaves
	// all client keys unmanaged; an explicit empty map removes protected operator-owned keys.
	// +kubebuilder:validation:MaxProperties=20
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])?$'))",message="client key names must be lowercase DNS labels no longer than 32 characters"
	// +optional
	ClientKeys *map[string]ManagedClientKeySpec `json:"clientKeys,omitempty"`

	// SymbolSources owns portable HTTP, S3, and GCS source children. An absent field leaves
	// custom sources unmanaged; an explicit empty map deletes only exact protected children.
	// +kubebuilder:validation:MaxProperties=20
	// +kubebuilder:validation:XValidation:rule="self.all(k, k.matches('^[a-z](?:[a-z0-9-]{0,30}[a-z0-9])?$'))",message="symbol source names must be lowercase DNS labels no longer than 32 characters"
	// +optional
	SymbolSources *map[string]CustomSymbolSourceSpec `json:"symbolSources,omitempty"`

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

	// ClientKeys exposes only local names and safe lifecycle state. External identities,
	// public DSNs, public key material, and secret key material remain protected or transient.
	// +mapType=granular
	// +optional
	ClientKeys map[string]ClientKeyStatus `json:"clientKeys,omitempty"`

	// SymbolSources exposes only local aliases and safe lifecycle state. Sentry source IDs,
	// credential fingerprints, and credential values remain protected or transient.
	// +mapType=granular
	// +optional
	SymbolSources map[string]SymbolSourceStatus `json:"symbolSources,omitempty"`

	// CodeMappings exposes only local aliases and safe lifecycle state. Every Sentry,
	// integration, repository, mapping, and CODEOWNERS identity remains protected.
	// +mapType=granular
	// +optional
	CodeMappings map[string]CodeMappingStatus `json:"codeMappings,omitempty"`

	// NotificationTargets exposes only local names and safe lifecycle state. External
	// integration, channel, and service identities remain protected.
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

// ClientKeyStatus is the safe status of one local managed client-key child.
type ClientKeyStatus struct {
	// +kubebuilder:validation:MaxLength=64
	State string `json:"state"`

	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`

	// RotationRevision is safe desired-state metadata, not an external identity.
	RotationRevision int64 `json:"rotationRevision"`
}

// SymbolSourceStatus is the safe status of one local custom symbol-source child.
type SymbolSourceStatus struct {
	// +kubebuilder:validation:MaxLength=64
	State string `json:"state"`

	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
}

// CodeMappingStatus is the safe status of one local repository path mapping.
type CodeMappingStatus struct {
	// +kubebuilder:validation:MaxLength=64
	State string `json:"state"`

	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
}

// DestinationStatus is the safe status for one canonical notification target key.
type DestinationStatus struct {
	// +kubebuilder:validation:Enum=slack;pagerduty
	Type string `json:"type"`

	// +kubebuilder:validation:MaxLength=64
	State string `json:"state"`

	// +kubebuilder:validation:MaxLength=64
	Reason string `json:"reason"`
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
