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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

const (
	controllerProject    = "project"
	controllerConnection = "connection"
	metricsUnknown       = "unknown"
	syncProject          = "project"
	syncDSN              = "dsn"
	syncSettings         = "settings"
	syncDestinations     = "destinations"
	syncAlerts           = "alerts"
	syncReady            = "ready"
	syncBlocked          = "blocked"
	syncInactive         = "inactive"
)

var syncMechanisms = []string{syncProject, syncDSN, syncSettings, syncDestinations, syncAlerts}

var ErrMetrics = errors.New("controller metrics initialization failed")

// Metrics accepts only closed enum labels. Namespaces, object names, Sentry
// identities, condition reasons/messages and raw errors never cross this API.
type Metrics struct {
	reconciles           *prometheus.CounterVec
	duration             *prometheus.HistogramVec
	conditionTransitions *prometheus.CounterVec
	synchronizations     *prometheus.GaugeVec
	synchronizationMu    sync.Mutex
	projectSync          map[string]map[string]string
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
		synchronizations: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "sentry_operator_sync_resources",
			Help: "Current observed SentryProject resources by fixed synchronization mechanism and state.",
		}, []string{"mechanism", "state"}),
		projectSync: make(map[string]map[string]string),
	}
	for _, collector := range []prometheus.Collector{
		metrics.reconciles, metrics.duration, metrics.conditionTransitions, metrics.synchronizations,
	} {
		if err := registerer.Register(collector); err != nil {
			return nil, ErrMetrics
		}
	}
	for _, mechanism := range syncMechanisms {
		for _, state := range []string{syncReady, syncBlocked, syncInactive} {
			metrics.synchronizations.WithLabelValues(mechanism, state).Set(0)
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

// ObserveProjectSynchronization replaces the last observed component states for
// one Kubernetes object. The object key is retained only in memory; it is never
// exported as a Prometheus label.
func (m *Metrics) ObserveProjectSynchronization(key string, conditions []metav1.Condition) {
	if m == nil || key == "" {
		return
	}
	next := make(map[string]string)
	for _, condition := range conditions {
		mechanism := syncMechanismForCondition(condition.Type)
		if mechanism != metricsUnknown {
			next[mechanism] = syncStateForCondition(condition.Status)
		}
	}
	m.synchronizationMu.Lock()
	defer m.synchronizationMu.Unlock()
	m.replaceProjectSynchronization(key, next)
}

func (m *Metrics) ForgetProjectSynchronization(key string) {
	if m == nil || key == "" {
		return
	}
	m.synchronizationMu.Lock()
	defer m.synchronizationMu.Unlock()
	m.replaceProjectSynchronization(key, nil)
}

func (m *Metrics) replaceProjectSynchronization(key string, next map[string]string) {
	for mechanism, state := range m.projectSync[key] {
		m.synchronizations.WithLabelValues(mechanism, state).Dec()
	}
	if len(next) == 0 {
		delete(m.projectSync, key)
		return
	}
	for mechanism, state := range next {
		m.synchronizations.WithLabelValues(mechanism, state).Inc()
	}
	m.projectSync[key] = next
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
	case "CredentialsReady", "DSNReady", "SettingsReady", "NotificationTargetsReady", "AlertsReady", "ProjectReady", "Ready":
		return value
	default:
		return metricsUnknown
	}
}

func syncMechanismForCondition(condition string) string {
	switch condition {
	case "ProjectReady":
		return syncProject
	case "DSNReady":
		return syncDSN
	case "SettingsReady":
		return syncSettings
	case "NotificationTargetsReady":
		return syncDestinations
	case "AlertsReady":
		return syncAlerts
	default:
		return metricsUnknown
	}
}

func syncStateForCondition(status metav1.ConditionStatus) string {
	switch status {
	case metav1.ConditionTrue:
		return syncReady
	case metav1.ConditionFalse:
		return syncBlocked
	default:
		return syncInactive
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
