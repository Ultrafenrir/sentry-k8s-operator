/*
Copyright 2026 Sentry Kubernetes Operator Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

// Package ownership implements connection-wide regional fencing decisions. It does not
// provide a distributed lock: Sentry has no proven conditional-write primitive for the
// project marker, so callers must revalidate immediately before every mutation.
package ownership

import (
	"context"
	"errors"
	"time"
)

var (
	ErrStandby  = errors.New("connection is standby")
	ErrFenced   = errors.New("connection revision is fenced")
	ErrConflict = errors.New("equal revision ownership conflict")
	ErrChanged  = errors.New("ownership marker changed during fencing")
)

// Marker is protected remote ownership evidence. Installation is a stable operator
// installation identity, not a pod or leader identity.
type Marker struct {
	Region       string
	Revision     int64
	Owner        string
	Installation string
}

type Local struct {
	Marker
	Active bool
}

// ValidateMutation classifies one exact marker read immediately before an external write.
func ValidateMutation(local Local, remote Marker) error {
	if !local.Active {
		return ErrStandby
	}
	if remote.Revision > local.Revision {
		return ErrFenced
	}
	if remote.Revision < local.Revision {
		return ErrFenced
	}
	if remote.Region != local.Region || remote.Installation != local.Installation || remote.Owner != local.Owner {
		return ErrConflict
	}
	return nil
}

// CanTakeOver accepts only an Active connection with a strictly larger revision.
func CanTakeOver(local Local, remote Marker) error {
	if !local.Active {
		return ErrStandby
	}
	if local.Revision <= remote.Revision {
		if local.Revision == remote.Revision {
			return ErrConflict
		}
		return ErrFenced
	}
	return nil
}

type Clock interface {
	Sleep(context.Context, time.Duration) error
}

type MarkerStore interface {
	Read(context.Context) (Marker, error)
	Write(context.Context, Marker) error
}

// TakeOver performs pre-read, marker write, bounded fencing delay, and read-after-write.
// A caller may mutate child resources only after this returns nil.
func TakeOver(ctx context.Context, store MarkerStore, clock Clock, delay time.Duration, local Local) error {
	remote, err := store.Read(ctx)
	if err != nil {
		return err
	}
	if err := CanTakeOver(local, remote); err != nil {
		return err
	}
	if err := store.Write(ctx, local.Marker); err != nil {
		return err
	}
	if err := clock.Sleep(ctx, delay); err != nil {
		return err
	}
	observed, err := store.Read(ctx)
	if err != nil {
		return err
	}
	if observed != local.Marker {
		return ErrChanged
	}
	return nil
}
