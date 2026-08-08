package storage_test

import (
	"context"
	"testing"

	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
)

// A staged ref survives the round-trip BYTE-EXACT.
//
// This is the sharp edge of the whole unstaging design. shale derives both the
// bound-ref guard read and the object delete key from the ref's fields, so a
// field this round-trip loses or corrupts does not fail loudly: it addresses a
// DIFFERENT key than BindBlob wrote, reads as "unbound" for a blob that is
// bound, and deletes committed bytes - discovered only when someone reads the
// paste. shale's validation catches a MISSING key-forming field; a
// corrupted-but-present RouteShard it cannot.
//
// RouteShard is the field to watch: it is []byte, so it is the one that a
// hand-rolled encoder would mangle silently.
func TestStagedRefs_RoundTripIsByteExact(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()
	const scope = "key:staged-owner"
	slug := domain.Slug("stagd123")

	want := cluster.BlobRef{
		Unit:       "0-13",
		RouteShard: []byte{0x00, 0x7f, 0xff, 0x2f, 0x00, 0xfe}, // NULs, a slash, high bytes
		BlobID:     "0193f2c1-8a4b-7c3d-9e1f-2a3b4c5d6e7f",
		Size:       4096,
	}
	if err := repo.RecordStagedRef(ctx, scope, slug, want); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := repo.StagedRefs(scope, slug)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read back %d refs, want 1", len(got))
	}
	if got[0].Unit != want.Unit {
		t.Errorf("Unit: got %q, want %q", got[0].Unit, want.Unit)
	}
	if got[0].BlobID != want.BlobID {
		t.Errorf("BlobID: got %q, want %q", got[0].BlobID, want.BlobID)
	}
	if got[0].Size != want.Size {
		t.Errorf("Size: got %d, want %d", got[0].Size, want.Size)
	}
	// The one that matters most, compared byte by byte rather than by length.
	if string(got[0].RouteShard) != string(want.RouteShard) {
		t.Errorf("RouteShard did NOT round-trip: got %#v, want %#v.\n"+
			"A route shard that changes shape addresses a different bref than BindBlob wrote, "+
			"so the guard reads 'unbound' for a bound blob and unstaging deletes committed bytes.",
			got[0].RouteShard, want.RouteShard)
	}
}

// Records are per-upload and per-owner: one upload's list never contains
// another's, or recovery for one paste would delete a different paste's bytes.
func TestStagedRefs_ScopedToTheirUpload(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()

	mine := cluster.BlobRef{Unit: "0-1", RouteShard: []byte("a"), BlobID: "blob-mine", Size: 1}
	otherSlug := cluster.BlobRef{Unit: "0-1", RouteShard: []byte("a"), BlobID: "blob-other-slug", Size: 1}
	otherOwner := cluster.BlobRef{Unit: "0-1", RouteShard: []byte("a"), BlobID: "blob-other-owner", Size: 1}

	if err := repo.RecordStagedRef(ctx, "key:a", "aaaa1111", mine); err != nil {
		t.Fatalf("record mine: %v", err)
	}
	if err := repo.RecordStagedRef(ctx, "key:a", "bbbb2222", otherSlug); err != nil {
		t.Fatalf("record other slug: %v", err)
	}
	if err := repo.RecordStagedRef(ctx, "key:b", "aaaa1111", otherOwner); err != nil {
		t.Fatalf("record other owner: %v", err)
	}

	got, err := repo.StagedRefs("key:a", "aaaa1111")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 1 || got[0].BlobID != "blob-mine" {
		t.Fatalf("got %+v, want exactly [blob-mine]: a list that reaches past its own upload "+
			"makes recovery delete bytes belonging to a different paste", got)
	}
}

// Clearing is what a COMMIT does: the bytes are bound now, and a record that
// outlives its commit is a standing instruction to delete live data.
func TestStagedRefs_ClearedOnCommit(t *testing.T) {
	repo := newShaleRepoForTest(t)
	ctx := context.Background()
	const scope = "key:clear"
	slug := domain.Slug("clear123")

	for _, id := range []string{"b1", "b2", "b3"} {
		if err := repo.RecordStagedRef(ctx, scope, slug, cluster.BlobRef{
			Unit: "0-2", RouteShard: []byte("r"), BlobID: id, Size: 10,
		}); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	if got, _ := repo.StagedRefs(scope, slug); len(got) != 3 {
		t.Fatalf("fixture: recorded %d refs, want 3", len(got))
	}

	if err := repo.ClearStagedRefs(ctx, scope, slug); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, err := repo.StagedRefs(scope, slug)
	if err != nil {
		t.Fatalf("read after clear: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("after clear: %d refs remain, want 0. A record outliving its commit "+
			"tells recovery to delete bytes a committed paste is using", len(got))
	}

	// Idempotent: a partial clear must converge, not fail.
	if err := repo.ClearStagedRefs(ctx, scope, slug); err != nil {
		t.Fatalf("second clear must be a no-op, got %v", err)
	}
}
