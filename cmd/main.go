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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sentryconfigurationv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/audit"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/Ultrafenrir/sentry-kubernetes-operator/internal/connection"
	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(sentryconfigurationv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// disabledWebhookServer satisfies controller-runtime's manager contract without opening a
// listener. Validation and defaulting for the shipped API are provided by the CRD schema.
type disabledWebhookServer struct {
	mux *http.ServeMux
}

func newDisabledWebhookServer() *disabledWebhookServer {
	return &disabledWebhookServer{mux: http.NewServeMux()}
}

func (*disabledWebhookServer) NeedLeaderElection() bool { return false }

func (*disabledWebhookServer) Register(string, http.Handler) {
	panic("runtime callback server is disabled")
}

func (*disabledWebhookServer) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (*disabledWebhookServer) StartedChecker() healthz.Checker {
	return func(*http.Request) error { return nil }
}

func (s *disabledWebhookServer) WebhookMux() *http.ServeMux { return s.mux }

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var operatorNamespace string
	var stateNamespace string
	var auditNamespace string
	var managedBy string
	var auditGCInterval time.Duration
	var auditRetention time.Duration
	var auditGCBatch int
	var enableLeaderElection bool
	var leaderElectionID string
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "sentry-kubernetes-operator", "Leader election lease name.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	defaultOperatorNamespace := os.Getenv("POD_NAMESPACE")
	flag.StringVar(&operatorNamespace, "operator-namespace", defaultOperatorNamespace,
		"Fixed namespace from which platform-owned connection Secrets are read.")
	defaultStateNamespace := os.Getenv("STATE_NAMESPACE")
	if defaultStateNamespace == "" {
		defaultStateNamespace = defaultOperatorNamespace
	}
	flag.StringVar(&stateNamespace, "state-namespace", defaultStateNamespace,
		"Namespace for protected binding and operation state Secrets.")
	defaultAuditNamespace := os.Getenv("AUDIT_NAMESPACE")
	if defaultAuditNamespace == "" {
		defaultAuditNamespace = defaultOperatorNamespace
	}
	flag.StringVar(&auditNamespace, "audit-namespace", defaultAuditNamespace,
		"Namespace for immutable mutation audit Secrets.")
	flag.DurationVar(&auditGCInterval, "audit-gc-interval", 10*time.Minute,
		"Leader-only interval for durable mutation audit retention.")
	flag.DurationVar(&auditRetention, "audit-retention", 30*24*time.Hour,
		"Minimum durable mutation audit retention.")
	flag.IntVar(&auditGCBatch, "audit-gc-batch", 100,
		"Maximum expired mutation audit records deleted per sweep.")
	flag.StringVar(&managedBy, "managed-by", "company-sentry-op",
		"Lowercase human-readable operator alias embedded in external ownership markers.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for a secure metrics server")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if operatorNamespace == "" || stateNamespace == "" || auditNamespace == "" || leaderElectionID == "" || auditGCInterval <= 0 || auditRetention <= 0 || auditGCBatch <= 0 {
		setupLog.Error(errors.New("invalid manager configuration"), "Namespaces, leader election ID and audit retention settings must be valid")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Metrics options configure the controller-runtime server. More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// A custom certificate remains available for non-default secure metrics deployments.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	restConfig := ctrl.GetConfigOrDie()
	secretNamespaces := map[string]cache.Config{
		operatorNamespace: {},
		stateNamespace:    {},
	}
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {Namespaces: secretNamespaces},
		}},
		Metrics:                metricsServerOptions,
		WebhookServer:          newDisabledWebhookServer(),
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}
	controllerMetrics, err := controller.NewMetrics(controllermetrics.Registry)
	if err != nil {
		setupLog.Error(err, "Failed to initialize controller metrics")
		os.Exit(1)
	}

	connectionRegistry := connectionruntime.NewRegistry()
	bindingStore := &binding.Store{Client: mgr.GetClient(), Namespace: stateNamespace}
	outputSecretClient, err := client.New(restConfig, client.Options{
		HTTPClient: mgr.GetHTTPClient(),
		Scheme:     mgr.GetScheme(),
		Mapper:     mgr.GetRESTMapper(),
	})
	if err != nil {
		setupLog.Error(err, "Failed to create uncached output Secret client")
		os.Exit(1)
	}
	projectAudit := &audit.KubernetesStore{Client: outputSecretClient, Namespace: auditNamespace, Logger: ctrl.Log.WithName("project-mutation-audit")}
	if err := mgr.Add(&audit.RetentionJanitor{
		Store:    &audit.KubernetesStore{Client: outputSecretClient, Namespace: auditNamespace},
		Interval: auditGCInterval, Retention: auditRetention, Batch: auditGCBatch,
		OnError: func(err error) {
			setupLog.Error(err, "Durable mutation audit retention failed")
		},
	}); err != nil {
		setupLog.Error(err, "Failed to add durable mutation audit retention")
		os.Exit(1)
	}
	if err := (&controller.SentryConnectionReconciler{
		Client:            mgr.GetClient(),
		Scheme:            mgr.GetScheme(),
		OperatorNamespace: operatorNamespace,
		Registry:          connectionRegistry,
		Recorder:          mgr.GetEventRecorder("sentryconnection-controller"),
		Metrics:           controllerMetrics,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "sentryconnection")
		os.Exit(1)
	}
	if err := (&controller.SentryAlertPolicyReconciler{
		Client: mgr.GetClient(), Bindings: bindingStore,
		Recorder: mgr.GetEventRecorder("sentryalertpolicy-controller"), Metrics: controllerMetrics,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "sentryalertpolicy")
		os.Exit(1)
	}
	if err := (&controller.SentryProjectReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Registry:      connectionRegistry,
		Bindings:      bindingStore,
		OutputSecrets: outputSecretClient,
		ManagedBy:     managedBy,
		Recorder:      mgr.GetEventRecorder("sentryproject-controller"),
		Metrics:       controllerMetrics,
		Audit:         projectAudit,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "sentryproject")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
