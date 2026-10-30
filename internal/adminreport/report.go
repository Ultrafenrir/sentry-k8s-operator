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

// Package adminreport builds a read-only, redacted recovery inventory. It must
// never copy protected external identities, DSNs, Telegram targets, operation
// keys, message payloads or Secret data into its output.
package adminreport

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

const recordTypeLabel = "sentryconfiguration.io/record-type"

var ErrReport = errors.New("protected recovery report failed")

type Reporter struct {
	Client              client.Client
	StateNamespace      string
	RoutingNamespace    string
	DeliveryNamespace   string
	HookSourceNamespace string
	Now                 func() time.Time
}

type Report struct {
	GeneratedAt time.Time          `json:"generatedAt"`
	Bindings    []BindingSummary   `json:"bindings"`
	Claims      []ClaimSummary     `json:"claims"`
	Operations  []OperationSummary `json:"operations"`
	Challenges  TerminalSummary    `json:"challenges"`
	Routes      TerminalSummary    `json:"routes"`
	HookSources TerminalSummary    `json:"hookSources"`
	Queue       QueueSummary       `json:"queue"`
	Invalid     map[string]int     `json:"invalidRecords"`
}

type BindingSummary struct {
	Namespace    string               `json:"namespace"`
	Name         string               `json:"name"`
	Generation   int64                `json:"generation"`
	Origin       binding.Origin       `json:"origin"`
	AccessMode   string               `json:"accessMode"`
	Pending      bool                 `json:"pending"`
	DSN          string               `json:"dsn"`
	Alerts       AlertSummary         `json:"alerts"`
	Destinations []DestinationSummary `json:"destinations"`
}

type AlertSummary struct {
	Total      int `json:"total"`
	Enabled    int `json:"enabled"`
	Disabled   int `json:"disabled"`
	Tombstones int `json:"tombstones"`
}

type DestinationSummary struct {
	Name              string `json:"name"`
	Kind              string `json:"kind"`
	State             string `json:"state"`
	ChallengePending  bool   `json:"challengePending,omitempty"`
	ChallengeExpired  bool   `json:"challengeExpired,omitempty"`
	RegistrationPhase string `json:"registrationPhase,omitempty"`
	TargetPinned      bool   `json:"targetPinned,omitempty"`
	RoutePresent      bool   `json:"routePresent,omitempty"`
	RouteRevoked      bool   `json:"routeRevoked,omitempty"`
}

type ClaimSummary struct {
	Installation     string `json:"installation"`
	Organization     string `json:"organization"`
	Slug             string `json:"slug"`
	ManagerNamespace string `json:"managerNamespace"`
	ManagerName      string `json:"managerName"`
	HandoffPending   bool   `json:"handoffPending"`
}

type OperationSummary struct {
	Phase    sentryapi.OperationPhase `json:"phase"`
	Revision int64                    `json:"revision"`
}

type TerminalSummary struct {
	Total   int `json:"total"`
	Expired int `json:"expired,omitempty"`
}

type QueueSummary struct {
	Total            int            `json:"total"`
	ByState          map[string]int `json:"byState"`
	MaxAttempts      int            `json:"maxAttempts"`
	AdmissionPending int            `json:"admissionPending"`
	ExpiredAdmission int            `json:"expiredAdmission"`
	ExpiredTerminal  int            `json:"expiredTerminal"`
}

func (r *Reporter) Build(ctx context.Context) (Report, error) {
	if r == nil || r.Client == nil || !distinctNamespaces(r.StateNamespace, r.RoutingNamespace, r.DeliveryNamespace, r.HookSourceNamespace) {
		return Report{}, ErrReport
	}
	now := r.now()
	report := Report{
		GeneratedAt: now, Invalid: map[string]int{},
		Queue: QueueSummary{ByState: map[string]int{}},
	}
	if err := r.readState(ctx, &report, now); err != nil {
		return Report{}, ErrReport
	}
	if err := r.readRoutes(ctx, &report); err != nil {
		return Report{}, ErrReport
	}
	if err := r.readQueue(ctx, &report, now); err != nil {
		return Report{}, ErrReport
	}
	if err := r.readHookSources(ctx, &report); err != nil {
		return Report{}, ErrReport
	}
	sort.Slice(report.Bindings, func(i, j int) bool {
		if report.Bindings[i].Namespace == report.Bindings[j].Namespace {
			return report.Bindings[i].Name < report.Bindings[j].Name
		}
		return report.Bindings[i].Namespace < report.Bindings[j].Namespace
	})
	sort.Slice(report.Claims, func(i, j int) bool {
		if report.Claims[i].Installation == report.Claims[j].Installation {
			if report.Claims[i].Organization == report.Claims[j].Organization {
				return report.Claims[i].Slug < report.Claims[j].Slug
			}
			return report.Claims[i].Organization < report.Claims[j].Organization
		}
		return report.Claims[i].Installation < report.Claims[j].Installation
	})
	sort.Slice(report.Operations, func(i, j int) bool {
		if report.Operations[i].Phase == report.Operations[j].Phase {
			return report.Operations[i].Revision < report.Operations[j].Revision
		}
		return report.Operations[i].Phase < report.Operations[j].Phase
	})
	return report, nil
}

func (r *Reporter) readHookSources(ctx context.Context, report *Report) error {
	secrets := &corev1.SecretList{}
	if err := r.Client.List(ctx, secrets, client.InNamespace(r.HookSourceNamespace), client.MatchingLabels{recordTypeLabel: "sentry-hook-source"}); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.Type != corev1.SecretTypeOpaque || secret.Labels["app.kubernetes.io/managed-by"] != "sentry-kubernetes-operator" || len(secret.Data) != 1 || sentryhook.ValidateSourceRecordPayload(secret.Name, secret.Data["source.json"]) != nil {
			report.Invalid["sentry-hook-source"]++
			continue
		}
		report.HookSources.Total++
	}
	return nil
}

func distinctNamespaces(values ...string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			return false
		}
		if _, found := seen[value]; found {
			return false
		}
		seen[value] = struct{}{}
	}
	return true
}

func (r *Reporter) readState(ctx context.Context, report *Report, now time.Time) error {
	secrets := &corev1.SecretList{}
	if err := r.Client.List(ctx, secrets, client.InNamespace(r.StateNamespace)); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		switch secret.Labels[recordTypeLabel] {
		case "project-binding":
			var value binding.ProjectBinding
			if !decode(secret, "state.json", &value) || value.Version != binding.CurrentVersion || value.Project.Namespace == "" || value.Project.Name == "" || value.Project.Generation <= 0 {
				report.Invalid["project-binding"]++
				continue
			}
			alerts, validAlerts := safeAlertSummary(value)
			if !validAlerts {
				report.Invalid["project-binding"]++
				continue
			}
			summary := BindingSummary{
				Namespace: value.Project.Namespace, Name: value.Project.Name, Generation: value.Project.Generation,
				Origin: value.Origin, AccessMode: value.AccessMode, DSN: dsnState(value.DSN), Alerts: alerts,
			}
			if value.Pending != nil {
				summary.Pending = true
			}
			for name, destination := range value.Destinations {
				item := DestinationSummary{Name: name, Kind: safeDestinationKind(destination.Kind), State: safeDestinationState(destination.State)}
				if destination.Telegram != nil {
					item.ChallengePending = destination.Telegram.Challenge != nil
					if destination.Telegram.Challenge != nil {
						item.ChallengeExpired = !destination.Telegram.Challenge.ExpiresAt.After(now)
					}
					item.RegistrationPhase = safeRegistrationPhase(destination.Telegram.Phase)
					item.TargetPinned = destination.Telegram.Target != nil
					item.RoutePresent = destination.Telegram.RouteKey != ""
					item.RouteRevoked = destination.Telegram.RouteRevoked
				}
				summary.Destinations = append(summary.Destinations, item)
			}
			sort.Slice(summary.Destinations, func(i, j int) bool { return summary.Destinations[i].Name < summary.Destinations[j].Name })
			report.Bindings = append(report.Bindings, summary)
		case "manager-claim":
			var value binding.ManagerClaim
			if !decode(secret, "state.json", &value) || value.Version != binding.CurrentVersion || value.Installation == "" || value.Organization == "" || value.Slug == "" {
				report.Invalid["manager-claim"]++
				continue
			}
			report.Claims = append(report.Claims, ClaimSummary{
				Installation: value.Installation, Organization: value.Organization, Slug: value.Slug,
				ManagerNamespace: value.Manager.Namespace, ManagerName: value.Manager.Name, HandoffPending: value.Handoff != nil,
			})
		case "operation-journal":
			var value sentryapi.Operation
			if !decode(secret, "state.json", &value) || binding.ValidateOperation(value) != nil {
				report.Invalid["operation-journal"]++
				continue
			}
			report.Operations = append(report.Operations, OperationSummary{Phase: value.Phase, Revision: value.Revision})
		case "telegram-challenge-index":
			var value struct {
				ExpiresAt time.Time `json:"expiresAt"`
			}
			if !decode(secret, "challenge.json", &value) || secret.Immutable == nil || !*secret.Immutable || len(secret.Data) != 1 || value.ExpiresAt.IsZero() {
				report.Invalid["telegram-challenge-index"]++
				continue
			}
			report.Challenges.Total++
			if !value.ExpiresAt.After(now) {
				report.Challenges.Expired++
			}
		}
	}
	return nil
}

func (r *Reporter) readRoutes(ctx context.Context, report *Report) error {
	secrets := &corev1.SecretList{}
	if err := r.Client.List(ctx, secrets, client.InNamespace(r.RoutingNamespace), client.MatchingLabels{recordTypeLabel: "telegram-route"}); err != nil {
		return err
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 1 || len(secret.Data["route.json"]) == 0 {
			report.Invalid["telegram-route"]++
			continue
		}
		report.Routes.Total++
	}
	return nil
}

func (r *Reporter) readQueue(ctx context.Context, report *Report, now time.Time) error {
	secrets := &corev1.SecretList{}
	if err := r.Client.List(ctx, secrets, client.InNamespace(r.DeliveryNamespace)); err != nil {
		return err
	}
	for i := range secrets.Items {
		if secrets.Items[i].Labels[recordTypeLabel] == "telegram-delivery-admission" {
			var value struct {
				ExpiresAt time.Time `json:"expiresAt"`
			}
			if secrets.Items[i].Immutable == nil || !*secrets.Items[i].Immutable || len(secrets.Items[i].Data) != 1 ||
				!decode(&secrets.Items[i], "admission.json", &value) || delivery.ValidateAdmissionPayload(secrets.Items[i].Name, secrets.Items[i].Data["admission.json"]) != nil {
				report.Invalid["telegram-delivery-admission"]++
				continue
			}
			report.Queue.AdmissionPending++
			if !value.ExpiresAt.After(now) {
				report.Queue.ExpiredAdmission++
			}
			continue
		}
		if secrets.Items[i].Labels[recordTypeLabel] != "telegram-delivery" {
			report.Invalid["delivery-unknown"]++
			continue
		}
		var value struct {
			State     delivery.State `json:"state"`
			Attempts  int            `json:"attempts"`
			ExpiresAt time.Time      `json:"expiresAt"`
		}
		if !decode(&secrets.Items[i], "delivery.json", &value) || !safeDeliveryState(value.State) || value.Attempts < 0 {
			report.Invalid["telegram-delivery"]++
			continue
		}
		report.Queue.Total++
		report.Queue.ByState[string(value.State)]++
		if value.Attempts > report.Queue.MaxAttempts {
			report.Queue.MaxAttempts = value.Attempts
		}
		if (value.State == delivery.StateSent || value.State == delivery.StateDeadLetter) && !value.ExpiresAt.IsZero() && !value.ExpiresAt.After(now) {
			report.Queue.ExpiredTerminal++
		}
	}
	return nil
}

func decode(secret *corev1.Secret, key string, target any) bool {
	return secret != nil && secret.Type == corev1.SecretTypeOpaque && len(secret.Data[key]) > 0 && json.Unmarshal(secret.Data[key], target) == nil
}

func dsnState(value *binding.DSNBinding) string {
	if value == nil {
		return "None"
	}
	if value.PendingSecret != "" {
		return "PendingCutover"
	}
	return "Bound"
}

func safeDestinationKind(value string) string {
	switch value {
	case "slack", "pagerduty", "telegram":
		return value
	default:
		return "Unknown"
	}
}

func safeDestinationState(value string) string {
	switch value {
	case "Active", "RegistrationRequired", "RestorePending", "Revoked", "CleanupPending", "Unresolved":
		return value
	default:
		return "Unknown"
	}
}

func safeRegistrationPhase(value string) string {
	switch value {
	case "", "PendingPublish", "PendingFenceRelease":
		return value
	default:
		return "Unknown"
	}
}

func safeAlertSummary(value binding.ProjectBinding) (AlertSummary, bool) {
	summary := AlertSummary{}
	for _, tombstone := range value.Tombstones {
		if tombstone {
			summary.Tombstones++
		}
	}
	if len(value.Alerts) == 0 {
		return summary, true
	}
	if value.Origin != binding.OriginManaged || value.AccessMode != binding.AccessManage || value.Marker == nil || value.PinnedAdapters["alerts"] == "" {
		return AlertSummary{}, false
	}
	for _, alert := range value.Alerts {
		digest, err := hex.DecodeString(alert.DesiredHash)
		if err != nil || len(digest) != 32 || alert.Adapter != value.PinnedAdapters["alerts"] || alert.ExternalIdentity == "" || alert.Trigger == "" || len(alert.Destinations) == 0 || !alert.Marker.Matches(*value.Marker) {
			return AlertSummary{}, false
		}
		summary.Total++
		if alert.Enabled {
			summary.Enabled++
		} else {
			summary.Disabled++
		}
	}
	return summary, true
}

func safeDeliveryState(value delivery.State) bool {
	switch value {
	case delivery.StatePending, delivery.StateInFlight, delivery.StateRetry, delivery.StateSent, delivery.StateDeadLetter:
		return true
	default:
		return false
	}
}

func (r *Reporter) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC().Round(0)
	}
	return time.Now().UTC().Round(0)
}
