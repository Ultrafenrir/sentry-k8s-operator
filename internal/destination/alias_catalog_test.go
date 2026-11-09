package destination

import (
	"context"
	"testing"

	sentryv1alpha1 "github.com/Ultrafenrir/sentry-kubernetes-operator/api/v1alpha1"
)

type aliasTestCatalog struct{ integrations []Integration }

func (c aliasTestCatalog) ListIntegrations(context.Context, string, string) ([]Integration, error) {
	return c.integrations, nil
}
func (aliasTestCatalog) ListSlackChannels(context.Context, string, string) ([]Resource, error) {
	return nil, nil
}
func (aliasTestCatalog) ListPagerDutyServices(context.Context, string, string) ([]Resource, error) {
	return nil, nil
}

func TestAliasCatalogExposesOnlyDeclaredIntegrationNames(t *testing.T) {
	catalog := &AliasCatalog{
		Catalog: aliasTestCatalog{integrations: []Integration{{Identity: "private-a", Name: "Company Slack"}, {Identity: "private-b", Name: "Other Slack"}}},
		Integrations: &sentryv1alpha1.ConnectionIntegrations{Slack: &sentryv1alpha1.SlackIntegration{
			Workspaces: []sentryv1alpha1.IntegrationCatalogEntry{{Name: "company", SentryName: "Company Slack"}},
		}},
	}
	got, err := catalog.ListIntegrations(context.Background(), "organization", "slack")
	if err != nil || len(got) != 1 || got[0].Name != "company" || got[0].Identity != "private-a" {
		t.Fatalf("integrations=%#v err=%v", got, err)
	}
}
