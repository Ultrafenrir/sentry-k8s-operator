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

package main

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

func TestVerifyRequiredCRDs(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{sentryv1alpha1.GroupVersion})
	for _, kind := range requiredCRDKinds {
		mapper.Add(schema.GroupVersionKind{Group: sentryv1alpha1.GroupVersion.Group, Version: sentryv1alpha1.GroupVersion.Version, Kind: kind}, meta.RESTScopeRoot)
	}
	if err := verifyRequiredCRDs(mapper); err != nil {
		t.Fatalf("verifyRequiredCRDs() error = %v", err)
	}
}

func TestVerifyRequiredCRDsReportsEveryMissingKind(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{sentryv1alpha1.GroupVersion})
	err := verifyRequiredCRDs(mapper)
	if err == nil {
		t.Fatal("verifyRequiredCRDs() succeeded without mappings")
	}
	for _, kind := range requiredCRDKinds {
		if !strings.Contains(err.Error(), kind) {
			t.Errorf("error %q does not name missing kind %q", err, kind)
		}
	}
}
