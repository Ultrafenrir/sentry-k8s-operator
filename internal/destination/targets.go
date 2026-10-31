/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package destination

import (
	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
)

var ErrAmbiguousTargetName = sentryv1alpha1.ErrAmbiguousNotificationTarget

// FlattenTargets converts the provider-first public API to canonical protected keys. The
// readable terminal name is deliberately part of the reference; no synthetic ID is added.
func FlattenTargets(targets *sentryv1alpha1.NotificationTargets) (map[string]sentryv1alpha1.DestinationSpec, error) {
	return targets.Flatten()
}
