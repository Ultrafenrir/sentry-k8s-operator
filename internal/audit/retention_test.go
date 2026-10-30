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

package audit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetentionJanitorRunsImmediatelyAndRequiresLeader(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	called := make(chan struct{}, 1)
	sweeper := &fakeRetentionSweeper{call: func(_ context.Context, cutoff time.Time, limit int) (int, error) {
		if !cutoff.Equal(now.Add(-30*24*time.Hour)) || limit != 17 {
			t.Errorf("sweep cutoff=%s limit=%d", cutoff, limit)
		}
		called <- struct{}{}
		return 1, nil
	}}
	janitor := &RetentionJanitor{
		Store: sweeper, Interval: time.Hour, Retention: 30 * 24 * time.Hour, Batch: 17,
		Now: func() time.Time { return now }, Jitter: func(value time.Duration) time.Duration { return value },
	}
	if !janitor.NeedLeaderElection() {
		t.Fatal("retention janitor must require leader election")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- janitor.Start(ctx) }()
	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("retention janitor did not run immediately")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRetentionJanitorReportsErrorAndKeepsRunningUntilCancel(t *testing.T) {
	t.Parallel()
	reported := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	janitor := &RetentionJanitor{
		Store:    &fakeRetentionSweeper{call: func(context.Context, time.Time, int) (int, error) { return 0, ErrStore }},
		Interval: time.Hour, Retention: time.Hour, Batch: 1,
		Jitter: func(value time.Duration) time.Duration { return value },
		OnError: func(err error) {
			reported <- err
			cancel()
		},
	}
	if err := janitor.Start(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-reported:
		if !errors.Is(err, ErrStore) {
			t.Fatalf("reported error=%v", err)
		}
	default:
		t.Fatal("retention error was not reported")
	}
}

func TestRetentionJitterIsBounded(t *testing.T) {
	t.Parallel()
	for range 100 {
		value := retentionJitter(time.Minute)
		if value < 54*time.Second || value > 66*time.Second {
			t.Fatalf("jitter %s outside ±10%%", value)
		}
	}
}

type fakeRetentionSweeper struct {
	call func(context.Context, time.Time, int) (int, error)
}

func (s *fakeRetentionSweeper) SweepBefore(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	return s.call(ctx, cutoff, limit)
}
