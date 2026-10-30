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
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	controllerProject    = "project"
	controllerConnection = "connection"
	metricsUnknown       = "unknown"
)

var ErrMetrics = errors.New("controller metrics initialization failed")

// Metrics accepts only closed enum labels. Namespaces, object names, Sentry
// identities, condition reasons/messages and raw errors never cross this API.
type Metrics struct {
	reconciles           *prometheus.CounterVec
	duration             *prometheus.HistogramVec
	conditionTransitions *prometheus.CounterVec
}

func NewMetrics(registerer prometheus.Registerer) (*Metrics, error) {
	if registerer == nil {
		return nil, ErrMetrics
	}
	metrics := &Metrics{
		reconciles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sentry_operator_reconciles_total",
			Help: "Sentry resource reconciliations by fixed controller and outcome.",
		}, []string{"controller", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "sentry_operator_reconcile_duration_seconds",
			Help:    "Sentry resource reconciliation duration by fixed controller.",
			Buckets: prometheus.DefBuckets,
		}, []string{"controller"}),
		conditionTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sentry_operator_condition_transitions_total",
			Help: "Sentry resource condition transitions by fixed controller, condition and status.",
		}, []string{"controller", "condition", "status"}),
	}
	for _, collector := range []prometheus.Collector{metrics.reconciles, metrics.duration, metrics.conditionTransitions} {
		if err := registerer.Register(collector); err != nil {
			return nil, ErrMetrics
		}
	}
	return metrics, nil
}

func (m *Metrics) ObserveReconcile(controller string, result ctrl.Result, reconcileErr error, duration time.Duration) {
	if m == nil {
		return
	}
	controller = safeController(controller)
	outcome := "success"
	if reconcileErr != nil {
		outcome = "error"
	} else if result.Requeue || result.RequeueAfter > 0 {
		outcome = "requeue"
	}
	m.reconciles.WithLabelValues(controller, outcome).Inc()
	seconds := duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	m.duration.WithLabelValues(controller).Observe(seconds)
}

func (m *Metrics) ObserveCondition(controller, condition string, status metav1.ConditionStatus) {
	if m == nil {
		return
	}
	m.conditionTransitions.WithLabelValues(safeController(controller), safeCondition(condition), safeConditionStatus(status)).Inc()
}

func safeController(value string) string {
	switch value {
	case controllerProject, controllerConnection:
		return value
	default:
		return metricsUnknown
	}
}

func safeCondition(value string) string {
	switch value {
	case "CredentialsReady", "DSNReady", "SettingsReady", "DestinationsReady", "ProjectReady", "Ready":
		return value
	default:
		return metricsUnknown
	}
}

func safeConditionStatus(value metav1.ConditionStatus) string {
	switch value {
	case metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionUnknown:
		return string(value)
	default:
		return metricsUnknown
	}
}
