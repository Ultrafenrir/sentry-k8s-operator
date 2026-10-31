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
	"crypto/tls"
	"errors"
	"flag"
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
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	sentryconfigurationv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	connectionruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/connection"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/controller"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
	webhookv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/internal/webhook/v1alpha1"
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

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var operatorNamespace string
	var stateNamespace string
	var routingNamespace string
	var auditNamespace string
	var managedBy string
	var challengeGCInterval time.Duration
	var challengeGCBatch int
	var auditGCInterval time.Duration
	var auditRetention time.Duration
	var auditGCBatch int
	var enableLeaderElection bool
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
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	defaultOperatorNamespace := os.Getenv("POD_NAMESPACE")
	if defaultOperatorNamespace == "" {
		defaultOperatorNamespace = "sentry-kubernetes-operator-system"
	}
	flag.StringVar(&operatorNamespace, "operator-namespace", defaultOperatorNamespace,
		"Fixed namespace from which platform-owned connection Secrets are read.")
	defaultStateNamespace := os.Getenv("STATE_NAMESPACE")
	if defaultStateNamespace == "" {
		defaultStateNamespace = "sentry-kubernetes-operator-state"
	}
	flag.StringVar(&stateNamespace, "state-namespace", defaultStateNamespace,
		"Dedicated namespace for protected binding and operation state Secrets.")
	defaultRoutingNamespace := os.Getenv("ROUTING_NAMESPACE")
	if defaultRoutingNamespace == "" {
		defaultRoutingNamespace = "sentry-kubernetes-operator-routing"
	}
	flag.StringVar(&routingNamespace, "routing-namespace", defaultRoutingNamespace,
		"Dedicated namespace for protected routing Secrets.")
	defaultAuditNamespace := os.Getenv("AUDIT_NAMESPACE")
	if defaultAuditNamespace == "" {
		defaultAuditNamespace = "sentry-kubernetes-operator-audit"
	}
	flag.StringVar(&auditNamespace, "audit-namespace", defaultAuditNamespace,
		"Dedicated namespace for immutable mutation audit Secrets.")
	flag.DurationVar(&challengeGCInterval, "challenge-gc-interval", 5*time.Minute,
		"Leader-only interval for expired Telegram challenge index collection.")
	flag.IntVar(&challengeGCBatch, "challenge-gc-batch", 100,
		"Maximum expired Telegram challenge indexes deleted per sweep.")
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
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if stateNamespace == "" || routingNamespace == "" || auditNamespace == "" || stateNamespace == operatorNamespace || routingNamespace == operatorNamespace || auditNamespace == operatorNamespace || routingNamespace == stateNamespace || auditNamespace == stateNamespace || auditNamespace == routingNamespace || challengeGCInterval <= 0 || challengeGCBatch <= 0 || auditGCInterval <= 0 || auditRetention <= 0 || auditGCBatch <= 0 {
		setupLog.Error(errors.New("namespace isolation is required"), "State, routing and audit namespaces must be non-empty and mutually separate from operator credentials")
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

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
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
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
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
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c4d7095e.io",
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
	registrationState := &binding.Store{Client: outputSecretClient, Namespace: stateNamespace}
	registrationRoutes := &registration.RouteStore{Client: outputSecretClient, Namespace: routingNamespace}
	registrationIndexes := &registration.ChallengeIndexStore{Client: outputSecretClient, Namespace: stateNamespace}
	registrationAudit := &audit.KubernetesStore{Client: outputSecretClient, Namespace: auditNamespace, Logger: ctrl.Log.WithName("registration-mutation-audit")}
	projectAudit := &audit.KubernetesStore{Client: outputSecretClient, Namespace: auditNamespace, Logger: ctrl.Log.WithName("project-mutation-audit")}
	destinationAudit := &audit.KubernetesStore{Client: outputSecretClient, Namespace: auditNamespace, Logger: ctrl.Log.WithName("destination-mutation-audit")}
	registrationRecovery := &registration.Service{
		Store:     registrationState,
		Publisher: registrationRoutes,
		Fences:    &registration.FenceCoordinator{Client: outputSecretClient},
		Audit:     registrationAudit,
	}
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
	if err := mgr.Add(&registration.ChallengeJanitor{
		Store: registrationIndexes, Interval: challengeGCInterval, Batch: challengeGCBatch,
		OnError: func(err error) {
			setupLog.Error(err, "Expired Telegram challenge index collection failed")
		},
	}); err != nil {
		setupLog.Error(err, "Failed to add Telegram challenge index collector")
		os.Exit(1)
	}
	if err := mgr.Add(&registration.RouteMigrator{
		Routes: registrationRoutes, SourceNamespace: stateNamespace,
		Interval: challengeGCInterval, Batch: challengeGCBatch,
		OnError: func(err error) {
			setupLog.Error(err, "Legacy Telegram route migration failed")
		},
	}); err != nil {
		setupLog.Error(err, "Failed to add legacy Telegram route migrator")
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
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		Registry:             connectionRegistry,
		Bindings:             bindingStore,
		OutputSecrets:        outputSecretClient,
		ManagedBy:            managedBy,
		RegistrationRecovery: registrationRecovery,
		RegistrationRoutes:   registrationRoutes,
		RegistrationIndexes:  registrationIndexes,
		Recorder:             mgr.GetEventRecorder("sentryproject-controller"),
		Metrics:              controllerMetrics,
		Audit:                projectAudit,
		DestinationAudit:     destinationAudit,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "sentryproject")
		os.Exit(1)
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha1.SetupSentryConnectionWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "SentryConnection")
			os.Exit(1)
		}
	}
	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha1.SetupSentryAlertPolicyWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "SentryAlertPolicy")
			os.Exit(1)
		}
	}
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1alpha1.SetupSentryProjectWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "SentryProject")
			os.Exit(1)
		}
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
