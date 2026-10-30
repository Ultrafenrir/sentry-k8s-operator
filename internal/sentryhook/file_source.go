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
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	PublicEventPrefix = "/events/"
	sourceConfigLimit = 8 << 10
)

// FileSourceHandler is the tokenless public boundary. A projected Secret volume
// supplies one <canonical source hash>.json file per source. The file is read on
// every request so projected-secret rotation does not require Kubernetes API access.
type FileSourceHandler struct {
	Directory    string
	Sink         Sink
	MaxBodyBytes int64
	MaxClockSkew time.Duration
	Now          func() time.Time
}

func (h *FileSourceHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	if h == nil || h.Sink == nil {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	sourceBindingKey := strings.TrimPrefix(request.URL.Path, PublicEventPrefix)
	if request.URL.Path == sourceBindingKey || !canonicalHash(sourceBindingKey) {
		response.WriteHeader(http.StatusNotFound)
		return
	}
	configuration, secrets, err := loadFileSource(h.Directory, sourceBindingKey)
	if err != nil {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	delegate := &Handler{
		Secrets: func(context.Context) ([][]byte, error) {
			result := make([][]byte, len(secrets))
			for index := range secrets {
				result[index] = append([]byte(nil), secrets[index]...)
			}
			return result, nil
		},
		Sink: h.Sink, Path: request.URL.Path, SourceBindingKey: sourceBindingKey,
		ExpectedProjectSlug: configuration.ExpectedProjectSlug, MaxBodyBytes: h.MaxBodyBytes,
		MaxClockSkew: h.MaxClockSkew, Now: h.Now,
	}
	delegate.ServeHTTP(response, request)
}

func loadFileSource(directory, sourceBindingKey string) (VerifierConfig, [][]byte, error) {
	if directory == "" || !canonicalHash(sourceBindingKey) {
		return VerifierConfig{}, nil, ErrSink
	}
	file, err := os.Open(filepath.Join(directory, sourceBindingKey+".json"))
	if err != nil {
		return VerifierConfig{}, nil, ErrSink
	}
	defer file.Close()
	payload, err := io.ReadAll(io.LimitReader(file, sourceConfigLimit+1))
	if err != nil || len(payload) == 0 || len(payload) > sourceConfigLimit {
		return VerifierConfig{}, nil, ErrSink
	}
	configuration, secrets, err := decodeVerifierConfig(payload)
	if err != nil {
		return VerifierConfig{}, nil, ErrSink
	}
	return configuration, secrets, nil
}

// Ready validates every visible projected entry without returning source names or secret
// material. Kubernetes' internal ..data symlinks are ignored. An empty directory is ready:
// sources may be added later without restarting the tokenless receiver.
func (h *FileSourceHandler) Ready() error {
	if h == nil || h.Directory == "" || h.Sink == nil {
		return ErrSink
	}
	entries, err := os.ReadDir(h.Directory)
	if err != nil {
		return ErrSink
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "..") {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return ErrSink
		}
		sourceBindingKey := strings.TrimSuffix(entry.Name(), ".json")
		if !canonicalHash(sourceBindingKey) {
			return ErrSink
		}
		if _, _, err := loadFileSource(h.Directory, sourceBindingKey); err != nil {
			return errors.Join(ErrSink, err)
		}
	}
	return nil
}
