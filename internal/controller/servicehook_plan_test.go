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

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

func TestTelegramHookDeliveriesUseOnlyActiveProtectedRoutes(t *testing.T) {
	t.Parallel()
	route := strings.Repeat("a", 64)
	protected := binding.ProjectBinding{
		Destinations: map[string]binding.DestinationBinding{
			"telegram/ops":     {Kind: "telegram", State: destination.StateActive, Telegram: &binding.TelegramBinding{RouteKey: route, Target: &binding.TelegramTarget{ChatIdentity: "-1001"}}},
			"telegram/revoked": {Kind: "telegram", State: destination.StateRevoked, Telegram: &binding.TelegramBinding{RouteKey: strings.Repeat("b", 64), Target: &binding.TelegramTarget{ChatIdentity: "-1002"}, RouteRevoked: true}},
		},
		Alerts: map[string]binding.AlertBinding{
			"volume": {Trigger: "event-count", Enabled: true, Destinations: []string{"telegram/ops", "telegram/revoked"}},
		},
	}
	deliveries, err := telegramHookDeliveries("checkout", protected)
	if err != nil || len(deliveries) != 1 || deliveries[0].RouteKey != route || deliveries[0].Render.AlertName != "volume" || deliveries[0].Render.Severity != sentryhook.SeverityCritical {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
}

func TestTelegramHookDeliveriesRejectAmbiguousSharedRoute(t *testing.T) {
	t.Parallel()
	route := strings.Repeat("c", 64)
	protected := binding.ProjectBinding{
		Destinations: map[string]binding.DestinationBinding{
			"telegram/ops": {Kind: "telegram", State: destination.StateActive, Telegram: &binding.TelegramBinding{RouteKey: route, Target: &binding.TelegramTarget{ChatIdentity: "-1001"}}},
		},
		Alerts: map[string]binding.AlertBinding{
			"first":  {Trigger: "first-seen", Enabled: true, Destinations: []string{"telegram/ops"}},
			"second": {Trigger: "regression", Enabled: true, Destinations: []string{"telegram/ops"}},
		},
	}
	if _, err := telegramHookDeliveries("checkout", protected); err == nil {
		t.Fatal("ambiguous shared Telegram route was accepted")
	}
}
