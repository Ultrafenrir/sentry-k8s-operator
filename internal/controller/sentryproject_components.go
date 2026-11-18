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
	"context"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryconfigurationv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	alertpolicyruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/alertpolicy"
	alertsruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/alerts"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	clientkeyruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/clientkey"
	codemappingruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/codemapping"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/destination"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/dsn"
	issueownershipruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/issueownership"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/policy"
	projectruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/project"
	projectaccessruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/projectaccess"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
	settingsruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/settings"
	symbolsourceruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/symbolsource"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/validation"
)

func dsnCondition(applicable bool, externalNameChanged bool, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "DSN publication was not requested for this read-only project."
	}
	if err != nil {
		if errors.Is(err, dsn.ErrPinnedKeyLost) {
			return metav1.ConditionFalse, "DSNKeyRevoked", "The output Secret was removed after the pinned client key was proven missing or inactive."
		}
		return metav1.ConditionFalse, reasonForError(err), "DSN reconciliation is safely blocked; an existing working Secret was preserved unless revoke was proven."
	}
	if externalNameChanged {
		return metav1.ConditionTrue, "ExternalNameChanged", "The pinned client key was renamed in Sentry; its verified identity and output Secret remain current."
	}
	return metav1.ConditionTrue, "DSNReady", "The managed output Secret is current."
}

func clientKeysCondition(applicable bool, result clientkeyruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No managed client keys are configured."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil && status.State == clientkeyruntime.StateReady {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "ClientKeysPartiallyApplied", "At least one managed client key failed; independently verified keys were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Managed client keys could not be safely reconciled; protected identities were preserved."
	}
	for _, status := range result.Statuses {
		if status.State == clientkeyruntime.StateRotationPending {
			return metav1.ConditionFalse, "ClientKeyCutoverPending", "A replacement key is verified and waiting for public DSN cutover before guarded old-key deletion."
		}
	}
	return metav1.ConditionTrue, "ClientKeysReady", "All managed client keys match the requested lifecycle state."
}

func symbolSourcesCondition(applicable bool, result symbolsourceruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No custom symbol sources are configured."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil && status.State == symbolsourceruntime.StateReady {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "SymbolSourcesPartiallyApplied", "At least one custom symbol source failed; independently verified sources were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Custom symbol sources could not be safely reconciled; protected identities were preserved."
	}
	return metav1.ConditionTrue, "SymbolSourcesReady", "All managed custom symbol sources match the requested lifecycle state."
}

func codeMappingsCondition(applicable bool, result codemappingruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No repository code mappings are configured."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil && status.State == codemappingruntime.StateReady {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "CodeMappingsPartiallyApplied", "At least one code mapping failed; independently verified mappings were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Repository code mappings could not be safely reconciled; protected identities were preserved."
	}
	return metav1.ConditionTrue, "CodeMappingsReady", "All managed repository code mappings and requested CODEOWNERS imports match desired state."
}

func settingsCondition(applicable bool, result settingsruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No typed project settings are managed."
	}
	if err != nil {
		if (result.Filters.Managed && result.Filters.Err == nil) || (result.PII.Managed && result.PII.Err == nil) || (result.Behavior.Managed && result.Behavior.Err == nil) || (result.Grouping.Managed && result.Grouping.Err == nil) || (result.Symbolication.Managed && result.Symbolication.Err == nil) || (result.Notifications.Managed && result.Notifications.Err == nil) {
			return metav1.ConditionFalse, "SettingsPartiallyApplied", "At least one independent settings component failed; successful components were preserved."
		}
		return metav1.ConditionFalse, reasonForError(err), "Typed project settings could not be fully reconciled."
	}
	return metav1.ConditionTrue, "SettingsReady", "All requested typed project settings match desired state."
}

func issueOwnershipCondition(applicable bool, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No issue ownership configuration is managed."
	}
	if err != nil {
		return metav1.ConditionFalse, reasonForError(err), "Issue ownership configuration could not be safely reconciled."
	}
	return metav1.ConditionTrue, "IssueOwnershipReady", "Team-only issue ownership rules and assignment behavior match the requested state."
}

func projectAccessCondition(applicable bool, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No additional project team access is managed."
	}
	if err != nil {
		return metav1.ConditionFalse, reasonForError(err), "Additional project team access could not be fully reconciled."
	}
	return metav1.ConditionTrue, "GrantAccessReady", "All requested additional teams have project access."
}

func destinationCondition(applicable bool, result destination.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "No notification destinations are configured."
	}
	if err != nil {
		if result.RevokeOnly {
			return metav1.ConditionFalse, "DestinationAccessRevoked", "Destination access was revoked; only protected cleanup state remains."
		}
		return metav1.ConditionFalse, reasonForError(err), "One or more destinations could not be safely resolved; pinned identities were preserved."
	}
	for _, status := range result.Statuses {
		if status.State == destination.StateCleanupPending {
			return metav1.ConditionFalse, "DestinationCleanupPending", "Removed destination routing cleanup is pending."
		}
	}
	return metav1.ConditionTrue, "NotificationTargetsReady", "All configured notification targets have verified pinned identities."
}

func alertCondition(applicable bool, result alertsruntime.Result, err error) (metav1.ConditionStatus, string, string) {
	if !applicable {
		return metav1.ConditionUnknown, "NotApplicable", "Alert management was not requested."
	}
	if err != nil {
		ready := 0
		for _, status := range result.Statuses {
			if status.Err == nil {
				ready++
			}
		}
		if ready > 0 {
			return metav1.ConditionFalse, "AlertsPartiallyApplied", "At least one alert failed; independently verified alerts were preserved."
		}
		return metav1.ConditionFalse, "AlertsNotReady", "Alerts could not be safely reconciled; protected identities were preserved."
	}
	return metav1.ConditionTrue, "AlertsReady", "All managed alerts match the requested state."
}

type dsnReconcileResult struct {
	Secret              string
	ClientKey           string
	Phase               dsn.Phase
	ExternalNameChanged bool
}

func (r *SentryProjectReconciler) reconcileProjectAccess(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) projectaccessruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return projectaccessruntime.Result{Err: err}
	}
	if resource.Spec.GrantAccess == nil && len(record.Binding.GrantedTeams) == 0 {
		return projectaccessruntime.Result{Bindings: map[string]string{}}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return projectaccessruntime.Result{Bindings: record.Binding.GrantedTeams, Err: projectaccessruntime.ErrInvalidInput}
	}
	var adapter projectaccessruntime.Adapter
	if r.NewProjectAccessAdapter != nil {
		adapter = r.NewProjectAccessAdapter(apiClient)
	} else {
		adapter = &projectaccessruntime.HTTPAdapter{Client: apiClient}
	}
	desiredTeams, policyErr := policy.ProjectAccessTeams(resource.Spec)
	result := (&projectaccessruntime.Engine{Adapter: adapter, Audit: r.Audit}).Reconcile(ctx, projectaccessruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug,
		PrimaryTeam: resource.Spec.Team, Desired: desiredTeams, Current: record.Binding.GrantedTeams,
		LocalResource: resource.Namespace + "/" + resource.Name, Generation: resource.Generation,
		Authorize: r.projectAccessAuthorizer(resource, connection, record.Binding, desiredTeams),
	})
	result.Err = errors.Join(result.Err, policyErr)
	if reflect.DeepEqual(record.Binding.GrantedTeams, result.Bindings) {
		return result
	}
	next := record.Binding
	next.GrantedTeams = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Err = errors.Join(result.Err, err)
	}
	return result
}

func (r *SentryProjectReconciler) projectAccessAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding, expectedTeams []string) projectaccessruntime.Authorization {
	return func(ctx context.Context) error {
		if err := r.dsnAuthorizer(original, connection, expected)(ctx); err != nil {
			return err
		}
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latest); err != nil {
			return err
		}
		if !original.DeletionTimestamp.IsZero() {
			if latest.DeletionTimestamp.IsZero() || original.Spec.GrantAccess == nil || len(original.Spec.GrantAccess.Teams) != 0 || len(expectedTeams) != 0 {
				return projectaccessruntime.ErrAuthorization
			}
		} else {
			if !reflect.DeepEqual(latest.Spec.GrantAccess, original.Spec.GrantAccess) {
				return projectaccessruntime.ErrAuthorization
			}
			latestTeams, policyErr := policy.ProjectAccessTeams(latest.Spec)
			if policyErr != nil || !reflect.DeepEqual(latestTeams, expectedTeams) {
				return projectaccessruntime.ErrAuthorization
			}
		}
		state, err := r.Bindings.GetBinding(ctx, string(original.UID))
		if err != nil || !reflect.DeepEqual(state.Binding.GrantedTeams, expected.GrantedTeams) {
			return projectaccessruntime.ErrAuthorization
		}
		return nil
	}
}

func (r *SentryProjectReconciler) reconcileClientKeys(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) clientkeyruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return clientkeyruntime.Result{Statuses: map[string]clientkeyruntime.Status{"": {State: clientkeyruntime.StateError, Reason: "ClientKeyStateUnavailable", Err: err}}}
	}
	if resource.Spec.ClientKeys == nil {
		return clientkeyruntime.Result{Bindings: record.Binding.ClientKeys, Statuses: map[string]clientkeyruntime.Status{}, PublicDSNs: map[string]string{}}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return clientkeyruntime.Result{Bindings: record.Binding.ClientKeys, Statuses: map[string]clientkeyruntime.Status{"": {State: clientkeyruntime.StateError, Reason: "InvalidClientKeyInput", Err: clientkeyruntime.ErrInvalidInput}}, PublicDSNs: map[string]string{}}
	}
	desired := make(map[string]clientkeyruntime.Desired, len(*resource.Spec.ClientKeys))
	for name, spec := range *resource.Spec.ClientKeys {
		revision := spec.RotationRevision
		if revision == 0 {
			revision = 1
		}
		value := clientkeyruntime.Desired{Active: spec.Active, RotationRevision: revision}
		if spec.RateLimit != nil {
			value.RateLimit = &clientkeyruntime.RateLimit{Count: spec.RateLimit.Count, Window: spec.RateLimit.WindowSeconds}
		}
		if spec.Loader != nil {
			value.Loader = &clientkeyruntime.LoaderOptions{
				HasPerformance:    spec.Loader.Tracing,
				HasReplay:         spec.Loader.Replay,
				HasLogsAndMetrics: spec.Loader.LogsAndMetrics,
				HasFeedback:       spec.Loader.UserFeedback,
				HasDebug:          spec.Loader.Debug,
			}
		}
		desired[name] = value
	}
	promote := make(map[string]bool, len(desired))
	selected := ""
	for name, spec := range *resource.Spec.ClientKeys {
		if spec.PublishDSN {
			selected = name
			break
		}
	}
	for name, current := range record.Binding.ClientKeys {
		if current.ReplacementIdentity == "" {
			continue
		}
		promote[name] = name != selected || record.Binding.DSN != nil && record.Binding.DSN.KeyIdentity == current.ReplacementIdentity
	}
	adapter := clientkeyruntime.Adapter(&clientkeyruntime.HTTPAdapter{Client: apiClient})
	if r.NewClientKeyAdapter != nil {
		adapter = r.NewClientKeyAdapter(apiClient)
	}
	result := (&clientkeyruntime.Engine{Adapter: adapter, Journal: r.Bindings}).Reconcile(ctx, clientkeyruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug, ProjectUID: string(resource.UID),
		Desired: desired, Current: record.Binding.ClientKeys, Promote: promote,
		Authorize: clientkeyruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
	if reflect.DeepEqual(record.Binding.ClientKeys, result.Bindings) {
		return result
	}
	next := record.Binding
	next.ClientKeys = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = clientkeyruntime.Status{State: clientkeyruntime.StateError, Reason: "ClientKeyStatePersistFailed", Err: err}
	}
	return result
}

var errSymbolSourceSecret = errors.New("symbol-source Secret input is unavailable")

func (r *SentryProjectReconciler) reconcileSymbolSources(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) symbolsourceruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return symbolsourceruntime.Result{Bindings: map[string]binding.SymbolSourceBinding{}, Statuses: map[string]symbolsourceruntime.Status{"": {State: symbolsourceruntime.StateError, Reason: "SymbolSourceStateUnavailable", Err: err}}}
	}
	if resource.Spec.SymbolSources == nil {
		return symbolsourceruntime.Result{Bindings: record.Binding.SymbolSources, Statuses: map[string]symbolsourceruntime.Status{}}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return symbolsourceruntime.Result{Bindings: record.Binding.SymbolSources, Statuses: map[string]symbolsourceruntime.Status{"": {State: symbolsourceruntime.StateError, Reason: "InvalidSymbolSource", Err: symbolsourceruntime.ErrInvalidInput}}}
	}
	desired, err := r.loadSymbolSourceDesired(ctx, resource)
	if err != nil {
		return symbolsourceruntime.Result{Bindings: record.Binding.SymbolSources, Statuses: map[string]symbolsourceruntime.Status{"": {State: symbolsourceruntime.StateError, Reason: "SymbolSourceSecretUnavailable", Err: err}}}
	}
	adapter := symbolsourceruntime.Adapter(&symbolsourceruntime.HTTPAdapter{Client: apiClient})
	if r.NewSymbolSourceAdapter != nil {
		adapter = r.NewSymbolSourceAdapter(apiClient)
	}
	result := (&symbolsourceruntime.Engine{Adapter: adapter}).Reconcile(ctx, symbolsourceruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug, ProjectUID: string(resource.UID),
		Desired: desired, Current: record.Binding.SymbolSources,
		Authorize: r.dsnAuthorizer(resource, connection, record.Binding),
	})
	if reflect.DeepEqual(record.Binding.SymbolSources, result.Bindings) {
		return result
	}
	next := record.Binding
	next.SymbolSources = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = symbolsourceruntime.Status{State: symbolsourceruntime.StateError, Reason: "SymbolSourceStatePersistFailed", Err: err}
	}
	return result
}

func (r *SentryProjectReconciler) reconcileCodeMappings(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) codemappingruntime.Result {
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return codemappingruntime.Result{Bindings: map[string]binding.CodeMappingBinding{}, Statuses: map[string]codemappingruntime.Status{"": {State: codemappingruntime.StateError, Reason: "CodeMappingStateUnavailable", Err: err}}}
	}
	if resource.Spec.CodeMappings == nil {
		return codemappingruntime.Result{Bindings: record.Binding.CodeMappings, Statuses: map[string]codemappingruntime.Status{}}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return codemappingruntime.Result{Bindings: record.Binding.CodeMappings, Statuses: map[string]codemappingruntime.Status{"": {State: codemappingruntime.StateError, Reason: "InvalidCodeMapping", Err: codemappingruntime.ErrInvalidInput}}}
	}
	desired, err := codeMappingDesired(*resource.Spec.CodeMappings, connection.Spec.Integrations)
	if err != nil {
		return codemappingruntime.Result{Bindings: record.Binding.CodeMappings, Statuses: map[string]codemappingruntime.Status{"": {State: codemappingruntime.StateError, Reason: "SourceCodeAliasUnresolved", Err: err}}}
	}
	adapter := codemappingruntime.Adapter(&codemappingruntime.HTTPAdapter{Client: apiClient})
	if r.NewCodeMappingAdapter != nil {
		adapter = r.NewCodeMappingAdapter(apiClient)
	}
	result := (&codemappingruntime.Engine{Adapter: adapter, Journal: r.Bindings}).Reconcile(ctx, codemappingruntime.Input{
		Organization: connection.Spec.Organization, ProjectSlug: record.Binding.Slug, ProjectIdentity: record.Binding.ExternalIdentity, ProjectUID: string(resource.UID),
		Desired: desired, Current: record.Binding.CodeMappings, Authorize: codemappingruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
	if reflect.DeepEqual(record.Binding.CodeMappings, result.Bindings) {
		return result
	}
	next := record.Binding
	next.CodeMappings = result.Bindings
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = codemappingruntime.Status{State: codemappingruntime.StateError, Reason: "CodeMappingStatePersistFailed", Err: err}
	}
	return result
}

func codeMappingDesired(specs map[string]sentryconfigurationv1alpha1.CodeMappingSpec, integrations *sentryconfigurationv1alpha1.ConnectionIntegrations) (map[string]codemappingruntime.Desired, error) {
	if len(specs) > 0 && (integrations == nil || integrations.SourceCode == nil) {
		return nil, codemappingruntime.ErrCatalogMissing
	}
	result := make(map[string]codemappingruntime.Desired, len(specs))
	for name, spec := range specs {
		var installation *sentryconfigurationv1alpha1.SourceCodeIntegrationCatalogEntry
		for index := range integrations.SourceCode.Installations {
			candidate := &integrations.SourceCode.Installations[index]
			if candidate.Name == spec.Integration {
				if installation != nil {
					return nil, codemappingruntime.ErrCatalogMissing
				}
				installation = candidate
			}
		}
		if installation == nil {
			return nil, codemappingruntime.ErrCatalogMissing
		}
		repositoryName := ""
		for _, repository := range installation.Repositories {
			if repository.Name == spec.Repository {
				if repositoryName != "" {
					return nil, codemappingruntime.ErrCatalogMissing
				}
				repositoryName = repository.SentryName
			}
		}
		if repositoryName == "" {
			return nil, codemappingruntime.ErrCatalogMissing
		}
		result[name] = codemappingruntime.Desired{IntegrationAlias: spec.Integration, Provider: string(installation.Provider), IntegrationName: installation.SentryName, RepositoryAlias: spec.Repository, RepositoryName: repositoryName, StackRoot: spec.StackRoot, SourceRoot: spec.SourceRoot, DefaultBranch: spec.DefaultBranch, ImportCodeowners: spec.ImportCodeowners}
	}
	return result, nil
}

func (r *SentryProjectReconciler) loadSymbolSourceDesired(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject) (map[string]symbolsourceruntime.Desired, error) {
	result := make(map[string]symbolsourceruntime.Desired, len(*resource.Spec.SymbolSources))
	for name, spec := range *resource.Spec.SymbolSources {
		desired := symbolsourceruntime.Desired{Layout: symbolsourceruntime.Layout{Type: string(spec.Layout.Type), Casing: string(spec.Layout.Casing)}}
		types := 0
		if spec.HTTP != nil {
			types++
			desired.Type = "http"
			desired.URL = spec.HTTP.URL
			desired.Username = spec.HTTP.Username
			if spec.HTTP.PasswordSecretRef != nil {
				value, err := r.loadProjectSecretKey(ctx, resource.Namespace, *spec.HTTP.PasswordSecretRef)
				if err != nil {
					return nil, err
				}
				desired.Password = value
			}
		}
		if spec.S3 != nil {
			types++
			desired.Type = "s3"
			desired.Bucket = spec.S3.Bucket
			desired.Region = spec.S3.Region
			desired.Prefix = spec.S3.Prefix
			accessKey, err := r.loadProjectSecretKey(ctx, resource.Namespace, spec.S3.AccessKeySecretRef)
			if err != nil {
				return nil, err
			}
			secretKey, err := r.loadProjectSecretKey(ctx, resource.Namespace, spec.S3.SecretKeySecretRef)
			if err != nil {
				return nil, err
			}
			desired.AccessKey = accessKey
			desired.SecretKey = secretKey
		}
		if spec.GCS != nil {
			types++
			desired.Type = "gcs"
			desired.Bucket = spec.GCS.Bucket
			desired.ClientEmail = spec.GCS.ClientEmail
			desired.Prefix = spec.GCS.Prefix
			privateKey, err := r.loadProjectSecretKey(ctx, resource.Namespace, spec.GCS.PrivateKeySecretRef)
			if err != nil {
				return nil, err
			}
			desired.PrivateKey = privateKey
		}
		if types != 1 {
			return nil, symbolsourceruntime.ErrInvalidInput
		}
		result[name] = desired
	}
	return result, nil
}

func (r *SentryProjectReconciler) loadProjectSecretKey(ctx context.Context, namespace string, ref sentryconfigurationv1alpha1.SecretKeyReference) (string, error) {
	if namespace == "" || ref.Name != sentryconfigurationv1alpha1.ProjectInputSecretName || ref.Key == "" || strings.ContainsAny(ref.Key, "\r\n\x00") {
		return "", errSymbolSourceSecret
	}
	secret := &corev1.Secret{}
	if err := r.outputSecrets().Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		return "", errSymbolSourceSecret
	}
	value, found := secret.Data[ref.Key]
	if !found || len(value) == 0 {
		return "", errSymbolSourceSecret
	}
	return string(value), nil
}

func (r *SentryProjectReconciler) reconcileDSN(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, clientKeys clientkeyruntime.Result) (dsnReconcileResult, error) {
	current := dsnReconcileResult{Secret: resource.Status.DSNSecret, ClientKey: resource.Status.DSNClientKey}
	if projectResult.AccessMode == binding.AccessReadOnly {
		return dsnReconcileResult{}, nil
	}
	decision, err := policy.EvaluateProject(resource.Namespace, resource.Spec, connection.Spec)
	if err != nil {
		return current, err
	}
	if projectResult.AccessMode == binding.AccessReadOnly && !decision.ConsumeDSN {
		return current, dsn.ErrAuthorization
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return current, err
	}
	current.Secret = existingSecret(record.Binding.DSN)
	selector := ""
	if resource.Spec.ExistingClientKey != nil {
		selector = resource.Spec.ExistingClientKey.Name
	}
	allowInsecureHTTP := connection.Spec.Transport != nil && connection.Spec.Transport.AllowInsecureHTTP
	var selection dsn.Selection
	managedSelector := ""
	if resource.Spec.ClientKeys != nil {
		for name, spec := range *resource.Spec.ClientKeys {
			if spec.PublishDSN {
				managedSelector = name
				break
			}
		}
	}
	if managedSelector != "" {
		status, found := clientKeys.Statuses[managedSelector]
		if !found || status.Err != nil {
			return current, clientKeys.Err()
		}
		managed, found := record.Binding.ClientKeys[managedSelector]
		if !found {
			return current, clientkeyruntime.ErrExternalMissing
		}
		identity := managed.ExternalIdentity
		if managed.ReplacementIdentity != "" {
			identity = managed.ReplacementIdentity
		}
		publicDSN := clientKeys.PublicDSNs[managedSelector]
		key := dsn.ClientKey{ID: identity, Name: managedSelector, Label: managedSelector, IsActive: true}
		key.DSN.Public = publicDSN
		selection, err = dsn.SelectManaged(key, managedSelector, record.Binding.DSN, allowInsecureHTTP)
	} else {
		listKeys := r.ListKeys
		if listKeys == nil {
			listKeys = func(ctx context.Context, client *sentryapi.Client, organization, projectSlug string) ([]dsn.ClientKey, error) {
				return (&dsn.KeyReader{Client: client}).List(ctx, organization, projectSlug)
			}
		}
		var keys []dsn.ClientKey
		keys, err = listKeys(ctx, apiClient, connection.Spec.Organization, record.Binding.Slug)
		if err == nil {
			selection, err = dsn.SelectWithTransport(keys, selector, record.Binding.DSN, allowInsecureHTTP)
		}
	}
	if err != nil {
		if errors.Is(err, dsn.ErrPinnedKeyLost) && record.Binding.DSN != nil {
			expected := *record.Binding.DSN
			if revokeErr := (&dsn.Publisher{Client: r.outputSecrets()}).Revoke(ctx, resource, expected, r.dsnAuthorizer(resource, connection, record.Binding)); revokeErr != nil {
				return current, revokeErr
			}
			next := record.Binding
			nextDSN := expected
			nextDSN.CurrentSecret = ""
			nextDSN.PendingSecret = ""
			next.DSN = &nextDSN
			if _, updateErr := r.Bindings.UpdateBinding(ctx, record, next); updateErr != nil {
				return current, updateErr
			}
			return dsnReconcileResult{}, dsn.ErrPinnedKeyLost
		}
		return current, err
	}
	nextDSN := selection.Binding
	if record.Binding.DSN != nil {
		nextDSN.CurrentSecret = record.Binding.DSN.CurrentSecret
		nextDSN.PendingSecret = record.Binding.DSN.PendingSecret
	}
	authorize := r.dsnAuthorizer(resource, connection, record.Binding)
	nextDSN, phase, err := (&dsn.Publisher{Client: r.outputSecrets(), AllowInsecureHTTP: allowInsecureHTTP}).Reconcile(ctx, resource, selection.Key.DSN.Public, nextDSN, authorize)
	if err != nil {
		return current, err
	}
	nextBinding := record.Binding
	nextBinding.DSN = &nextDSN
	if _, err := r.Bindings.UpdateBinding(ctx, record, nextBinding); err != nil {
		return current, err
	}
	return dsnReconcileResult{
		Secret: nextDSN.CurrentSecret, ClientKey: selection.ReadableName, Phase: phase,
		ExternalNameChanged: selection.ExternalRenamed,
	}, nil
}

func (r *SentryProjectReconciler) dsnAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding) dsn.Authorization {
	return func(ctx context.Context) error {
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latest); err != nil {
			return err
		}
		deletionCleanup := !original.DeletionTimestamp.IsZero()
		if latest.UID != original.UID || latest.Generation != original.Generation || latest.DeletionTimestamp.IsZero() != original.DeletionTimestamp.IsZero() || (!deletionCleanup && latest.Spec.Paused) {
			return dsn.ErrAuthorization
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		if err := validation.Project(latest.Spec); err != nil {
			return dsn.ErrAuthorization
		}
		decision, err := policy.EvaluateProject(latest.Namespace, latest.Spec, latestConnection.Spec)
		if err != nil {
			return err
		}
		state, err := r.Bindings.GetBinding(ctx, string(latest.UID))
		if err != nil || state.Binding.ExternalIdentity != expected.ExternalIdentity || state.Binding.Connection != connection.Name || state.Binding.AccessMode != expected.AccessMode {
			return dsn.ErrAuthorization
		}
		if err := r.validateRegionalRevision(ctx, latestConnection, state.Binding); err != nil {
			return err
		}
		if state.Binding.AccessMode == binding.AccessReadOnly {
			return dsn.ErrAuthorization
		}
		claim, err := r.Bindings.GetClaim(ctx, expected.Installation, expected.Organization, expected.Slug)
		if err != nil || decision.AccessMode != binding.AccessManage || claim.Claim.Manager.UID != string(latest.UID) || claim.Claim.ExternalIdentity != expected.ExternalIdentity || claim.Claim.Handoff != nil {
			return dsn.ErrAuthorization
		}
		return nil
	}
}

func (r *SentryProjectReconciler) reconcileSettings(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result) settingsruntime.Result {
	if resource.Spec.Settings == nil {
		return settingsruntime.Result{}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return failedSettingsResult(resource.Spec.Settings, settingsruntime.ErrInvalidDesiredState)
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return failedSettingsResult(resource.Spec.Settings, err)
	}
	factory := r.NewSettingsAdapter
	if factory == nil {
		factory = func(client *sentryapi.Client) settingsruntime.Adapter {
			return &settingsruntime.HTTPAdapter{Client: client}
		}
	}
	return (&settingsruntime.Engine{Adapter: factory(apiClient)}).Reconcile(ctx, settingsruntime.Input{
		Organization: connection.Spec.Organization,
		ProjectSlug:  record.Binding.Slug,
		Desired:      resource.Spec.Settings,
		Modern:       connection.Spec.Compatibility.Alerts == sentryconfigurationv1alpha1.AlertCompatibilityModern26,
		Authorize:    settingsruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
}

func failedSettingsResult(desired *sentryconfigurationv1alpha1.ProjectSettings, err error) settingsruntime.Result {
	failed := func(managed bool) settingsruntime.Component {
		if !managed {
			return settingsruntime.Component{}
		}
		return settingsruntime.Component{Managed: true, Err: err}
	}
	return settingsruntime.Result{
		Filters:       failed(desired.Filters != nil),
		PII:           failed(desired.PII != nil),
		Behavior:      failed(desired.Behavior != nil),
		Grouping:      failed(desired.Grouping != nil),
		Symbolication: failed(desired.Symbolication != nil),
		Notifications: failed(desired.Notifications != nil),
	}
}

func (r *SentryProjectReconciler) reconcileIssueOwnership(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, projectAccessErr error) issueownershipruntime.Result {
	if resource.Spec.IssueOwnership == nil {
		return issueownershipruntime.Result{}
	}
	if projectResult.AccessMode != binding.AccessManage {
		return issueownershipruntime.Result{Managed: true, Err: issueownershipruntime.ErrInvalidDesiredState}
	}
	if projectAccessErr != nil {
		return issueownershipruntime.Result{Managed: true, Err: errors.Join(issueownershipruntime.ErrAuthorization, projectAccessErr)}
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return issueownershipruntime.Result{Managed: true, Err: err}
	}
	allowedTeams, policyErr := policy.IssueOwnershipTeams(resource.Spec)
	if policyErr != nil {
		return issueownershipruntime.Result{Managed: true, Err: errors.Join(issueownershipruntime.ErrAuthorization, policyErr)}
	}
	adapter := issueownershipruntime.Adapter(&issueownershipruntime.HTTPAdapter{Client: apiClient})
	if r.NewIssueOwnershipAdapter != nil {
		adapter = r.NewIssueOwnershipAdapter(apiClient)
	}
	return (&issueownershipruntime.Engine{Adapter: adapter}).Reconcile(ctx, issueownershipruntime.Input{
		Organization: connection.Spec.Organization,
		ProjectSlug:  record.Binding.Slug,
		Desired:      resource.Spec.IssueOwnership,
		AllowedTeams: allowedTeams,
		Authorize:    issueownershipruntime.Authorization(r.dsnAuthorizer(resource, connection, record.Binding)),
	})
}

func (r *SentryProjectReconciler) reconcileDestinations(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, application alertpolicyruntime.Result) destination.Result {
	if !canManageChildren(resource, projectResult) {
		return destination.Result{}
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return destination.Result{PolicyErr: err}
	}
	if resource.Spec.NotificationTargets == nil && len(record.Binding.Destinations) == 0 {
		return destination.Result{}
	}
	var catalog destination.Catalog
	if r.NewDestinationCatalog != nil {
		catalog = r.NewDestinationCatalog(apiClient)
	} else {
		catalog = &destination.HTTPCatalog{Client: apiClient}
	}
	catalog = &destination.AliasCatalog{Catalog: catalog, Integrations: connection.Spec.Integrations}
	result := (&destination.Engine{Catalog: catalog}).Reconcile(ctx, destination.Input{
		Namespace:        resource.Namespace,
		Project:          resource.Spec,
		Connection:       connection.Spec,
		Current:          record.Binding.Destinations,
		Owners:           application.TargetOwners,
		AllowedOwnerUIDs: allowedChildOwners(resource, record.Binding, application),
	})
	if reflect.DeepEqual(record.Binding.Destinations, result.Bindings) && reflect.DeepEqual(record.Binding.AlertPolicy, appliedPolicyBinding(application)) && reflect.DeepEqual(record.Binding.AlertPolicies, appliedPolicyBindings(application)) {
		return result
	}
	if err := r.destinationAuthorizer(resource, connection, record.Binding, result)(ctx); err != nil {
		result.PolicyErr = err
		return result
	}
	next := record.Binding
	next.Destinations = result.Bindings
	next.AlertPolicy = appliedPolicyBinding(application)
	next.AlertPolicies = appliedPolicyBindings(application)
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.PolicyErr = err
	}
	return result
}

func (r *SentryProjectReconciler) reconcileAlerts(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, apiClient *sentryapi.Client, projectResult projectruntime.Result, destinationCleanup []string, application alertpolicyruntime.Result) alertsruntime.Result {
	if !canManageChildren(resource, projectResult) {
		if resource.Spec.Alerts == nil {
			return alertsruntime.Result{}
		}
		return alertsruntime.Result{Statuses: map[string]alertsruntime.Status{"": {State: alertsruntime.StateError, Reason: "InvalidAlertInput", Err: alertsruntime.ErrInvalidInput}}}
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return alertsruntime.Result{Statuses: map[string]alertsruntime.Status{"": {State: alertsruntime.StateError, Reason: "AlertStateUnavailable", Err: err}}}
	}
	if resource.Spec.Alerts == nil && len(record.Binding.Alerts) == 0 {
		return alertsruntime.Result{}
	}
	desiredCount := 0
	if resource.Spec.Alerts != nil {
		desiredCount = len(*resource.Spec.Alerts)
	}
	desired := make(map[string]alertsruntime.Desired, desiredCount)
	if resource.Spec.Alerts != nil {
		for name, alert := range *resource.Spec.Alerts {
			converted, err := alertDesiredFromSpec(alert, application.AlertOwners[name], connection.Spec.Integrations)
			if err != nil {
				return alertsruntime.Result{Statuses: map[string]alertsruntime.Status{name: {State: alertsruntime.StateError, Reason: "InvalidAlertInput", Err: err}}}
			}
			desired[name] = converted
		}
	}

	legacy := &alertsruntime.LegacyRouterAdapter{Legacy: &alertsruntime.LegacyHTTPAdapter{Client: apiClient, Destinations: record.Binding.Destinations}}
	modern := &alertsruntime.ModernHTTPAdapter{Client: apiClient, Destinations: record.Binding.Destinations}
	var adapter alertsruntime.Adapter = &alertsruntime.MuxAdapter{Adapters: map[string]alertsruntime.Adapter{
		alertsruntime.LegacyCombinedAdapter:    legacy,
		alertsruntime.ModernMetricIssueAdapter: modern,
	}}
	if r.NewAlertAdapter != nil {
		adapter = r.NewAlertAdapter(apiClient, record.Binding.Destinations)
	}
	capabilities := alertCapabilities(connection.Spec)
	marker := binding.Marker{}
	if record.Binding.Marker != nil {
		marker = *record.Binding.Marker
	}
	result := (&alertsruntime.Engine{Adapter: adapter, Journal: r.Bindings, Audit: r.Audit}).Reconcile(ctx, alertsruntime.Input{
		Organization:       connection.Spec.Organization,
		ProjectSlug:        record.Binding.Slug,
		Manage:             resource.Spec.Alerts != nil,
		Desired:            desired,
		Current:            record.Binding.Alerts,
		Tombstones:         record.Binding.Tombstones,
		Destinations:       record.Binding.Destinations,
		DestinationCleanup: destinationCleanup,
		PinnedAdapter: func() string {
			if record.Binding.PinnedAdapters == nil {
				return ""
			}
			return record.Binding.PinnedAdapters["alerts"]
		}(),
		Capabilities:     capabilities,
		Marker:           marker,
		Authorize:        r.alertAuthorizer(resource, connection, record.Binding),
		AllowedOwnerUIDs: allowedChildOwners(resource, record.Binding, application),
	})
	if resource.Spec.Alerts == nil {
		return result
	}
	next := record.Binding
	next.Alerts = result.Bindings
	next.Tombstones = result.Tombstones
	next.PinnedAdapters = cloneStringMap(record.Binding.PinnedAdapters)
	if result.PinnedAdapter != "" {
		next.PinnedAdapters["alerts"] = result.PinnedAdapter
	}
	next.AlertPolicy = appliedPolicyBinding(application)
	next.AlertPolicies = appliedPolicyBindings(application)
	if reflect.DeepEqual(record.Binding.Alerts, next.Alerts) && reflect.DeepEqual(record.Binding.Tombstones, next.Tombstones) && reflect.DeepEqual(record.Binding.PinnedAdapters, next.PinnedAdapters) && reflect.DeepEqual(record.Binding.AlertPolicy, next.AlertPolicy) && reflect.DeepEqual(record.Binding.AlertPolicies, next.AlertPolicies) {
		return result
	}
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		result.Statuses[""] = alertsruntime.Status{State: alertsruntime.StateError, Reason: "AlertStatePersistFailed", Err: err}
	}
	return result
}

func alertDesiredFromSpec(alert sentryconfigurationv1alpha1.AlertSpec, owner binding.ChildOwner, integrations *sentryconfigurationv1alpha1.ConnectionIntegrations) (alertsruntime.Desired, error) {
	enabled := true
	if alert.Enabled != nil {
		enabled = *alert.Enabled
	}
	value := alertsruntime.Desired{
		Owner: owner, Trigger: alertsruntime.Trigger(alert.Trigger), Enabled: enabled,
		Notify: append([]string(nil), alert.Notify...), OwnerTeam: alert.OwnerTeam,
		Description: alert.Description, ThrottleMinutes: alert.ThrottleMinutes,
	}
	if alert.WindowMinutes != nil {
		value.WindowMinutes = int(*alert.WindowMinutes)
	}
	if alert.Threshold != nil {
		value.Threshold = *alert.Threshold
	}
	for _, action := range alert.Actions {
		converted := alertsruntime.ActionDesired{
			Destination: action.Destination, Status: string(action.Status),
			PagerDutySeverity: string(action.PagerDutySeverity),
		}
		if action.Email != nil {
			converted.EmailTarget = string(action.Email.Target)
			converted.EmailTeam = action.Email.Team
		}
		if action.GitHubIssue != nil {
			github, err := githubIssueDesired(*action.GitHubIssue, integrations)
			if err != nil {
				return alertsruntime.Desired{}, err
			}
			converted.GitHubIssue = github
		}
		value.Actions = append(value.Actions, converted)
	}
	if alert.Issue != nil {
		issue := &alertsruntime.IssueDesired{
			TriggerMatch: string(alert.Issue.TriggerMatch), FilterMatch: string(alert.Issue.FilterMatch),
			Environment: alert.Issue.Environment,
		}
		for _, trigger := range alert.Issue.Triggers {
			converted := alertsruntime.IssueTriggerDesired{Type: alertsruntime.Trigger(trigger.Type)}
			if trigger.WindowMinutes != nil {
				converted.WindowMinutes = *trigger.WindowMinutes
			}
			if trigger.Threshold != nil {
				converted.Threshold = float64(*trigger.Threshold)
			}
			issue.Triggers = append(issue.Triggers, converted)
		}
		for _, filter := range alert.Issue.Filters {
			issue.Filters = append(issue.Filters, alertsruntime.IssueFilterDesired{Type: string(filter.Type), Operator: string(filter.Operator), Key: filter.Key, Value: filter.Value})
		}
		value.Issue = issue
		if len(issue.Triggers) != 0 {
			value.Trigger = issue.Triggers[0].Type
		}
	}
	if alert.Metric != nil {
		metric := alert.Metric
		value.Trigger = triggerForMetricPreset(metric.Preset)
		value.Metric = &alertsruntime.MetricDesired{
			Preset:      string(metric.Preset),
			Environment: metric.Environment, IntervalMinutes: metric.IntervalMinutes,
		}
		if metric.ApplicationMetric != nil {
			value.Metric.ApplicationMetric = &alertsruntime.ApplicationMetricExpressionDesired{
				Left:     applicationMetricTermDesired(metric.ApplicationMetric.Left),
				Operator: string(metric.ApplicationMetric.Operator),
			}
			if metric.ApplicationMetric.Right != nil {
				right := applicationMetricTermDesired(*metric.ApplicationMetric.Right)
				value.Metric.ApplicationMetric.Right = &right
			}
		}
		switch {
		case metric.Detection.Static != nil:
			static := metric.Detection.Static
			value.Metric.Detection = alertsruntime.DetectionDesired{Kind: "static", Direction: string(static.Direction), High: float64(static.High), Medium: float64Pointer(static.Medium), Resolve: float64Pointer(static.Resolve)}
		case metric.Detection.Change != nil:
			change := metric.Detection.Change
			value.Metric.Detection = alertsruntime.DetectionDesired{Kind: "change", Direction: string(change.Direction), High: float64(change.HighPercent), Medium: float64Pointer(change.MediumPercent), Resolve: float64Pointer(change.ResolvePercent), CompareMinutes: change.CompareMinutes}
		case metric.Detection.Dynamic != nil:
			dynamic := metric.Detection.Dynamic
			value.Metric.Detection = alertsruntime.DetectionDesired{Kind: "dynamic", Responsiveness: string(dynamic.Responsiveness), AnomalyDirection: string(dynamic.Direction)}
		}
	}
	return value, nil
}

func githubIssueDesired(action sentryconfigurationv1alpha1.GitHubIssueAlertAction, integrations *sentryconfigurationv1alpha1.ConnectionIntegrations) (*alertsruntime.GitHubIssueActionDesired, error) {
	if integrations == nil || integrations.SourceCode == nil {
		return nil, alertsruntime.ErrInvalidInput
	}
	var installation *sentryconfigurationv1alpha1.SourceCodeIntegrationCatalogEntry
	for index := range integrations.SourceCode.Installations {
		candidate := &integrations.SourceCode.Installations[index]
		if candidate.Name == action.Integration {
			if installation != nil {
				return nil, alertsruntime.ErrInvalidInput
			}
			installation = candidate
		}
	}
	if installation == nil || (installation.Provider != sentryconfigurationv1alpha1.SourceCodeProvider("github") && installation.Provider != sentryconfigurationv1alpha1.SourceCodeProvider("github_enterprise")) {
		return nil, alertsruntime.ErrInvalidInput
	}
	repositoryName := ""
	for _, repository := range installation.Repositories {
		if repository.Name == action.Repository {
			if repositoryName != "" {
				return nil, alertsruntime.ErrInvalidInput
			}
			repositoryName = repository.SentryName
		}
	}
	if repositoryName == "" {
		return nil, alertsruntime.ErrInvalidInput
	}
	return &alertsruntime.GitHubIssueActionDesired{
		PayloadVersion: 2, Provider: string(installation.Provider), IntegrationName: installation.SentryName,
		RepositoryName: repositoryName, Labels: append([]string(nil), action.Labels...), Assignee: action.Assignee,
	}, nil
}

func applicationMetricTermDesired(term sentryconfigurationv1alpha1.ApplicationMetricTerm) alertsruntime.ApplicationMetricTermDesired {
	return alertsruntime.ApplicationMetricTermDesired{Name: term.Name, Type: string(term.Type), Unit: term.Unit, Aggregation: string(term.Aggregation)}
}

func float64Pointer(value *int64) *float64 {
	if value == nil {
		return nil
	}
	converted := float64(*value)
	return &converted
}

func triggerForMetricPreset(preset sentryconfigurationv1alpha1.MetricPreset) alertsruntime.Trigger {
	switch preset {
	case "error-count":
		return alertsruntime.TriggerEventCount
	case "affected-users":
		return alertsruntime.TriggerUserCount
	case "crash-free-sessions":
		return alertsruntime.TriggerCrashFreeSessions
	case "crash-free-users":
		return alertsruntime.TriggerCrashFreeUsers
	case "throughput":
		return alertsruntime.TriggerThroughput
	case "duration":
		return alertsruntime.TriggerDuration
	case "failure-rate":
		return alertsruntime.TriggerFailureRate
	case "largest-contentful-paint":
		return alertsruntime.TriggerLCP
	case "logs":
		return alertsruntime.TriggerLogs
	case "application-metric":
		return alertsruntime.TriggerApplicationMetric
	default:
		return ""
	}
}

func canManageChildren(resource *sentryconfigurationv1alpha1.SentryProject, result projectruntime.Result) bool {
	return result.AccessMode == binding.AccessManage || (resource != nil && result.AccessMode == binding.AccessReadOnly && result.Origin == binding.OriginManaged)
}

func appliedPolicyBinding(application alertpolicyruntime.Result) *binding.AppliedAlertPolicy {
	if len(application.Policies) != 1 {
		return nil
	}
	policy := application.Policies[0]
	return &policy
}

func appliedPolicyBindings(application alertpolicyruntime.Result) []binding.AppliedAlertPolicy {
	return slices.Clone(application.Policies)
}

func allowedChildOwners(resource *sentryconfigurationv1alpha1.SentryProject, current binding.ProjectBinding, application alertpolicyruntime.Result) map[string]bool {
	result := map[string]bool{string(resource.UID): true}
	if application.PolicyUID != "" {
		result[application.PolicyUID] = true
	}
	for _, policy := range application.Policies {
		result[policy.UID] = true
	}
	if current.AlertPolicy != nil {
		result[current.AlertPolicy.UID] = true
	}
	for _, policy := range current.AlertPolicies {
		result[policy.UID] = true
	}
	return result
}

func alertCapabilities(spec sentryconfigurationv1alpha1.SentryConnectionSpec) []alertsruntime.Capability {
	if spec.Compatibility == nil {
		return nil
	}
	switch spec.Compatibility.Alerts {
	case sentryconfigurationv1alpha1.AlertCompatibilityLegacy24:
		return []alertsruntime.Capability{{
			Name: alertsruntime.LegacyCombinedAdapter,
			Triggers: map[alertsruntime.Trigger]bool{
				alertsruntime.TriggerFirstSeen: true, alertsruntime.TriggerRegression: true, alertsruntime.TriggerEscalation: true, alertsruntime.TriggerSessions: true, alertsruntime.TriggerEventCount: true, alertsruntime.TriggerUserCount: true,
				alertsruntime.TriggerCrashFreeSessions: true, alertsruntime.TriggerCrashFreeUsers: true, alertsruntime.TriggerThroughput: true,
				alertsruntime.TriggerDuration: true, alertsruntime.TriggerFailureRate: true, alertsruntime.TriggerLCP: true,
			},
			SupportsUpdate: true, SupportsToggle: false,
		}}
	case sentryconfigurationv1alpha1.AlertCompatibilityModern26:
		return []alertsruntime.Capability{{
			Name: alertsruntime.ModernMetricIssueAdapter,
			Triggers: map[alertsruntime.Trigger]bool{
				alertsruntime.TriggerFirstSeen: true, alertsruntime.TriggerRegression: true, alertsruntime.TriggerResolved: true, alertsruntime.TriggerEscalation: true,
				alertsruntime.TriggerEventCount: true, alertsruntime.TriggerUserCount: true, alertsruntime.TriggerCrashFreeSessions: true,
				alertsruntime.TriggerCrashFreeUsers: true, alertsruntime.TriggerThroughput: true, alertsruntime.TriggerDuration: true,
				alertsruntime.TriggerFailureRate: true, alertsruntime.TriggerLCP: true, alertsruntime.TriggerLogs: true,
				alertsruntime.TriggerApplicationMetric: true,
			},
			SupportsUpdate: true, SupportsToggle: true,
		}}
	default:
		return nil
	}
}

func (r *SentryProjectReconciler) alertAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding) alertsruntime.Authorization {
	return func(ctx context.Context) error {
		if err := r.dsnAuthorizer(original, connection, expected)(ctx); err != nil {
			return err
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		if !reflect.DeepEqual(latestConnection.Spec.Compatibility, connection.Spec.Compatibility) {
			return dsn.ErrAuthorization
		}
		latestProject := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latestProject); err != nil {
			return err
		}
		if !original.DeletionTimestamp.IsZero() {
			targets, flattenErr := original.Spec.NotificationTargets.Flatten()
			if latestProject.DeletionTimestamp.IsZero() || original.Spec.Alerts == nil || len(*original.Spec.Alerts) != 0 || flattenErr != nil || len(targets) != 0 {
				return dsn.ErrAuthorization
			}
		} else {
			effectiveLatest, _, _, err := r.applyAlertPolicy(ctx, latestProject, latestConnection)
			if err != nil || !reflect.DeepEqual(effectiveLatest.Spec.Alerts, original.Spec.Alerts) || !reflect.DeepEqual(effectiveLatest.Spec.NotificationTargets, original.Spec.NotificationTargets) {
				return dsn.ErrAuthorization
			}
		}
		state, err := r.Bindings.GetBinding(ctx, string(original.UID))
		if err != nil || !reflect.DeepEqual(state.Binding.Alerts, expected.Alerts) || !reflect.DeepEqual(state.Binding.Destinations, expected.Destinations) || !reflect.DeepEqual(state.Binding.PinnedAdapters, expected.PinnedAdapters) {
			return dsn.ErrAuthorization
		}
		if err := r.validateChildMutationOwnership(ctx, latestConnection, state.Binding); err != nil {
			return err
		}
		return nil
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	result := make(map[string]string, len(input)+1)
	for key, value := range input {
		result[key] = value
	}
	return result
}

func destinationCleanupKeys(result destination.Result) []string {
	keys := make([]string, 0)
	for name, current := range result.Bindings {
		if current.State == destination.StateCleanupPending {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
}

// finalizeDestinationCleanup releases protected destination identities only after
// the alert engine has proven that no desired or still-bound Sentry alert refers
// to them.
func (r *SentryProjectReconciler) finalizeDestinationCleanup(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, result *destination.Result, releasable []string) error {
	if result == nil || len(releasable) == 0 {
		return nil
	}
	record, err := r.Bindings.GetBinding(ctx, string(resource.UID))
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(releasable))
	for _, name := range releasable {
		current, found := result.Bindings[name]
		if !found || current.State != destination.StateCleanupPending {
			return destination.ErrInvalidDestination
		}
		allowed[name] = true
	}
	if err := r.destinationAuthorizer(resource, connection, record.Binding, *result)(ctx); err != nil {
		return err
	}
	nextDestinations := make(map[string]binding.DestinationBinding, len(result.Bindings))
	for name, current := range result.Bindings {
		nextDestinations[name] = current
	}
	for _, name := range releasable {
		delete(nextDestinations, name)
	}
	next := record.Binding
	next.Destinations = nextDestinations
	if _, err := r.Bindings.UpdateBinding(ctx, record, next); err != nil {
		return err
	}
	for _, name := range releasable {
		delete(result.Bindings, name)
		delete(result.Statuses, name)
	}
	return nil
}

func (r *SentryProjectReconciler) destinationAuthorizer(original *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection, expected binding.ProjectBinding, result destination.Result) dsn.Authorization {
	return func(ctx context.Context) error {
		if err := r.dsnAuthorizer(original, connection, expected)(ctx); err != nil {
			return err
		}
		latest := &sentryconfigurationv1alpha1.SentryProject{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(original), latest); err != nil {
			return err
		}
		latestConnection := &sentryconfigurationv1alpha1.SentryConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: connection.Name}, latestConnection); err != nil {
			return err
		}
		if !original.DeletionTimestamp.IsZero() {
			if latest.DeletionTimestamp.IsZero() {
				return dsn.ErrAuthorization
			}
			latest = original
		} else {
			effectiveLatest, _, _, err := r.applyAlertPolicy(ctx, latest, latestConnection)
			if err != nil {
				return dsn.ErrAuthorization
			}
			latest = effectiveLatest
		}
		desiredTargets, flattenErr := latest.Spec.NotificationTargets.Flatten()
		if flattenErr != nil {
			return dsn.ErrAuthorization
		}
		for name, desired := range desiredTargets {
			_, policyErr := policy.EvaluateDestination(latest.Namespace, latest.Spec, latestConnection.Spec, desired)
			status, statusFound := result.Statuses[name]
			next, nextFound := result.Bindings[name]
			if policy.IsKind(policyErr, policy.ErrorDestinationUnavailable) {
				if !statusFound || status.Reason != "DestinationAccessRevoked" || (nextFound && next.State != destination.StateRevoked) {
					return dsn.ErrAuthorization
				}
				continue
			}
			if policyErr != nil || (statusFound && status.Reason == "DestinationAccessRevoked") {
				return dsn.ErrAuthorization
			}
		}
		state, err := r.Bindings.GetBinding(ctx, string(latest.UID))
		if err != nil || state.Binding.ExternalIdentity != expected.ExternalIdentity || state.Binding.Connection != expected.Connection || !reflect.DeepEqual(state.Binding.Destinations, expected.Destinations) {
			return dsn.ErrAuthorization
		}
		if err := r.validateChildMutationOwnership(ctx, latestConnection, state.Binding); err != nil {
			return err
		}
		return nil
	}
}

func (r *SentryProjectReconciler) applyAlertPolicy(ctx context.Context, project *sentryconfigurationv1alpha1.SentryProject, connection *sentryconfigurationv1alpha1.SentryConnection) (*sentryconfigurationv1alpha1.SentryProject, *sentryconfigurationv1alpha1.SentryAlertPolicy, alertpolicyruntime.Result, error) {
	effective := project.DeepCopy()
	names := slices.Clone(project.Spec.AlertPolicies)
	if len(names) == 0 && project.Spec.AlertPolicy != "" {
		names = []string{project.Spec.AlertPolicy}
	}
	if len(names) == 0 {
		merged, err := alertpolicyruntime.Merge(effective, nil)
		return effective, nil, merged, err
	}
	policies := make([]*sentryconfigurationv1alpha1.SentryAlertPolicy, 0, len(names))
	var first *sentryconfigurationv1alpha1.SentryAlertPolicy
	deleting := false
	for _, name := range names {
		policyResource := &sentryconfigurationv1alpha1.SentryAlertPolicy{}
		if err := r.Get(ctx, client.ObjectKey{Name: name}, policyResource); err != nil {
			return nil, nil, alertpolicyruntime.Result{}, err
		}
		if !policyResource.DeletionTimestamp.IsZero() {
			deleting = true
			continue
		}
		if first == nil {
			first = policyResource
		}
		policies = append(policies, policyResource)
	}
	merged, err := alertpolicyruntime.MergeMany(effective, policies)
	if err != nil {
		return nil, nil, alertpolicyruntime.Result{}, err
	}
	merged.PolicyDeleting = deleting
	if deleting && effective.Spec.Alerts == nil && len(policies) == 0 {
		emptyAlerts := map[string]sentryconfigurationv1alpha1.AlertSpec{}
		effective.Spec.Alerts = &emptyAlerts
		merged.Alerts = &emptyAlerts
	}
	effective.Spec.NotificationTargets = merged.Targets
	effective.Spec.Alerts = merged.Alerts
	return effective, first, merged, nil
}

func reasonForAlertPolicyError(err error) string {
	switch {
	case apierrors.IsNotFound(err):
		return "AlertPolicyNotFound"
	case errors.Is(err, alertpolicyruntime.ErrSentryMismatch):
		return "AlertPolicySentryMismatch"
	case errors.Is(err, alertpolicyruntime.ErrPolicyLocalConflict):
		return "PolicyLocalConflict"
	case errors.Is(err, alertpolicyruntime.ErrTargetNotFound):
		return "AlertPolicyTargetNotFound"
	default:
		return "AlertPolicyInvalid"
	}
}

func (r *SentryProjectReconciler) reportAlertPolicyFailure(ctx context.Context, resource *sentryconfigurationv1alpha1.SentryProject, reason string) (ctrl.Result, error) {
	base := resource.DeepCopy()
	resource.Status.ObservedGeneration = resource.Generation
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{Type: "AlertPolicyReady", Status: metav1.ConditionFalse, ObservedGeneration: resource.Generation, Reason: reason, Message: "The referenced alert policy could not be merged safely; no alert or notification target mutation was attempted."})
	meta.SetStatusCondition(&resource.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, ObservedGeneration: resource.Generation, Reason: reason, Message: "The referenced alert policy is not ready."})
	if err := r.Status().Patch(ctx, resource, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func safeDestinationStatuses(statuses map[string]destination.Status, existing map[string]sentryconfigurationv1alpha1.DestinationStatus) map[string]sentryconfigurationv1alpha1.DestinationStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.DestinationStatus, len(statuses))
	for name, status := range statuses {
		reason := status.Reason
		if status.Err != nil {
			reason = reasonForError(status.Err)
		}
		result[name] = sentryconfigurationv1alpha1.DestinationStatus{Type: status.Kind, State: status.State, Reason: reason}
	}
	return result
}

func safeAlertStatuses(statuses map[string]alertsruntime.Status) map[string]sentryconfigurationv1alpha1.AlertStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.AlertStatus, len(statuses))
	for name, status := range statuses {
		if name == "" {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.AlertStatus{State: status.State, Reason: status.Reason}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func safeClientKeyStatuses(statuses map[string]clientkeyruntime.Status) map[string]sentryconfigurationv1alpha1.ClientKeyStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.ClientKeyStatus, len(statuses))
	for name, status := range statuses {
		if name == "" {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.ClientKeyStatus{State: status.State, Reason: status.Reason, RotationRevision: status.RotationRevision}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func safeSymbolSourceStatuses(statuses map[string]symbolsourceruntime.Status) map[string]sentryconfigurationv1alpha1.SymbolSourceStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.SymbolSourceStatus, len(statuses))
	for name, status := range statuses {
		if name == "" || status.State == symbolsourceruntime.StateDeleted {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.SymbolSourceStatus{State: status.State, Reason: status.Reason}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func safeCodeMappingStatuses(statuses map[string]codemappingruntime.Status) map[string]sentryconfigurationv1alpha1.CodeMappingStatus {
	if len(statuses) == 0 {
		return nil
	}
	result := make(map[string]sentryconfigurationv1alpha1.CodeMappingStatus, len(statuses))
	for name, status := range statuses {
		if name == "" || status.State == codemappingruntime.StateDeleted {
			continue
		}
		result[name] = sentryconfigurationv1alpha1.CodeMappingStatus{State: status.State, Reason: status.Reason}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func existingSecret(state *binding.DSNBinding) string {
	if state == nil {
		return ""
	}
	return state.CurrentSecret
}

func sameDSNBinding(left, right *binding.DSNBinding) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (r *SentryProjectReconciler) outputSecrets() client.Client {
	if r.OutputSecrets != nil {
		return r.OutputSecrets
	}
	return r.Client
}
