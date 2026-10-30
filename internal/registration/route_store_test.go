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
	"sort"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
)

func TestRouteStorePublishesImmutableIdempotentRecordAndRevokesExactlyIt(t *testing.T) {
	t.Parallel()

	store := routeStore(t)
	key := strings.Repeat("a", 64)
	target := binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"}
	if err := store.PublishTelegramRoute(context.Background(), key, target); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishTelegramRoute(context.Background(), key, target); err != nil {
		t.Fatalf("idempotent publish: %v", err)
	}
	got, err := store.GetTelegramRoute(context.Background(), key)
	if err != nil || got != target {
		t.Fatalf("target=%#v err=%v", got, err)
	}
	secret := &corev1.Secret{}
	if err := store.Client.Get(context.Background(), types.NamespacedName{Namespace: "state", Name: routeName(key)}, secret); err != nil {
		t.Fatal(err)
	}
	if secret.Immutable == nil || !*secret.Immutable || secret.Labels[recordTypeLabel] != routeRecordLabel || len(secret.Data) != 1 {
		t.Fatalf("secret=%#v", secret)
	}
	if err := store.PublishTelegramRoute(context.Background(), key, binding.TelegramTarget{ChatIdentity: "-100999"}); !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("conflicting publish=%v", err)
	}
	if err := store.RevokeTelegramRoute(context.Background(), key, binding.TelegramTarget{ChatIdentity: "-100999"}); !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("wrong-target revoke=%v", err)
	}
	if err := store.RevokeTelegramRoute(context.Background(), key, target); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeTelegramRoute(context.Background(), key, target); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
	if _, err := store.GetTelegramRoute(context.Background(), key); !apierrors.IsNotFound(err) {
		t.Fatalf("get after revoke=%v", err)
	}
}

func TestRouteStoreConcurrentSamePublishConverges(t *testing.T) {
	t.Parallel()

	store := routeStore(t)
	key := strings.Repeat("b", 64)
	target := binding.TelegramTarget{ChatIdentity: "-100123"}
	start := make(chan struct{})
	errs := make(chan error, 8)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- store.PublishTelegramRoute(context.Background(), key, target)
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	var messages []string
	for err := range errs {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	sort.Strings(messages)
	if len(messages) != 0 {
		t.Fatalf("concurrent errors=%v", messages)
	}
	got, err := store.GetTelegramRoute(context.Background(), key)
	if err != nil || got != target {
		t.Fatalf("target=%#v err=%v", got, err)
	}
}

func TestRouteStoreRejectsMalformedKeysTargetsAndRecords(t *testing.T) {
	t.Parallel()

	store := routeStore(t)
	for name, input := range map[string]struct {
		key    string
		target binding.TelegramTarget
	}{
		"key":   {key: "not-a-hash", target: binding.TelegramTarget{ChatIdentity: "-100123"}},
		"chat":  {key: strings.Repeat("c", 64), target: binding.TelegramTarget{ChatIdentity: "chat-name"}},
		"topic": {key: strings.Repeat("d", 64), target: binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "0"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.PublishTelegramRoute(context.Background(), input.key, input.target); !errors.Is(err, ErrRouteInvalid) {
				t.Fatalf("error=%v", err)
			}
		})
	}
	key := strings.Repeat("e", 64)
	immutable := true
	corrupt := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "state", Name: routeName(key), Labels: map[string]string{recordTypeLabel: routeRecordLabel}},
		Immutable:  &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{routeRecordKey: []byte(`{"version":"future"}`)},
	}
	if err := store.Client.Create(context.Background(), corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetTelegramRoute(context.Background(), key); !errors.Is(err, ErrRouteInvalid) {
		t.Fatalf("corrupt get=%v", err)
	}
}

func TestRouteStoreMigratesLegacyRecordPublishBeforeDelete(t *testing.T) {
	t.Parallel()

	destination := routeStore(t)
	destination.Namespace = "routing"
	legacy := &RouteStore{Client: destination.Client, Namespace: "state"}
	key := strings.Repeat("f", 64)
	target := binding.TelegramTarget{ChatIdentity: "-100123", TopicIdentity: "42"}
	if err := legacy.PublishTelegramRoute(context.Background(), key, target); err != nil {
		t.Fatal(err)
	}
	migrated, err := destination.MigrateLegacy(context.Background(), "state", 10)
	if err != nil || migrated != 1 {
		t.Fatalf("migration = (%d, %v)", migrated, err)
	}
	if got, err := destination.GetTelegramRoute(context.Background(), key); err != nil || got != target {
		t.Fatalf("destination = (%+v, %v)", got, err)
	}
	if _, err := legacy.GetTelegramRoute(context.Background(), key); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy route remains: %v", err)
	}
	// A repeat after source deletion is a no-op.
	if migrated, err := destination.MigrateLegacy(context.Background(), "state", 10); err != nil || migrated != 0 {
		t.Fatalf("repeat migration = (%d, %v)", migrated, err)
	}
}

func TestRouteStoreMigrationConflictPreservesLegacyRecord(t *testing.T) {
	t.Parallel()

	destination := routeStore(t)
	destination.Namespace = "routing"
	legacy := &RouteStore{Client: destination.Client, Namespace: "state"}
	key := strings.Repeat("9", 64)
	legacyTarget := binding.TelegramTarget{ChatIdentity: "-100123"}
	if err := legacy.PublishTelegramRoute(context.Background(), key, legacyTarget); err != nil {
		t.Fatal(err)
	}
	if err := destination.PublishTelegramRoute(context.Background(), key, binding.TelegramTarget{ChatIdentity: "-100999"}); err != nil {
		t.Fatal(err)
	}
	migrated, err := destination.MigrateLegacy(context.Background(), "state", 10)
	if migrated != 0 || !errors.Is(err, ErrRouteConflict) {
		t.Fatalf("conflicting migration = (%d, %v)", migrated, err)
	}
	if got, err := legacy.GetTelegramRoute(context.Background(), key); err != nil || got != legacyTarget {
		t.Fatalf("legacy route was changed = (%+v, %v)", got, err)
	}
}

func routeStore(t *testing.T) *RouteStore {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &RouteStore{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "state"}
}
