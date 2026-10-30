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
	"sort"
)

// CapabilityState distinguishes evidence from assumptions.
type CapabilityState string

const (
	CapabilityAvailable   CapabilityState = "Available"
	CapabilityUnavailable CapabilityState = "Unavailable"
	CapabilityUnknown     CapabilityState = "Unknown"
)

// CapabilityProbe is a non-mutating endpoint probe. Paths must address non-sensitive data.
type CapabilityProbe struct {
	Name                     string
	Path                     string
	Method                   string
	NotFoundMeansUnavailable bool
}

// CapabilityResult records evidence without exposing a response body.
type CapabilityResult struct {
	Name  string
	State CapabilityState
}

// DiscoverCapabilities executes read-only probes. Permission errors remain errors and are
// never reclassified as an unavailable feature.
func DiscoverCapabilities(ctx context.Context, client *Client, probes []CapabilityProbe) ([]CapabilityResult, error) {
	results := make([]CapabilityResult, 0, len(probes))
	for _, probe := range probes {
		method := probe.Method
		if method == "" {
			method = http.MethodGet
		}
		if method != http.MethodGet && method != http.MethodHead {
			return nil, fmt.Errorf("capability probes must be read-only")
		}
		_, err := client.DoJSON(ctx, Request{
			Method:    method,
			Path:      probe.Path,
			RetrySafe: true,
			Operation: "capability-probe",
		}, nil)
		result := CapabilityResult{Name: probe.Name, State: CapabilityAvailable}
		if err != nil {
			if IsKind(err, ErrorNotFound) {
				result.State = CapabilityUnknown
				if probe.NotFoundMeansUnavailable {
					result.State = CapabilityUnavailable
				}
			} else {
				return nil, err
			}
		}
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })
	return results, nil
}
