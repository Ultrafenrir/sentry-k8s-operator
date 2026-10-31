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
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	connectionruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/connection"
)

// SentryConnectionReconciler reconciles a SentryConnection object
type SentryConnectionReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	OperatorNamespace string
	Registry          *connectionruntime.Registry
	Recorder          events.EventRecorder
	Metrics           *Metrics
}

// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryconnections,verbs=get;list;watch
// +kubebuilder:rbac:groups=sentryconfiguration.io,resources=sentryconnections/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *SentryConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (reconcileResult ctrl.Result, reconcileErr error) {
	started := time.Now()
	defer func() {
		r.Metrics.ObserveReconcile(controllerConnection, reconcileResult, reconcileErr, time.Since(started))
	}()
	log := logf.FromContext(ctx)
	connection := &sentryconfigurationv1alpha1.SentryConnection{}
	if err := r.Get(ctx, req.NamespacedName, connection); err != nil {
		if apierrors.IsNotFound(err) {
			if r.Registry != nil {
				r.Registry.Delete(req.Name)
			}
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if r.Registry == nil || r.OperatorNamespace == "" {
		return ctrl.Result{}, fmt.Errorf("connection runtime is not configured")
	}

	inputs, err := connectionruntime.LoadSecretInputs(ctx, r.Client, r.OperatorNamespace, connection.Spec)
	if err != nil {
		if statusErr := r.setCredentialCondition(ctx, connection, metav1.ConditionFalse, "SecretInputInvalid", "One or more referenced Secret inputs are unavailable or invalid."); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		log.Info("Connection credentials are not ready", "connection", connection.Name)
		return ctrl.Result{RequeueAfter: jitterDuration(30 * time.Second)}, nil
	}
	if _, _, err := r.Registry.Apply(connection.Name, connection.Spec.URL, inputs); err != nil {
		if statusErr := r.setCredentialCondition(ctx, connection, metav1.ConditionFalse, "ClientConfigurationInvalid", "The HTTPS client configuration is invalid."); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: jitterDuration(30 * time.Second)}, nil
	}
	if err := r.setCredentialCondition(ctx, connection, metav1.ConditionTrue, "SecretInputsLoaded", "Referenced Secret inputs are loaded and watched for rotation."); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SentryConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Registry == nil || r.OperatorNamespace == "" {
		return fmt.Errorf("connection runtime and operator namespace are required")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&sentryconfigurationv1alpha1.SentryConnection{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.connectionsForSecret)).
		Named("sentryconnection").
		Complete(r)
}

func (r *SentryConnectionReconciler) connectionsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	if object.GetNamespace() != r.OperatorNamespace {
		return nil
	}
	connections := &sentryconfigurationv1alpha1.SentryConnectionList{}
	if err := r.List(ctx, connections); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for _, connection := range connections.Items {
		for _, name := range connectionruntime.SecretNames(connection.Spec) {
			if name == object.GetName() {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKey{Name: connection.Name}})
				break
			}
		}
	}
	return requests
}

func (r *SentryConnectionReconciler) setCredentialCondition(ctx context.Context, connection *sentryconfigurationv1alpha1.SentryConnection, status metav1.ConditionStatus, reason, message string) error {
	base := connection.DeepCopy()
	connection.Status.ObservedGeneration = connection.Generation
	setConnectionOwnershipStatus(connection)
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               "CredentialsReady",
		Status:             status,
		ObservedGeneration: connection.Generation,
		Reason:             reason,
		Message:            message,
	})
	if err := r.Status().Patch(ctx, connection, client.MergeFrom(base)); err != nil {
		return err
	}
	emitConditionChangesObserved(r.Recorder, r.Metrics, controllerConnection, connection, base.Status.Conditions, connection.Status.Conditions)
	return nil
}

func setConnectionOwnershipStatus(connection *sentryconfigurationv1alpha1.SentryConnection) {
	connection.Status.Region = ""
	connection.Status.ObservedRevision = 0
	connection.Status.EffectiveRole = string(sentryconfigurationv1alpha1.ConnectionRoleStandby)
	condition := metav1.Condition{
		Type:               "OwnershipReady",
		Status:             metav1.ConditionUnknown,
		ObservedGeneration: connection.Generation,
		Reason:             "NotConfigured",
		Message:            "Regional ownership fencing is not configured.",
	}
	if connection.Spec.Ownership != nil {
		connection.Status.Region = connection.Spec.Ownership.Region
		if connection.Spec.Ownership.Role == sentryconfigurationv1alpha1.ConnectionRoleStandby {
			condition.Status = metav1.ConditionTrue
			condition.Reason = "StandbyConfigured"
			condition.Message = "The connection is safely fenced in Standby; no external mutation is authorized."
		} else {
			condition.Status = metav1.ConditionFalse
			condition.Reason = "RevisionUnverified"
			condition.Message = "The requested Active revision has no verified remote ownership marker; external mutation remains fenced."
		}
	}
	meta.SetStatusCondition(&connection.Status.Conditions, condition)
}
