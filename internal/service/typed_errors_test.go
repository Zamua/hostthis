package service

// The behavioral CONSEQUENCE of sentinel classification (remint vs surface,
// Sybil refusal vs generic error), pinned through the service entry points: a
// text lookalike must not classify, and the real sentinel must classify bare
// and %w-wrapped. The boolean table itself is TestClassifyCommitErr.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// slugLookalikeErrRepo fails the first insert with an error whose text
// contains "slug" but which is NOT the slug-taken sentinel, and accepts any
// later one, so a remint would turn the failure into a success. The embedded
// nil PasteRepo makes any other call panic.
type slugLookalikeErrRepo struct {
	PasteRepo
	failed bool
}

var errSlugLookalike = fmt.Errorf("room slug validation failed")

func (r *slugLookalikeErrRepo) InsertWithQuotaCheck(context.Context, domain.Paste, int64, time.Time) error {
	if r.failed {
		return nil
	}
	r.failed = true
	return errSlugLookalike
}

// TestUpload_Create_LookalikeErrorIsNotARemint pins the classification
// CONSEQUENCE: an insert failure whose text contains "slug" but is not the
// sentinel surfaces VERBATIM from Create instead of being retried under a new
// slug, which would hide the real failure.
func TestUpload_Create_LookalikeErrorIsNotARemint(t *testing.T) {
	repo := &slugLookalikeErrRepo{}
	u := NewUpload(repo, fakeBlobUnit())

	_, err := u.Create(bytes.NewReader([]byte("# body")), ownerOf("key:owner"), "", "")
	if !errors.Is(err, errSlugLookalike) {
		t.Fatalf("Create = %v, want the repo's own error surfaced verbatim", err)
	}
	if errors.Is(err, ErrSlugTaken) {
		t.Fatalf("Create = %v; a non-collision error must not exhaust the remint budget", err)
	}
}

// fakeKeyGateRepo returns a canned AdmitNewKey error and a fixed subnet
// snapshot. The embedded nil KeyGateRepo makes any other call panic.
type fakeKeyGateRepo struct {
	KeyGateRepo
	admitErr error
}

var fakeOldest = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func (f *fakeKeyGateRepo) AdmitNewKey(string, string, time.Time, int, time.Duration) (bool, error) {
	return false, f.admitErr
}
func (f *fakeKeyGateRepo) SubnetSnapshot(string, time.Time, time.Duration) (int, time.Time, error) {
	return 3, fakeOldest, nil
}

// TestKeyGateAdmit_RateLimitClassification pins what Admit DOES with the
// repo error, per class:
//
//   - the rate-limit sentinel (bare or wrapped) -> a *SybilRefusal
//     (errors.Is ErrSybilRateLimit) enriched from SubnetSnapshot, so the
//     SSH layer prints the self-diagnosing cap/next-slot message;
//   - any other repo error -> surfaced verbatim, NOT a Sybil refusal.
//
// The wrapped-sentinel row matters: a comparison against the sentinel's exact
// text would demote any backend-wrapped rate-limit to a generic 500-class error.
func TestKeyGateAdmit_RateLimitClassification(t *testing.T) {
	cases := []struct {
		name        string
		admitErr    error
		wantRefusal bool
	}{
		{"bare sentinel", storage.ErrTooManyNewKeys, true},
		{"wrapped sentinel", fmt.Errorf("keygate admit: %w", storage.ErrTooManyNewKeys), true},
		{"generic error passes through", errors.New("storage: disk on fire"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeKeyGateRepo{admitErr: tc.admitErr}
			g := NewKeyGate(repo)
			err := g.Admit(ownerOf("key:fp"), "1.2.3.0/24")
			if err == nil {
				t.Fatalf("Admit = nil, want an error")
			}
			if got := errors.Is(err, ErrSybilRateLimit); got != tc.wantRefusal {
				t.Fatalf("errors.Is(err, ErrSybilRateLimit) = %v, want %v (err = %v)", got, tc.wantRefusal, err)
			}
			if tc.wantRefusal {
				var refusal *SybilRefusal
				if !errors.As(err, &refusal) {
					t.Fatalf("Admit = %T (%v), want *SybilRefusal", err, err)
				}
				if refusal.FreshCountInWindow != 3 || !refusal.OldestEntryFirstSeen.Equal(fakeOldest) {
					t.Fatalf("refusal not enriched from snapshot: %+v", refusal)
				}
			} else if !errors.Is(err, tc.admitErr) {
				t.Fatalf("Admit = %v, want the repo error surfaced verbatim", err)
			}
		})
	}
}
