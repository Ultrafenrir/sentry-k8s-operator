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

package sentryapi

import "context"

// OperationPhase records durable progress around non-idempotent API mutations.
type OperationPhase string

const (
	OperationPlanned   OperationPhase = "Planned"
	OperationSubmitted OperationPhase = "Submitted"
	OperationVerified  OperationPhase = "Verified"
	OperationFailed    OperationPhase = "Failed"
)

// Operation deliberately stores only non-secret local identity and hashes.
type Operation struct {
	Key             string
	Kind            string
	LocalResource   string
	IdempotencyHash string
	Phase           OperationPhase
	Revision        int64
}

// OperationJournal is the persistence boundary used to recover uncertain mutations.
type OperationJournal interface {
	Load(ctx context.Context, key string) (Operation, bool, error)
	CompareAndSwap(ctx context.Context, previousRevision int64, next Operation) error
}
