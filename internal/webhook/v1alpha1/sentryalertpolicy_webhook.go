/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha1

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

func SetupSentryAlertPolicyWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &sentryv1alpha1.SentryAlertPolicy{}).
		WithValidator(&SentryAlertPolicyValidator{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-sentryconfiguration-io-v1alpha1-sentryalertpolicy,mutating=false,failurePolicy=fail,sideEffects=None,groups=sentryconfiguration.io,resources=sentryalertpolicies,verbs=create;update;delete,versions=v1alpha1,name=vsentryalertpolicy-v1alpha1.kb.io,admissionReviewVersions=v1

type SentryAlertPolicyValidator struct{}

func (v *SentryAlertPolicyValidator) ValidateCreate(_ context.Context, obj *sentryv1alpha1.SentryAlertPolicy) (admission.Warnings, error) {
	return nil, validateSentryAlertPolicy(obj)
}

func (v *SentryAlertPolicyValidator) ValidateUpdate(_ context.Context, oldObj, newObj *sentryv1alpha1.SentryAlertPolicy) (admission.Warnings, error) {
	if oldObj.Spec.Sentry != newObj.Spec.Sentry {
		return nil, apierrors.NewInvalid(schema.GroupKind{Group: sentryv1alpha1.GroupVersion.Group, Kind: "SentryAlertPolicy"}, newObj.Name, field.ErrorList{
			field.Forbidden(field.NewPath("spec", "sentry"), "sentry is immutable"),
		})
	}
	return nil, validateSentryAlertPolicy(newObj)
}

func (v *SentryAlertPolicyValidator) ValidateDelete(context.Context, *sentryv1alpha1.SentryAlertPolicy) (admission.Warnings, error) {
	return nil, nil
}

func validateSentryAlertPolicy(obj *sentryv1alpha1.SentryAlertPolicy) error {
	specPath := field.NewPath("spec")
	allErrs := field.ErrorList{}
	for _, message := range validation.IsDNS1123Subdomain(obj.Spec.Sentry) {
		allErrs = append(allErrs, field.Invalid(specPath.Child("sentry"), obj.Spec.Sentry, message))
	}
	targets, err := obj.Spec.NotificationTargets.Flatten()
	if err != nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("notificationTargets"), obj.Spec.NotificationTargets, "target names must be unique within each provider"))
	}
	for name, alert := range obj.Spec.Alerts {
		path := specPath.Child("alerts").Key(name)
		if len(validation.IsDNS1123Label(name)) != 0 {
			allErrs = append(allErrs, field.Invalid(path, name, "must be a lowercase DNS label"))
		}
		for index, target := range alert.Notify {
			targetPath := path.Child("notify").Index(index)
			selector, found := targets[target]
			if !found {
				allErrs = append(allErrs, field.NotFound(targetPath, target))
			} else if selector.Telegram != nil {
				allErrs = append(allErrs, field.NotSupported(targetPath, target, []string{"slack target", "pagerDuty target"}))
			}
		}
	}
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(schema.GroupKind{Group: sentryv1alpha1.GroupVersion.Group, Kind: "SentryAlertPolicy"}, obj.Name, allErrs)
}

var _ admission.Validator[*sentryv1alpha1.SentryAlertPolicy] = &SentryAlertPolicyValidator{}
