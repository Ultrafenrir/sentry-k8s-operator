/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package destination

import (
	"context"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

// AliasCatalog exposes only platform-declared local integration names while preserving the
// protected identity returned by Sentry. It never creates or modifies an integration.
type AliasCatalog struct {
	Catalog      Catalog
	Integrations *sentryv1alpha1.ConnectionIntegrations
}

func (c *AliasCatalog) ListIntegrations(ctx context.Context, organization, provider string) ([]Integration, error) {
	observed, err := c.Catalog.ListIntegrations(ctx, organization, provider)
	if err != nil {
		return nil, err
	}
	entries := c.entries(provider)
	if len(entries) == 0 {
		return nil, nil
	}
	result := make([]Integration, 0, len(entries))
	for _, entry := range entries {
		for _, integration := range observed {
			if integration.Name == entry.SentryName {
				integration.Name = entry.Name
				result = append(result, integration)
			}
		}
	}
	return result, nil
}

func (c *AliasCatalog) ListSlackChannels(ctx context.Context, organization, integration string) ([]Resource, error) {
	return c.Catalog.ListSlackChannels(ctx, organization, integration)
}

func (c *AliasCatalog) ListPagerDutyServices(ctx context.Context, organization, integration string) ([]Resource, error) {
	return c.Catalog.ListPagerDutyServices(ctx, organization, integration)
}

func (c *AliasCatalog) entries(provider string) []sentryv1alpha1.IntegrationCatalogEntry {
	if c.Integrations == nil {
		return nil
	}
	if provider == "slack" && c.Integrations.Slack != nil {
		return c.Integrations.Slack.Workspaces
	}
	if provider == "pagerduty" && c.Integrations.PagerDuty != nil {
		return c.Integrations.PagerDuty.Accounts
	}
	return nil
}
