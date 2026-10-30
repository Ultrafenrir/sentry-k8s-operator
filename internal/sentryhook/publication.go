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

package sentryhook

import (
	"context"
	"errors"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

var ErrPublication = errors.New("Sentry hook source publication failed")

type SourcePlan struct {
	SourceBindingKey    string
	ExpectedProjectSlug string
	Deliveries          []TelegramDelivery
}

type SourcePublicationStore interface {
	Create(context.Context, SourceRecord) (SourceRecord, error)
	Get(context.Context, string) (SourceRecord, error)
	CompareAndSwap(context.Context, SourceRecord, SourceRecord) (SourceRecord, error)
	DeleteExact(context.Context, SourceRecord) error
}

type VerifierPublicationStore interface {
	Upsert(context.Context, string, []byte) error
	DeleteExact(context.Context, string, []byte) error
}

// Publisher establishes a fail-closed cross-namespace ordering. Protected routing is
// durable before public authentication is enabled. Removal disables public authentication
// before deleting routing. There is deliberately no false cross-namespace transaction;
// either partial state is safe and a normal reconcile can resume it.
type Publisher struct {
	Sources  SourcePublicationStore
	Verifier VerifierPublicationStore
}

func (p *Publisher) Publish(ctx context.Context, plan SourcePlan, verifierPayload []byte) (SourceRecord, error) {
	configuration, _, err := decodeVerifierConfig(verifierPayload)
	desired := SourceRecord{
		Version: sourceRecordVersion, Revision: 1, SourceBindingKey: plan.SourceBindingKey,
		ExpectedProjectSlug: plan.ExpectedProjectSlug, Deliveries: SortTelegramDeliveries(plan.Deliveries),
	}
	if p == nil || p.Sources == nil || p.Verifier == nil || err != nil ||
		configuration.ExpectedProjectSlug != plan.ExpectedProjectSlug || validateSourceRecord(desired) != nil {
		return SourceRecord{}, ErrPublication
	}
	current, err := p.Sources.Get(ctx, plan.SourceBindingKey)
	switch {
	case apierrors.IsNotFound(err):
		current, err = p.Sources.Create(ctx, desired)
	case err != nil:
		return SourceRecord{}, errors.Join(ErrPublication, err)
	case current.ExpectedProjectSlug != desired.ExpectedProjectSlug:
		return SourceRecord{}, ErrPublication
	case !slices.Equal(current.Deliveries, desired.Deliveries):
		next := current
		next.Revision++
		next.Deliveries = desired.Deliveries
		current, err = p.Sources.CompareAndSwap(ctx, current, next)
	}
	if err != nil {
		return SourceRecord{}, errors.Join(ErrPublication, err)
	}
	if err := p.Verifier.Upsert(ctx, plan.SourceBindingKey, verifierPayload); err != nil {
		return current, errors.Join(ErrPublication, err)
	}
	return current, nil
}

func (p *Publisher) Unpublish(ctx context.Context, record SourceRecord, verifierPayload []byte) error {
	configuration, _, verifierErr := decodeVerifierConfig(verifierPayload)
	if p == nil || p.Sources == nil || p.Verifier == nil || validateSourceRecord(record) != nil ||
		verifierErr != nil || configuration.ExpectedProjectSlug != record.ExpectedProjectSlug {
		return ErrPublication
	}
	if err := p.Verifier.DeleteExact(ctx, record.SourceBindingKey, verifierPayload); err != nil {
		return errors.Join(ErrPublication, err)
	}
	if err := p.Sources.DeleteExact(ctx, record); err != nil {
		return errors.Join(ErrPublication, err)
	}
	return nil
}

var _ SourcePublicationStore = (*SourceStore)(nil)
var _ VerifierPublicationStore = (*VerifierStore)(nil)
