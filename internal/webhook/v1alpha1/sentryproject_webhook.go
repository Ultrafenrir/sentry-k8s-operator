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
var sentryprojectlog = logf.Log.WithName("sentryproject-resource")

// SetupSentryProjectWebhookWithManager registers the webhook for SentryProject in the manager.
func SetupSentryProjectWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &sentryconfigurationv1alpha1.SentryProject{}).
		WithValidator(&SentryProjectValidator{}).
		WithDefaulter(&SentryProjectDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-sentryconfiguration-io-v1alpha1-sentryproject,mutating=true,failurePolicy=fail,sideEffects=None,groups=sentryconfiguration.io,resources=sentryprojects,verbs=create;update,versions=v1alpha1,name=msentryproject-v1alpha1.kb.io,admissionReviewVersions=v1

// SentryProjectDefaulter struct is responsible for setting default values on the custom resource of the
// Kind SentryProject when those are created or updated.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as it is used only for temporary operations and does not need to be deeply copied.
type SentryProjectDefaulter struct {
}

// Default implements admission.Defaulter so a webhook will be registered for the Kind SentryProject.
func (d *SentryProjectDefaulter) Default(_ context.Context, obj *sentryconfigurationv1alpha1.SentryProject) error {
	sentryprojectlog.Info("Defaulting for SentryProject", "name", obj.GetName())

	if obj.Spec.Lifecycle.OnDelete == "" {
		obj.Spec.Lifecycle.OnDelete = sentryconfigurationv1alpha1.OnDeleteRetain
	}
	if obj.Spec.DSN != nil && obj.Spec.DSN.Secret == "" {
		obj.Spec.DSN.Secret = obj.Name + sentryconfigurationv1alpha1.DefaultDSNSecretSuffix
	}

	return nil
}

// NOTE: If you want to customise the 'path', use the flags '--defaulting-path' or '--validation-path'.
// +kubebuilder:webhook:path=/validate-sentryconfiguration-io-v1alpha1-sentryproject,mutating=false,failurePolicy=fail,sideEffects=None,groups=sentryconfiguration.io,resources=sentryprojects,verbs=create;update;delete,versions=v1alpha1,name=vsentryproject-v1alpha1.kb.io,admissionReviewVersions=v1

// SentryProjectValidator struct is responsible for validating the SentryProject resource
// when it is created, updated, or deleted.
//
// NOTE: The +kubebuilder:object:generate=false marker prevents controller-gen from generating DeepCopy methods,
// as this struct is used only for temporary operations and does not need to be deeply copied.
type SentryProjectValidator struct {
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type SentryProject.
func (v *SentryProjectValidator) ValidateCreate(_ context.Context, obj *sentryconfigurationv1alpha1.SentryProject) (admission.Warnings, error) {
	sentryprojectlog.Info("Validation for SentryProject upon creation", "name", obj.GetName())

	return nil, validateSentryProject(obj)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type SentryProject.
func (v *SentryProjectValidator) ValidateUpdate(_ context.Context, oldObj, newObj *sentryconfigurationv1alpha1.SentryProject) (admission.Warnings, error) {
	sentryprojectlog.Info("Validation for SentryProject upon update", "name", newObj.GetName())

	allErrs := field.ErrorList{}
	if oldObj.Spec.Connection != newObj.Spec.Connection {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "connection"), "connection is immutable"))
	}
	if !reflect.DeepEqual(oldObj.Spec.Project, newObj.Spec.Project) {
		allErrs = append(allErrs, field.Forbidden(field.NewPath("spec", "project"), "project identity is immutable"))
	}
	for name, status := range oldObj.Status.Destinations {
		if status.RegistrationCommit == nil {
			continue
		}
		oldDestination, oldFound := oldObj.Spec.Destinations[name]
		newDestination, newFound := newObj.Spec.Destinations[name]
		if !oldFound || !newFound || !reflect.DeepEqual(oldDestination, newDestination) {
			allErrs = append(allErrs, field.Forbidden(
				field.NewPath("spec", "destinations").Key(name),
				"destination update is blocked while registrationCommit is active",
			))
		}
	}
	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: sentryconfigurationv1alpha1.GroupVersion.Group, Kind: "SentryProject"},
			newObj.Name,
			allErrs,
		)
	}

	return nil, validateSentryProject(newObj)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type SentryProject.
func (v *SentryProjectValidator) ValidateDelete(_ context.Context, obj *sentryconfigurationv1alpha1.SentryProject) (admission.Warnings, error) {
	sentryprojectlog.Info("Validation for SentryProject upon deletion", "name", obj.GetName())

	for name, status := range obj.Status.Destinations {
		if status.RegistrationCommit == nil {
			continue
		}
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: sentryconfigurationv1alpha1.GroupVersion.Group, Kind: "SentryProject"},
			obj.Name,
			field.ErrorList{field.Forbidden(field.NewPath("status", "destinations").Key(name).Child("registrationCommit"), "deletion is blocked while registrationCommit is active")},
		)
	}

	return nil, nil
}

func validateSentryProject(obj *sentryconfigurationv1alpha1.SentryProject) error {
	specPath := field.NewPath("spec")
	allErrs := field.ErrorList{}
	create := obj.Spec.Project.Create != nil
	use := obj.Spec.Project.Use != nil
	if create == use {
		allErrs = append(allErrs, field.Invalid(specPath.Child("project"), obj.Spec.Project, "exactly one of create or use is required"))
	}
	if obj.Spec.Connection != "" {
		for _, message := range validation.IsDNS1123Subdomain(obj.Spec.Connection) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("connection"), obj.Spec.Connection, message))
		}
	}
	if create && obj.Spec.Project.Create.Slug != "" {
		for _, message := range validation.IsDNS1123Label(obj.Spec.Project.Create.Slug) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("project", "create", "slug"), obj.Spec.Project.Create.Slug, message))
		}
	}
	if use {
		for _, message := range validation.IsDNS1123Label(*obj.Spec.Project.Use) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("project", "use"), *obj.Spec.Project.Use, message))
		}
		if obj.Spec.Settings != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("settings"), "project.use is read-only"))
		}
		if obj.Spec.Destinations != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("destinations"), "project.use is read-only"))
		}
		if obj.Spec.Alerts != nil {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("alerts"), "project.use is read-only"))
		}
		if obj.Spec.Lifecycle.OnDelete == sentryconfigurationv1alpha1.OnDeleteDeleteProject {
			allErrs = append(allErrs, field.Forbidden(specPath.Child("lifecycle", "onDelete"), "project.use cannot delete an external project"))
		}
	}
	if obj.Spec.DSN != nil && obj.Spec.DSN.Secret != "" {
		for _, message := range validation.IsDNS1123Subdomain(obj.Spec.DSN.Secret) {
			allErrs = append(allErrs, field.Invalid(specPath.Child("dsn", "secret"), obj.Spec.DSN.Secret, message))
		}
	}
	for name, destination := range obj.Spec.Destinations {
		destinationPath := specPath.Child("destinations").Key(name)
		for _, message := range validation.IsDNS1123Label(name) {
			allErrs = append(allErrs, field.Invalid(destinationPath, name, message))
		}
		count := 0
		if destination.Slack != nil {
			count++
		}
		if destination.PagerDuty != nil {
			count++
		}
		if destination.Telegram != nil {
			count++
		}
		if count != 1 {
			allErrs = append(allErrs, field.Invalid(destinationPath, destination, "exactly one destination type is required"))
		}
	}
	if obj.Spec.Alerts != nil {
		for name, alert := range *obj.Spec.Alerts {
			alertPath := specPath.Child("alerts").Key(name)
			for _, message := range validation.IsDNS1123Label(name) {
				allErrs = append(allErrs, field.Invalid(alertPath, name, message))
			}
			switch alert.Trigger {
			case sentryconfigurationv1alpha1.AlertTriggerFirstSeen, sentryconfigurationv1alpha1.AlertTriggerRegression:
				if alert.WindowMinutes != nil || alert.Threshold != nil {
					allErrs = append(allErrs, field.Invalid(alertPath, alert, "issue triggers forbid windowMinutes and threshold"))
				}
			case sentryconfigurationv1alpha1.AlertTriggerEventCount:
				if alert.WindowMinutes == nil || *alert.WindowMinutes < 1 || *alert.WindowMinutes > 1440 || alert.Threshold == nil || *alert.Threshold < 1 {
					allErrs = append(allErrs, field.Invalid(alertPath, alert, "event-count requires windowMinutes 1..1440 and a positive threshold"))
				}
			default:
				allErrs = append(allErrs, field.NotSupported(alertPath.Child("trigger"), alert.Trigger, []string{"first-seen", "regression", "event-count"}))
			}
			if len(alert.Notify) == 0 || len(alert.Notify) > 20 {
				allErrs = append(allErrs, field.Invalid(alertPath.Child("notify"), alert.Notify, "must contain 1..20 destination keys"))
			}
			seen := make(map[string]bool, len(alert.Notify))
			for index, destinationName := range alert.Notify {
				notifyPath := alertPath.Child("notify").Index(index)
				if seen[destinationName] {
					allErrs = append(allErrs, field.Duplicate(notifyPath, destinationName))
					continue
				}
				seen[destinationName] = true
				destination, found := obj.Spec.Destinations[destinationName]
				if !found {
					allErrs = append(allErrs, field.NotFound(notifyPath, destinationName))
					continue
				}
				if destination.Slack == nil && destination.PagerDuty == nil {
					allErrs = append(allErrs, field.NotSupported(notifyPath, destinationName, []string{"slack destination", "pagerDuty destination"}))
				}
			}
		}
	}

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: sentryconfigurationv1alpha1.GroupVersion.Group, Kind: "SentryProject"},
		obj.Name,
		allErrs,
	)
}
