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
	"errors"
	"testing"
	"time"
)

func TestChallengeJanitorRunsImmediatelyAndRequiresLeader(t *testing.T) {
	t.Parallel()
	called := make(chan struct{}, 1)
	sweeper := &fakeChallengeSweeper{call: func(_ context.Context, now time.Time, limit int) (int, error) {
		if now.IsZero() || limit != 7 {
			t.Errorf("sweep now=%s limit=%d", now, limit)
		}
		called <- struct{}{}
		return 1, nil
	}}
	janitor := &ChallengeJanitor{
		Store: sweeper, Interval: time.Hour, Batch: 7,
		Jitter: func(value time.Duration) time.Duration { return value },
	}
	if !janitor.NeedLeaderElection() {
		t.Fatal("janitor must require leader election")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- janitor.Start(ctx) }()
	select {
	case <-called:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("janitor did not run immediately")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestChallengeJanitorReportsErrorAndKeepsRunningUntilCancel(t *testing.T) {
	t.Parallel()
	reported := make(chan error, 1)
	sweeper := &fakeChallengeSweeper{call: func(context.Context, time.Time, int) (int, error) {
		return 0, ErrChallengeIndexInvalid
	}}
	ctx, cancel := context.WithCancel(context.Background())
	janitor := &ChallengeJanitor{
		Store: sweeper, Interval: time.Hour, Batch: 1,
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
		if !errors.Is(err, ErrChallengeIndexInvalid) {
			t.Fatalf("reported error = %v", err)
		}
	default:
		t.Fatal("sweep error was not reported")
	}
}

func TestChallengeJitterIsBounded(t *testing.T) {
	t.Parallel()
	for range 100 {
		value := challengeJitter(time.Minute)
		if value < 54*time.Second || value > 66*time.Second {
			t.Fatalf("jitter %s outside ±10%%", value)
		}
	}
}

type fakeChallengeSweeper struct {
	call func(context.Context, time.Time, int) (int, error)
}

func (s *fakeChallengeSweeper) SweepExpired(ctx context.Context, now time.Time, limit int) (int, error) {
	return s.call(ctx, now, limit)
}
