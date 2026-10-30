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

package sentryhook

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
)

// DeriveSourceBindingKey creates a stable local identity without using a Sentry external
// ID or readable project data. Re-creation with a new Kubernetes UID intentionally creates
// a new source and prevents accidental adoption of an old hook.
func DeriveSourceBindingKey(connectionUID, projectUID string) (string, error) {
	if !validLocalUID(connectionUID) || !validLocalUID(projectUID) {
		return "", ErrSourceRecord
	}
	digest := sha256.Sum256([]byte("sentry-hook-source-v1\x00" + connectionUID + "\x00" + projectUID))
	return hex.EncodeToString(digest[:]), nil
}

// SourceEndpointURL binds an administrator-owned receiver base to exactly one opaque
// source. The base path is deliberately fixed to the route implemented by FileSourceHandler.
func SourceEndpointURL(receiverBase, sourceBindingKey string) (string, error) {
	parsed, err := url.Parse(receiverBase)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "/events" ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || !canonicalHash(sourceBindingKey) {
		return "", errors.New("receiver base must be exact HTTPS /events URL")
	}
	parsed.Path += "/" + sourceBindingKey
	return parsed.String(), nil
}

func validLocalUID(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character <= 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}
