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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/adminreport"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/kubectlplugin"
)

type registrationOptions struct {
	kubeconfig  string
	context     string
	namespace   string
	endpoint    string
	caFile      string
	serverName  string
	tokenFile   string
	timeout     time.Duration
	project     string
	destination string
}

type adminReportOptions struct {
	kubeconfig          string
	context             string
	stateNamespace      string
	routingNamespace    string
	deliveryNamespace   string
	hookSourceNamespace string
	timeout             time.Duration
}

type auditExportOptions struct {
	kubeconfig string
	context    string
	namespace  string
	timeout    time.Duration
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: kubectl sentry <telegram-register|admin-report|audit-export> [flags]")
	}
	switch args[0] {
	case "telegram-register":
		return runTelegramRegister(args, output)
	case "admin-report":
		return runAdminReport(args, output)
	case "audit-export":
		return runAuditExport(args, output)
	default:
		return errors.New("usage: kubectl sentry <telegram-register|admin-report|audit-export> [flags]")
	}
}

func runAuditExport(args []string, output io.Writer) error {
	var opts auditExportOptions
	flags := flag.NewFlagSet("audit-export", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to kubeconfig.")
	flags.StringVar(&opts.context, "context", "", "Kubeconfig context.")
	flags.StringVar(&opts.namespace, "audit-namespace", "sentry-kubernetes-operator-audit", "Protected mutation audit namespace.")
	flags.DurationVar(&opts.timeout, "timeout", 15*time.Second, "Read-only export timeout.")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return errors.New("invalid audit-export flags")
	}
	if opts.timeout <= 0 || opts.namespace == "" {
		return errors.New("positive timeout and --audit-namespace are required")
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = opts.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.context}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return errors.New("cannot load kubeconfig authentication")
	}
	config = rest.CopyConfig(config)
	config.Timeout = opts.timeout
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return errors.New("cannot initialize audit export client")
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return errors.New("cannot initialize audit export client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	return exportAudit(ctx, kubernetesClient, opts.namespace, output)
}

func exportAudit(ctx context.Context, kubernetesClient client.Client, namespace string, output io.Writer) error {
	records, err := (&audit.KubernetesStore{Client: kubernetesClient, Namespace: namespace}).List(ctx)
	if err != nil {
		return errors.New("audit export failed validation")
	}
	events := make([]audit.Event, len(records))
	for i := range records {
		events[i] = records[i].Event
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(struct {
		Events []audit.Event `json:"events"`
	}{Events: events}); err != nil {
		return errors.New("cannot write audit export")
	}
	return nil
}

func runTelegramRegister(args []string, output io.Writer) error {
	var opts registrationOptions
	flags := flag.NewFlagSet("telegram-register", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to kubeconfig.")
	flags.StringVar(&opts.context, "context", "", "Kubeconfig context.")
	flags.StringVar(&opts.namespace, "namespace", "", "SentryProject namespace.")
	flags.StringVar(&opts.namespace, "n", "", "SentryProject namespace.")
	flags.StringVar(&opts.endpoint, "endpoint", os.Getenv("SENTRY_REGISTRATION_ENDPOINT"), "Registration endpoint base URL.")
	flags.StringVar(&opts.caFile, "ca-file", "", "Registration endpoint CA bundle.")
	flags.StringVar(&opts.serverName, "server-name", "", "Registration endpoint TLS server name override.")
	flags.StringVar(&opts.tokenFile, "token-file", "", "Short-lived audience-bound bearer token file.")
	flags.DurationVar(&opts.timeout, "timeout", 15*time.Second, "Request timeout.")
	if err := flags.Parse(args[1:]); err != nil {
		return errors.New("invalid telegram-register flags")
	}
	positionals := flags.Args()
	if len(positionals) != 2 {
		return errors.New("telegram-register requires PROJECT and DESTINATION")
	}
	opts.project, opts.destination = positionals[0], positionals[1]
	if opts.endpoint == "" || opts.timeout <= 0 {
		return errors.New("--endpoint (or SENTRY_REGISTRATION_ENDPOINT) and a positive --timeout are required")
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = opts.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.context}
	deferred := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	if opts.namespace == "" {
		namespace, _, err := deferred.Namespace()
		if err != nil {
			return errors.New("cannot resolve kubeconfig namespace")
		}
		opts.namespace = namespace
	}
	config, err := deferred.ClientConfig()
	if err != nil {
		return errors.New("cannot load kubeconfig authentication")
	}
	transportConfig := rest.CopyConfig(config)
	transportConfig.Host = opts.endpoint
	transportConfig.APIPath = ""
	transportConfig.TLSClientConfig.CAFile = opts.caFile
	transportConfig.TLSClientConfig.CAData = nil
	transportConfig.TLSClientConfig.ServerName = opts.serverName
	transportConfig.TLSClientConfig.Insecure = false
	if opts.tokenFile != "" {
		token, err := os.ReadFile(opts.tokenFile)
		if err != nil {
			return errors.New("cannot read bearer token file")
		}
		transportConfig.BearerToken = strings.TrimSpace(string(token))
		transportConfig.BearerTokenFile = ""
		transportConfig.ExecProvider = nil
		transportConfig.AuthProvider = nil
		transportConfig.Username = ""
		transportConfig.Password = ""
		if transportConfig.BearerToken == "" || len(transportConfig.BearerToken) > 16384 {
			return errors.New("bearer token file is empty or invalid")
		}
	}
	httpClient, err := rest.HTTPClientFor(transportConfig)
	if err != nil {
		return errors.New("cannot construct authenticated registration client")
	}
	httpClient, err = kubectlplugin.NewHTTPClient(httpClient, opts.timeout)
	if err != nil {
		return err
	}
	result, err := (&kubectlplugin.Client{HTTPClient: httpClient, Endpoint: opts.endpoint}).Issue(context.Background(), kubectlplugin.Request{
		Namespace: opts.namespace, Project: opts.project, Destination: opts.destination,
	})
	if err != nil {
		return errors.New("registration challenge request failed")
	}
	_, err = fmt.Fprintln(output, result.Command)
	return err
}

func runAdminReport(args []string, output io.Writer) error {
	var opts adminReportOptions
	flags := flag.NewFlagSet("admin-report", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&opts.kubeconfig, "kubeconfig", "", "Path to kubeconfig.")
	flags.StringVar(&opts.context, "context", "", "Kubeconfig context.")
	flags.StringVar(&opts.stateNamespace, "state-namespace", "sentry-kubernetes-operator-state", "Protected binding state namespace.")
	flags.StringVar(&opts.routingNamespace, "routing-namespace", "sentry-kubernetes-operator-routing", "Protected routing namespace.")
	flags.StringVar(&opts.deliveryNamespace, "delivery-namespace", "sentry-kubernetes-operator-delivery", "Protected delivery queue namespace.")
	flags.StringVar(&opts.hookSourceNamespace, "hook-source-namespace", "sentry-kubernetes-operator-hook-sources", "Protected authenticated hook source namespace.")
	flags.DurationVar(&opts.timeout, "timeout", 15*time.Second, "Read-only report timeout.")
	if err := flags.Parse(args[1:]); err != nil || len(flags.Args()) != 0 {
		return errors.New("invalid admin-report flags")
	}
	if opts.timeout <= 0 || !distinctStrings(opts.stateNamespace, opts.routingNamespace, opts.deliveryNamespace, opts.hookSourceNamespace) {
		return errors.New("positive timeout and four distinct protected recovery namespaces are required")
	}
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	loadingRules.ExplicitPath = opts.kubeconfig
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.context}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		return errors.New("cannot load kubeconfig authentication")
	}
	config = rest.CopyConfig(config)
	config.Timeout = opts.timeout
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return errors.New("cannot initialize report client")
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return errors.New("cannot initialize report client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()
	report, err := (&adminreport.Reporter{
		Client: kubernetesClient, StateNamespace: opts.stateNamespace,
		RoutingNamespace: opts.routingNamespace, DeliveryNamespace: opts.deliveryNamespace,
		HookSourceNamespace: opts.hookSourceNamespace,
	}).Build(ctx)
	if err != nil {
		return errors.New("admin report failed")
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(report); err != nil {
		return errors.New("cannot write admin report")
	}
	return nil
}

func distinctStrings(values ...string) bool {
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
