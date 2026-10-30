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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryhook"
)

func TestOptionsRequireExactProducerTLSAndBoundedIngress(t *testing.T) {
	t.Parallel()
	valid := options{
		address: ":8446", serverCertFile: "server.crt", serverKeyFile: "server.key", sourcesDir: "/sources",
		producerURL: "https://producer.internal" + sentryhook.InternalDeliveryPath, producerCAFile: "ca.crt",
		clientCertFile: "client.crt", clientKeyFile: "client.key", maxBodyBytes: 64 << 10, maxClockSkew: 5 * time.Minute,
	}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*options){
		func(value *options) { value.producerURL = "http://producer.internal" + sentryhook.InternalDeliveryPath },
		func(value *options) { value.producerURL += "?source=private" },
		func(value *options) { value.sourcesDir = "" },
		func(value *options) { value.clientCertFile = "" },
		func(value *options) { value.maxBodyBytes = 0 },
		func(value *options) { value.maxClockSkew = 2 * time.Hour },
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.validate(); err == nil {
			t.Fatalf("invalid options accepted: %#v", candidate)
		}
	}
}

func TestProducerClientTLSConfigReloadsCABundleAndFailsClosed(t *testing.T) {
	t.Parallel()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeTestCA(t, caFile, "old-producer-ca", 1)
	loader := func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &tls.Certificate{}, nil }
	first, err := producerClientTLSConfig("producer.internal", caFile, loader)
	if err != nil || first.ServerName != "producer.internal" || first.MinVersion != tls.VersionTLS13 || len(first.RootCAs.Subjects()) != 1 {
		t.Fatalf("first config=%#v error=%v", first, err)
	}
	writeTestCA(t, caFile, "new-producer-ca", 2)
	second, err := producerClientTLSConfig("producer.internal", caFile, loader)
	if err != nil || reflect.DeepEqual(first.RootCAs.Subjects(), second.RootCAs.Subjects()) {
		t.Fatalf("CA bundle was not reloaded: first=%q second=%q error=%v", first.RootCAs.Subjects(), second.RootCAs.Subjects(), err)
	}
	if err := os.WriteFile(caFile, []byte("not a CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := producerClientTLSConfig("producer.internal", caFile, loader); err == nil {
		t.Fatal("malformed replacement CA bundle was accepted")
	}
}

func writeTestCA(t *testing.T, path, commonName string, serial int64) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReceiverReadinessTracksProjectedConfigWithoutBreakingLiveness(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	handler := receiverHandler(&sentryhook.FileSourceHandler{Directory: directory, Sink: receiverSink{}})
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("empty readiness=%d", response.Code)
	}
	if err := os.WriteFile(filepath.Join(directory, strings.Repeat("a", 64)+".json"), []byte(`{"invalid":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable || response.Body.Len() != 0 {
		t.Fatalf("malformed readiness=%d body=%q", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("liveness=%d", response.Code)
	}
}

type receiverSink struct{}

func (receiverSink) Accept(context.Context, sentryhook.Envelope) error { return nil }
