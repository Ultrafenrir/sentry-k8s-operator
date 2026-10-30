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
	"encoding/hex"
	"errors"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sentryv1alpha1 "github.com/artemantipov/sentry-kubernetes-operator/api/v1alpha1"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/binding"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/destination"
	"github.com/artemantipov/sentry-kubernetes-operator/internal/policy"
)

const defaultFenceTTL = 5 * time.Minute

var (
	ErrFenceConflict = errors.New("telegram registration fence is held by another operation")
	ErrFenceStale    = errors.New("telegram registration challenge no longer matches live policy")
)

// FenceCoordinator uses a direct, uncached client. It deliberately has no Sentry client
// and can mutate only status subresources. Callers must give it a client constructed
// directly from rest.Config rather than a manager cache-backed delegating client.
type FenceCoordinator struct {
	Client client.Client
	Now    func() time.Time
	TTL    time.Duration
}

// Begin validates the challenge, acquires Connection then Project status fences, and
// finally re-reads both objects. Any object or policy drift prevents binding mutation.
func (c *FenceCoordinator) Begin(ctx context.Context, challenge binding.TelegramChallenge, operationHash string) error {
	if c.Client == nil || !validOperationHash(operationHash) || challenge.Project.Namespace == "" || challenge.Project.Name == "" || challenge.Project.UID == "" || challenge.Project.Generation <= 0 || challenge.Destination == "" || challenge.Connection == "" || challenge.PolicyGeneration <= 0 || challenge.PolicyResourceVersion == "" || !c.now().Before(challenge.ExpiresAt) {
		return ErrFenceStale
	}
	project, connection, err := c.readAndValidate(ctx, challenge, true)
	if err != nil {
		return err
	}
	commit := sentryv1alpha1.RegistrationCommit{
		OperationHash:    operationHash,
		PolicyGeneration: challenge.PolicyGeneration,
		ExpiresAt:        metav1.NewTime(c.now().Add(c.ttl())),
	}
	if err := c.acquireConnection(ctx, connection, commit); err != nil {
		return err
	}

	// Read the project again only after the policy fence is visible. This closes the
	// window between the initial checks and the destination fence.
	project = &sentryv1alpha1.SentryProject{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: challenge.Project.Namespace, Name: challenge.Project.Name}, project); err != nil {
		return errors.Join(ErrFenceStale, c.clearConnection(ctx, challenge, operationHash))
	}
	connection = &sentryv1alpha1.SentryConnection{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: challenge.Connection}, connection); err != nil || validateLive(challenge, project, connection, false) != nil || !matchesCommit(connection.Status.RegistrationCommit, operationHash, challenge.PolicyGeneration) {
		return errors.Join(ErrFenceStale, c.clearConnection(ctx, challenge, operationHash))
	}
	if err := c.acquireProject(ctx, project, challenge, commit); err != nil {
		return errors.Join(err, c.Clear(ctx, challenge, operationHash))
	}

	project, connection, err = c.readAndValidate(ctx, challenge, false)
	if err != nil || !matchesCommit(connection.Status.RegistrationCommit, operationHash, challenge.PolicyGeneration) || !matchesProjectCommit(project, challenge.Destination, operationHash, challenge.PolicyGeneration) {
		return errors.Join(ErrFenceStale, c.Clear(ctx, challenge, operationHash))
	}
	return nil
}

// Clear releases Project before Connection. It is idempotent and never removes a fence
// owned by a different operation.
func (c *FenceCoordinator) Clear(ctx context.Context, challenge binding.TelegramChallenge, operationHash string) error {
	if c.Client == nil || !validOperationHash(operationHash) {
		return ErrFenceStale
	}
	return errors.Join(
		c.clearProject(ctx, challenge, operationHash),
		c.clearConnection(ctx, challenge, operationHash),
	)
}

func (c *FenceCoordinator) readAndValidate(ctx context.Context, challenge binding.TelegramChallenge, requirePolicyResourceVersion bool) (*sentryv1alpha1.SentryProject, *sentryv1alpha1.SentryConnection, error) {
	project := &sentryv1alpha1.SentryProject{}
	if err := c.Client.Get(ctx, types.NamespacedName{Namespace: challenge.Project.Namespace, Name: challenge.Project.Name}, project); err != nil {
		return nil, nil, ErrFenceStale
	}
	connection := &sentryv1alpha1.SentryConnection{}
	if err := c.Client.Get(ctx, types.NamespacedName{Name: challenge.Connection}, connection); err != nil {
		return nil, nil, ErrFenceStale
	}
	if requirePolicyResourceVersion && connection.ResourceVersion != challenge.PolicyResourceVersion {
		return nil, nil, ErrFenceStale
	}
	if err := validateLive(challenge, project, connection, requirePolicyResourceVersion); err != nil {
		return nil, nil, err
	}
	return project, connection, nil
}

func validateLive(challenge binding.TelegramChallenge, project *sentryv1alpha1.SentryProject, connection *sentryv1alpha1.SentryConnection, _ bool) error {
	if string(project.UID) != challenge.Project.UID || project.Generation != challenge.Project.Generation || !project.DeletionTimestamp.IsZero() || project.Spec.Paused || connection.Name != challenge.Connection || connection.Generation != challenge.PolicyGeneration || !connection.DeletionTimestamp.IsZero() {
		return ErrFenceStale
	}
	if project.Spec.Connection != challenge.Connection && !(project.Spec.Connection == "" && connection.Spec.Default) {
		return ErrFenceStale
	}
	desired, found := project.Spec.Destinations[challenge.Destination]
	if !found {
		return ErrFenceStale
	}
	kind, selectorHash, err := destination.SelectorFingerprint(connection.Spec, desired)
	if err != nil || kind != "telegram" || selectorHash != challenge.SelectorHash {
		return ErrFenceStale
	}
	decision, err := policy.EvaluateDestination(project.Namespace, project.Spec, connection.Spec, desired)
	if err != nil || decision.AccessMode != binding.AccessManage || decision.Owner != challenge.Owner {
		return ErrFenceStale
	}
	return nil
}

func (c *FenceCoordinator) acquireConnection(ctx context.Context, connection *sentryv1alpha1.SentryConnection, commit sentryv1alpha1.RegistrationCommit) error {
	if current := connection.Status.RegistrationCommit; current != nil && current.OperationHash != commit.OperationHash && c.now().Before(current.ExpiresAt.Time) {
		return ErrFenceConflict
	}
	if matchesCommit(connection.Status.RegistrationCommit, commit.OperationHash, commit.PolicyGeneration) {
		return nil
	}
	connection.Status.RegistrationCommit = commit.DeepCopy()
	if err := c.Client.Status().Update(ctx, connection); err != nil {
		if apierrors.IsConflict(err) {
			return ErrFenceConflict
		}
		return err
	}
	return nil
}

func (c *FenceCoordinator) acquireProject(ctx context.Context, project *sentryv1alpha1.SentryProject, challenge binding.TelegramChallenge, commit sentryv1alpha1.RegistrationCommit) error {
	if project.Status.Destinations == nil {
		project.Status.Destinations = make(map[string]sentryv1alpha1.DestinationStatus)
	}
	status := project.Status.Destinations[challenge.Destination]
	if current := status.RegistrationCommit; current != nil && current.OperationHash != commit.OperationHash && c.now().Before(current.ExpiresAt.Time) {
		return ErrFenceConflict
	}
	if matchesCommit(status.RegistrationCommit, commit.OperationHash, commit.PolicyGeneration) {
		return nil
	}
	if status.Type != "" && status.Type != "telegram" {
		return ErrFenceStale
	}
	if status.Type == "" {
		status.Type = "telegram"
		status.State = destination.StateRegistrationRequired
		status.Reason = "TelegramRegistrationRequired"
	}
	status.RegistrationCommit = commit.DeepCopy()
	project.Status.Destinations[challenge.Destination] = status
	if err := c.Client.Status().Update(ctx, project); err != nil {
		if apierrors.IsConflict(err) {
			return ErrFenceConflict
		}
		return err
	}
	return nil
}

func (c *FenceCoordinator) clearProject(ctx context.Context, challenge binding.TelegramChallenge, operationHash string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		project := &sentryv1alpha1.SentryProject{}
		key := types.NamespacedName{Namespace: challenge.Project.Namespace, Name: challenge.Project.Name}
		if err := c.Client.Get(ctx, key, project); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if string(project.UID) != challenge.Project.UID {
			return nil
		}
		status, found := project.Status.Destinations[challenge.Destination]
		if !found || status.RegistrationCommit == nil || status.RegistrationCommit.OperationHash != operationHash {
			return nil
		}
		status.RegistrationCommit = nil
		project.Status.Destinations[challenge.Destination] = status
		return c.Client.Status().Update(ctx, project)
	})
}

func (c *FenceCoordinator) clearConnection(ctx context.Context, challenge binding.TelegramChallenge, operationHash string) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		connection := &sentryv1alpha1.SentryConnection{}
		if err := c.Client.Get(ctx, types.NamespacedName{Name: challenge.Connection}, connection); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		if connection.Status.RegistrationCommit == nil || connection.Status.RegistrationCommit.OperationHash != operationHash {
			return nil
		}
		connection.Status.RegistrationCommit = nil
		return c.Client.Status().Update(ctx, connection)
	})
}

func matchesProjectCommit(project *sentryv1alpha1.SentryProject, destinationName, operationHash string, policyGeneration int64) bool {
	status, found := project.Status.Destinations[destinationName]
	return found && matchesCommit(status.RegistrationCommit, operationHash, policyGeneration)
}

func matchesCommit(commit *sentryv1alpha1.RegistrationCommit, operationHash string, policyGeneration int64) bool {
	return commit != nil && commit.OperationHash == operationHash && commit.PolicyGeneration == policyGeneration
}

func validOperationHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func (c *FenceCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *FenceCoordinator) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return defaultFenceTTL
}
