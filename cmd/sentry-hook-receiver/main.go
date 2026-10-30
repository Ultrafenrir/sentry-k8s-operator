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
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/httphealth"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

type options struct {
	address        string
	serverCertFile string
	serverKeyFile  string
	sourcesDir     string
	producerURL    string
	producerCAFile string
	clientCertFile string
	clientKeyFile  string
	maxBodyBytes   int64
	maxClockSkew   time.Duration
}

func main() {
	var opts options
	flag.StringVar(&opts.address, "address", ":8446", "Public Sentry service-hook HTTPS address.")
	flag.StringVar(&opts.serverCertFile, "tls-cert-file", "", "Public HTTPS server certificate; reloaded per handshake.")
	flag.StringVar(&opts.serverKeyFile, "tls-key-file", "", "Public HTTPS server key; reloaded per handshake.")
	flag.StringVar(&opts.sourcesDir, "sources-dir", "", "Projected source configuration directory; reloaded per request.")
	flag.StringVar(&opts.producerURL, "producer-url", "", "Exact internal producer delivery URL.")
	flag.StringVar(&opts.producerCAFile, "producer-ca-file", "", "CA bundle for the internal producer; reloaded per connection.")
	flag.StringVar(&opts.clientCertFile, "producer-cert-file", "", "mTLS client certificate; reloaded per operation/handshake.")
	flag.StringVar(&opts.clientKeyFile, "producer-key-file", "", "mTLS client key; reloaded per operation/handshake.")
	flag.Int64Var(&opts.maxBodyBytes, "max-body-bytes", 64<<10, "Maximum authenticated Sentry request body.")
	flag.DurationVar(&opts.maxClockSkew, "max-clock-skew", 5*time.Minute, "Maximum accepted Sentry timestamp skew.")
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
			return nil, fmt.Errorf("reload Sentry hook server certificate: %w", err)
		}
		return &certificate, nil
	}
	loadClientCertificate := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		certificate, err := tls.LoadX509KeyPair(opts.clientCertFile, opts.clientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("reload producer client certificate: %w", err)
		}
		return &certificate, nil
	}
	loadSigner := func() (crypto.Signer, error) {
		certificate, err := tls.LoadX509KeyPair(opts.clientCertFile, opts.clientKeyFile)
		if err != nil {
			return nil, err
		}
		signer, ok := certificate.PrivateKey.(crypto.Signer)
		if !ok {
			return nil, errors.New("producer client key cannot sign operations")
		}
		return signer, nil
	}
	if _, err := loadServerCertificate(nil); err != nil {
		return err
	}
	if _, err := loadClientCertificate(nil); err != nil {
		return err
	}
	if _, err := loadSigner(); err != nil {
		return err
	}
	producerTransport, err := newProducerTransport(opts, loadClientCertificate)
	if err != nil {
		return err
	}
	producerHTTPClient := &http.Client{
		Timeout: 10 * time.Second, Transport: producerTransport,
	}
	producer := &sentryhook.ProducerClient{URL: opts.producerURL, HTTPClient: producerHTTPClient, SignerProvider: loadSigner}
	handler := &sentryhook.FileSourceHandler{
		Directory: opts.sourcesDir, Sink: producer, MaxBodyBytes: opts.maxBodyBytes, MaxClockSkew: opts.maxClockSkew,
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, GetCertificate: loadServerCertificate}
	server := &http.Server{
		Addr: opts.address, Handler: receiverHandler(handler), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	listener, err := net.Listen("tcp", opts.address)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.Serve(tls.NewListener(listener, tlsConfig)) }()
	log.Printf("Sentry hook receiver listening on %s", opts.address)
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
	return errors.Join(runErr, server.Shutdown(shutdownContext))
}

func newProducerTransport(opts options, loadClientCertificate func(*tls.CertificateRequestInfo) (*tls.Certificate, error)) (*http.Transport, error) {
	parsed, err := url.Parse(opts.producerURL)
	if err != nil || parsed.Hostname() == "" {
		return nil, errors.New("producer URL has no TLS server name")
	}
	if _, err := producerClientTLSConfig(parsed.Hostname(), opts.producerCAFile, loadClientCertificate); err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil, DialContext: dialer.DialContext,
		DialTLSContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			configuration, err := producerClientTLSConfig(parsed.Hostname(), opts.producerCAFile, loadClientCertificate)
			if err != nil {
				return nil, err
			}
			return (&tls.Dialer{NetDialer: dialer, Config: configuration}).DialContext(ctx, network, address)
		},
		TLSHandshakeTimeout: 5 * time.Second, DisableKeepAlives: true,
		MaxIdleConns: 0, MaxIdleConnsPerHost: 0, IdleConnTimeout: 30 * time.Second,
	}, nil
}

func producerClientTLSConfig(serverName, caFile string, loadClientCertificate func(*tls.CertificateRequestInfo) (*tls.Certificate, error)) (*tls.Config, error) {
	roots, err := loadCertPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: serverName,
		GetClientCertificate: loadClientCertificate, NextProtos: []string{"http/1.1"},
	}, nil
}

func receiverHandler(sources *sentryhook.FileSourceHandler) http.Handler {
	liveness := httphealth.Wrap(sources)
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/readyz" && request.URL.RawQuery == "" {
			writer.Header().Set("Cache-Control", "no-store")
			if sources.Ready() != nil {
				writer.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			writer.WriteHeader(http.StatusOK)
			return
		}
		liveness.ServeHTTP(writer, request)
	})
}

func loadCertPool(path string) (*x509.CertPool, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read producer CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(contents) {
		return nil, errors.New("producer CA bundle contains no certificates")
	}
	return pool, nil
}

func (o options) validate() error {
	parsed, err := url.Parse(o.producerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != sentryhook.InternalDeliveryPath || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("producer-url must be the exact HTTPS internal delivery URL")
	}
	if o.address == "" || o.serverCertFile == "" || o.serverKeyFile == "" || o.sourcesDir == "" || o.producerCAFile == "" ||
		o.clientCertFile == "" || o.clientKeyFile == "" || o.maxBodyBytes <= 0 || o.maxBodyBytes > 1<<20 || o.maxClockSkew <= 0 || o.maxClockSkew > time.Hour {
		return errors.New("address, TLS files, source directory and bounded request policy are required")
	}
	return nil
}
