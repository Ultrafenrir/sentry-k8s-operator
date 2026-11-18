/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package monitorstate

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestStorePinsIdentityAndUsesResourceVersionForUpdates(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := &Store{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "operator"}
	binding := NewBinding("Uptime", "apps", "checkout", "uid-1", "project-uid", "primary", "company", "checkout", "remote-1", "managed-name", strings.Repeat("a", 64))

	created, err := store.Create(context.Background(), binding)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	loaded, err := store.Get(context.Background(), "uid-1")
	if err != nil || loaded.Binding.ExternalIdentity != "remote-1" || loaded.Name != created.Name {
		t.Fatalf("Get = %#v, %v", loaded, err)
	}

	next := loaded.Binding
	next.DesiredHash = strings.Repeat("b", 64)
	updated, err := store.Update(context.Background(), loaded, next)
	if err != nil || updated.ResourceVersion == loaded.ResourceVersion {
		t.Fatalf("Update = %#v, %v", updated, err)
	}
	if _, err := store.Update(context.Background(), loaded, next); err == nil {
		t.Fatal("stale resource version unexpectedly updated protected state")
	}
	if err := store.Delete(context.Background(), updated); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Get(context.Background(), "uid-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("post-delete Get error = %v", err)
	}
}

func TestStoreRejectsIncompleteState(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store := &Store{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Namespace: "operator"}
	invalid := NewBinding("Cron", "apps", "checkout", "uid-1", "project-uid", "primary", "company", "checkout", "remote-1", "slug", "not-a-digest")
	if _, err := store.Create(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid digest error = %v", err)
	}
	invalid = NewBinding("Cron", "apps", "checkout", "uid-1", "project-uid", "primary", "company", "checkout", "", "slug", strings.Repeat("a", 64))
	if _, err := store.Create(context.Background(), invalid); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing identity error = %v", err)
	}
}
