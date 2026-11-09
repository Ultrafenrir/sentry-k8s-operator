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

package settings

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

type relayPIIRedaction struct {
	Method string `json:"method"`
	Text   string `json:"text,omitempty"`
}

type relayPIIRule struct {
	Type      string            `json:"type"`
	Redaction relayPIIRedaction `json:"redaction"`
}

type relayPIIConfig struct {
	Rules        map[string]relayPIIRule `json:"rules"`
	Applications map[string][]string     `json:"applications"`
}

func serializeAdvancedPIIRules(rules []sentryv1alpha1.AdvancedPIIRule) (string, error) {
	config := relayPIIConfig{
		Rules:        make(map[string]relayPIIRule, len(rules)),
		Applications: map[string][]string{},
	}
	for index, rule := range rules {
		if !validAdvancedPIIType(string(rule.Type)) || !validAdvancedPIIRedaction(string(rule.Redaction)) || len(rule.Applications) == 0 {
			return "", ErrInvalidDesiredState
		}
		if rule.Redaction == "replace" {
			if rule.Replacement == "" || strings.ContainsAny(rule.Replacement, "\r\n\f") {
				return "", ErrInvalidDesiredState
			}
		} else if rule.Replacement != "" {
			return "", ErrInvalidDesiredState
		}
		id := strconv.Itoa(index)
		config.Rules[id] = relayPIIRule{
			Type:      string(rule.Type),
			Redaction: relayPIIRedaction{Method: string(rule.Redaction), Text: rule.Replacement},
		}
		seen := map[string]bool{}
		for _, application := range rule.Applications {
			selector, err := serializeAdvancedPIISelector(application)
			if err != nil || seen[selector] {
				return "", ErrInvalidDesiredState
			}
			seen[selector] = true
			config.Applications[selector] = append(config.Applications[selector], id)
		}
	}
	payload, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func serializeAdvancedPIISelector(selector sentryv1alpha1.AdvancedPIISelector) (string, error) {
	if selector.Root == "message" {
		if len(selector.Path) != 0 {
			return "", ErrInvalidDesiredState
		}
		return "$message", nil
	}
	if !validAdvancedPIIRoot(string(selector.Root)) || len(selector.Path) == 0 || len(selector.Path) > 8 {
		return "", ErrInvalidDesiredState
	}
	parts := make([]string, 0, len(selector.Path)+1)
	parts = append(parts, string(selector.Root))
	for _, part := range selector.Path {
		if !validPathSegment(part) {
			return "", ErrInvalidDesiredState
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "."), nil
}

func serializeGroupingEnhancements(rules []sentryv1alpha1.GroupingEnhancementRule) (string, error) {
	lines := make([]string, 0, len(rules))
	for _, rule := range rules {
		matcher, found := map[sentryv1alpha1.GroupingEnhancementMatcher]string{
			"path": "stack.abs_path", "module": "stack.module", "function": "stack.function", "package": "stack.package",
		}[rule.Matcher]
		action, actionFound := map[sentryv1alpha1.GroupingEnhancementAction]string{
			"MarkInApp": "+app", "MarkOutOfApp": "-app", "Include": "+group", "Exclude": "-group",
		}[rule.Action]
		if !found || !actionFound || !validRulePattern(rule.Pattern) {
			return "", ErrInvalidDesiredState
		}
		negation := ""
		if rule.Negate {
			negation = "!"
		}
		lines = append(lines, negation+matcher+":"+strconv.Quote(rule.Pattern)+" "+action)
	}
	return strings.Join(lines, "\n"), nil
}

func serializeFingerprintRules(rules []sentryv1alpha1.FingerprintRule) (string, error) {
	allowedMatchers := map[sentryv1alpha1.FingerprintMatcher]bool{
		"error.type": true, "error.value": true, "message": true, "logger": true, "level": true,
		"stack.module": true, "stack.abs_path": true, "stack.package": true, "stack.function": true,
	}
	lines := make([]string, 0, len(rules))
	for _, rule := range rules {
		if !allowedMatchers[rule.Matcher] || !validRulePattern(rule.Pattern) || len(rule.Values) > 8 || len(rule.Values) == 0 && !rule.IncludeDefault {
			return "", ErrInvalidDesiredState
		}
		values := make([]string, 0, len(rule.Values)+1)
		seen := map[string]bool{}
		for _, value := range rule.Values {
			if !validFingerprintValue(value) || seen[value] {
				return "", ErrInvalidDesiredState
			}
			seen[value] = true
			values = append(values, strconv.Quote(value))
		}
		if rule.IncludeDefault {
			values = append(values, "{{ default }}")
		}
		negation := ""
		if rule.Negate {
			negation = "!"
		}
		lines = append(lines, negation+string(rule.Matcher)+":"+strconv.Quote(rule.Pattern)+" -> "+strings.Join(values, ", "))
	}
	return strings.Join(lines, "\n"), nil
}

func equalJSONString(current *string, desired string) bool {
	if current == nil {
		return false
	}
	var currentValue, desiredValue any
	if json.Unmarshal([]byte(*current), &currentValue) != nil || json.Unmarshal([]byte(desired), &desiredValue) != nil {
		return *current == desired
	}
	return reflect.DeepEqual(currentValue, desiredValue)
}

func validAdvancedPIIType(value string) bool {
	values := []string{"anything", "creditcard", "email", "imei", "ip", "mac", "password", "pemkey", "url_auth", "us_ssn", "userpath", "uuid"}
	index := sort.SearchStrings(values, value)
	return index < len(values) && values[index] == value
}

func validAdvancedPIIRedaction(value string) bool {
	return value == "mask" || value == "remove" || value == "hash" || value == "replace"
}

func validAdvancedPIIRoot(value string) bool {
	return value == "extra" || value == "request" || value == "user" || value == "contexts" || value == "tags" || value == "breadcrumbs"
}

func validPathSegment(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r != '_' && r != '-' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func validRulePattern(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "\r\n\f\"")
}

func validFingerprintValue(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r != '_' && r != '-' && r != '.' && r != ':' && r != '/' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
