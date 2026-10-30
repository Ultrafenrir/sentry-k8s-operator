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

package registration

import (
	"context"
	"testing"
	"time"
)

func TestRouteMigratorRunsImmediatelyAndRequiresLeader(t *testing.T) {
	t.Parallel()
	called := make(chan struct{}, 1)
	store := &fakeLegacyRouteStore{call: func(_ context.Context, source string, limit int) (int, error) {
		if source != "state" || limit != 7 {
			t.Errorf("source=%q limit=%d", source, limit)
		}
		called <- struct{}{}
		return 1, nil
	}}
	migrator := &RouteMigrator{
		Routes: store, SourceNamespace: "state", Interval: time.Hour, Batch: 7,
		Jitter: func(value time.Duration) time.Duration { return value },
	}
	if !migrator.NeedLeaderElection() {
		t.Fatal("route migrator must require leader election")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- migrator.Start(ctx) }()
	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("route migrator did not run immediately")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type fakeLegacyRouteStore struct {
	call func(context.Context, string, int) (int, error)
}

func (s *fakeLegacyRouteStore) MigrateLegacy(ctx context.Context, source string, limit int) (int, error) {
	return s.call(ctx, source, limit)
}
