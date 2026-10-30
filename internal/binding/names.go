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

package binding

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	pathpkg "path"
	"strings"
)

// CanonicalInstallation normalizes cosmetic URL aliases without making a network call.
// A discovered canonical installation URL can be passed through the same function.
func CanonicalInstallation(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("installation URL must be absolute HTTP or HTTPS without userinfo, query, or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	port := parsed.Port()
	defaultPort := "443"
	if parsed.Scheme == "http" {
		defaultPort = "80"
	}
	if port != "" && port != defaultPort {
		host += ":" + port
	}
	cleanPath := pathpkg.Clean("/" + strings.TrimPrefix(parsed.EscapedPath(), "/"))
	if cleanPath == "/" {
		cleanPath = ""
	}
	return parsed.Scheme + "://" + host + cleanPath, nil
}

func BindingName(projectUID string) string {
	return hashedName("sentry-binding-", projectUID, 40)
}

func ClaimName(installation, organization, slug string) string {
	return hashedName("sentry-manager-", installation+"\x00"+organization+"\x00"+slug, 40)
}

func OperationName(key string) string {
	return hashedName("sentry-operation-", key, 38)
}

func hashedName(prefix, value string, digits int) string {
	digest := sha256.Sum256([]byte(value))
	return prefix + hex.EncodeToString(digest[:])[:digits]
}
