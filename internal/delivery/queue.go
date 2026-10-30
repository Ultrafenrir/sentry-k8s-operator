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

// Package delivery implements protected, at-least-once delivery state. It does
// not perform network delivery and never stores destination identities in
// Kubernetes object metadata.
package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	queueDataKey        = "delivery.json"
	recordTypeLabel     = "sentryconfiguration.io/record-type"
	recordTypeQueue     = "telegram-delivery"
	recordTypeAdmission = "telegram-delivery-admission"
	recordStateLabel    = "sentryconfiguration.io/delivery-state"
	admissionDataKey    = "admission.json"
	recordVersion       = "v1alpha1"
	maxMessageRunes     = 4096
	maxMessageBytes     = 16 << 10
	admissionTimeout    = 5 * time.Minute
)

type State string

const (
	StatePending    State = "Pending"
	StateInFlight   State = "InFlight"
	StateRetry      State = "Retry"
	StateSent       State = "Sent"
	StateDeadLetter State = "DeadLetter"
)

type Failure string

const (
	FailureTransport    Failure = "Transport"
	FailureUnavailable  Failure = "Unavailable"
	FailureRateLimited  Failure = "RateLimited"
	FailureRejected     Failure = "Rejected"
	FailureInvalidRoute Failure = "InvalidRoute"
)

var (
	ErrInvalid      = errors.New("delivery record is invalid")
	ErrConflict     = errors.New("dedupe key is already bound to another message")
	ErrBackpressure = errors.New("delivery queue capacity is exhausted")
	ErrNoClaim      = errors.New("delivery record is not claimable")
	ErrClaimLost    = errors.New("delivery claim is no longer current")
)

type Message struct {
	DedupeKey string
	RouteKey  string
	Text      string
}

// Record is protected state. ResourceVersion is deliberately not serialized in
// the Secret payload and is used only as the Kubernetes CAS token.
type Record struct {
	Version          string    `json:"version"`
	Revision         int64     `json:"revision"`
	DedupeKey        string    `json:"dedupeKey"`
	MessageHash      string    `json:"messageHash"`
	RouteKey         string    `json:"routeKey,omitempty"`
	Text             string    `json:"text,omitempty"`
	State            State     `json:"state"`
	Attempts         int       `json:"attempts"`
	ClaimOwner       string    `json:"claimOwner,omitempty"`
	LeaseUntil       time.Time `json:"leaseUntil,omitempty"`
	NextAttempt      time.Time `json:"nextAttempt,omitempty"`
	LastFailure      Failure   `json:"lastFailure,omitempty"`
	AdmissionPending bool      `json:"admissionPending,omitempty"`
	CreatedAt        time.Time `json:"createdAt"`
	UpdatedAt        time.Time `json:"updatedAt"`
	ExpiresAt        time.Time `json:"expiresAt,omitempty"`
	ResourceVersion  string    `json:"-"`
}

type Queue struct {
	Client          client.Client
	Namespace       string
	MaxOutstanding  int
	MaxAttempts     int
	DedupeRetention time.Duration
	DLQRetention    time.Duration
	Now             func() time.Time
}

// Statistics is an identity- and payload-free queue snapshot suitable for
// operational metrics. It never exposes record names, hashes, routes or text.
type Statistics struct {
	ByState              map[State]int
	OldestOutstandingAge time.Duration
}

func (q *Queue) Statistics(ctx context.Context) (Statistics, error) {
	if err := q.validate(); err != nil {
		return Statistics{}, ErrInvalid
	}
	records, err := q.list(ctx)
	if err != nil {
		return Statistics{}, err
	}
	statistics := Statistics{ByState: map[State]int{}}
	now := q.now()
	var oldest time.Time
	for _, record := range records {
		statistics.ByState[record.State]++
		if record.State != StateSent && (oldest.IsZero() || record.CreatedAt.Before(oldest)) {
			oldest = record.CreatedAt
		}
	}
	admissions, err := q.listAdmissions(ctx)
	if err != nil {
		return Statistics{}, err
	}
	statistics.ByState[StatePending] += len(admissions)
	for _, admission := range admissions {
		if oldest.IsZero() || admission.CreatedAt.Before(oldest) {
			oldest = admission.CreatedAt
		}
	}
	if !oldest.IsZero() && now.After(oldest) {
		statistics.OldestOutstandingAge = now.Sub(oldest)
	}
	return statistics, nil
}

// Enqueue creates one deterministic final record through a create/delete-only admission
// candidate. A repeated identical message is
// idempotent even after success; the same dedupe key with different content is
// always a conflict. Capacity is rechecked after Create so concurrent producers
// fail conservatively instead of silently growing an unbounded queue.
func (q *Queue) Enqueue(ctx context.Context, message Message) (Record, bool, error) {
	if err := q.validate(); err != nil || !validMessage(message) {
		return Record{}, false, ErrInvalid
	}
	hash := messageHash(message)
	current, err := q.get(ctx, message.DedupeKey)
	if err == nil {
		if current.MessageHash != hash {
			return Record{}, false, ErrConflict
		}
		return current, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return Record{}, false, err
	}
	now := q.now()
	candidate := Record{
		Version: recordVersion, Revision: 1, DedupeKey: message.DedupeKey,
		MessageHash: hash, RouteKey: message.RouteKey, Text: message.Text,
		State: StatePending, AdmissionPending: true, CreatedAt: now, UpdatedAt: now,
		ExpiresAt: now.Add(admissionTimeout),
	}
	secret, err := encodeAdmission(q.Namespace, candidate)
	if err != nil {
		return Record{}, false, err
	}
	if err := q.Client.Create(ctx, secret); err != nil {
		current, getErr := q.getAdmission(ctx, message.DedupeKey)
		if getErr != nil {
			// Another identical enqueue may have committed the final record and
			// removed the deterministic candidate between our Create and Get.
			// A direct final-record read closes that AlreadyExists/cleanup race and
			// also recovers a lost candidate Create response after a concurrent
			// request completed admission.
			final, finalErr := q.get(ctx, message.DedupeKey)
			if finalErr == nil {
				if final.MessageHash != hash {
					return Record{}, false, ErrConflict
				}
				return final, false, nil
			}
			if !apierrors.IsNotFound(finalErr) {
				return Record{}, false, finalErr
			}
			return Record{}, false, err
		}
		if current.MessageHash != hash {
			return Record{}, false, ErrConflict
		}
		candidate = current
	} else {
		candidate.ResourceVersion = secret.ResourceVersion
	}
	admitted, created, err := q.admit(ctx, candidate)
	if err != nil {
		return Record{}, false, err
	}
	return admitted, created, nil
}

// admit performs the post-candidate capacity check and commits the final deterministic
// record with Create. A failed List/delete leaves only an immutable, unclaimable,
// expiring candidate. An identical retry resumes it, while the worker continues to see
// only final telegram-delivery records.
func (q *Queue) admit(ctx context.Context, candidate Record) (Record, bool, error) {
	if !validAdmissionRecord(candidate) || candidate.ResourceVersion == "" {
		return Record{}, false, ErrInvalid
	}
	current, err := q.get(ctx, candidate.DedupeKey)
	if err == nil {
		if current.MessageHash != candidate.MessageHash {
			return Record{}, false, ErrConflict
		}
		_ = q.deleteAdmissionExact(ctx, candidate)
		return current, false, nil
	}
	if !apierrors.IsNotFound(err) {
		return Record{}, false, err
	}
	count, err := q.outstanding(ctx)
	if err != nil || count > q.MaxOutstanding {
		deleteErr := q.deleteAdmissionExact(ctx, candidate)
		if err != nil {
			return Record{}, false, errors.Join(err, deleteErr)
		}
		return Record{}, false, errors.Join(ErrBackpressure, deleteErr)
	}
	now := q.now()
	final := candidate
	final.Revision++
	final.AdmissionPending = false
	final.UpdatedAt = now
	final.ExpiresAt = time.Time{}
	final.ResourceVersion = ""
	secret, err := encode(q.Namespace, final)
	if err != nil {
		return Record{}, false, err
	}
	created := true
	if err := q.Client.Create(ctx, secret); err != nil {
		current, getErr := q.get(ctx, candidate.DedupeKey)
		if getErr != nil {
			if apierrors.IsAlreadyExists(err) {
				return Record{}, false, getErr
			}
			return Record{}, false, err
		}
		if current.MessageHash != candidate.MessageHash {
			return Record{}, false, ErrConflict
		}
		final = current
		created = false
	} else {
		final.ResourceVersion = secret.ResourceVersion
	}
	// The final Create is the durable commit. Candidate cleanup is best-effort here and
	// exact/expiry-bounded in Sweep, so a cleanup outage cannot turn a committed delivery
	// into a false enqueue failure.
	_ = q.deleteAdmissionExact(ctx, candidate)
	return final, created, nil
}

// Claim takes the oldest eligible item using ResourceVersion CAS. Expired
// leases are claimable, which provides crash recovery and at-least-once rather
// than exactly-once semantics.
func (q *Queue) Claim(ctx context.Context, owner string, lease time.Duration) (Record, error) {
	if err := q.validate(); err != nil || !validHash(owner) || lease <= 0 {
		return Record{}, ErrInvalid
	}
	records, err := q.list(ctx)
	if err != nil {
		return Record{}, err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].DedupeKey < records[j].DedupeKey
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	now := q.now()
	for _, record := range records {
		if !eligible(record, now) {
			continue
		}
		next := record
		next.Revision++
		next.State = StateInFlight
		next.ClaimOwner = owner
		next.LeaseUntil = now.Add(lease)
		next.NextAttempt = time.Time{}
		next.UpdatedAt = now
		updated, err := q.update(ctx, record, next)
		if apierrors.IsConflict(err) || errors.Is(err, ErrClaimLost) {
			continue
		}
		return updated, err
	}
	return Record{}, ErrNoClaim
}

// Complete replaces the payload with a dedupe tombstone. A stale worker cannot
// complete a record reclaimed after lease expiry.
func (q *Queue) Complete(ctx context.Context, claimed Record) (Record, error) {
	current, err := q.currentClaim(ctx, claimed)
	if err != nil {
		return Record{}, err
	}
	now := q.now()
	next := current
	next.Revision++
	next.State = StateSent
	next.RouteKey = ""
	next.Text = ""
	next.ClaimOwner = ""
	next.LeaseUntil = time.Time{}
	next.NextAttempt = time.Time{}
	next.LastFailure = ""
	next.UpdatedAt = now
	next.ExpiresAt = now.Add(q.DedupeRetention)
	return q.update(ctx, current, next)
}

// Fail schedules a bounded retry or moves the protected payload to the DLQ.
// Only a fixed Failure enum is stored; raw upstream error text is excluded.
func (q *Queue) Fail(ctx context.Context, claimed Record, failure Failure, retryAfter time.Duration, permanent bool) (Record, error) {
	if !validFailure(failure) || retryAfter < 0 {
		return Record{}, ErrInvalid
	}
	current, err := q.currentClaim(ctx, claimed)
	if err != nil {
		return Record{}, err
	}
	now := q.now()
	next := current
	next.Revision++
	next.Attempts++
	next.ClaimOwner = ""
	next.LeaseUntil = time.Time{}
	next.LastFailure = failure
	next.UpdatedAt = now
	if permanent || next.Attempts >= q.MaxAttempts {
		next.State = StateDeadLetter
		next.NextAttempt = time.Time{}
		next.ExpiresAt = now.Add(q.DLQRetention)
	} else {
		next.State = StateRetry
		next.NextAttempt = now.Add(retryAfter)
		next.ExpiresAt = time.Time{}
	}
	return q.update(ctx, current, next)
}

// Sweep deletes expired terminal records and abandoned pre-admission records with exact
// ResourceVersion preconditions. Admitted Pending work is never removed by retention.
func (q *Queue) Sweep(ctx context.Context, limit int) (int, error) {
	if err := q.validate(); err != nil || limit <= 0 {
		return 0, ErrInvalid
	}
	admissions, err := q.listAdmissions(ctx)
	if err != nil {
		return 0, err
	}
	now := q.now()
	removed := 0
	for _, admission := range admissions {
		if removed == limit {
			return removed, nil
		}
		if admission.ExpiresAt.After(now) {
			continue
		}
		if err := q.deleteAdmissionExact(ctx, admission); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				continue
			}
			return removed, err
		}
		removed++
	}
	records, err := q.list(ctx)
	if err != nil {
		return removed, err
	}
	for _, record := range records {
		if removed == limit {
			break
		}
		expiredTerminal := (record.State == StateSent || record.State == StateDeadLetter) && !record.ExpiresAt.IsZero() && !record.ExpiresAt.After(now)
		if !expiredTerminal {
			continue
		}
		if err := q.deleteExact(ctx, record); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				continue
			}
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (q *Queue) currentClaim(ctx context.Context, claimed Record) (Record, error) {
	if err := q.validate(); err != nil || claimed.State != StateInFlight || !validHash(claimed.DedupeKey) || !validHash(claimed.ClaimOwner) || claimed.ResourceVersion == "" {
		return Record{}, ErrInvalid
	}
	current, err := q.get(ctx, claimed.DedupeKey)
	if err != nil {
		return Record{}, err
	}
	if current.ResourceVersion != claimed.ResourceVersion || current.State != StateInFlight || current.ClaimOwner != claimed.ClaimOwner || current.Revision != claimed.Revision || !current.LeaseUntil.After(q.now()) {
		return Record{}, ErrClaimLost
	}
	return current, nil
}

func (q *Queue) get(ctx context.Context, dedupeKey string) (Record, error) {
	secret := &corev1.Secret{}
	if err := q.Client.Get(ctx, types.NamespacedName{Namespace: q.Namespace, Name: recordName(dedupeKey)}, secret); err != nil {
		return Record{}, err
	}
	return decode(secret, dedupeKey)
}

func (q *Queue) getAdmission(ctx context.Context, dedupeKey string) (Record, error) {
	secret := &corev1.Secret{}
	if err := q.Client.Get(ctx, types.NamespacedName{Namespace: q.Namespace, Name: admissionName(dedupeKey)}, secret); err != nil {
		return Record{}, err
	}
	return decodeAdmission(secret, dedupeKey)
}

func (q *Queue) list(ctx context.Context) ([]Record, error) {
	secrets := &corev1.SecretList{}
	if err := q.Client.List(ctx, secrets, client.InNamespace(q.Namespace), client.MatchingLabels{recordTypeLabel: recordTypeQueue}); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(secrets.Items))
	for i := range secrets.Items {
		record, err := decode(&secrets.Items[i], "")
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (q *Queue) listAdmissions(ctx context.Context) ([]Record, error) {
	secrets := &corev1.SecretList{}
	if err := q.Client.List(ctx, secrets, client.InNamespace(q.Namespace), client.MatchingLabels{recordTypeLabel: recordTypeAdmission}); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(secrets.Items))
	for i := range secrets.Items {
		record, err := decodeAdmission(&secrets.Items[i], "")
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (q *Queue) outstanding(ctx context.Context) (int, error) {
	records, err := q.list(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, record := range records {
		if record.State != StateSent {
			count++
		}
	}
	admissions, err := q.listAdmissions(ctx)
	if err != nil {
		return 0, err
	}
	count += len(admissions)
	return count, nil
}

func (q *Queue) update(ctx context.Context, previous, next Record) (Record, error) {
	if previous.ResourceVersion == "" || next.Revision != previous.Revision+1 || next.DedupeKey != previous.DedupeKey || next.MessageHash != previous.MessageHash {
		return Record{}, ErrInvalid
	}
	secret, err := encode(q.Namespace, next)
	if err != nil {
		return Record{}, err
	}
	secret.ResourceVersion = previous.ResourceVersion
	if err := q.Client.Update(ctx, secret); err != nil {
		return Record{}, err
	}
	next.ResourceVersion = secret.ResourceVersion
	return next, nil
}

func (q *Queue) deleteExact(ctx context.Context, record Record) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: recordName(record.DedupeKey)}}
	resourceVersion := record.ResourceVersion
	return q.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion})
}

func (q *Queue) deleteAdmissionExact(ctx context.Context, record Record) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: q.Namespace, Name: admissionName(record.DedupeKey)}}
	resourceVersion := record.ResourceVersion
	return q.Client.Delete(ctx, secret, client.Preconditions{ResourceVersion: &resourceVersion})
}

func encode(namespace string, record Record) (*corev1.Secret, error) {
	if namespace == "" || record.AdmissionPending || !validRecord(record) {
		return nil, ErrInvalid
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, ErrInvalid
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: recordName(record.DedupeKey),
			Labels: map[string]string{recordTypeLabel: recordTypeQueue, recordStateLabel: string(record.State)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{queueDataKey: payload},
	}, nil
}

func encodeAdmission(namespace string, record Record) (*corev1.Secret, error) {
	if namespace == "" || !validAdmissionRecord(record) {
		return nil, ErrInvalid
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, ErrInvalid
	}
	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: admissionName(record.DedupeKey),
			Labels: map[string]string{recordTypeLabel: recordTypeAdmission},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      map[string][]byte{admissionDataKey: payload},
	}, nil
}

func decode(secret *corev1.Secret, expectedKey string) (Record, error) {
	if secret == nil || secret.Type != corev1.SecretTypeOpaque || secret.Labels[recordTypeLabel] != recordTypeQueue || len(secret.Data) != 1 {
		return Record{}, ErrInvalid
	}
	var record Record
	if err := json.Unmarshal(secret.Data[queueDataKey], &record); err != nil || record.AdmissionPending || !validRecord(record) || secret.Name != recordName(record.DedupeKey) || secret.Labels[recordStateLabel] != string(record.State) || (expectedKey != "" && record.DedupeKey != expectedKey) {
		return Record{}, ErrInvalid
	}
	record.ResourceVersion = secret.ResourceVersion
	return record, nil
}

func decodeAdmission(secret *corev1.Secret, expectedKey string) (Record, error) {
	if secret == nil || secret.Type != corev1.SecretTypeOpaque || secret.Immutable == nil || !*secret.Immutable ||
		secret.Labels[recordTypeLabel] != recordTypeAdmission || len(secret.Labels) != 1 || len(secret.Data) != 1 {
		return Record{}, ErrInvalid
	}
	var record Record
	if err := json.Unmarshal(secret.Data[admissionDataKey], &record); err != nil || !validAdmissionRecord(record) ||
		secret.Name != admissionName(record.DedupeKey) || (expectedKey != "" && record.DedupeKey != expectedKey) {
		return Record{}, ErrInvalid
	}
	record.ResourceVersion = secret.ResourceVersion
	return record, nil
}

// ValidateRecordPayload validates a protected delivery payload, deterministic object name,
// and fixed state label without requiring a Kubernetes client.
func ValidateRecordPayload(name, stateLabel string, payload []byte) error {
	var record Record
	if json.Unmarshal(payload, &record) != nil || record.AdmissionPending || !validRecord(record) || name != recordName(record.DedupeKey) || stateLabel != string(record.State) {
		return ErrInvalid
	}
	return nil
}

// ValidateAdmissionPayload validates an immutable create/delete-only admission candidate.
func ValidateAdmissionPayload(name string, payload []byte) error {
	var record Record
	if json.Unmarshal(payload, &record) != nil || !validAdmissionRecord(record) || name != admissionName(record.DedupeKey) {
		return ErrInvalid
	}
	return nil
}

func validRecord(record Record) bool {
	if record.Version != recordVersion || record.Revision <= 0 || !validHash(record.DedupeKey) || !validHash(record.MessageHash) || record.CreatedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.CreatedAt) || record.Attempts < 0 {
		return false
	}
	if record.AdmissionPending && record.State != StatePending {
		return false
	}
	switch record.State {
	case StatePending:
		if !validPayload(record) || record.ClaimOwner != "" || !record.LeaseUntil.IsZero() || !record.NextAttempt.IsZero() || record.LastFailure != "" || record.Attempts != 0 {
			return false
		}
		if record.AdmissionPending {
			return !record.ExpiresAt.IsZero()
		}
		return record.ExpiresAt.IsZero()
	case StateInFlight:
		return validPayload(record) && validHash(record.ClaimOwner) && !record.LeaseUntil.IsZero() && record.NextAttempt.IsZero() && record.ExpiresAt.IsZero()
	case StateRetry:
		return validPayload(record) && record.ClaimOwner == "" && record.LeaseUntil.IsZero() && !record.NextAttempt.IsZero() && validFailure(record.LastFailure) && record.ExpiresAt.IsZero()
	case StateSent:
		return record.RouteKey == "" && record.Text == "" && record.ClaimOwner == "" && record.LeaseUntil.IsZero() && record.NextAttempt.IsZero() && record.LastFailure == "" && !record.ExpiresAt.IsZero()
	case StateDeadLetter:
		return validPayload(record) && record.ClaimOwner == "" && record.LeaseUntil.IsZero() && record.NextAttempt.IsZero() && validFailure(record.LastFailure) && !record.ExpiresAt.IsZero()
	default:
		return false
	}
}

func validAdmissionRecord(record Record) bool {
	return record.AdmissionPending && record.Revision == 1 && record.State == StatePending && validRecord(record)
}

func validPayload(record Record) bool {
	return validHash(record.RouteKey) && record.Text != "" && utf8.ValidString(record.Text) && utf8.RuneCountInString(record.Text) <= maxMessageRunes && len(record.Text) <= maxMessageBytes
}

func validMessage(message Message) bool {
	return validHash(message.DedupeKey) && validHash(message.RouteKey) && message.Text != "" && utf8.ValidString(message.Text) && utf8.RuneCountInString(message.Text) <= maxMessageRunes && len(message.Text) <= maxMessageBytes
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == hex.EncodeToString(decoded)
}

func validFailure(failure Failure) bool {
	switch failure {
	case FailureTransport, FailureUnavailable, FailureRateLimited, FailureRejected, FailureInvalidRoute:
		return true
	default:
		return false
	}
}

func eligible(record Record, now time.Time) bool {
	if record.AdmissionPending {
		return false
	}
	switch record.State {
	case StatePending:
		return true
	case StateRetry:
		return !record.NextAttempt.After(now)
	case StateInFlight:
		return !record.LeaseUntil.After(now)
	default:
		return false
	}
}

func messageHash(message Message) string {
	sum := sha256.Sum256([]byte(message.RouteKey + "\x00" + message.Text))
	return hex.EncodeToString(sum[:])
}

func recordName(dedupeKey string) string {
	return "sentry-telegram-delivery-" + dedupeKey
}

func admissionName(dedupeKey string) string {
	return "sentry-telegram-admission-" + dedupeKey
}

func (q *Queue) validate() error {
	if q == nil || q.Client == nil || q.Namespace == "" || q.MaxOutstanding <= 0 || q.MaxAttempts <= 0 || q.DedupeRetention <= 0 || q.DLQRetention <= 0 {
		return ErrInvalid
	}
	return nil
}

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now().UTC().Round(0)
	}
	return time.Now().UTC().Round(0)
}
