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
	"fmt"
	"strings"
	"unicode/utf8"
)

var ErrRender = errors.New("Sentry alert message configuration is invalid")

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// RenderInput is local operator configuration. It deliberately has no field
// for a raw event, issue title, URL, user, request, stack trace, or Sentry ID.
type RenderInput struct {
	ProjectName string   `json:"projectName"`
	AlertName   string   `json:"alertName"`
	Environment string   `json:"environment,omitempty"`
	Severity    Severity `json:"severity"`
}

func Render(input RenderInput) (string, error) {
	if !validLocalLabel(input.ProjectName, 253) || !validLocalLabel(input.AlertName, 63) ||
		(input.Environment != "" && !validLocalLabel(input.Environment, 128)) || !validSeverity(input.Severity) {
		return "", ErrRender
	}
	message := fmt.Sprintf("Sentry alert\nProject: %s\nAlert: %s\nSeverity: %s", input.ProjectName, input.AlertName, input.Severity)
	if input.Environment != "" {
		message += "\nEnvironment: " + input.Environment
	}
	if utf8.RuneCountInString(message) > 1024 || len(message) > 4096 {
		return "", ErrRender
	}
	return message, nil
}

func validSeverity(value Severity) bool {
	return value == SeverityInfo || value == SeverityWarning || value == SeverityCritical
}

func validLocalLabel(value string, maximum int) bool {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f || character == '\u2028' || character == '\u2029' {
			return false
		}
	}
	return true
}
