package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

func newSiteTestDB(t *testing.T) (*SiteRepo, *PasteRepo) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSiteRepo(db), NewPasteRepo(db)
}

func sampleSite(slug, owner string, now time.Time) domain.Site {
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-index", Size: 100, ContentType: "text/html; charset=utf-8"})
	m.Add("css/style.css", domain.ManifestEntry{SHA: "sha-css", Size: 50, ContentType: "text/css; charset=utf-8"})
	return domain.Site{
		Slug:      domain.Slug(slug),
		Identity:  domain.Identity(owner),
		Manifest:  m,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RetentionWindow),
	}
}

func TestSiteRepo_InsertAndGet(t *testing.T) {
	sites, _ := newSiteTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	s := sampleSite("abc23456", "key:test", now)
	if err := sites.Insert(s); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := sites.Get(s.Slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Slug != s.Slug || got.Identity != s.Identity {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Manifest.Files) != 2 {
		t.Fatalf("manifest files: got %d, want 2", len(got.Manifest.Files))
	}
	if e := got.Manifest.Files["index.html"]; e.SHA != "sha-index" || e.Size != 100 {
		t.Fatalf("index entry round-trip: %+v", e)
	}
	if !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatalf("expires round-trip: got %v want %v", got.ExpiresAt, s.ExpiresAt)
	}
}

func TestSiteRepo_GetNotFound(t *testing.T) {
	sites, _ := newSiteTestDB(t)
	if _, err := sites.Get("zzzzzzzz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: got %v, want ErrNotFound", err)
	}
}

func TestSiteRepo_Delete(t *testing.T) {
	sites, _ := newSiteTestDB(t)
	now := time.Now().UTC()
	s := sampleSite("abc23456", "key:test", now)
	if err := sites.Insert(s); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := sites.Delete(s.Slug); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := sites.Get(s.Slug); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
}

func TestSiteRepo_QuotaCountsAcrossPastesAndSites(t *testing.T) {
	sites, pastes := newSiteTestDB(t)
	now := time.Now().UTC()
	owner := "key:test"

	// Insert a paste that uses most of the budget.
	cap := int64(domain.UserQuotaBytes) // 10 MiB
	p := domain.Paste{
		Slug: "paste234", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-p", Size: int(cap) - 100, // leaves 100 bytes
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := pastes.InsertWithQuotaCheck(p, cap, now); err != nil {
		t.Fatalf("insert paste: %v", err)
	}

	// A site whose deduped size (150) exceeds the remaining 100 bytes must
	// be rejected - the quota check sums BOTH pastes and sites.
	s := sampleSite("site2345", owner, now) // deduped 150
	err := sites.InsertWithQuotaCheck(s, s.Manifest.DedupedSize(), cap, now)
	if !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("site over combined quota: got %v, want ErrOverUserQuota", err)
	}

	// And a paste upload must ALSO see the (zero, since the site didn't
	// insert) site bytes - sanity that the combined sum is symmetric.
	p2 := domain.Paste{
		Slug: "paste235", Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-p2", Size: 200, // 200 > remaining 100
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := pastes.InsertWithQuotaCheck(p2, cap, now); !errors.Is(err, ErrOverUserQuota) {
		t.Fatalf("paste over quota with paste present: got %v, want ErrOverUserQuota", err)
	}
}

func TestSiteRepo_SlugCollidesWithPaste(t *testing.T) {
	sites, pastes := newSiteTestDB(t)
	now := time.Now().UTC()
	p := domain.Paste{
		Slug: "shared12", Identity: "key:test", Kind: domain.KindHTML,
		ContentSHA: "sha", Size: 10,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := pastes.Insert(p); err != nil {
		t.Fatalf("insert paste: %v", err)
	}
	s := sampleSite("shared12", "key:test", now)
	if err := sites.Insert(s); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("site reusing paste slug: got %v, want ErrSlugTaken", err)
	}
}

func TestSiteRepo_ExpiredAndReferenced(t *testing.T) {
	sites, _ := newSiteTestDB(t)
	now := time.Now().UTC()

	live := sampleSite("liveeeee", "key:test", now)
	if err := sites.Insert(live); err != nil {
		t.Fatalf("insert live: %v", err)
	}
	dead := sampleSite("deadeeee", "key:test", now.Add(-2*domain.RetentionWindow))
	dead.ExpiresAt = now.Add(-time.Hour)
	if err := sites.Insert(dead); err != nil {
		t.Fatalf("insert dead: %v", err)
	}

	expired, err := sites.ExpiredSiteSlugs(now)
	if err != nil {
		t.Fatalf("expired: %v", err)
	}
	if len(expired) != 1 || expired[0] != "deadeeee" {
		t.Fatalf("expired site slugs: got %v, want [deadeeee]", expired)
	}

	refs, err := sites.ReferencedSiteBlobSHAs()
	if err != nil {
		t.Fatalf("referenced: %v", err)
	}
	// Both sites share the same two SHAs; distinct set is {sha-index, sha-css}.
	set := map[string]bool{}
	for _, r := range refs {
		set[r] = true
	}
	if !set["sha-index"] || !set["sha-css"] {
		t.Fatalf("referenced site shas missing expected: got %v", refs)
	}
}
