package service

// Pins for the typed error detectors (isSlugTaken, isStorageRateLimitErr,
// isCrossShard). Each detector gets BOTH directions pinned:
//
//   - a text LOOKALIKE (an unrelated error whose message contains the substring
//     a text match would key on) must NOT classify;
//   - the real sentinel, bare AND %w-wrapped, MUST classify: identity survives
//     any wrapping the storage layers add.
//
// Where the classification has a behavioral consequence (remint vs surface,
// Sybil refusal vs generic error, deploy-failed translation vs verbatim), the
// consequence is pinned through the service entry point, not just the boolean.

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

// --- 2a: isSlugTaken -------------------------------------------------------

func TestIsSlugTaken_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Unrelated error text containing "slug" must not read as a collision:
		// a remint would silently swallow the real error and burn the retry
		// budget.
		{"text lookalike does not misfire", fmt.Errorf("room slug validation failed"), false},
		{"nil", nil, false},
		{"unrelated error", errors.New("disk on fire"), false},
		{"bare sentinel", storage.ErrSlugTaken, true},
		{"wrapped sentinel", fmt.Errorf("insert: %w", storage.ErrSlugTaken), true},
		{"domain name for the same sentinel", fmt.Errorf("preclaim: %w", domain.ErrSlugTaken), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSlugTaken(tc.err); got != tc.want {
				t.Fatalf("isSlugTaken(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// slugLookalikeErrRepo fails every insert with an error whose text
// contains "slug" but which is NOT the slug-taken sentinel.
type slugLookalikeErrRepo struct {
	calls int
}

var errSlugLookalike = fmt.Errorf("room slug validation failed")

func (r *slugLookalikeErrRepo) InsertWithQuotaCheck(context.Context, domain.Paste, int64, time.Time) error {
	r.calls++
	return errSlugLookalike
}
func (r *slugLookalikeErrRepo) Get(domain.Slug) (domain.Paste, error) {
	return domain.Paste{}, storage.ErrNotFound
}
func (r *slugLookalikeErrRepo) MarkReady(domain.Paste) error  { return nil }
func (r *slugLookalikeErrRepo) MarkFailed(domain.Paste) error { return nil }

// TestUpload_Create_LookalikeErrorIsNotARemint pins the classification
// CONSEQUENCE: an insert failure whose text contains "slug" but is not the
// sentinel surfaces VERBATIM from Create, after exactly one attempt. Classified
// as a collision it would instead burn the whole remint budget and report a
// bogus "slug taken (after retries)", hiding the real failure.
func TestUpload_Create_LookalikeErrorIsNotARemint(t *testing.T) {
	repo := &slugLookalikeErrRepo{}
	u := NewUpload(repo, NewStandaloneBlobUnit(newFakeBlobs()))

	_, err := u.Create(bytes.NewReader([]byte("# body")), "key:owner", "", "")
	if !errors.Is(err, errSlugLookalike) {
		t.Fatalf("Create = %v, want the repo's own error surfaced verbatim", err)
	}
	if errors.Is(err, ErrSlugTaken) {
		t.Fatalf("Create = %v; a non-collision error must not exhaust the remint budget", err)
	}
	if repo.calls != 1 {
		t.Fatalf("insert attempts = %d, want 1 (no remint on a non-collision error)", repo.calls)
	}
}

// --- 2b: isStorageRateLimitErr ---------------------------------------------

// fakeKeyGateRepo returns a canned AdmitNewKey error and records whether
// the refusal-enrichment snapshot was consulted.
type fakeKeyGateRepo struct {
	admitErr      error
	snapshotCalls int
}

var fakeOldest = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func (f *fakeKeyGateRepo) AdmitNewKey(string, string, time.Time, int, time.Duration) (bool, error) {
	return false, f.admitErr
}
func (f *fakeKeyGateRepo) DeleteFirstSeenOlderThan(time.Time) (int, error) { return 0, nil }
func (f *fakeKeyGateRepo) SubnetSnapshot(string, time.Time, time.Duration) (int, time.Time, error) {
	f.snapshotCalls++
	return 3, fakeOldest, nil
}
func (f *fakeKeyGateRepo) SubnetsForIdentity(string, time.Time, time.Duration) (int, error) {
	return 0, nil
}

// TestKeyGateAdmit_RateLimitClassification pins what Admit DOES with the
// repo error, per class:
//
//   - the rate-limit sentinel (bare or wrapped) -> a *SybilRefusal
//     (errors.Is ErrSybilRateLimit) enriched from SubnetSnapshot, so the
//     SSH layer prints the self-diagnosing cap/next-slot message;
//   - any other repo error -> surfaced verbatim, NOT a Sybil refusal,
//     and the snapshot is never consulted.
//
// The wrapped-sentinel row matters: a comparison against the sentinel's exact
// text would demote any backend-wrapped rate-limit to a generic 500-class error.
func TestKeyGateAdmit_RateLimitClassification(t *testing.T) {
	cases := []struct {
		name          string
		admitErr      error
		wantRefusal   bool
		wantSnapshots int
	}{
		{"bare sentinel", storage.ErrTooManyNewKeys, true, 1},
		{"wrapped sentinel", fmt.Errorf("keygate admit: %w", storage.ErrTooManyNewKeys), true, 1},
		{"generic error passes through", errors.New("storage: disk on fire"), false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeKeyGateRepo{admitErr: tc.admitErr}
			g := NewKeyGate(repo)
			err := g.Admit("key:fp", "1.2.3.0/24")
			if err == nil {
				t.Fatalf("Admit = nil, want an error")
			}
			if got := errors.Is(err, ErrSybilRateLimit); got != tc.wantRefusal {
				t.Fatalf("errors.Is(err, ErrSybilRateLimit) = %v, want %v (err = %v)", got, tc.wantRefusal, err)
			}
			if repo.snapshotCalls != tc.wantSnapshots {
				t.Fatalf("SubnetSnapshot calls = %d, want %d", repo.snapshotCalls, tc.wantSnapshots)
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
