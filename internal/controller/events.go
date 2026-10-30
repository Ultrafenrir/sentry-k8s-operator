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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
)

// emitConditionChanges publishes only fixed, already-redacted condition transitions.
// Raw errors and external responses never cross this boundary, and an unchanged periodic
// reconciliation cannot create duplicate Events.
func emitConditionChanges(recorder events.EventRecorder, object runtime.Object, before, after []metav1.Condition) {
	emitConditionChangesObserved(recorder, nil, "", object, before, after)
}

func emitConditionChangesObserved(recorder events.EventRecorder, observer *Metrics, controller string, object runtime.Object, before, after []metav1.Condition) {
	if object == nil {
		return
	}
	if observer != nil && controller == controllerProject {
		if accessor, err := meta.Accessor(object); err == nil {
			observer.ObserveProjectSynchronization(accessor.GetNamespace()+"/"+accessor.GetName(), after)
		}
	}
	for _, condition := range after {
		previous := meta.FindStatusCondition(before, condition.Type)
		if previous != nil && previous.Status == condition.Status && previous.Reason == condition.Reason && previous.Message == condition.Message {
			continue
		}
		observer.ObserveCondition(controller, condition.Type, condition.Status)
		if recorder == nil {
			continue
		}
		eventType := corev1.EventTypeNormal
		if condition.Status == metav1.ConditionFalse {
			eventType = corev1.EventTypeWarning
		}
		recorder.Eventf(object, nil, eventType, condition.Reason, "ConditionChanged", "%s", condition.Message)
	}
}
