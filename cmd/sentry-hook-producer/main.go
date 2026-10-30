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
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/httphealth"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

type options struct {
	address             string
	healthAddress       string
	serverCertFile      string
	serverKeyFile       string
	clientCAFile        string
	allowedClientURI    string
	hookSourceNamespace string
	deliveryNamespace   string
	maxOutstanding      int
	maxAttempts         int
	dedupeRetention     time.Duration
	dlqRetention        time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.address, "address", ":8447", "Internal signed mTLS producer address.")
	flag.StringVar(&opts.healthAddress, "health-address", ":8081", "Internal HTTP health listener.")
	flag.StringVar(&opts.serverCertFile, "tls-cert-file", "", "Internal server certificate; reloaded per handshake.")
	flag.StringVar(&opts.serverKeyFile, "tls-key-file", "", "Internal server key; reloaded per handshake.")
	flag.StringVar(&opts.clientCAFile, "client-ca-file", "", "CA bundle used to verify the tokenless public receiver; reloaded per handshake.")
	flag.StringVar(&opts.allowedClientURI, "allowed-client-uri", "", "Exact URI SAN allowed to submit verified envelopes.")
	flag.StringVar(&opts.hookSourceNamespace, "hook-source-namespace", "sentry-kubernetes-operator-hook-sources", "Protected hook source namespace.")
	flag.StringVar(&opts.deliveryNamespace, "delivery-namespace", "sentry-kubernetes-operator-delivery", "Protected delivery queue namespace.")
	flag.IntVar(&opts.maxOutstanding, "max-outstanding", 1000, "Maximum outstanding delivery records.")
	flag.IntVar(&opts.maxAttempts, "max-attempts", 8, "Maximum worker attempts retained in queue semantics.")
	flag.DurationVar(&opts.dedupeRetention, "dedupe-retention", 24*time.Hour, "Sent tombstone retention.")
	flag.DurationVar(&opts.dlqRetention, "dlq-retention", 7*24*time.Hour, "Dead-letter retention.")
	flag.Parse()
	if err := opts.validate(); err != nil {
		log.Fatal(err)
	}
	if err := run(opts); err != nil {
		log.Fatal(err)
	}
}

func run(opts options) error {
	loadServerCertificate := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		certificate, err := tls.LoadX509KeyPair(opts.serverCertFile, opts.serverKeyFile)
		if err != nil {
			return nil, fmt.Errorf("reload producer certificate: %w", err)
		}
		return &certificate, nil
	}
	if _, err := loadServerCertificate(nil); err != nil {
		return err
	}
	tlsConfig, err := producerTLSConfig(opts.clientCAFile, loadServerCertificate)
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	kubernetesClient, err := client.New(ctrl.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	queue := &delivery.Queue{
		Client: kubernetesClient, Namespace: opts.deliveryNamespace, MaxOutstanding: opts.maxOutstanding,
		MaxAttempts: opts.maxAttempts, DedupeRetention: opts.dedupeRetention, DLQRetention: opts.dlqRetention,
	}
	producer := &sentryhook.Producer{
		Resolver: &sentryhook.SourceStore{Client: kubernetesClient, Namespace: opts.hookSourceNamespace},
		Queue:    queue,
	}
	handler := &sentryhook.InternalProducerHandler{Sink: producer, AllowedClientURI: opts.allowedClientURI}
	server := &http.Server{
		Addr: opts.address, Handler: handler, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}
	healthServer := &http.Server{
		Addr: opts.healthAddress, Handler: httphealth.Wrap(nil),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
		IdleTimeout: 10 * time.Second, MaxHeaderBytes: 1 << 10,
	}
	listener, err := net.Listen("tcp", opts.address)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- server.Serve(tls.NewListener(listener, tlsConfig)) }()
	go func() { errorsChannel <- healthServer.ListenAndServe() }()
	log.Printf("Sentry hook producer listening on %s", opts.address)
	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errorsChannel:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownContext)
	shutdownErr = errors.Join(shutdownErr, healthServer.Shutdown(shutdownContext))
	return errors.Join(runErr, shutdownErr)
}

func producerTLSConfig(clientCAFile string, loadServerCertificate func(*tls.ClientHelloInfo) (*tls.Certificate, error)) (*tls.Config, error) {
	clientCAs, err := loadCertPool(clientCAFile)
	if err != nil {
		return nil, err
	}
	configuration, err := sentryhook.InternalProducerTLSConfig(clientCAs)
	if err != nil {
		return nil, err
	}
	configuration.GetCertificate = loadServerCertificate
	configuration.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		pool, err := loadCertPool(clientCAFile)
		if err != nil {
			return nil, fmt.Errorf("reload producer client CA bundle: %w", err)
		}
		next := configuration.Clone()
		next.ClientCAs = pool
		next.GetConfigForClient = nil
		return next, nil
	}
	return configuration, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(contents) {
		return nil, errors.New("client CA bundle contains no certificates")
	}
	return pool, nil
}

func (o options) validate() error {
	if o.address == "" || o.healthAddress == "" || o.address == o.healthAddress ||
		o.serverCertFile == "" || o.serverKeyFile == "" || o.clientCAFile == "" || o.allowedClientURI == "" ||
		o.hookSourceNamespace == "" || o.deliveryNamespace == "" || o.hookSourceNamespace == o.deliveryNamespace ||
		o.maxOutstanding <= 0 || o.maxAttempts <= 0 || o.dedupeRetention <= 0 || o.dlqRetention <= 0 {
		return errors.New("TLS identity, distinct protected namespaces and positive queue limits are required")
	}
	return nil
}
