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
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/httphealth"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/telegramdelivery"
)

type options struct {
	healthAddress     string
	metricsAddress    string
	apiURL            string
	tokenFile         string
	routingNamespace  string
	deliveryNamespace string
	auditNamespace    string
	pollInterval      time.Duration
	lease             time.Duration
	retryBase         time.Duration
	retryMax          time.Duration
	maxAttempts       int
	maxOutstanding    int
	dedupeRetention   time.Duration
	dlqRetention      time.Duration
	sweepInterval     time.Duration
	metricsInterval   time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.healthAddress, "health-address", ":8081", "Internal HTTP health listener.")
	flag.StringVar(&opts.metricsAddress, "metrics-address", ":8080", "Internal identity-free Prometheus metrics listener.")
	flag.StringVar(&opts.apiURL, "telegram-api-url", "https://api.telegram.org", "HTTPS Telegram Bot API base URL.")
	flag.StringVar(&opts.tokenFile, "bot-token-file", "", "Projected Telegram bot token file, reloaded for every delivery.")
	flag.StringVar(&opts.routingNamespace, "routing-namespace", "sentry-kubernetes-operator-routing", "Protected routing namespace.")
	flag.StringVar(&opts.deliveryNamespace, "delivery-namespace", "sentry-kubernetes-operator-delivery", "Protected delivery queue namespace.")
	flag.StringVar(&opts.auditNamespace, "audit-namespace", "sentry-kubernetes-operator-audit", "Immutable mutation audit namespace.")
	flag.DurationVar(&opts.pollInterval, "poll-interval", 2*time.Second, "Idle queue poll interval.")
	flag.DurationVar(&opts.lease, "lease-duration", 30*time.Second, "Exclusive delivery claim duration.")
	flag.DurationVar(&opts.retryBase, "retry-base", 2*time.Second, "Initial delivery retry delay.")
	flag.DurationVar(&opts.retryMax, "retry-max", 5*time.Minute, "Maximum local exponential retry delay.")
	flag.IntVar(&opts.maxAttempts, "max-attempts", 10, "Attempts before an item enters the DLQ.")
	flag.IntVar(&opts.maxOutstanding, "max-outstanding", 10000, "Maximum pending/retry/in-flight/DLQ records.")
	flag.DurationVar(&opts.dedupeRetention, "dedupe-retention", 24*time.Hour, "Successful dedupe tombstone retention.")
	flag.DurationVar(&opts.dlqRetention, "dlq-retention", 7*24*time.Hour, "Dead-letter retention.")
	flag.DurationVar(&opts.sweepInterval, "sweep-interval", time.Minute, "Terminal-record cleanup interval.")
	flag.DurationVar(&opts.metricsInterval, "metrics-interval", 15*time.Second, "Aggregate queue metrics refresh interval.")
	flag.Parse()
	if err := opts.validate(); err != nil {
		log.Fatal(err)
	}
	if err := run(opts); err != nil {
		log.Fatal(err)
	}
}

func run(opts options) error {
	config, err := rest.InClusterConfig()
	if err != nil {
		config = ctrl.GetConfigOrDie()
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	directClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create direct Kubernetes client: %w", err)
	}
	token := telegramdelivery.FileToken(opts.tokenFile)
	if _, err := token(); err != nil {
		return errors.New("Telegram bot token is unavailable")
	}
	httpClient := &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13,
				NextProtos: []string{"http/1.1"},
			},
			MaxIdleConns: 10, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
		},
	}
	auditStore := &audit.KubernetesStore{Client: directClient, Namespace: opts.auditNamespace, Logger: funcr.New(func(prefix, arguments string) {
		log.Printf("%s%s", prefix, arguments)
	}, funcr.Options{})}
	queue := &delivery.Queue{
		Client: directClient, Namespace: opts.deliveryNamespace,
		MaxOutstanding: opts.maxOutstanding, MaxAttempts: opts.maxAttempts,
		DedupeRetention: opts.dedupeRetention, DLQRetention: opts.dlqRetention,
		Audit: auditStore,
	}
	owner, err := newOwner()
	if err != nil {
		return errors.New("delivery worker identity is unavailable")
	}
	worker := &telegramdelivery.Worker{
		Queue: queue, Routes: &registration.RouteStore{Client: directClient, Namespace: opts.routingNamespace},
		Sender: &telegramdelivery.Client{HTTPClient: httpClient, BaseURL: opts.apiURL, Token: token},
		Owner:  owner, Lease: opts.lease, RetryBase: opts.retryBase, RetryMax: opts.retryMax,
		Audit: auditStore,
	}
	metricsRegistry := prometheus.NewRegistry()
	deliveryMetrics, err := telegramdelivery.NewMetrics(metricsRegistry)
	if err != nil {
		return err
	}
	worker.Observer = deliveryMetrics
	healthServer := &http.Server{
		Addr: opts.healthAddress, Handler: workerHealthHandler(token),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	metricsServer := &http.Server{
		Addr: opts.metricsAddress, Handler: metricsHandler(promhttp.HandlerFor(metricsRegistry, promhttp.HandlerOpts{})),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- healthServer.ListenAndServe() }()
	go func() { errorsChannel <- metricsServer.ListenAndServe() }()
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		workLoop(ctx, worker, queue, deliveryMetrics, opts.pollInterval, opts.sweepInterval, opts.metricsInterval)
	}()
	log.Printf("telegram delivery worker started")
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errorsChannel:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
			stop()
		}
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := healthServer.Shutdown(shutdownContext)
	shutdownErr = errors.Join(shutdownErr, metricsServer.Shutdown(shutdownContext))
	select {
	case <-workerDone:
	case <-shutdownContext.Done():
		runErr = errors.Join(runErr, errors.New("delivery worker shutdown timed out"))
	}
	return errors.Join(runErr, shutdownErr)
}

func metricsHandler(metrics http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/metrics" || request.URL.RawQuery != "" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		metrics.ServeHTTP(writer, request)
	})
}

func workerHealthHandler(token telegramdelivery.TokenProvider) http.Handler {
	liveness := httphealth.Wrap(nil)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/readyz" && request.URL.RawQuery == "" {
			writer.Header().Set("Cache-Control", "no-store")
			if _, err := token(); err != nil {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusOK)
			return
		}
		liveness.ServeHTTP(writer, request)
	})
}

func workLoop(ctx context.Context, worker *telegramdelivery.Worker, queue *delivery.Queue, metrics *telegramdelivery.Metrics, pollInterval, sweepInterval, metricsInterval time.Duration) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	nextSweep := time.Now().Add(sweepInterval)
	nextMetrics := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			processed, err := worker.ProcessOne(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("delivery processing failed")
			}
			if !time.Now().Before(nextSweep) {
				if _, err := queue.Sweep(ctx, 100); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("delivery retention sweep failed")
				}
				nextSweep = time.Now().Add(sweepInterval)
			}
			if !time.Now().Before(nextMetrics) {
				if err := metrics.Refresh(ctx, queue); err != nil && !errors.Is(err, context.Canceled) {
					log.Printf("delivery metrics refresh failed")
				}
				nextMetrics = time.Now().Add(metricsInterval)
			}
			delay := time.Duration(0)
			if !processed || err != nil {
				delay = pollInterval
			}
			timer.Reset(delay)
		}
	}
}

func newOwner() (string, error) {
	material := make([]byte, 32)
	if n, err := rand.Read(material); err != nil || n != len(material) {
		return "", errors.New("random source failed")
	}
	return hex.EncodeToString(material), nil
}

func (o options) validate() error {
	parsed, err := url.Parse(o.apiURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("telegram-api-url must be an HTTPS base URL")
	}
	if o.healthAddress == "" || o.metricsAddress == "" || o.healthAddress == o.metricsAddress || o.tokenFile == "" || o.routingNamespace == "" || o.deliveryNamespace == "" || o.auditNamespace == "" || o.routingNamespace == o.deliveryNamespace || o.auditNamespace == o.routingNamespace || o.auditNamespace == o.deliveryNamespace || o.pollInterval <= 0 || o.lease <= 0 || o.retryBase <= 0 || o.retryMax < o.retryBase || o.maxAttempts <= 0 || o.maxOutstanding <= 0 || o.dedupeRetention <= 0 || o.dlqRetention <= 0 || o.sweepInterval <= 0 || o.metricsInterval <= 0 {
		return errors.New("health, token, isolated namespaces and positive bounded queue settings are required")
	}
	return nil
}
