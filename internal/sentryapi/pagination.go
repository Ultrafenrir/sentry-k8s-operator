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

package sentryapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// CollectPages follows Sentry's Link rel="next" headers and returns all decoded items.
func CollectPages[T any](ctx context.Context, client *Client, path, operation string) ([]T, error) {
	var all []T
	for page := 0; path != ""; page++ {
		if page >= 1000 {
			return nil, &APIError{Kind: ErrorInvalid, Operation: safeOperation(operation)}
		}
		var items []T
		headers, err := client.DoJSON(ctx, Request{
			Method:    http.MethodGet,
			Path:      path,
			RetrySafe: true,
			Operation: operation,
		}, &items)
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		path, err = nextPage(headers.Values("Link"))
		if err != nil {
			return nil, &APIError{Kind: ErrorDecode, Operation: safeOperation(operation)}
		}
	}
	return all, nil
}

func nextPage(headers []string) (string, error) {
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			part = strings.TrimSpace(part)
			if !strings.Contains(part, `rel="next"`) {
				continue
			}
			start := strings.IndexByte(part, '<')
			end := strings.IndexByte(part, '>')
			if start < 0 || end <= start+1 {
				return "", fmt.Errorf("malformed next link")
			}
			if strings.Contains(part[end+1:], `results="true"`) {
				return part[start+1 : end], nil
			}
		}
	}
	return "", nil
}
