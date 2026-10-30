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
	"strings"
	"testing"
)

func TestSourceIdentityIsStableOpaqueAndUIDBound(t *testing.T) {
	t.Parallel()
	key, err := DeriveSourceBindingKey("connection-uid", "project-uid")
	if err != nil || !canonicalHash(key) || strings.Contains(key, "project") || strings.Contains(key, "connection") {
		t.Fatalf("key=%q error=%v", key, err)
	}
	again, _ := DeriveSourceBindingKey("connection-uid", "project-uid")
	changed, _ := DeriveSourceBindingKey("connection-uid", "recreated-project-uid")
	if again != key || changed == key {
		t.Fatalf("stable=%t recreatedChanged=%t", again == key, changed != key)
	}
	endpoint, err := SourceEndpointURL("https://hooks.example.test/events", key)
	if err != nil || endpoint != "https://hooks.example.test/events/"+key {
		t.Fatalf("endpoint=%q error=%v", endpoint, err)
	}
}

func TestSourceIdentityRejectsAmbiguousOrUnsafeInputs(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]string{{"", "project"}, {"connection", " project"}, {"connection", "project\nuid"}} {
		if _, err := DeriveSourceBindingKey(pair[0], pair[1]); err == nil {
			t.Fatalf("UID pair accepted: %#v", pair)
		}
	}
	key := strings.Repeat("a", 64)
	for _, raw := range []string{
		"http://hooks.example.test/events", "https://user:pass@hooks.example.test/events",
		"https://hooks.example.test/events/", "https://hooks.example.test/events?source=x",
	} {
		if _, err := SourceEndpointURL(raw, key); err == nil {
			t.Fatalf("receiver base accepted: %q", raw)
		}
	}
}
