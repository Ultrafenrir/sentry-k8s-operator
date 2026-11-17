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

package destination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

var ErrCatalogUnsupported = errors.New("destination catalog route is not supported by the proven adapter")

// HTTPCatalog implements source-confirmed public integration catalog routes.
type HTTPCatalog struct {
	Client *sentryapi.Client

	mu                sync.Mutex
	pagerDutyServices map[string][]Resource
}

type integrationResponse struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider struct {
		Key string `json:"key"`
	} `json:"provider"`
	ConfigData struct {
		ServiceTable []struct {
			ID      catalogID `json:"id"`
			Service string    `json:"service"`
		} `json:"service_table"`
	} `json:"configData"`
}

type channelResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type channelsResponse struct {
	Results []channelResponse `json:"results"`
}

// PagerDuty service ids are strings in some supported responses and JSON
// numbers in others. Normalize both without exposing that version difference
// to the protected binding layer.
type catalogID string

func (id *catalogID) UnmarshalJSON(value []byte) error {
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		*id = catalogID(text)
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var number json.Number
	if err := decoder.Decode(&number); err != nil {
		return err
	}
	*id = catalogID(number.String())
	return nil
}

func (c *HTTPCatalog) ListIntegrations(ctx context.Context, organization, provider string) ([]Integration, error) {
	if c.Client == nil || (provider != "slack" && provider != "pagerduty") {
		return nil, ErrCatalogUnsupported
	}
	path := "/api/0/organizations/" + url.PathEscape(organization) + "/integrations/?providerKey=" + url.QueryEscape(provider) + "&includeConfig=1"
	items, err := sentryapi.CollectPages[integrationResponse](ctx, c.Client, path, "list-organization-integrations")
	if err != nil {
		return nil, err
	}
	result := make([]Integration, 0, len(items))
	pagerDuty := make(map[string][]Resource)
	for _, item := range items {
		if item.ID == "" || item.Name == "" || item.Provider.Key != provider {
			return nil, ErrCatalogUnsupported
		}
		result = append(result, Integration{Identity: item.ID, Name: item.Name})
		if provider == "pagerduty" {
			services := make([]Resource, 0, len(item.ConfigData.ServiceTable))
			for _, service := range item.ConfigData.ServiceTable {
				if service.ID == "" || service.Service == "" {
					return nil, ErrCatalogUnsupported
				}
				services = append(services, Resource{Identity: string(service.ID), Name: service.Service})
			}
			pagerDuty[item.ID] = services
		}
	}
	if provider == "pagerduty" {
		c.mu.Lock()
		c.pagerDutyServices = pagerDuty
		c.mu.Unlock()
	}
	return result, nil
}

func (c *HTTPCatalog) ListSlackChannels(ctx context.Context, organization, integration string) ([]Resource, error) {
	if c.Client == nil || organization == "" || integration == "" {
		return nil, ErrCatalogUnsupported
	}
	path := "/api/0/organizations/" + url.PathEscape(organization) + "/integrations/" + url.PathEscape(integration) + "/channels/"
	var response channelsResponse
	_, err := c.Client.DoJSON(ctx, sentryapi.Request{
		Method:    http.MethodGet,
		Path:      path,
		RetrySafe: true,
		Operation: "list-integration-channels",
	}, &response)
	if err != nil {
		return nil, err
	}
	result := make([]Resource, 0, len(response.Results))
	for _, item := range response.Results {
		if item.ID == "" || item.Name == "" {
			return nil, ErrCatalogUnsupported
		}
		result = append(result, Resource{Identity: item.ID, Name: item.Name})
	}
	return result, nil
}

func (c *HTTPCatalog) ListPagerDutyServices(_ context.Context, _ string, integration string) ([]Resource, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	services, found := c.pagerDutyServices[integration]
	if !found {
		return nil, ErrCatalogUnsupported
	}
	return append([]Resource(nil), services...), nil
}
