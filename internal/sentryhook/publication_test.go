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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestPublisherOrdersProtectedBeforePublicAndPublicBeforeProtectedDelete(t *testing.T) {
	t.Parallel()
	events := []string{}
	sources := &publicationSourceFake{events: &events}
	verifier := &publicationVerifierFake{events: &events}
	publisher := &Publisher{Sources: sources, Verifier: verifier}
	payload, err := EncodeVerifierConfig("checkout", [][]byte{[]byte("old-source-secret"), []byte("new-source-secret")})
	if err != nil {
		t.Fatal(err)
	}
	plan := SourcePlan{
		SourceBindingKey: strings.Repeat("a", 64), ExpectedProjectSlug: "checkout",
		Deliveries: []TelegramDelivery{{RouteKey: strings.Repeat("c", 64), Render: RenderInput{ProjectName: "checkout", AlertName: "regression", Severity: SeverityCritical}}, {RouteKey: strings.Repeat("b", 64), Render: RenderInput{ProjectName: "checkout", AlertName: "first-seen", Severity: SeverityWarning}}},
	}
	record, err := publisher.Publish(context.Background(), plan, payload)
	if err != nil || strings.Join(events, ",") != "source-create,verifier-upsert" || record.Revision != 1 || record.Deliveries[0].RouteKey != strings.Repeat("b", 64) {
		t.Fatalf("record=%#v events=%v error=%v", record, events, err)
	}
	events = nil
	plan.Deliveries = plan.Deliveries[:1]
	record, err = publisher.Publish(context.Background(), plan, payload)
	if err != nil || strings.Join(events, ",") != "source-cas,verifier-upsert" || record.Revision != 2 {
		t.Fatalf("updated=%#v events=%v error=%v", record, events, err)
	}
	events = nil
	if err := publisher.Unpublish(context.Background(), record, payload); err != nil || strings.Join(events, ",") != "verifier-delete,source-delete" {
		t.Fatalf("events=%v error=%v", events, err)
	}
}

func TestPublisherPartialFailuresRemainFailClosedAndResumable(t *testing.T) {
	t.Parallel()
	events := []string{}
	sources := &publicationSourceFake{events: &events}
	verifier := &publicationVerifierFake{events: &events, failUpsert: true}
	publisher := &Publisher{Sources: sources, Verifier: verifier}
	payload, _ := EncodeVerifierConfig("checkout", [][]byte{[]byte("valid-source-secret")})
	plan := SourcePlan{SourceBindingKey: strings.Repeat("d", 64), ExpectedProjectSlug: "checkout"}
	record, err := publisher.Publish(context.Background(), plan, payload)
	if !errors.Is(err, ErrPublication) || record.ResourceVersion == "" || sources.record.ResourceVersion == "" || verifier.present {
		t.Fatalf("record=%#v source=%#v verifier=%v error=%v", record, sources.record, verifier.present, err)
	}
	verifier.failUpsert = false
	if _, err := publisher.Publish(context.Background(), plan, payload); err != nil || !verifier.present {
		t.Fatalf("resume error=%v present=%v", err, verifier.present)
	}
	verifier.failDelete = true
	if err := publisher.Unpublish(context.Background(), sources.record, payload); !errors.Is(err, ErrPublication) || sources.deleted {
		t.Fatalf("unsafe delete: sourceDeleted=%v error=%v", sources.deleted, err)
	}
}

type publicationSourceFake struct {
	events  *[]string
	record  SourceRecord
	deleted bool
}

func (f *publicationSourceFake) Get(context.Context, string) (SourceRecord, error) {
	if f.record.ResourceVersion == "" {
		return SourceRecord{}, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "source")
	}
	return f.record, nil
}

func (f *publicationSourceFake) Create(_ context.Context, record SourceRecord) (SourceRecord, error) {
	*f.events = append(*f.events, "source-create")
	record.ResourceVersion = "1"
	f.record = record
	return record, nil
}

func (f *publicationSourceFake) CompareAndSwap(_ context.Context, _, next SourceRecord) (SourceRecord, error) {
	*f.events = append(*f.events, "source-cas")
	next.ResourceVersion = "2"
	f.record = next
	return next, nil
}

func (f *publicationSourceFake) DeleteExact(context.Context, SourceRecord) error {
	*f.events = append(*f.events, "source-delete")
	f.deleted = true
	return nil
}

type publicationVerifierFake struct {
	events     *[]string
	present    bool
	failUpsert bool
	failDelete bool
}

func (f *publicationVerifierFake) Upsert(context.Context, string, []byte) error {
	*f.events = append(*f.events, "verifier-upsert")
	if f.failUpsert {
		return errors.New("injected verifier write failure")
	}
	f.present = true
	return nil
}

func (f *publicationVerifierFake) DeleteExact(context.Context, string, []byte) error {
	*f.events = append(*f.events, "verifier-delete")
	if f.failDelete {
		return errors.New("injected verifier delete failure")
	}
	f.present = false
	return nil
}
