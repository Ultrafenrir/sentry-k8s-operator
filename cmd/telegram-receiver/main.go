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
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/telegramreceiver"
)

type options struct {
	address            string
	serverCertFile     string
	serverKeyFile      string
	telegramSecretFile string
	botUsername        string
	writerURL          string
	writerCAFile       string
	writerCertFile     string
	writerKeyFile      string
}

func main() {
	var opts options
	flag.StringVar(&opts.address, "address", ":8443", "Public Telegram webhook HTTPS address.")
	flag.StringVar(&opts.serverCertFile, "tls-cert-file", "", "Public HTTPS server certificate file; reloaded on each handshake.")
	flag.StringVar(&opts.serverKeyFile, "tls-key-file", "", "Public HTTPS server private key file; reloaded on each handshake.")
	flag.StringVar(&opts.telegramSecretFile, "telegram-secret-file", "", "File containing the setWebhook secret_token; reloaded on each request.")
	flag.StringVar(&opts.botUsername, "bot-username", "", "Bot username used to accept /register@username commands.")
	flag.StringVar(&opts.writerURL, "writer-url", "", "Exact internal registration writer redemption URL.")
	flag.StringVar(&opts.writerCAFile, "writer-ca-file", "", "CA bundle for the registration writer.")
	flag.StringVar(&opts.writerCertFile, "writer-cert-file", "", "mTLS client certificate for the registration writer; reloaded per operation/handshake.")
	flag.StringVar(&opts.writerKeyFile, "writer-key-file", "", "mTLS client key for the registration writer; reloaded per operation/handshake.")
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
			return nil, fmt.Errorf("reload webhook certificate: %w", err)
		}
		return &certificate, nil
	}
	loadWriterCertificate := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		certificate, err := tls.LoadX509KeyPair(opts.writerCertFile, opts.writerKeyFile)
		if err != nil {
			return nil, fmt.Errorf("reload writer client certificate: %w", err)
		}
		return &certificate, nil
	}
	loadSigner := func() (crypto.Signer, error) {
		certificate, err := tls.LoadX509KeyPair(opts.writerCertFile, opts.writerKeyFile)
		if err != nil {
			return nil, err
		}
		signer, ok := certificate.PrivateKey.(crypto.Signer)
		if !ok {
			return nil, errors.New("writer client key cannot sign operations")
		}
		return signer, nil
	}
	if _, err := loadServerCertificate(nil); err != nil {
		return err
	}
	if _, err := loadWriterCertificate(nil); err != nil {
		return err
	}
	if _, err := loadSigner(); err != nil {
		return err
	}
	roots, err := loadCertPool(opts.writerCAFile)
	if err != nil {
		return err
	}
	writerHTTPClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS13, RootCAs: roots, GetClientCertificate: loadWriterCertificate,
				NextProtos: []string{"http/1.1"},
			},
			MaxIdleConns: 10, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
		},
	}
	writer := &telegramreceiver.WriterClient{URL: opts.writerURL, HTTPClient: writerHTTPClient, SignerProvider: loadSigner}
	handler := &telegramreceiver.Handler{
		Secret: telegramreceiver.FileSecret(opts.telegramSecretFile), Writer: writer, BotUsername: opts.botUsername,
	}
	server := &http.Server{
		Addr: opts.address, Handler: httphealth.Wrap(handler),
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, GetCertificate: loadServerCertificate},
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	listener, err := net.Listen("tcp", opts.address)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	log.Printf("telegram receiver listening on %s", opts.address)
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

func loadCertPool(path string) (*x509.CertPool, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read writer CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(contents) {
		return nil, errors.New("writer CA bundle contains no certificates")
	}
	return pool, nil
}

func (o options) validate() error {
	parsed, err := url.Parse(o.writerURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != registration.InternalRedemptionPath || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("writer-url must be an exact HTTPS internal redemption URL")
	}
	if o.address == "" || o.serverCertFile == "" || o.serverKeyFile == "" || o.telegramSecretFile == "" || o.writerCAFile == "" || o.writerCertFile == "" || o.writerKeyFile == "" {
		return errors.New("address, public TLS files, Telegram secret and writer mTLS files are required")
	}
	return nil
}
