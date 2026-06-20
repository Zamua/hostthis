//go:build slatedb

// Concurrency regression for the per-call blob-bind isolation (P1-1).
//
// Before the fix, a ShaleBlobUnit.Commit stashed the staged refs in a per-repo
// map keyed by SLUG (StashBinds/popBinds). Two concurrent same-slug writes
// shared that one map entry, so goroutine B's stash could overwrite A's refs
// mid-metaWrite (A binds B's blob), or B's deferred ClearBinds could wipe the
// entry during A's CAS-retry (A commits with NO bind -> an orphaned blob + an
// unreadable row). The refs now ride the per-Commit context.Context, so each
// call binds its OWN blob with no shared mutable state.
//
// This test drives the exact race: two parallel AppendVersion Commits on ONE
// slug, each staging a DISTINCT blob, then asserts every version reads back its
// OWN bytes and carries a non-empty blob id. A regression (the shared map)
// shows up as a version resolving to the wrong content, a Read 404 (no bind),
// or an empty BlobID.

package shaleblob_test

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// TestConcurrentSameSlugBindsOwnBlob runs two parallel AppendVersion Commits on
// the same slug, each staging its own blob, and proves each version binds its
// own bytes (per-call isolation, no shared slug-keyed stash).
func TestConcurrentSameSlugBindsOwnBlob(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const slug = "concslug"

	// v1: seed the paste so the slug exists and AppendVersion has a head to
	// extend. Its own blob is bound here.
	rawV1 := []byte("<!doctype html><h1>v1 seed</h1>")
	shaV1 := "sha-conc-v1"
	bodyV1 := encode(t, rawV1)
	h1, err := unit.Stage(ctx, slug, shaV1, bodyV1)
	if err != nil {
		t.Fatalf("Stage v1: %v", err)
	}
	p := mkPaste(slug, "owner-conc", shaV1, len(bodyV1), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h1}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit v1: %v", err)
	}

	// Two concurrent appends on the SAME slug, each with DISTINCT content. With
	// the old slug-keyed stash these two Commits raced on one map entry; with
	// per-call context binds they are isolated.
	type appended struct {
		sha string
		raw []byte
	}
	const goroutines = 2
	want := []appended{
		{sha: "sha-conc-vA", raw: []byte("<!doctype html><h1>append A distinct</h1>")},
		{sha: "sha-conc-vB", raw: []byte("<!doctype html><h1>append B totally different body</h1>")},
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	start := make(chan struct{})
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a := want[i]
			body := encode(t, a.raw)
			// Each goroutine stages its own blob and commits the append under a
			// fresh Commit (its refs ride that Commit's context, not a shared
			// per-slug stash).
			h, serr := unit.Stage(ctx, slug, a.sha, body)
			if serr != nil {
				errs[i] = fmt.Errorf("stage %s: %w", a.sha, serr)
				return
			}
			<-start // release both goroutines together to maximize overlap
			errs[i] = unit.Commit(ctx, []service.BlobHandle{h}, func(ctx context.Context) error {
				_, aerr := repo.AppendVersionWithQuotaCheck(ctx, domain.Slug(slug), domain.KindHTML, a.sha, len(body), int64(domain.UserQuotaBytes), now)
				return aerr
			})
		}(i)
	}
	close(start)
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("concurrent append %d failed: %v", i, e)
		}
	}

	// Both appended versions plus the seed must each read back THEIR OWN bytes
	// (resolved via the version's content sha -> its bound blob id). A shared-
	// stash regression binds the wrong blob (wrong bytes) or none (Read 404).
	expectReads := append([]appended{{sha: shaV1, raw: rawV1}}, want...)
	for _, a := range expectReads {
		got, rerr := readAll(t, unit, slug, a.sha)
		if rerr != nil {
			t.Fatalf("read %s: %v (a missing bind = the shared-stash race)", a.sha, rerr)
		}
		if !bytes.Equal(got, a.raw) {
			t.Fatalf("read %s = %q, want %q (wrong blob bound = the shared-stash race)", a.sha, got, a.raw)
		}
	}

	// Every version row carries a non-empty blob id: a ClearBinds-during-retry
	// regression commits a version with NO bind, which would leave an empty
	// BlobID here (and the orphaned blob the Read check above already catches).
	versions, verr := repo.ListVersions(domain.Slug(slug))
	if verr != nil {
		t.Fatalf("ListVersions: %v", verr)
	}
	if len(versions) != goroutines+1 {
		t.Fatalf("version count = %d, want %d (v1 + %d concurrent appends)", len(versions), goroutines+1, goroutines)
	}
	for _, v := range versions {
		id, rerr := repo.ResolveBlobID(domain.Slug(slug), v.ContentSHA)
		if rerr != nil {
			t.Fatalf("ResolveBlobID(v%d sha=%s): %v", v.VerNum, v.ContentSHA, rerr)
		}
		if id == "" {
			t.Fatalf("version %d (sha %s) has empty blob id (a missing bind)", v.VerNum, v.ContentSHA)
		}
	}

	// Sanity: the two appended blobs got DISTINCT ids (no accidental id reuse),
	// confirming each Commit minted + bound its own staged blob.
	idA, _ := repo.ResolveBlobID(domain.Slug(slug), want[0].sha)
	idB, _ := repo.ResolveBlobID(domain.Slug(slug), want[1].sha)
	if idA == idB {
		t.Fatalf("concurrent appends bound the SAME blob id %q (the shared-stash race aliased their refs)", idA)
	}
}
