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

// Package dsn selects client keys and publishes public DSNs without logging them.
package dsn

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"unicode"
	"unicode/utf8"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/resolver"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/sentryapi"
)

var (
	ErrKeyNotFound    = errors.New("DSN client key not found")
	ErrKeyAmbiguous   = errors.New("DSN client key is ambiguous")
	ErrInvalidKeyName = errors.New("Sentry returned an invalid client key name")
	ErrInvalidDSN     = errors.New("Sentry returned an invalid public DSN")
	ErrPinnedKeyLost  = errors.New("pinned DSN client key is missing or inactive")
)

type ClientKey struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Label    string `json:"label"`
	IsActive bool   `json:"isActive"`
	DSN      struct {
		Public string `json:"public"`
	} `json:"dsn"`
}

type KeyReader struct {
	Client *sentryapi.Client
}

func (r *KeyReader) List(ctx context.Context, organization, projectSlug string) ([]ClientKey, error) {
	return sentryapi.CollectPages[ClientKey](ctx, r.Client,
		"/api/0/projects/"+url.PathEscape(organization)+"/"+url.PathEscape(projectSlug)+"/keys/",
		"list-client-keys")
}

type Selection struct {
	Key             ClientKey
	Binding         binding.DSNBinding
	ReadableName    string
	ExternalRenamed bool
}

// Select chooses only from a complete, already paginated list and preserves a pinned key.
func Select(keys []ClientKey, selector string, current *binding.DSNBinding) (Selection, error) {
	return SelectWithTransport(keys, selector, current, false)
}

// SelectWithTransport applies the same identity-safe selection while allowing a
// plain HTTP public DSN only when the owning SentryConnection explicitly opted
// into insecure HTTP transport.
func SelectWithTransport(keys []ClientKey, selector string, current *binding.DSNBinding, allowInsecureHTTP bool) (Selection, error) {
	selectorHash := hashSelector(selector)
	if current != nil && current.KeyIdentity != "" && current.SelectorHash == selectorHash {
		for _, key := range keys {
			if key.ID == current.KeyIdentity {
				if !key.IsActive || !validPublicDSN(key.DSN.Public, allowInsecureHTTP) {
					return Selection{}, ErrPinnedKeyLost
				}
				name := readableName(key)
				if !validReadableName(name) {
					return Selection{}, ErrInvalidKeyName
				}
				return Selection{Key: key, Binding: *current, ReadableName: name, ExternalRenamed: name != current.KeyName}, nil
			}
		}
		return Selection{}, ErrPinnedKeyLost
	}

	active := make([]ClientKey, 0, len(keys))
	for _, key := range keys {
		if key.IsActive {
			active = append(active, key)
		}
	}
	var selected ClientKey
	var err error
	if selector == "" {
		if len(active) == 0 {
			return Selection{}, ErrKeyNotFound
		}
		if len(active) > 1 {
			return Selection{}, ErrKeyAmbiguous
		}
		selected = active[0]
	} else {
		selected, err = resolver.Exact(active, selector, "client-key", readableName)
		if resolver.IsKind(err, resolver.ErrorNotFound) {
			return Selection{}, ErrKeyNotFound
		}
		if resolver.IsKind(err, resolver.ErrorAmbiguous) {
			return Selection{}, ErrKeyAmbiguous
		}
		if err != nil {
			return Selection{}, err
		}
	}
	name := readableName(selected)
	if !validReadableName(name) {
		return Selection{}, ErrInvalidKeyName
	}
	if selected.ID == "" || !validPublicDSN(selected.DSN.Public, allowInsecureHTTP) {
		return Selection{}, ErrInvalidDSN
	}
	return Selection{
		Key:          selected,
		ReadableName: name,
		Binding: binding.DSNBinding{
			KeyIdentity:  selected.ID,
			KeyName:      name,
			SelectorHash: selectorHash,
		},
	}, nil
}

func readableName(key ClientKey) string {
	if key.Label != "" {
		return key.Label
	}
	return key.Name
}

func validReadableName(value string) bool {
	if value == "" || len(value) > 253 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validPublicDSN(raw string, allowInsecureHTTP bool) bool {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && !(allowInsecureHTTP && parsed.Scheme == "http")) || parsed.Host == "" || parsed.User == nil {
		return false
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return false
	}
	return parsed.User.Username() != ""
}

func hashSelector(selector string) string {
	digest := sha256.Sum256([]byte(selector))
	return hex.EncodeToString(digest[:])
}
