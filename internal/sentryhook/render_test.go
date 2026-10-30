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

package sentryhook

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderUsesOnlyBoundedLocalConfiguration(t *testing.T) {
	t.Parallel()
	message, err := Render(RenderInput{
		ProjectName: "checkout-api", AlertName: "new-issue", Environment: "production", Severity: SeverityWarning,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Sentry alert\nProject: checkout-api\nAlert: new-issue\nSeverity: warning\nEnvironment: production"
	if message != want || len(message) > 4096 {
		t.Fatalf("message = %q", message)
	}
}

func TestRenderRejectsControlCharactersAndUnboundedFields(t *testing.T) {
	t.Parallel()
	for _, input := range []RenderInput{
		{ProjectName: "checkout\nexternal-id-SHOULD-NOT-LEAK", AlertName: "rule", Severity: SeverityInfo},
		{ProjectName: "checkout", AlertName: strings.Repeat("a", 64), Severity: SeverityInfo},
		{ProjectName: "checkout", AlertName: "rule", Severity: "native-sentry-severity"},
	} {
		if message, err := Render(input); !errors.Is(err, ErrRender) || message != "" {
			t.Fatalf("Render(%+v) = %q, %v", input, message, err)
		}
	}
}
