//go:build slatedb

// Concurrency regression for per-call blob-bind isolation.
//
// A Commit's staged refs ride that call's context.Context, never a shared
// slug-keyed stash. Two concurrent same-slug writes sharing one stash entry
// could bind each other's blob mid-metaWrite, or have a deferred clear wipe the
// entry during a CAS retry and commit a version with NO bind, leaving an
// orphaned blob and an unreadable row.
//
// This drives that race: two parallel AppendVersion Commits on ONE slug, each
// staging a DISTINCT blob. A regression surfaces as a version resolving to the
// wrong content, a Read 404, or an empty BlobID.

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

// TestConcurrentSameSlugBindsOwnBlob pins that two parallel AppendVersion
// Commits on one slug each bind their OWN blob.
func TestConcurrentSameSlugBindsOwnBlob(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	const slug = "concslug"

	// Seed the paste so the slug exists and AppendVersion has a head to extend.
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

	// Two concurrent appends on the SAME slug, each with DISTINCT content.
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
			// Each goroutine's refs ride its own Commit's context.
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

	// Every version, seed included, reads back THEIR OWN bytes via its content
	// sha -> bound blob id.
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

	// Every version row carries a non-empty blob id: a clear-during-retry
	// commits a version with NO bind, which shows up as an empty BlobID here.
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

	// DISTINCT ids: each Commit minted and bound its own staged blob.
	idA, _ := repo.ResolveBlobID(domain.Slug(slug), want[0].sha)
	idB, _ := repo.ResolveBlobID(domain.Slug(slug), want[1].sha)
	if idA == idB {
		t.Fatalf("concurrent appends bound the SAME blob id %q (the shared-stash race aliased their refs)", idA)
	}
}
