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
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type RetentionSweeper interface {
	SweepBefore(context.Context, time.Time, int) (int, error)
}

// RetentionJanitor exact-deletes expired durable audit records. Only the elected manager
// runs it; mutation writers retain create-only access to the audit namespace.
type RetentionJanitor struct {
	Store     RetentionSweeper
	Interval  time.Duration
	Retention time.Duration
	Batch     int
	Now       func() time.Time
	Jitter    func(time.Duration) time.Duration
	OnError   func(error)
}

func (j *RetentionJanitor) Start(ctx context.Context) error {
	if j == nil || j.Store == nil || j.Interval <= 0 || j.Retention <= 0 || j.Batch <= 0 {
		return ErrStore
	}
	for {
		cutoff := j.now().Add(-j.Retention)
		if _, err := j.Store.SweepBefore(ctx, cutoff, j.Batch); err != nil && !errors.Is(err, context.Canceled) && j.OnError != nil {
			j.OnError(err)
		}
		delay := retentionJitter(j.Interval)
		if j.Jitter != nil {
			delay = j.Jitter(j.Interval)
		}
		if delay < j.Interval*9/10 {
			delay = j.Interval * 9 / 10
		}
		if delay > j.Interval*11/10 {
			delay = j.Interval * 11 / 10
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (*RetentionJanitor) NeedLeaderElection() bool { return true }

func (j *RetentionJanitor) now() time.Time {
	if j.Now != nil {
		return j.Now().UTC().Round(0)
	}
	return time.Now().UTC().Round(0)
}

func retentionJitter(base time.Duration) time.Duration {
	offset, err := rand.Int(rand.Reader, big.NewInt(2001))
	if err != nil {
		return base
	}
	return time.Duration(int64(base) * (9000 + offset.Int64()) / 10000)
}

var _ manager.LeaderElectionRunnable = (*RetentionJanitor)(nil)
