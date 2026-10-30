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

package connection

import (
	"crypto/sha256"
	"fmt"
	"sync"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

// InputVersions are safe hashes used only to detect rotations.
type InputVersions struct {
	Token       [32]byte
	CABundle    [32]byte
	BotToken    [32]byte
	WebhookAuth [32]byte
}

type registryEntry struct {
	baseURL string
	client  *sentryapi.Client
	inputs  SecretInputs
	version InputVersions
}

// Registry owns one live client and secret snapshot per SentryConnection.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*registryEntry
}

// NewRegistry creates an empty connection registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]*registryEntry)}
}

// Apply creates or rotates a live client. Existing clients are retained when the URL is stable.
func (r *Registry) Apply(name, baseURL string, inputs SecretInputs) (*sentryapi.Client, bool, error) {
	if name == "" {
		return nil, false, fmt.Errorf("connection name is required")
	}
	version := hashInputs(inputs)
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, found := r.entries[name]; found && current.baseURL == baseURL {
		if current.version == version {
			return current.client, false, nil
		}
		if err := current.client.UpdateCABundle(inputs.CABundle); err != nil {
			return nil, false, err
		}
		current.client.UpdateToken(string(inputs.Token))
		current.inputs = cloneInputs(inputs)
		current.version = version
		return current.client, true, nil
	}
	next, err := sentryapi.NewClient(sentryapi.Config{
		BaseURL:       baseURL,
		Token:         string(inputs.Token),
		CABundle:      inputs.CABundle,
		MaxConcurrent: 4,
		MaxRetries:    3,
		UserAgent:     "sentry-kubernetes-operator",
	})
	if err != nil {
		return nil, false, err
	}
	r.entries[name] = &registryEntry{
		baseURL: baseURL,
		client:  next,
		inputs:  cloneInputs(inputs),
		version: version,
	}
	return next, true, nil
}

// Get returns the current live client.
func (r *Registry) Get(name string) (*sentryapi.Client, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, found := r.entries[name]
	if !found {
		return nil, false
	}
	return entry.client, true
}

// Inputs returns a defensive copy for trusted in-process integration adapters.
func (r *Registry) Inputs(name string) (SecretInputs, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, found := r.entries[name]
	if !found {
		return SecretInputs{}, false
	}
	return cloneInputs(entry.inputs), true
}

// Versions returns non-secret fingerprints suitable for rotation tests and metrics.
func (r *Registry) Versions(name string) (InputVersions, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, found := r.entries[name]
	if !found {
		return InputVersions{}, false
	}
	return entry.version, true
}

// Delete forgets in-memory credentials when a connection is removed.
func (r *Registry) Delete(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, name)
}

func hashInputs(inputs SecretInputs) InputVersions {
	return InputVersions{
		Token:       sha256.Sum256(inputs.Token),
		CABundle:    sha256.Sum256(inputs.CABundle),
		BotToken:    sha256.Sum256(inputs.BotToken),
		WebhookAuth: sha256.Sum256(inputs.WebhookAuth),
	}
}

func cloneInputs(inputs SecretInputs) SecretInputs {
	return SecretInputs{
		Token:       append([]byte(nil), inputs.Token...),
		CABundle:    append([]byte(nil), inputs.CABundle...),
		BotToken:    append([]byte(nil), inputs.BotToken...),
		WebhookAuth: append([]byte(nil), inputs.WebhookAuth...),
	}
}
