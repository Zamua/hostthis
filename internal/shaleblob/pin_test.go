//go:build slatedb

// Deterministic, single-node reproduction of the version-pinning serve bug.
//
// The public read path (internal/http/server.go) resolves the bytes to serve
// by the PASTE HEAD's ContentSHA: it does `p := Pastes.Get(slug)` then
// `Blobs.Read(slug, p.ContentSHA)`. Under value-separation that Read maps the
// sha -> a BlobID via ResolveBlobID, whose first branch returns the paste
// head's own BlobID whenever the head's ContentSHA equals the requested sha
// (always true for the serving path, which passes the head's own sha).
//
// SetPinnedVersion / Unpin update the head's ContentSHA/Size/Kind but NOT its
// BlobID (and domain.Version carries no BlobID to copy from), so after pinning
// an OLDER version the head ends up {ContentSHA: pinned-sha, BlobID: stale
// pre-pin-head blob}. The serving Read then streams the stale head blob while
// the ETag (= ContentSHA) reflects the pinned version: the page serves the
// wrong bytes. This was inert before the blob value-separation cutover because
// the standalone disk store keyed bytes by content-sha, not by a BlobID.
//
// This test fails until SetPinnedVersion/Unpin repoint the head BlobID at the
// selected version's blob. It needs only a single-node R=1 durable metadata
// cluster + the in-memory blob plane: no replication, relaxed durability, or
// timing is involved, which is the point - the bug is plain missing-field
// logic, not a consistency window.

package shaleblob_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

func TestPin_ServesPinnedVersionBytes(t *testing.T) {
	repo, unit, _ := newBlobRepo(t)
	ctx := context.Background()
	now := time.Now().UTC()
	slug := "pinblob1"

	// v1: insert + bind its blob.
	rawV1 := []byte("<h1>PIN-V1-original-bytes</h1>")
	shaV1 := "sha-pinblob-v1"
	bodyV1 := encode(t, rawV1)
	h1, err := unit.Stage(ctx, slug, shaV1, bodyV1)
	if err != nil {
		t.Fatalf("Stage v1: %v", err)
	}
	p := mkPaste(slug, "owner-pin", shaV1, len(bodyV1), now)
	if err := unit.Commit(ctx, []service.BlobHandle{h1}, func(ctx context.Context) error {
		return repo.InsertWithQuotaCheck(ctx, p, int64(domain.UserQuotaBytes), now)
	}); err != nil {
		t.Fatalf("Commit v1: %v", err)
	}

	// v2: append a DIFFERENT blob; the unpinned head rolls forward to it.
	rawV2 := []byte("<h1>PIN-V2-newer-different-bytes</h1>")
	shaV2 := "sha-pinblob-v2"
	bodyV2 := encode(t, rawV2)
	h2, err := unit.Stage(ctx, slug, shaV2, bodyV2)
	if err != nil {
		t.Fatalf("Stage v2: %v", err)
	}
	if err := unit.Commit(ctx, []service.BlobHandle{h2}, func(ctx context.Context) error {
		_, aerr := repo.AppendVersionWithQuotaCheck(ctx, domain.Slug(slug), domain.KindHTML, shaV2, len(bodyV2), int64(domain.UserQuotaBytes), now)
		return aerr
	}); err != nil {
		t.Fatalf("Commit v2: %v", err)
	}

	// Sanity: the unpinned head serves v2's bytes (this already works).
	if out, _ := readAll(t, unit, slug, mustHeadSHA(t, repo, slug)); !bytes.Equal(out, rawV2) {
		t.Fatalf("unpinned head served %q, want v2 %q", out, rawV2)
	}

	// Pin v1. The public URL must now serve v1's BYTES.
	v1, err := repo.GetVersion(domain.Slug(slug), 1)
	if err != nil {
		t.Fatalf("GetVersion v1: %v", err)
	}
	if err := repo.SetPinnedVersion(domain.Slug(slug), v1); err != nil {
		t.Fatalf("SetPinnedVersion v1: %v", err)
	}

	head, err := repo.Get(domain.Slug(slug))
	if err != nil {
		t.Fatalf("Get after pin: %v", err)
	}
	// The metadata is correct: the pinned head reports v1's content-sha (this is
	// what the ETag is built from, and it matched v1 in the prod measurement).
	if head.ContentSHA != shaV1 {
		t.Fatalf("pinned head ContentSHA = %q, want %q (v1)", head.ContentSHA, shaV1)
	}
	// The BYTES served (resolved by the head's ContentSHA, exactly as the public
	// read path does) must be v1's. The bug: they are v2's, because the head's
	// BlobID was left pointing at v2's blob.
	out, rerr := readAll(t, unit, slug, head.ContentSHA)
	if rerr != nil {
		t.Fatalf("read pinned head: %v", rerr)
	}
	if !bytes.Equal(out, rawV1) {
		t.Fatalf("pinned paste served the WRONG blob:\n got  = %q\n want = %q (v1)\nThe head's ContentSHA is v1 but its BlobID still points at v2's blob (SetPinnedVersion does not repoint BlobID).", out, rawV1)
	}

	// Unpin: the head must roll forward to v2's bytes again.
	if err := repo.Unpin(domain.Slug(slug)); err != nil {
		t.Fatalf("Unpin: %v", err)
	}
	head2, err := repo.Get(domain.Slug(slug))
	if err != nil {
		t.Fatalf("Get after unpin: %v", err)
	}
	if head2.ContentSHA != shaV2 {
		t.Fatalf("unpinned head ContentSHA = %q, want %q (v2)", head2.ContentSHA, shaV2)
	}
	out2, rerr2 := readAll(t, unit, slug, head2.ContentSHA)
	if rerr2 != nil {
		t.Fatalf("read after unpin: %v", rerr2)
	}
	if !bytes.Equal(out2, rawV2) {
		t.Fatalf("after unpin served the WRONG blob:\n got  = %q\n want = %q (v2)", out2, rawV2)
	}
}

// mustHeadSHA returns the current paste-head ContentSHA.
func mustHeadSHA(t *testing.T, repo *storage.ShaleRepo, slug string) string {
	t.Helper()
	p, err := repo.Get(domain.Slug(slug))
	if err != nil {
		t.Fatalf("Get(%s): %v", slug, err)
	}
	return p.ContentSHA
}
