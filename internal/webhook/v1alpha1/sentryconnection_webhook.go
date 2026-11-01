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
	"context"
	"fmt"
	"net/url"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

// nolint:unused
// log is for logging in this package.
var sentryconnectionlog = logf.Log.WithName("sentryconnection-resource")

// SetupSentryConnectionWebhookWithManager registers the webhook for SentryConnection in the manager.
func SetupSentryConnectionWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &sentryconfigurationv1alpha1.SentryConnection{}).
		WithValidator(&SentryConnectionValidator{}).
		WithDefaulter(&SentryConnectionDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-sentryconfiguration-io-v1alpha1-sentryconnection,mutating=true,failurePolicy=fail,sideEffects=None,groups=sentryconfiguration.io,resources=sentryconnections,verbs=create;update,versions=v1alpha1,name=msentryconnection-v1alpha1.kb.io,admissionReviewVersions=v1

// SentryConnectionDefaulter struct is responsible for setting default values on the custom resource of the
// Kind SentryConnection when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type SentryConnectionDefaulter struct {
}

// Default implements admission.Defaulter so a webhook will be registered for the Kind SentryConnection.
func (d *SentryConnectionDefaulter) Default(_ context.Context, obj *sentryconfigurationv1alpha1.SentryConnection) error {
	sentryconnectionlog.Info("Defaulting for SentryConnection", "name", obj.GetName())

	return nil
}

// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-sentryconfiguration-io-v1alpha1-sentryconnection,mutating=false,failurePolicy=fail,sideEffects=None,groups=sentryconfiguration.io,resources=sentryconnections,verbs=create;update;delete,versions=v1alpha1,name=vsentryconnection-v1alpha1.kb.io,admissionReviewVersions=v1

// SentryConnectionValidator struct is responsible for validating the SentryConnection resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type SentryConnectionValidator struct {
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type SentryConnection.
func (v *SentryConnectionValidator) ValidateCreate(_ context.Context, obj *sentryconfigurationv1alpha1.SentryConnection) (admission.Warnings, error) {
	sentryconnectionlog.Info("Validation for SentryConnection upon creation", "name", obj.GetName())

	return insecureTransportWarnings(obj), validateSentryConnection(obj)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type SentryConnection.
func (v *SentryConnectionValidator) ValidateUpdate(_ context.Context, oldObj, newObj *sentryconfigurationv1alpha1.SentryConnection) (admission.Warnings, error) {
	sentryconnectionlog.Info("Validation for SentryConnection upon update", "name", newObj.GetName())

	if oldObj.Status.RegistrationCommit != nil && !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: sentryconfigurationv1alpha1.GroupVersion.Group, Kind: "SentryConnection"},
			newObj.Name,
			field.ErrorList{field.Forbidden(field.NewPath("spec"), "policy update is blocked while registrationCommit is active")},
		)
	}
	transitionErrs := validateOwnershipTransition(oldObj.Spec.Ownership, newObj.Spec.Ownership)
	transitionErrs = append(transitionErrs, validateServiceHookTransition(oldObj.Spec.Integrations, newObj.Spec.Integrations)...)
	if len(transitionErrs) != 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: sentryconfigurationv1alpha1.GroupVersion.Group, Kind: "SentryConnection"},
			newObj.Name,
			transitionErrs,
		)
	}

	return insecureTransportWarnings(newObj), validateSentryConnection(newObj)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type SentryConnection.
func (v *SentryConnectionValidator) ValidateDelete(_ context.Context, obj *sentryconfigurationv1alpha1.SentryConnection) (admission.Warnings, error) {
	sentryconnectionlog.Info("Validation for SentryConnection upon deletion", "name", obj.GetName())

	if obj.Status.RegistrationCommit != nil {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: sentryconfigurationv1alpha1.GroupVersion.Group, Kind: "SentryConnection"},
			obj.Name,
			field.ErrorList{field.Forbidden(field.NewPath("status", "registrationCommit"), "deletion is blocked while registrationCommit is active")},
		)
	}

	return nil, nil
}

func validateSentryConnection(obj *sentryconfigurationv1alpha1.SentryConnection) error {
	specPath := field.NewPath("spec")
	allErrs := field.ErrorList{}
	allErrs = append(allErrs, validateConnectionURL(obj.Spec, specPath)...)
	allErrs = append(allErrs, validateSecretKeyReference(obj.Spec.Credentials.TokenSecretRef, specPath.Child("credentials", "tokenSecretRef"))...)
	if obj.Spec.Credentials.CABundleSecretRef != nil {
		allErrs = append(allErrs, validateSecretKeyReference(*obj.Spec.Credentials.CABundleSecretRef, specPath.Child("credentials", "caBundleSecretRef"))...)
	}
	if obj.Spec.Compatibility != nil && obj.Spec.Compatibility.Alerts != "" && obj.Spec.Compatibility.Alerts != sentryconfigurationv1alpha1.AlertCompatibilityLegacy24 && obj.Spec.Compatibility.Alerts != sentryconfigurationv1alpha1.AlertCompatibilityModern26 {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("compatibility", "alerts"), obj.Spec.Compatibility.Alerts, []string{string(sentryconfigurationv1alpha1.AlertCompatibilityLegacy24), string(sentryconfigurationv1alpha1.AlertCompatibilityModern26)}))
	}

	for index, rule := range obj.Spec.Access {
		rulePath := specPath.Child("access").Index(index)
		for namespaceIndex, namespace := range rule.Namespaces.MatchNames {
			for _, message := range validation.IsDNS1123Label(namespace) {
				allErrs = append(allErrs, field.Invalid(rulePath.Child("namespaces", "matchNames").Index(namespaceIndex), namespace, message))
			}
		}
		if rule.AllowCreate && len(rule.Teams) == 0 {
			allErrs = append(allErrs, field.Required(rulePath.Child("teams"), "at least one team is required when allowCreate is true"))
		}
		if rule.ExistingProjects != nil {
			observed := make(map[string]struct{}, len(rule.ExistingProjects.Observe))
			for _, project := range rule.ExistingProjects.Observe {
				observed[project] = struct{}{}
			}
			for projectIndex, project := range rule.ExistingProjects.ConsumeDSN {
				if _, ok := observed[project]; !ok {
					allErrs = append(allErrs, field.Invalid(
						rulePath.Child("existingProjects", "consumeDSN").Index(projectIndex),
						project,
						"consumeDSN must be a subset of observe",
					))
				}
			}
		}
	}
	if obj.Spec.Ownership != nil {
		ownershipPath := specPath.Child("ownership")
		if len(validation.IsDNS1123Label(obj.Spec.Ownership.Region)) != 0 {
			allErrs = append(allErrs, field.Invalid(ownershipPath.Child("region"), obj.Spec.Ownership.Region, "must be a lowercase DNS label"))
		}
		if obj.Spec.Ownership.Role != sentryconfigurationv1alpha1.ConnectionRoleActive && obj.Spec.Ownership.Role != sentryconfigurationv1alpha1.ConnectionRoleStandby {
			allErrs = append(allErrs, field.NotSupported(ownershipPath.Child("role"), obj.Spec.Ownership.Role, []string{"Active", "Standby"}))
		}
		if obj.Spec.Ownership.Revision < 1 {
			allErrs = append(allErrs, field.Invalid(ownershipPath.Child("revision"), obj.Spec.Ownership.Revision, "must be positive"))
		}
	}
	if obj.Spec.ProjectManagement.UnmanagedProjects == sentryconfigurationv1alpha1.UnmanagedProjectsAdopt && (obj.Spec.Ownership == nil || obj.Spec.Ownership.Role != sentryconfigurationv1alpha1.ConnectionRoleActive) {
		allErrs = append(allErrs, field.Forbidden(specPath.Child("projectManagement", "unmanagedProjects"), "Adopt requires ownership.role Active"))
	}
	if obj.Spec.ProjectManagement.UnmanagedProjects != "" && obj.Spec.ProjectManagement.UnmanagedProjects != sentryconfigurationv1alpha1.UnmanagedProjectsReject && obj.Spec.ProjectManagement.UnmanagedProjects != sentryconfigurationv1alpha1.UnmanagedProjectsStandby && obj.Spec.ProjectManagement.UnmanagedProjects != sentryconfigurationv1alpha1.UnmanagedProjectsAdopt {
		allErrs = append(allErrs, field.NotSupported(specPath.Child("projectManagement", "unmanagedProjects"), obj.Spec.ProjectManagement.UnmanagedProjects, []string{"Reject", "Standby", "Adopt"}))
	}
	allErrs = append(allErrs, validateIntegrationCatalog(obj.Spec.Integrations, specPath.Child("integrations"))...)

	if obj.Spec.Integrations != nil && obj.Spec.Integrations.Telegram != nil {
		telegramPath := specPath.Child("integrations", "telegram")
		telegram := obj.Spec.Integrations.Telegram
		allErrs = append(allErrs, validateSecretKeyReference(telegram.BotTokenSecretRef, telegramPath.Child("botTokenSecretRef"))...)
		allErrs = append(allErrs, validateSentryReceiverURL(telegram.ReceiverURL, telegramPath.Child("receiverURL"))...)
	}

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: sentryconfigurationv1alpha1.GroupVersion.Group, Kind: "SentryConnection"},
		obj.Name,
		allErrs,
	)
}

func validateServiceHookTransition(oldIntegrations, newIntegrations *sentryconfigurationv1alpha1.ConnectionIntegrations) field.ErrorList {
	if oldIntegrations == nil || oldIntegrations.Telegram == nil || newIntegrations == nil || newIntegrations.Telegram == nil {
		return nil
	}
	path := field.NewPath("spec", "integrations", "telegram", "serviceHookRevision")
	oldTelegram := oldIntegrations.Telegram
	newTelegram := newIntegrations.Telegram
	oldRevision := effectiveServiceHookRevision(oldTelegram.ServiceHookRevision)
	newRevision := effectiveServiceHookRevision(newTelegram.ServiceHookRevision)
	allErrs := field.ErrorList{}
	if newRevision < oldRevision {
		allErrs = append(allErrs, field.Invalid(path, newTelegram.ServiceHookRevision, "must not decrease"))
	}
	if newTelegram.ReceiverURL != oldTelegram.ReceiverURL && newRevision <= oldRevision {
		allErrs = append(allErrs, field.Invalid(path, newTelegram.ServiceHookRevision, "must increase when receiverURL changes"))
	}
	return allErrs
}

func effectiveServiceHookRevision(revision int64) int64 {
	if revision < 1 {
		return 1
	}
	return revision
}

func validateOwnershipTransition(oldOwnership, newOwnership *sentryconfigurationv1alpha1.ConnectionOwnership) field.ErrorList {
	path := field.NewPath("spec", "ownership")
	if oldOwnership == nil {
		return nil
	}
	if newOwnership == nil {
		return field.ErrorList{field.Forbidden(path, "ownership fencing cannot be disabled after it is enabled")}
	}
	allErrs := field.ErrorList{}
	if newOwnership.Revision < oldOwnership.Revision {
		allErrs = append(allErrs, field.Invalid(path.Child("revision"), newOwnership.Revision, "must not decrease"))
	}
	if newOwnership.Region != oldOwnership.Region && newOwnership.Revision <= oldOwnership.Revision {
		allErrs = append(allErrs, field.Invalid(path.Child("revision"), newOwnership.Revision, "must increase when region changes"))
	}
	if oldOwnership.Role == sentryconfigurationv1alpha1.ConnectionRoleStandby && newOwnership.Role == sentryconfigurationv1alpha1.ConnectionRoleActive && newOwnership.Revision <= oldOwnership.Revision {
		allErrs = append(allErrs, field.Invalid(path.Child("revision"), newOwnership.Revision, "must increase when promoting Standby to Active"))
	}
	return allErrs
}

func validateIntegrationCatalog(integrations *sentryconfigurationv1alpha1.ConnectionIntegrations, path *field.Path) field.ErrorList {
	if integrations == nil {
		return nil
	}
	allErrs := field.ErrorList{}
	validate := func(entries []sentryconfigurationv1alpha1.IntegrationCatalogEntry, entriesPath *field.Path) {
		seenNames := map[string]bool{}
		seenRemote := map[string]bool{}
		for index, entry := range entries {
			entryPath := entriesPath.Index(index)
			if len(validation.IsDNS1123Label(entry.Name)) != 0 {
				allErrs = append(allErrs, field.Invalid(entryPath.Child("name"), entry.Name, "must be a lowercase DNS label"))
			}
			if seenNames[entry.Name] {
				allErrs = append(allErrs, field.Duplicate(entryPath.Child("name"), entry.Name))
			}
			if seenRemote[entry.SentryName] {
				allErrs = append(allErrs, field.Duplicate(entryPath.Child("sentryName"), entry.SentryName))
			}
			seenNames[entry.Name], seenRemote[entry.SentryName] = true, true
		}
	}
	if integrations.Slack != nil {
		validate(integrations.Slack.Workspaces, path.Child("slack", "workspaces"))
	}
	if integrations.PagerDuty != nil {
		validate(integrations.PagerDuty.Accounts, path.Child("pagerDuty", "accounts"))
	}
	if integrations.SourceCode != nil {
		installationsPath := path.Child("sourceCode", "installations")
		seenNames := map[string]bool{}
		seenRemote := map[string]bool{}
		for index, installation := range integrations.SourceCode.Installations {
			installationPath := installationsPath.Index(index)
			if len(validation.IsDNS1123Label(installation.Name)) != 0 {
				allErrs = append(allErrs, field.Invalid(installationPath.Child("name"), installation.Name, "must be a lowercase DNS label"))
			}
			remoteKey := string(installation.Provider) + "/" + installation.SentryName
			if seenNames[installation.Name] {
				allErrs = append(allErrs, field.Duplicate(installationPath.Child("name"), installation.Name))
			}
			if seenRemote[remoteKey] {
				allErrs = append(allErrs, field.Duplicate(installationPath.Child("sentryName"), installation.SentryName))
			}
			seenNames[installation.Name], seenRemote[remoteKey] = true, true
			seenRepositories := map[string]bool{}
			seenRemoteRepositories := map[string]bool{}
			for repositoryIndex, repository := range installation.Repositories {
				repositoryPath := installationPath.Child("repositories").Index(repositoryIndex)
				if len(validation.IsDNS1123Label(repository.Name)) != 0 {
					allErrs = append(allErrs, field.Invalid(repositoryPath.Child("name"), repository.Name, "must be a lowercase DNS label"))
				}
				if seenRepositories[repository.Name] {
					allErrs = append(allErrs, field.Duplicate(repositoryPath.Child("name"), repository.Name))
				}
				if seenRemoteRepositories[repository.SentryName] {
					allErrs = append(allErrs, field.Duplicate(repositoryPath.Child("sentryName"), repository.SentryName))
				}
				seenRepositories[repository.Name], seenRemoteRepositories[repository.SentryName] = true, true
			}
		}
	}
	return allErrs
}

func validateSecretKeyReference(ref sentryconfigurationv1alpha1.SecretKeyReference, path *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	for _, message := range validation.IsDNS1123Subdomain(ref.Name) {
		allErrs = append(allErrs, field.Invalid(path.Child("name"), ref.Name, message))
	}
	if ref.Key == "" {
		allErrs = append(allErrs, field.Required(path.Child("key"), "key is required"))
	}
	return allErrs
}

func validateConnectionURL(spec sentryconfigurationv1alpha1.SentryConnectionSpec, specPath *field.Path) field.ErrorList {
	path := specPath.Child("url")
	parsed, err := url.ParseRequestURI(spec.URL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return field.ErrorList{field.Invalid(path, spec.URL, fmt.Sprintf("must be an absolute HTTP or HTTPS URL without userinfo: %v", err))}
	}
	transport := spec.Transport
	if parsed.Scheme == "http" && (transport == nil || !transport.AllowInsecureHTTP) {
		return field.ErrorList{field.Forbidden(path, "plain HTTP requires spec.transport.allowInsecureHTTP=true")}
	}
	if transport != nil && transport.InsecureSkipTLSVerify {
		if parsed.Scheme != "https" {
			return field.ErrorList{field.Forbidden(specPath.Child("transport", "insecureSkipTLSVerify"), "requires an HTTPS URL")}
		}
		if spec.Credentials.CABundleSecretRef != nil {
			return field.ErrorList{field.Forbidden(specPath.Child("transport", "insecureSkipTLSVerify"), "is mutually exclusive with credentials.caBundleSecretRef")}
		}
	}
	if parsed.Scheme == "http" && spec.Credentials.CABundleSecretRef != nil {
		return field.ErrorList{field.Forbidden(specPath.Child("credentials", "caBundleSecretRef"), "requires an HTTPS URL")}
	}
	return nil
}

func insecureTransportWarnings(obj *sentryconfigurationv1alpha1.SentryConnection) admission.Warnings {
	if obj.Spec.Transport == nil {
		return nil
	}
	warnings := admission.Warnings{}
	if obj.Spec.Transport.AllowInsecureHTTP {
		warnings = append(warnings, "spec.transport.allowInsecureHTTP sends the Sentry API token without transport encryption")
	}
	if obj.Spec.Transport.InsecureSkipTLSVerify {
		warnings = append(warnings, "spec.transport.insecureSkipTLSVerify disables Sentry server certificate and hostname verification")
	}
	return warnings
}

func validateSentryReceiverURL(value string, path *field.Path) field.ErrorList {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "/events" ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return field.ErrorList{field.Invalid(path, value, "must be an exact HTTPS /events base URL without userinfo, query or fragment")}
	}
	return nil
}
