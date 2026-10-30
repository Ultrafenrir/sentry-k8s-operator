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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestOptionsRequireTLSIdentityNamespaceIsolationAndQueueBounds(t *testing.T) {
	t.Parallel()
	valid := options{
		address: ":8447", healthAddress: ":8081", serverCertFile: "tls.crt", serverKeyFile: "tls.key", clientCAFile: "ca.crt",
		allowedClientURI:    "spiffe://cluster.local/ns/system/sa/receiver",
		hookSourceNamespace: "hook-sources", deliveryNamespace: "delivery",
		maxOutstanding: 100, maxAttempts: 8, dedupeRetention: time.Hour, dlqRetention: 24 * time.Hour,
	}
	if err := valid.validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*options){
		func(value *options) { value.healthAddress = value.address },
		func(value *options) { value.allowedClientURI = "" },
		func(value *options) { value.clientCAFile = "" },
		func(value *options) { value.deliveryNamespace = value.hookSourceNamespace },
		func(value *options) { value.maxOutstanding = 0 },
		func(value *options) { value.maxAttempts = 0 },
		func(value *options) { value.dedupeRetention = 0 },
		func(value *options) { value.dlqRetention = 0 },
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.validate(); err == nil {
			t.Fatalf("invalid options accepted: %#v", candidate)
		}
	}
}

func TestProducerTLSConfigReloadsClientCABundleAndFailsClosed(t *testing.T) {
	t.Parallel()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeTestCA(t, caFile, "old-client-ca", 1)
	configuration, err := producerTLSConfig(caFile, func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		return &tls.Certificate{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := configuration.GetConfigForClient(nil)
	if err != nil || len(first.ClientCAs.Subjects()) != 1 {
		t.Fatalf("first subjects=%q error=%v", first.ClientCAs.Subjects(), err)
	}
	writeTestCA(t, caFile, "new-client-ca", 2)
	second, err := configuration.GetConfigForClient(nil)
	if err != nil || reflect.DeepEqual(first.ClientCAs.Subjects(), second.ClientCAs.Subjects()) {
		t.Fatalf("CA bundle was not reloaded: first=%q second=%q error=%v", first.ClientCAs.Subjects(), second.ClientCAs.Subjects(), err)
	}
	if err := os.WriteFile(caFile, []byte("not a CA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := configuration.GetConfigForClient(nil); err == nil {
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
