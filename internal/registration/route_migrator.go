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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/manager"
)

type LegacyRouteStore interface {
	MigrateLegacy(context.Context, string, int) (int, error)
}

type RouteMigrator struct {
	Routes          LegacyRouteStore
	SourceNamespace string
	Interval        time.Duration
	Batch           int
	Jitter          func(time.Duration) time.Duration
	OnError         func(error)
}

func (m *RouteMigrator) Start(ctx context.Context) error {
	if m == nil || m.Routes == nil || m.SourceNamespace == "" || m.Interval <= 0 || m.Batch <= 0 {
		return ErrRouteInvalid
	}
	for {
		if _, err := m.Routes.MigrateLegacy(ctx, m.SourceNamespace, m.Batch); err != nil && !errors.Is(err, context.Canceled) && m.OnError != nil {
			m.OnError(err)
		}
		delay := challengeJitter(m.Interval)
		if m.Jitter != nil {
			delay = m.Jitter(m.Interval)
		}
		if delay < m.Interval*9/10 {
			delay = m.Interval * 9 / 10
		}
		if delay > m.Interval*11/10 {
			delay = m.Interval * 11 / 10
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

func (*RouteMigrator) NeedLeaderElection() bool { return true }

var _ manager.LeaderElectionRunnable = (*RouteMigrator)(nil)
