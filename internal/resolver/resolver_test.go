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

package resolver

import "testing"

type named struct {
	Name     string
	Internal string
}

func TestExactDoesNotChooseAmbiguousOrPartialName(t *testing.T) {
	t.Parallel()

	items := []named{{Name: "payments", Internal: "1"}, {Name: "payments", Internal: "2"}, {Name: "payments-api", Internal: "3"}}
	name := func(item named) string { return item.Name }
	if _, err := Exact(items, "payments", "team", name); !IsKind(err, ErrorAmbiguous) {
		t.Fatalf("duplicate error = %v, want ambiguous", err)
	}
	if _, err := Exact(items, "pay", "team", name); !IsKind(err, ErrorNotFound) {
		t.Fatalf("partial match error = %v, want not found", err)
	}
	item, err := Exact(items, "payments-api", "team", name)
	if err != nil || item.Internal != "3" {
		t.Fatalf("unique exact result = %#v, %v", item, err)
	}
}

func TestSlackNormalizationOnlyRemovesLeadingHash(t *testing.T) {
	t.Parallel()

	items := []named{{Name: "alerts"}, {Name: "Alerts"}, {Name: "alerts-prod"}}
	item, err := ExactNormalized(items, "#alerts", "slack-channel", func(item named) string { return item.Name }, SlackChannelName)
	if err != nil || item.Name != "alerts" {
		t.Fatalf("normalized result = %#v, %v", item, err)
	}
	if _, err := ExactNormalized(items, "ALERTS", "slack-channel", func(item named) string { return item.Name }, SlackChannelName); !IsKind(err, ErrorNotFound) {
		t.Fatalf("case was normalized unexpectedly: %v", err)
	}
}

func TestActiveClientKeyUsesAllCollectedPages(t *testing.T) {
	t.Parallel()

	keys := []ClientKey{{Name: "Default", Active: true}, {Name: "old", Active: false}, {Name: "Default", Active: true}}
	if _, err := ActiveClientKey(keys, "Default"); !IsKind(err, ErrorAmbiguous) {
		t.Fatalf("duplicate active labels = %v, want ambiguous", err)
	}
	if _, err := ActiveClientKey(keys, ""); !IsKind(err, ErrorAmbiguous) {
		t.Fatalf("multiple active default = %v, want ambiguous", err)
	}
	key, err := ActiveClientKey([]ClientKey{{Name: "old"}, {Name: "only", Active: true}}, "")
	if err != nil || key.Name != "only" {
		t.Fatalf("single active = %#v, %v", key, err)
	}
	if _, err := ActiveClientKey([]ClientKey{{Name: "old"}}, ""); !IsKind(err, ErrorNotFound) {
		t.Fatalf("no active = %v, want not found", err)
	}
}
