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

package controller

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/audit"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	projectruntime "github.com/artemantipov/sentry-kubernetes-operator/internal/project"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

var _ = Describe("Live Sentry project recovery", Label("live-sentry"), func() {
	It("recovers a lost successful create response through the Kubernetes journal without a second POST", func() {
		baseURL := os.Getenv("SENTRY_LIVE_BASE_URL")
		tokenFile := os.Getenv("SENTRY_LIVE_TOKEN_FILE")
		organization := os.Getenv("SENTRY_LIVE_ORGANIZATION")
		team := os.Getenv("SENTRY_LIVE_TEAM")
		version := os.Getenv("SENTRY_LIVE_VERSION")
		if baseURL == "" || tokenFile == "" || organization == "" || team == "" || version == "" {
			Skip("live Sentry acceptance environment is not configured")
		}

		tokenBytes, err := os.ReadFile(tokenFile)
		Expect(err).NotTo(HaveOccurred())
		token := strings.TrimSpace(string(tokenBytes))
		Expect(token).To(MatchRegexp(`^sntryu_[0-9a-f]{64}$`))
		upstream, err := url.Parse(baseURL)
		Expect(err).NotTo(HaveOccurred())
		Expect(upstream.Scheme).To(Equal("http"))
		Expect(upstream.Hostname()).To(Equal("127.0.0.1"))

		slug := "sko-controller-recovery-" + strings.ReplaceAll(version, ".", "-")
		proxy := newLostCreateResponseProxy(upstream, slug)
		defer proxy.Close()
		defer deleteLiveProject(upstream, token, organization, slug)

		clientConfig := sentryapi.Config{
			BaseURL:       proxy.URL,
			Token:         token,
			CABundle:      certificatePEM(proxy.Certificate()),
			Timeout:       30 * time.Second,
			MaxConcurrent: 2,
			MaxRetries:    0,
			UserAgent:     "sentry-kubernetes-operator/live-acceptance",
		}
		apiClient, err := sentryapi.NewClient(clientConfig)
		Expect(err).NotTo(HaveOccurred())

		store := &binding.Store{Client: k8sClient, Namespace: "default"}
		auditSink := &liveAuditSink{}
		engine := &projectruntime.Engine{
			Bindings:  store,
			Adapter:   &projectruntime.HTTPAdapter{Client: apiClient},
			ManagedBy: "sko",
			Audit:     auditSink,
		}
		input := liveManagedProjectInput(proxy.URL, organization, team, slug, version)

		_, err = engine.Reconcile(context.Background(), input)
		Expect(projectruntime.IsReason(err, projectruntime.ReasonCreationOutcomeUnknown)).To(BeTrue())
		Expect(proxy.createCalls.Load()).To(Equal(int32(1)))
		pending, err := store.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(pending.Binding.Pending).NotTo(BeNil())
		Expect(pending.Binding.Pending.Phase).To(Equal(string(sentryapi.OperationSubmitted)))

		restartedClient, err := sentryapi.NewClient(clientConfig)
		Expect(err).NotTo(HaveOccurred())
		restartedStore := &binding.Store{Client: k8sClient, Namespace: "default"}
		restartedEngine := &projectruntime.Engine{
			Bindings:  restartedStore,
			Adapter:   &projectruntime.HTTPAdapter{Client: restartedClient},
			ManagedBy: "sko",
			Audit:     auditSink,
		}
		result, err := restartedEngine.Reconcile(context.Background(), input)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.Origin).To(Equal(binding.OriginManaged))
		Expect(result.AccessMode).To(Equal(binding.AccessManage))
		Expect(proxy.createCalls.Load()).To(Equal(int32(1)))

		recovered, err := restartedStore.GetBinding(context.Background(), string(input.Project.UID))
		Expect(err).NotTo(HaveOccurred())
		Expect(recovered.Binding.ExternalIdentity).NotTo(BeEmpty())
		Expect(recovered.Binding.Pending).To(BeNil())
		Expect(auditSink.has(audit.OutcomeRecovered, audit.ReasonResultRecovered)).To(BeTrue())

		var deletion projectruntime.DeletionResult
		for attempt := 0; attempt < 30; attempt++ {
			deletion, err = restartedEngine.Delete(context.Background(), input)
			if err == nil && deletion.Complete {
				break
			}
			if err != nil && !projectruntime.IsReason(err, projectruntime.ReasonDeletionPending) {
				Fail("live project deletion failed: " + err.Error())
			}
			time.Sleep(time.Second)
		}
		Expect(err).NotTo(HaveOccurred())
		Expect(deletion.Complete).To(BeTrue())
		_, err = restartedStore.GetBinding(context.Background(), string(input.Project.UID))
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
		_, err = restartedStore.GetClaim(context.Background(), proxy.URL, organization, slug)
		Expect(errors.Is(err, binding.ErrNotFound)).To(BeTrue())
	})
})

type lostCreateResponseProxy struct {
	*httptest.Server
	upstream    *url.URL
	slug        string
	createCalls atomic.Int32
	dropped     atomic.Bool
}

func newLostCreateResponseProxy(upstream *url.URL, slug string) *lostCreateResponseProxy {
	proxy := &lostCreateResponseProxy{upstream: upstream, slug: slug}
	proxy.Server = httptest.NewTLSServer(http.HandlerFunc(proxy.serveHTTP))
	return proxy
}

func (p *lostCreateResponseProxy) serveHTTP(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(w, "request read failed", http.StatusBadGateway)
		return
	}
	target := *p.upstream
	target.Path = request.URL.Path
	target.RawQuery = request.URL.RawQuery
	forward, err := http.NewRequestWithContext(request.Context(), request.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, "request build failed", http.StatusBadGateway)
		return
	}
	forward.Header = request.Header.Clone()
	forward.Host = p.upstream.Host
	response, err := http.DefaultClient.Do(forward)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()

	if request.Method == http.MethodPost && strings.Contains(request.URL.Path, "/projects/") && bodyHasSlug(body, p.slug) {
		p.createCalls.Add(1)
		if response.StatusCode >= 200 && response.StatusCode < 300 && p.dropped.CompareAndSwap(false, true) {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "response hijacking unavailable", http.StatusInternalServerError)
				return
			}
			connection, _, hijackErr := hijacker.Hijack()
			if hijackErr == nil {
				_ = connection.Close()
			}
			return
		}
	}
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func bodyHasSlug(body []byte, slug string) bool {
	var payload struct {
		Slug string `json:"slug"`
	}
	return json.Unmarshal(body, &payload) == nil && payload.Slug == slug
}

func certificatePEM(certificate *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

func liveManagedProjectInput(baseURL, organization, team, slug, version string) projectruntime.Input {
	return projectruntime.Input{
		Project: &sentryv1alpha1.SentryProject{
			ObjectMeta: metav1.ObjectMeta{
				Name:       slug,
				Namespace:  "default",
				UID:        types.UID("live-recovery-" + strings.ReplaceAll(version, ".", "-")),
				Generation: 1,
			},
			Spec: sentryv1alpha1.SentryProjectSpec{
				Project: sentryv1alpha1.ProjectSelector{Create: &sentryv1alpha1.ProjectCreate{
					Team: team, Slug: slug, DisplayName: "Recovery",
				}},
				Lifecycle: sentryv1alpha1.LifecycleSpec{OnDelete: sentryv1alpha1.OnDeleteDeleteProject},
			},
		},
		ConnectionName: "live-sentry",
		Connection: sentryv1alpha1.SentryConnectionSpec{
			URL:          baseURL,
			Organization: organization,
			Access: []sentryv1alpha1.AccessRule{{
				Owner: "live", Namespaces: sentryv1alpha1.NamespaceAccess{MatchNames: []string{"default"}},
				Teams: []string{team}, AllowCreate: true,
			}},
		},
	}
}

func deleteLiveProject(upstream *url.URL, token, organization, slug string) {
	target := *upstream
	target.Path = "/api/0/projects/" + url.PathEscape(organization) + "/" + url.PathEscape(slug) + "/"
	request, err := http.NewRequest(http.MethodDelete, target.String(), nil)
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		_ = response.Body.Close()
	}
}

type liveAuditSink struct {
	mu     sync.Mutex
	events []audit.Event
}

func (s *liveAuditSink) Record(_ context.Context, event audit.Event) error {
	if err := audit.Validate(event); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
	return nil
}

func (s *liveAuditSink) has(outcome audit.Outcome, reason audit.Reason) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range s.events {
		if event.Outcome == outcome && event.Reason == reason {
			return true
		}
	}
	return false
}
