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
	"strings"
	"testing"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/delivery"
)

type staticResolver struct {
	source     string
	deliveries []TelegramDelivery
	err        error
}

func (r *staticResolver) ResolveTelegramDeliveries(_ context.Context, source string) ([]TelegramDelivery, error) {
	r.source = source
	return append([]TelegramDelivery(nil), r.deliveries...), r.err
}

type recordingQueue struct {
	messages  []delivery.Message
	failRoute string
}

func (q *recordingQueue) Enqueue(_ context.Context, message delivery.Message) (delivery.Record, bool, error) {
	q.messages = append(q.messages, message)
	if message.RouteKey == q.failRoute {
		return delivery.Record{}, false, delivery.ErrBackpressure
	}
	return delivery.Record{DedupeKey: message.DedupeKey}, true, nil
}

func TestProducerResolvesProtectedRoutesAndEnqueuesDeterministicMinimalMessages(t *testing.T) {
	t.Parallel()
	routeA := strings.Repeat("a", 64)
	routeB := strings.Repeat("b", 64)
	resolver := &staticResolver{deliveries: []TelegramDelivery{
		{RouteKey: routeB, Render: RenderInput{ProjectName: "checkout", AlertName: "regression", Severity: SeverityCritical}},
		{RouteKey: routeA, Render: RenderInput{ProjectName: "checkout", AlertName: "new-issue", Environment: "production", Severity: SeverityWarning}},
	}}
	queue := &recordingQueue{}
	producer := &Producer{Resolver: resolver, Queue: queue}
	envelope := Envelope{DedupeKey: strings.Repeat("c", 64), SourceBindingKey: "local-source-binding"}
	if err := producer.Accept(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if resolver.source != envelope.SourceBindingKey || len(queue.messages) != 2 {
		t.Fatalf("source = %q, messages = %+v", resolver.source, queue.messages)
	}
	if queue.messages[0].RouteKey != routeA || queue.messages[1].RouteKey != routeB {
		t.Fatalf("messages are not deterministic: %+v", queue.messages)
	}
	for _, message := range queue.messages {
		if !canonicalHash(message.DedupeKey) || strings.Contains(message.Text, "external-id-SHOULD-NOT-LEAK") {
			t.Fatalf("unsafe message = %+v", message)
		}
	}
}

func TestProducerRetryAfterPartialFailureReusesExactDedupeKeys(t *testing.T) {
	t.Parallel()
	routeA := strings.Repeat("a", 64)
	routeB := strings.Repeat("b", 64)
	resolver := &staticResolver{deliveries: []TelegramDelivery{
		{RouteKey: routeA, Render: RenderInput{ProjectName: "checkout", AlertName: "rule-a", Severity: SeverityInfo}},
		{RouteKey: routeB, Render: RenderInput{ProjectName: "checkout", AlertName: "rule-b", Severity: SeverityInfo}},
	}}
	queue := &recordingQueue{failRoute: routeB}
	producer := &Producer{Resolver: resolver, Queue: queue}
	envelope := Envelope{DedupeKey: strings.Repeat("d", 64), SourceBindingKey: "local-source-binding"}
	if err := producer.Accept(context.Background(), envelope); !errors.Is(err, ErrProducer) {
		t.Fatalf("first error = %v", err)
	}
	queue.failRoute = ""
	if err := producer.Accept(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if len(queue.messages) != 4 || queue.messages[0].DedupeKey != queue.messages[2].DedupeKey || queue.messages[1].DedupeKey != queue.messages[3].DedupeKey {
		t.Fatalf("retry changed messages: %+v", queue.messages)
	}
}

func TestProducerFailsBeforeWriteForInvalidOrDuplicateResolvedRoute(t *testing.T) {
	t.Parallel()
	route := strings.Repeat("a", 64)
	for _, deliveries := range [][]TelegramDelivery{
		{{RouteKey: "not-a-hash", Render: RenderInput{ProjectName: "checkout", AlertName: "rule", Severity: SeverityInfo}}},
		{
			{RouteKey: route, Render: RenderInput{ProjectName: "checkout", AlertName: "rule-a", Severity: SeverityInfo}},
			{RouteKey: route, Render: RenderInput{ProjectName: "checkout", AlertName: "rule-b", Severity: SeverityInfo}},
		},
		{{RouteKey: route, Render: RenderInput{ProjectName: "checkout\nunsafe", AlertName: "rule", Severity: SeverityInfo}}},
	} {
		queue := &recordingQueue{}
		producer := &Producer{Resolver: &staticResolver{deliveries: deliveries}, Queue: queue}
		err := producer.Accept(context.Background(), Envelope{DedupeKey: strings.Repeat("e", 64), SourceBindingKey: "source"})
		if !errors.Is(err, ErrProducer) || len(queue.messages) != 0 {
			t.Fatalf("error = %v, messages = %+v", err, queue.messages)
		}
	}
}

func TestProducerAcceptsResolvedEmptySetWithoutQueueMutation(t *testing.T) {
	t.Parallel()
	queue := &recordingQueue{}
	producer := &Producer{Resolver: &staticResolver{}, Queue: queue}
	if err := producer.Accept(context.Background(), Envelope{DedupeKey: strings.Repeat("f", 64), SourceBindingKey: "source"}); err != nil {
		t.Fatal(err)
	}
	if len(queue.messages) != 0 {
		t.Fatalf("messages = %+v", queue.messages)
	}
}
