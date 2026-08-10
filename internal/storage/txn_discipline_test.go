// Transaction-discipline regressions: an owner-gated {slug} write must re-check
// the paste identity INSIDE the transaction that acts, and every fail-open path
// must survive envelope-level damage, not just JSON damage.

//go:build !slatedb

package storage_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/durable"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/shale/pkg/cluster"
)

// truncatedEnvelope returns a magic-prefixed LWW envelope cut short in its
// header: the R>1 damage shape cluster.Decode rejects (distinct from a decodable
// value carrying invalid JSON).
func truncatedEnvelope() []byte {
	full := cluster.Encode(cluster.Envelope{
		Stamp:   cluster.Stamp{TimestampNanos: 1, NodeID: "n"},
		Payload: []byte("payload"),
	})
	return full[:5] // magic + 4 header bytes; short of the 11-byte minimum
}

// A whole-paste Delete must refuse when a delete+re-mint of the slug by ANOTHER
// identity commits between the caller's ownership read and the {slug}
// transaction. The re-mint is injected at the version scan, which Delete runs
// after its out-of-tx head read and before the authoritative transaction, so
// this exercises the IN-TX identity guard specifically: the out-of-tx pre-check
// already saw the old owner and passed.
func TestShaleDelete_InTxGuardRefusesReMintByAnotherIdentity(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()
	slug := domain.Slug("remint01")
	a := pasteOf(slug.String(), "key:remintA", 100)
	insert(t, repo, a)

	var once sync.Once
	repo.SetVersionScanHookForTest(func(s domain.Slug) {
		if s != slug {
			return
		}
		once.Do(func() {
			repo.SetVersionScanHookForTest(nil) // no re-entrancy from the nested delete
			if err := repo.Delete(slug, a.Identity, a.CreatedAt); err != nil {
				t.Errorf("re-mint setup: delete A: %v", err)
			}
			b := pasteOf(slug.String(), "key:remintB", 200)
			if err := repo.InsertWithQuotaCheck(ctx, b, 0, fixedNow); err != nil {
				t.Errorf("re-mint setup: insert B: %v", err)
			}
			repo.WaitPendingConfirms()
		})
	})

	// A's now-stale delete: the head belongs to B, so it must abort and leave B.
	err := repo.Delete(slug, a.Identity, a.CreatedAt)
	if !errors.Is(err, storage.ErrConcurrentChange) {
		t.Fatalf("stale delete over a re-minted slug: got %v, want ErrConcurrentChange", err)
	}
	got, gerr := repo.Get(slug)
	if gerr != nil {
		t.Fatalf("B's paste must survive A's stale delete: %v", gerr)
	}
	if got.Identity.String() != "key:remintB" {
		t.Fatalf("head owner after refused delete = %q, want key:remintB", got.Identity)
	}
	list, err := repo.ListByOwner("key:remintB")
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(list) != 1 || list[0].Slug != slug {
		t.Fatalf("B's enumeration entry must survive, got %+v", list)
	}
}

// SetName must refuse when the slug was deleted and re-minted by another
// identity since the caller's ownership read: the head rename re-checks identity
// inside its {slug} transaction (the only guard SetName has).
func TestShaleSetName_RefusesReMintByAnotherIdentity(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()
	slug := domain.Slug("rename01")
	a := pasteOf(slug.String(), "key:renameA", 100)
	insert(t, repo, a)

	if err := repo.Delete(slug, a.Identity, a.CreatedAt); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	b := pasteOf(slug.String(), "key:renameB", 200)
	if err := repo.InsertWithQuotaCheck(ctx, b, 0, fixedNow); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	repo.WaitPendingConfirms()

	err := repo.SetName(slug, "hijacked", a.Identity, a.CreatedAt)
	if !errors.Is(err, storage.ErrConcurrentChange) {
		t.Fatalf("stale rename over a re-minted slug: got %v, want ErrConcurrentChange", err)
	}
	got, _ := repo.Get(slug)
	if got.Name != "" {
		t.Fatalf("B's head was relabelled by A's stale rename: name=%q", got.Name)
	}
	list, _ := repo.ListByOwner("key:renameB")
	if len(list) != 1 || list[0].Name != "" {
		t.Fatalf("B's entry was relabelled by A's stale rename: %+v", list)
	}
}

// The keygate admission path is documented fail-open. A truncated-envelope
// keygate row (a damaged sibling AND the caller's own row) must not turn an
// admission into an error.
func TestShaleKeygate_AdmitToleratesTruncatedEnvelope(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Now().UTC()
	subnet := "203.0.113.0/24"
	window := time.Hour

	dmg := storage.KeygateKeyForTest(subnet, "key:dmgkg")
	if err := repo.PutRawForTest(dmg, truncatedEnvelope()); err != nil {
		t.Fatalf("plant damaged keygate row: %v", err)
	}
	// A different identity must still be admitted: the tolerant accounting scan
	// skips the damaged sibling instead of failing.
	known, err := repo.AdmitNewKey("key:freshkg", subnet, now, 100, window)
	if err != nil {
		t.Fatalf("admission must survive a damaged sibling row: %v", err)
	}
	if known {
		t.Fatalf("fresh key should admit as new, not known")
	}
	// The damaged OWN row must fall through the fast path to admission, not error.
	if _, err := repo.AdmitNewKey("key:dmgkg", subnet, now, 100, window); err != nil {
		t.Fatalf("a damaged own row must fail open, not error: %v", err)
	}
}

// The intent resolve-on-read listing is documented fail-open. A
// truncated-envelope intent row must be skipped, not fail the whole listing.
func TestShaleOutstanding_ToleratesTruncatedEnvelope(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()
	now := time.Now().UTC()
	log := repo.IntentLogForTest()
	scope := durable.Scope("key:intentdmg")

	good := durable.Intent{
		ID: "goodintent", Kind: durable.KindCreatePaste, Scope: scope,
		Subject: "goodslug", StartedAt: now,
	}
	if err := log.Begin(ctx, good); err != nil {
		t.Fatalf("begin good intent: %v", err)
	}
	bad := storage.IntentKeyForTest(string(scope), "badintent")
	if err := repo.PutRawForTest(bad, truncatedEnvelope()); err != nil {
		t.Fatalf("plant damaged intent: %v", err)
	}

	out, err := log.Outstanding(ctx, scope)
	if err != nil {
		t.Fatalf("Outstanding must fail open over envelope damage: %v", err)
	}
	if len(out) != 1 || out[0].ID != "goodintent" {
		t.Fatalf("want just the good intent, got %+v", out)
	}
}
