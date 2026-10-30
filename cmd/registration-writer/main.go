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

	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/httphealth"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/registration"
)

type options struct {
	challengeAddress string
	internalAddress  string
	certificateFile  string
	keyFile          string
	clientCAFile     string
	clientURI        string
	audience         string
	stateNamespace   string
	routingNamespace string
	auditNamespace   string
}

func main() {
	var opts options
	flag.StringVar(&opts.challengeAddress, "challenge-address", ":8444", "HTTPS address for the Kubernetes-authenticated challenge API.")
	flag.StringVar(&opts.internalAddress, "internal-address", ":8445", "mTLS address for receiver redemption handoff.")
	flag.StringVar(&opts.certificateFile, "tls-cert-file", "", "PEM server certificate file; reloaded on every TLS handshake.")
	flag.StringVar(&opts.keyFile, "tls-key-file", "", "PEM server private key file; reloaded on every TLS handshake.")
	flag.StringVar(&opts.clientCAFile, "client-ca-file", "", "PEM CA bundle for internal receiver client certificates; reloaded on every TLS handshake.")
	flag.StringVar(&opts.clientURI, "receiver-client-uri", "", "Exact URI SAN allowed on the internal mTLS listener.")
	flag.StringVar(&opts.audience, "token-audience", "sentry-registration", "Required Kubernetes service-account token audience.")
	flag.StringVar(&opts.stateNamespace, "state-namespace", "sentry-kubernetes-operator-state", "Dedicated protected-state namespace.")
	flag.StringVar(&opts.routingNamespace, "routing-namespace", "sentry-kubernetes-operator-routing", "Dedicated protected routing namespace.")
	flag.StringVar(&opts.auditNamespace, "audit-namespace", "sentry-kubernetes-operator-audit", "Dedicated immutable mutation audit namespace.")
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
	if err := sentryv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	directClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create direct Kubernetes client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create Kubernetes review client: %w", err)
	}
	state := &binding.Store{Client: directClient, Namespace: opts.stateNamespace}
	routes := &registration.RouteStore{Client: directClient, Namespace: opts.routingNamespace}
	index := &registration.ChallengeIndexStore{Client: directClient, Namespace: opts.stateNamespace}
	service := &registration.Service{
		Store: state, Index: index, Publisher: routes,
		Fences: &registration.FenceCoordinator{Client: directClient},
		Audit: &audit.KubernetesStore{Client: directClient, Namespace: opts.auditNamespace, Logger: funcr.New(func(prefix, arguments string) {
			log.Printf("%s %s", prefix, arguments)
		}, funcr.Options{})},
	}
	challengeHandler := &registration.ChallengeHandler{
		Tokens: clientset.AuthenticationV1().TokenReviews(), Access: clientset.AuthorizationV1().SubjectAccessReviews(),
		Client: directClient, Registration: service, Audiences: []string{opts.audience},
	}
	internalHandler := &registration.InternalHandler{Redeemer: service, AllowedClientURI: opts.clientURI}

	certificateLoader := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		certificate, err := tls.LoadX509KeyPair(opts.certificateFile, opts.keyFile)
		if err != nil {
			return nil, fmt.Errorf("reload server certificate: %w", err)
		}
		return &certificate, nil
	}
	if _, err := certificateLoader(nil); err != nil {
		return err
	}
	challengeTLS := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"http/1.1"}, GetCertificate: certificateLoader}
	clientCAs, err := loadCertPool(opts.clientCAFile)
	if err != nil {
		return err
	}
	internalTLS, err := registration.InternalServerTLSConfig(clientCAs)
	if err != nil {
		return err
	}
	internalTLS.GetCertificate = certificateLoader
	internalTemplate := internalTLS.Clone()
	internalTLS.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		pool, err := loadCertPool(opts.clientCAFile)
		if err != nil {
			return nil, err
		}
		next := internalTemplate.Clone()
		next.ClientCAs = pool
		next.GetCertificate = certificateLoader
		next.GetConfigForClient = nil
		return next, nil
	}

	challengeServer := newServer(opts.challengeAddress, httphealth.Wrap(challengeHandler), challengeTLS)
	internalServer := newServer(opts.internalAddress, internalHandler, internalTLS)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errorsChannel := make(chan error, 2)
	go func() { errorsChannel <- serveTLS(challengeServer) }()
	go func() { errorsChannel <- serveTLS(internalServer) }()
	log.Printf("registration writer listening on challenge=%s internal=%s", opts.challengeAddress, opts.internalAddress)

	var runErr error
	select {
	case <-ctx.Done():
	case err := <-errorsChannel:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			stop()
			runErr = err
		}
	}
	shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(runErr, challengeServer.Shutdown(shutdownContext), internalServer.Shutdown(shutdownContext))
}

func newServer(address string, handler http.Handler, tlsConfig *tls.Config) *http.Server {
	return &http.Server{
		Addr: address, Handler: handler, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
}

func serveTLS(server *http.Server) error {
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	return server.Serve(tls.NewListener(listener, server.TLSConfig))
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
	if o.challengeAddress == "" || o.internalAddress == "" || o.challengeAddress == o.internalAddress || o.certificateFile == "" || o.keyFile == "" || o.clientCAFile == "" || o.clientURI == "" || o.audience == "" || o.stateNamespace == "" || o.routingNamespace == "" || o.auditNamespace == "" || o.routingNamespace == o.stateNamespace || o.auditNamespace == o.stateNamespace || o.auditNamespace == o.routingNamespace {
		return errors.New("distinct listeners and state namespaces, TLS files, receiver URI and token audience are required")
	}
	return nil
}
