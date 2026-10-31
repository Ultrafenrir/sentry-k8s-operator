package ownership

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestValidateMutation(t *testing.T) {
	local := Local{Marker: Marker{Region: "eu-west-1", Revision: 8, Owner: "payments", Installation: "cluster-a"}, Active: true}
	if err := ValidateMutation(local, local.Marker); err != nil {
		t.Fatalf("exact marker: %v", err)
	}
	for name, test := range map[string]struct {
		remote Marker
		want   error
	}{
		"larger revision":             {Marker{Region: "us-east-1", Revision: 9, Owner: "payments", Installation: "cluster-b"}, ErrFenced},
		"smaller revision":            {Marker{Region: "eu-west-1", Revision: 7, Owner: "payments", Installation: "cluster-a"}, ErrFenced},
		"equal region conflict":       {Marker{Region: "us-east-1", Revision: 8, Owner: "payments", Installation: "cluster-a"}, ErrConflict},
		"equal installation conflict": {Marker{Region: "eu-west-1", Revision: 8, Owner: "payments", Installation: "cluster-b"}, ErrConflict},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateMutation(local, test.remote); !errors.Is(err, test.want) {
				t.Fatalf("error=%v, want %v", err, test.want)
			}
		})
	}
}

func TestCompactEvidenceBindsCompleteMarkerAndClassifiesRevision(t *testing.T) {
	t.Parallel()
	local := Local{Marker: Marker{Region: "eu-west-1", Revision: 35, Owner: "payments", Installation: "cluster-a"}, Active: true}
	evidence, err := EvidenceFor(local.Marker)
	if err != nil || evidence.Revision != 35 || len(evidence.Fingerprint) != evidenceFingerprintBytes*2 {
		t.Fatalf("evidence=%#v err=%v", evidence, err)
	}
	if err := ValidateEvidence(local, evidence); err != nil {
		t.Fatalf("exact evidence: %v", err)
	}
	stale := evidence
	stale.Revision--
	if err := ValidateEvidence(local, stale); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale evidence error=%v", err)
	}
	conflict := evidence
	conflict.Fingerprint = "000000000000000000000000"
	if err := ValidateEvidence(local, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting evidence error=%v", err)
	}
	other := local.Marker
	other.Region = "us-east-1"
	otherEvidence, err := EvidenceFor(other)
	if err != nil || otherEvidence.Fingerprint == evidence.Fingerprint {
		t.Fatalf("marker fields were not bound: first=%#v other=%#v err=%v", evidence, otherEvidence, err)
	}
}

type fakeClock struct{ calls int }

func (c *fakeClock) Sleep(context.Context, time.Duration) error { c.calls++; return nil }

type fakeStore struct {
	reads []Marker
	write Marker
}

func (s *fakeStore) Read(context.Context) (Marker, error) {
	value := s.reads[0]
	s.reads = s.reads[1:]
	return value, nil
}
func (s *fakeStore) Write(_ context.Context, marker Marker) error { s.write = marker; return nil }

func TestTakeOverUsesFencingDelayAndReadAfterWrite(t *testing.T) {
	local := Local{Marker: Marker{Region: "eu-west-1", Revision: 8, Owner: "payments", Installation: "cluster-a"}, Active: true}
	store := &fakeStore{reads: []Marker{{Region: "us-east-1", Revision: 7}, local.Marker}}
	clock := &fakeClock{}
	if err := TakeOver(context.Background(), store, clock, 5*time.Second, local); err != nil {
		t.Fatalf("TakeOver: %v", err)
	}
	if clock.calls != 1 || store.write != local.Marker {
		t.Fatalf("clock calls=%d write=%#v", clock.calls, store.write)
	}
}

func TestTakeOverFailsWhenMarkerChangesDuringDelay(t *testing.T) {
	local := Local{Marker: Marker{Region: "eu-west-1", Revision: 8, Owner: "payments", Installation: "cluster-a"}, Active: true}
	store := &fakeStore{reads: []Marker{{Region: "us-east-1", Revision: 7}, {Region: "us-east-1", Revision: 9}}}
	if err := TakeOver(context.Background(), store, &fakeClock{}, time.Second, local); !errors.Is(err, ErrChanged) {
		t.Fatalf("error=%v", err)
	}
}
