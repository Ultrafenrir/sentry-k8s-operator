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
	"strings"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
)

func TestMonitorProjectFailuresAreActionable(t *testing.T) {
	tests := []struct {
		err     error
		reason  string
		message string
	}{
		{errMonitorProjectDeleting, "ProjectDeleting", "deleting"},
		{errMonitorProjectNotReady, "ProjectNotReady", "Ready condition"},
		{binding.ErrNotFound, "ProjectNotReady", "Ready condition"},
		{errMonitorProjectNotManaged, "ProjectNotManaged", "accessMode Manage"},
		{errMonitorConnectionNotReady, "ConnectionNotReady", "credential, organization, or network"},
		{errMonitorProtectedStateMismatch, "ProtectedStateMismatch", "no Sentry mutation"},
	}
	for _, test := range tests {
		if got := monitorReason(test.err); got != test.reason {
			t.Errorf("monitorReason(%v) = %q, want %q", test.err, got, test.reason)
		}
		if got := monitorProjectFailureMessage(test.err); !strings.Contains(got, test.message) {
			t.Errorf("monitorProjectFailureMessage(%v) = %q, want substring %q", test.err, got, test.message)
		}
	}
}
