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

package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// TestQueueBackpressureAgainstAPIServer is deliberately a real API-server
// probe. The fake client cannot validate concurrent create/list/delete behavior
// or Kubernetes resourceVersion delete preconditions.
func TestQueueBackpressureAgainstAPIServer(t *testing.T) {
	testEnvironment := &envtest.Environment{}
	config, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("start envtest API server: %v", err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("stop envtest API server: %v", err)
		}
	})
	config.QPS = 100
	config.Burst = 200

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubernetesClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create Kubernetes client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "delivery-load-probe"}}
	if err := kubernetesClient.Create(ctx, namespace); err != nil {
		t.Fatalf("create probe namespace: %v", err)
	}

	const capacity = 12
	queue := &Queue{
		Client: kubernetesClient, Namespace: namespace.Name, MaxOutstanding: capacity,
		MaxAttempts: 3, DedupeRetention: time.Hour, DLQRetention: 24 * time.Hour,
	}
	accepted := make([]Message, 0, capacity)
	for index := range capacity {
		message := integrationMessage("accepted", index)
		if _, created, err := queue.Enqueue(ctx, message); err != nil || !created {
			t.Fatalf("seed enqueue %d = (created=%t, error=%v)", index, created, err)
		}
		accepted = append(accepted, message)
	}

	// A burst of unique producers forces every request through immutable
	// candidate Create, a capacity List, and exact candidate deletion.
	const overflowProducers = 48
	start := make(chan struct{})
	results := make(chan enqueueResult, overflowProducers)
	var wait sync.WaitGroup
	wait.Add(overflowProducers)
	for index := range overflowProducers {
		go func() {
			defer wait.Done()
			message := integrationMessage("overflow", index)
			<-start
			_, created, err := queue.Enqueue(ctx, message)
			results <- enqueueResult{message: message, created: created, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	for result := range results {
		if result.created || !errors.Is(result.err, ErrBackpressure) {
			t.Errorf("overflow enqueue %s = (created=%t, error=%v), want backpressure", result.message.DedupeKey, result.created, result.err)
		}
		secret := &corev1.Secret{}
		err := kubernetesClient.Get(ctx, types.NamespacedName{Namespace: namespace.Name, Name: recordName(result.message.DedupeKey)}, secret)
		if !apierrors.IsNotFound(err) {
			t.Errorf("rejected record %s still exists: %v", result.message.DedupeKey, err)
		}
	}

	secrets := &corev1.SecretList{}
	if err := kubernetesClient.List(ctx, secrets, client.InNamespace(namespace.Name), client.MatchingLabels{recordTypeLabel: recordTypeQueue}); err != nil {
		t.Fatalf("list final queue: %v", err)
	}
	if len(secrets.Items) != capacity {
		t.Fatalf("final queue contains %d records, want capacity %d", len(secrets.Items), capacity)
	}
	admissions := &corev1.SecretList{}
	if err := kubernetesClient.List(ctx, admissions, client.InNamespace(namespace.Name), client.MatchingLabels{recordTypeLabel: recordTypeAdmission}); err != nil {
		t.Fatalf("list admission candidates: %v", err)
	}
	if len(admissions.Items) != 0 {
		t.Fatalf("queue contains %d admission candidates after overflow cleanup, want 0", len(admissions.Items))
	}
	for _, secret := range secrets.Items {
		if len(secret.Labels) != 2 || secret.Labels[recordTypeLabel] != recordTypeQueue || secret.Labels[recordStateLabel] != string(StatePending) {
			t.Errorf("record %s has unexpected metadata labels: %v", secret.Name, secret.Labels)
		}
	}
	for index, message := range accepted {
		record, created, err := queue.Enqueue(ctx, message)
		if err != nil || created || record.State != StatePending {
			t.Errorf("idempotent retry %d = (state=%s, created=%t, error=%v)", index, record.State, created, err)
		}
	}
}

type enqueueResult struct {
	message Message
	created bool
	err     error
}

func integrationMessage(prefix string, index int) Message {
	return Message{
		DedupeKey: integrationHash(fmt.Sprintf("%s-dedupe-%d", prefix, index)),
		RouteKey:  integrationHash(fmt.Sprintf("%s-route-%d", prefix, index)),
		Text:      fmt.Sprintf("protected load-probe payload %d", index),
	}
}

func integrationHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
