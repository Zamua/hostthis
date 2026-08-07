// Pins that the default build's storage engine actually serves ShaleRepo.
//
// The point is that this file carries NO build tag and needs no object store:
// if it runs at all, a plain `go test ./...` opened a real shale cluster and
// round-tripped through it.

//go:build !slatedb

package storage_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func newPebbleShaleRepo(t *testing.T) *storage.ShaleRepo {
	t.Helper()
	repo, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            "pebble-node",
		DbName:            fmt.Sprintf("pebble-%s", t.Name()),
		ReplicationFactor: 1,
	})
	if err != nil {
		t.Fatalf("NewShaleRepo: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

func TestPebbleBacking_RoundTrip(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)

	want := domain.Paste{
		Slug:       domain.Slug("pebble01"),
		Identity:   domain.Identity("owner-a"),
		Status:     domain.PasteStatusReady,
		Kind:       domain.KindMarkdown,
		ContentSHA: "sha-pebble01",
		Size:       42,
		CreatedAt:  now, UpdatedAt: now,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), want, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := repo.Get(want.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Identity != want.Identity || got.Size != want.Size || got.ContentSHA != want.ContentSHA {
		t.Fatalf("round trip: got %+v, want identity/size/sha %q/%d/%q",
			got, want.Identity, want.Size, want.ContentSHA)
	}

	// The enumeration entry is written on a DIFFERENT key family than the row,
	// so listing proves the engine is routing whole key families, not just
	// serving one lucky key.
	listed, err := repo.ListByOwner(want.Identity.String())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Slug != want.Slug {
		t.Fatalf("list by owner: got %+v, want exactly %s", listed, want.Slug)
	}

	sum, err := repo.SumActiveBytesByOwner(want.Identity.String(), now)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != want.Size {
		t.Fatalf("charged bytes: got %d, want %d", sum, want.Size)
	}
}

// A local engine has no shared store to fence unit mounts against, so asking
// for sharding must fail loudly rather than quietly returning one unit.
func TestPebbleBacking_RefusesMultiBackend(t *testing.T) {
	_, err := storage.NewShaleRepo(storage.ShaleConfig{
		NodeID:            "pebble-node",
		DbName:            "pebble-units",
		ReplicationFactor: 1,
		UnitCount:         4,
	})
	if err == nil {
		t.Fatal("UnitCount=4 opened on a local engine; want an error")
	}
}

// The served version's manifest rolls onto the head, so ONE read of the head
// resolves a request path. Pins the roll through a real repo rather than
// through the row type, because the roll happens at the six head-write sites.
func TestPebbleBacking_HeadCarriesServedManifest(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)

	p := domain.Paste{
		Slug: domain.Slug("manif001"), Identity: domain.Identity("owner-m"),
		Status: domain.PasteStatusReady, Kind: domain.KindMarkdown,
		ContentSHA: "sha-v1", Size: 11, CreatedAt: now, UpdatedAt: now,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}

	head, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// Asserting the ENTRY, not the Root accessors: those fall back to the flat
	// fields, so they answer correctly even when no manifest was stored and
	// would let a broken roll pass unnoticed.
	e, ok := head.Manifest.Files[domain.Root]
	if !ok {
		t.Fatalf("head carries no root manifest entry: %+v", head.Manifest.Files)
	}
	if e.SHA != "sha-v1" || e.Size != 11 || e.Kind != string(domain.KindMarkdown) {
		t.Fatalf("head root entry = %+v, want sha-v1/11/markdown", e)
	}

	// An append rolls the WHOLE served descriptor, so the head's manifest must
	// follow the new version rather than keeping v1's.
	if _, err := repo.AppendVersionWithQuotaCheck(
		context.Background(), p.Slug, domain.KindHTML, "sha-v2", 22, 0, now); err != nil {
		t.Fatalf("append: %v", err)
	}
	head, err = repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get after append: %v", err)
	}
	e, ok = head.Manifest.Files[domain.Root]
	if !ok {
		t.Fatalf("head lost its manifest on the roll: %+v", head.Manifest.Files)
	}
	if e.SHA != "sha-v2" || e.Size != 22 || e.Kind != string(domain.KindHTML) {
		t.Fatalf("head manifest did not roll to v2: %+v", e)
	}
}

// A directory is stored through the SAME insert a document uses: its manifest
// simply has more than one entry. Pins that no site-specific write path is
// needed to persist one.
func TestPebbleBacking_DirectoryArtifactRoundTrips(t *testing.T) {
	repo := newPebbleShaleRepo(t)
	now := time.Now().UTC().Truncate(time.Second)

	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{
		SHA: "sha-idx", Size: 100, CompressedSize: 40, ContentType: "text/html"})
	m.Add("app.css", domain.ManifestEntry{
		SHA: "sha-css", Size: 20, CompressedSize: 9, ContentType: "text/css"})

	p := domain.Paste{
		Slug: domain.Slug("dirart23"), Identity: domain.Identity("owner-d"),
		Status: domain.PasteStatusReady, Kind: domain.KindSite,
		ContentSHA: "sha-idx", Size: 100,
		CreatedAt: now, UpdatedAt: now,
		Manifest: m,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert directory: %v", err)
	}

	head, err := repo.Get(p.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if head.Kind != domain.KindSite {
		t.Fatalf("kind = %q, want site", head.Kind)
	}
	if head.IsSingle() {
		t.Fatalf("a two-file directory must not read back as single: %+v", head.Manifest.Files)
	}
	css, ok := head.Manifest.Files["app.css"]
	if !ok {
		t.Fatalf("head lost a manifest entry: %+v", head.Manifest.Files)
	}
	if css.SHA != "sha-css" || css.CompressedSize != 9 || css.ContentType != "text/css" {
		t.Fatalf("css entry = %+v", css)
	}

	// The v1 row must describe the same content, since the head serves it.
	vs, err := repo.ListVersions(p.Slug)
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(vs) != 1 {
		t.Fatalf("versions = %d, want 1", len(vs))
	}
	if len(vs[0].Manifest.Files) != 2 {
		t.Fatalf("v1 manifest = %+v, want the same 2 entries", vs[0].Manifest.Files)
	}
}
