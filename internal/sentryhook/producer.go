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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
)

var ErrProducer = errors.New("signed Sentry event could not be durably enqueued")

type Enqueuer interface {
	Enqueue(context.Context, delivery.Message) (delivery.Record, bool, error)
}

// DeliveryResolver is the protected source-to-route boundary. Implementations
// must resolve only the locally configured SourceBindingKey and must never use
// a destination, route key, or target from the Sentry event payload.
type DeliveryResolver interface {
	ResolveTelegramDeliveries(context.Context, string) ([]TelegramDelivery, error)
}

type TelegramDelivery struct {
	RouteKey string      `json:"routeKey"`
	Render   RenderInput `json:"render"`
}

type Producer struct {
	Resolver DeliveryResolver
	Queue    Enqueuer
}

// Accept validates and renders the complete resolved batch before the first
// queue write. Enqueues are deterministic and idempotent, so a retry after a
// partial failure safely revisits records that were already committed.
func (p *Producer) Accept(ctx context.Context, envelope Envelope) error {
	if p == nil || p.Resolver == nil || p.Queue == nil || !canonicalHash(envelope.DedupeKey) || envelope.SourceBindingKey == "" || len(envelope.SourceBindingKey) > 256 {
		return ErrProducer
	}
	resolved, err := p.Resolver.ResolveTelegramDeliveries(ctx, envelope.SourceBindingKey)
	if err != nil {
		return ErrProducer
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i].RouteKey < resolved[j].RouteKey })
	messages := make([]delivery.Message, 0, len(resolved))
	previousRoute := ""
	for _, candidate := range resolved {
		if !canonicalHash(candidate.RouteKey) || candidate.RouteKey == previousRoute {
			return ErrProducer
		}
		text, err := Render(candidate.Render)
		if err != nil {
			return ErrProducer
		}
		messages = append(messages, delivery.Message{
			DedupeKey: targetDedupeKey(envelope.DedupeKey, candidate.RouteKey),
			RouteKey:  candidate.RouteKey,
			Text:      text,
		})
		previousRoute = candidate.RouteKey
	}
	for _, message := range messages {
		if _, _, err := p.Queue.Enqueue(ctx, message); err != nil {
			return ErrProducer
		}
	}
	return nil
}

func targetDedupeKey(eventKey, routeKey string) string {
	sum := sha256.Sum256([]byte(eventKey + "\x00" + routeKey))
	return hex.EncodeToString(sum[:])
}

func canonicalHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == hex.EncodeToString(decoded)
}

var _ Sink = (*Producer)(nil)
