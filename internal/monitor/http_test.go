/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Ultrafenrir/sentry-kubernetes-operator/internal/sentryapi"
)

func TestHTTPAdapterUsesExactCronAndUptimeContracts(t *testing.T) {
	t.Parallel()
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.RequestURI()
		seen[key]++
		w.Header().Set("Content-Type", "application/json")
		switch key {
		case "GET /api/0/organizations/company/teams/":
			_, _ = w.Write([]byte(`[{"id":"2","slug":"platform"}]`))
		case "GET /api/0/organizations/company/monitors/?project=42":
			_, _ = w.Write([]byte(`[{"id":"10","name":"cron","slug":"cron","status":"active","project":{"slug":"checkout"},"config":{"schedule_type":"crontab","schedule":"*/5 * * * *","checkin_margin":5,"max_runtime":30,"timezone":"UTC","failure_issue_threshold":2,"recovery_threshold":1}}]`))
		case "POST /api/0/organizations/company/monitors/", "PUT /api/0/projects/company/checkout/monitors/10/":
			var body CronRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Project != "checkout" || body.Type != "cron_job" || body.Config.ScheduleType != "crontab" {
				t.Errorf("cron body = %#v, %v", body, err)
			}
			_, _ = w.Write([]byte(`{"id":"10","name":"cron","slug":"cron","status":"active","project":{"slug":"checkout"},"config":{"schedule_type":"crontab","schedule":"*/5 * * * *","checkin_margin":5,"max_runtime":30,"timezone":"UTC","failure_issue_threshold":2,"recovery_threshold":1}}`))
		case "GET /api/0/projects/company/checkout/monitors/10/":
			_, _ = w.Write([]byte(`{"id":"10","name":"cron","slug":"cron","status":"active","project":{"slug":"checkout"},"config":{"schedule_type":"crontab","schedule":"*/5 * * * *","checkin_margin":5,"max_runtime":30,"timezone":"UTC","failure_issue_threshold":2,"recovery_threshold":1}}`))
		case "DELETE /api/0/projects/company/checkout/monitors/10/", "DELETE /api/0/projects/company/checkout/uptime/20/":
			w.WriteHeader(http.StatusNoContent)
		case "POST /api/0/organizations/company/uptime-preview-check/":
			_, _ = w.Write([]byte(`{"status":"success"}`))
		case "GET /api/0/organizations/company/uptime/?project=checkout":
			_, _ = w.Write([]byte(`[{"id":"20","projectSlug":"checkout","name":"uptime","status":"active","url":"https://example.com/","intervalSeconds":300,"timeoutMs":5000,"method":"GET","headers":[],"body":null,"traceSampling":false,"responseCaptureEnabled":true,"recoveryThreshold":1,"downtimeThreshold":3,"assertion":null}]`))
		case "POST /api/0/projects/company/checkout/uptime/", "PUT /api/0/projects/company/checkout/uptime/20/":
			var body UptimeRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL != "https://example.com/" || body.Headers == nil {
				t.Errorf("uptime body = %#v, %v", body, err)
			}
			_, _ = w.Write([]byte(`{"id":"20","projectSlug":"checkout","name":"uptime","status":"active","url":"https://example.com/","intervalSeconds":300,"timeoutMs":5000,"method":"GET","headers":[],"body":null,"traceSampling":false,"responseCaptureEnabled":true,"recoveryThreshold":1,"downtimeThreshold":3,"assertion":null}`))
		case "GET /api/0/projects/company/checkout/uptime/20/":
			_, _ = w.Write([]byte(`{"id":"20","projectSlug":"checkout","name":"uptime","status":"active","url":"https://example.com/","intervalSeconds":300,"timeoutMs":5000,"method":"GET","headers":[],"body":null,"traceSampling":false,"responseCaptureEnabled":true,"recoveryThreshold":1,"downtimeThreshold":3,"assertion":null}`))
		default:
			http.Error(w, key, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := sentryapi.NewClient(sentryapi.Config{BaseURL: server.URL, Token: "test-token", AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	adapter := &HTTPAdapter{Client: client}
	ctx := context.Background()
	team, found, err := adapter.ResolveTeam(ctx, "company", "platform")
	if err != nil || !found || team != "2" {
		t.Fatalf("ResolveTeam = %q, %v, %v", team, found, err)
	}
	margin, runtime, failure, recovery := int32(5), int32(30), int32(2), int32(1)
	cron := CronRequest{Project: "checkout", Type: "cron_job", Name: "cron", Slug: "cron", Status: "active", Config: CronConfig{ScheduleType: "crontab", Schedule: "*/5 * * * *", CheckInMargin: &margin, MaxRuntime: &runtime, Timezone: "UTC", FailureThreshold: &failure, RecoveryThreshold: &recovery}}
	if _, err := adapter.ListCron(ctx, "company", "42"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CreateCron(ctx, "company", cron); err != nil {
		t.Fatal(err)
	}
	if _, found, err := adapter.GetCron(ctx, "company", "checkout", "10"); err != nil || !found {
		t.Fatalf("GetCron found=%v err=%v", found, err)
	}
	if _, err := adapter.UpdateCron(ctx, "company", "checkout", "10", cron); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DeleteCron(ctx, "company", "checkout", "10"); err != nil {
		t.Fatal(err)
	}
	uptime := UptimeRequest{Name: "uptime", Status: "active", URL: "https://example.com/", IntervalSeconds: 300, TimeoutMilliseconds: 5000, Method: "GET", Headers: [][2]string{}, ResponseCaptureEnabled: true, RecoveryThreshold: 1, DowntimeThreshold: 3}
	if err := adapter.PreflightUptime(ctx, "company", uptime); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.ListUptime(ctx, "company", "checkout"); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.CreateUptime(ctx, "company", "checkout", uptime); err != nil {
		t.Fatal(err)
	}
	if _, found, err := adapter.GetUptime(ctx, "company", "checkout", "20"); err != nil || !found {
		t.Fatalf("GetUptime found=%v err=%v", found, err)
	}
	if _, err := adapter.UpdateUptime(ctx, "company", "checkout", "20", uptime); err != nil {
		t.Fatal(err)
	}
	if err := adapter.DeleteUptime(ctx, "company", "checkout", "20"); err != nil {
		t.Fatal(err)
	}
	for operation, count := range seen {
		if count != 1 {
			t.Errorf("%s called %d times", operation, count)
		}
	}
}
