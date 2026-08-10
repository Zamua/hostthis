package storage_test

// Envelope damage on a legacy VERSION ROW. The document already tolerates a
// truncated envelope; the rows the heal walk and the delete cascade SCAN have
// to as well, because the strip happens one layer below the JSON decode those
// two paths skip on - so a strict scan fails before any per-record decision and
// puts the paste's whole write surface, and its delete, behind one value.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// stageDamagedVersionRow builds a three-version paste in the PRE-MIGRATION
// shape (no document, so every mutation heals from the rows) and truncates v2's
// row envelope.
func stageDamagedVersionRow(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, owner string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-venvrow-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-venvrow-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	deleteVersionsDoc(t, repo, slug)
	if err := repo.PutRawForTest(storage.LegacyVersionKeyForTest(slug, 2), truncatedEnvelope); err != nil {
		t.Fatalf("truncate v2's row envelope: %v", err)
	}
}

func TestVersionsDoc_EnvelopeDamagedRowDoesNotBrickTheWriteSurface(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("append heals around it", func(t *testing.T) {
		repo := newShaleRepoForTest(t)
		slug := domain.Slug("venvrw01")
		stageDamagedVersionRow(t, repo, slug, "key:venvrow1", now)

		res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-venvrow-v4", 50, 0, now)
		if err != nil {
			t.Fatalf("append over an envelope-damaged row must heal around it, not fail: %v", err)
		}
		if res.NewVer != 4 {
			t.Fatalf("numbering must reserve the unreadable row's number: got v%d, want v4", res.NewVer)
		}
		nums, highWater, complete := versionsDocFields(t, repo, slug)
		if !reflect.DeepEqual(nums, []int{1, 3, 4}) || highWater != 4 {
			t.Fatalf("healed document: versions %v high_water %d", nums, highWater)
		}
		if complete {
			t.Fatal("a document missing the skipped row must be INCOMPLETE")
		}
		// The row the walk could not read is left exactly as it is.
		if rows := localCount(t, repo, "versions/"+slug.String()+"/"); rows != 4 {
			t.Fatalf("want 4 version rows (the damaged one untouched), got %d", rows)
		}
	})

	t.Run("the per-version delete still tombstones", func(t *testing.T) {
		repo := newShaleRepoForTest(t)
		slug := domain.Slug("venvrw02")
		stageDamagedVersionRow(t, repo, slug, "key:venvrow2", now)

		// v1 is the unserved readable version; the head serves v3.
		if err := repo.DeleteVersion(slug, 1); err != nil {
			t.Fatalf("tombstoning a readable version must not be behind another row's damage: %v", err)
		}
		v1, err := repo.GetVersion(slug, 1)
		if err != nil || !v1.Deleted {
			t.Fatalf("v1 must read as tombstoned: %+v err=%v", v1, err)
		}
		if v3, err := repo.GetVersion(slug, 3); err != nil || v3.Deleted {
			t.Fatalf("v3 must stay live: %+v err=%v", v3, err)
		}
	})

	t.Run("the whole-paste delete removes the damaged row too", func(t *testing.T) {
		repo := newShaleRepoForTest(t)
		slug := domain.Slug("venvrw03")
		stageDamagedVersionRow(t, repo, slug, "key:venvrow3", now)

		logs := captureRepoLog(t)
		if err := repo.Delete(slug); err != nil {
			t.Fatalf("a delete must never be blocked by the value it would remove: %v", err)
		}
		if _, err := repo.Get(slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("the paste survived the delete: %v", err)
		}
		if rows := localCount(t, repo, "versions/"+slug.String()+"/"); rows != 0 {
			t.Fatalf("%d version row(s) survived the delete", rows)
		}
		// No record names the damaged row's blob, so the number is reported and
		// its bytes stay bound: a byte leak, not an undeletable paste.
		if n := countLogLines(logs, "whole-paste delete of "+slug.String(), "unreadable version row", "[2]"); n != 1 {
			t.Fatalf("want one line naming version 2, got %d:\n%s", n, logs.String())
		}
	})
}
