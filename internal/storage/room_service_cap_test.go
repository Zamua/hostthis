package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// newCapTestDB opens one sqlite db and returns all three repos that share
// the service-wide cap accounting, so a test can verify room bytes count
// in the SAME total pastes and sites are checked against.
func newCapTestDB(t *testing.T) (*RoomKVRepo, *PasteRepo, *SiteRepo) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "cap.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewRoomKVRepo(db), NewPasteRepo(db), NewSiteRepo(db)
}

// TestRoom_PutEnforcesServiceCap is the P1-CAP regression guard (room
// side): a room PUT that would push the service-wide active-byte total
// over the cap is refused with ErrServiceFull, prior state intact. Without
// the service-cap check inside PutValue's tx, this write would silently
// commit and room storage would be unbounded by --storage-cap-bytes.
func TestRoom_PutEnforcesServiceCap(t *testing.T) {
	rooms, _, _ := newCapTestDB(t)
	now := time.Now().UTC()
	const serviceCap = 10
	room := mkRoom(rooms, t, "app12345", now)

	// A write at the cap fits (appCap=0 -> unlimited per-app; serviceCap=10).
	if err := rooms.PutValue(room.AppSlug, room.ID, "k", []byte("1234567890"), 0, serviceCap, now); err != nil {
		t.Fatalf("write at service cap rejected: %v", err)
	}
	// One more byte (a new key) pushes the SERVICE total over -> ErrServiceFull.
	if err := rooms.PutValue(room.AppSlug, room.ID, "k2", []byte("x"), 0, serviceCap, now); !errors.Is(err, ErrServiceFull) {
		t.Fatalf("over-service-cap write = %v, want ErrServiceFull", err)
	}
	// The rejected key was NOT written (state intact).
	if _, err := rooms.GetValue(room.AppSlug, room.ID, "k2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected key was written anyway: %v", err)
	}
}

// TestRoom_BytesCountTowardPasteServiceCap is the P1-CAP regression guard
// (paste side): room bytes are included in the service-wide total the
// PASTE insert checks. A service already filled by room data must refuse a
// new paste with ErrServiceFull. Without SumActiveRoomBytes unioned into
// serviceWideActiveBytes, the paste path would not see room bytes and
// would over-admit past the cap.
func TestRoom_BytesCountTowardPasteServiceCap(t *testing.T) {
	rooms, pastes, _ := newCapTestDB(t)
	now := time.Now().UTC()
	const serviceCap = 100
	room := mkRoom(rooms, t, "app12345", now)

	// Fill the whole service cap with room data (90 of 100 bytes).
	if err := rooms.PutValue(room.AppSlug, room.ID, "k", make([]byte, 90), 0, serviceCap, now); err != nil {
		t.Fatalf("seed room bytes: %v", err)
	}
	// A 20-byte paste would push the service total to 110 > 100. It must be
	// refused because the paste's service-wide check now counts room bytes.
	p := domain.Paste{
		Slug: "paste234", Identity: "key:test", Kind: domain.KindHTML,
		ContentSHA: "sha-p", Size: 20,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if err := pastes.InsertWithQuotaCheck(p, serviceCap, 0, now); !errors.Is(err, ErrServiceFull) {
		t.Fatalf("paste insert ignored room bytes: got %v, want ErrServiceFull", err)
	}
}

// TestRoom_BytesCountTowardSiteServiceCap is the P1-CAP regression guard
// (site side): room bytes are included in the service-wide total the SITE
// deploy checks. A service already filled by room data must refuse a new
// site deploy with ErrServiceFull.
func TestRoom_BytesCountTowardSiteServiceCap(t *testing.T) {
	rooms, _, sites := newCapTestDB(t)
	now := time.Now().UTC()
	const serviceCap = 100
	room := mkRoom(rooms, t, "app12345", now)

	if err := rooms.PutValue(room.AppSlug, room.ID, "k", make([]byte, 90), 0, serviceCap, now); err != nil {
		t.Fatalf("seed room bytes: %v", err)
	}
	// A site deduped to 50 bytes FITS alone (50 < 100) but trips the cap
	// once the 90 room bytes are counted (90 + 50 = 140 > 100). The site is
	// deliberately small enough that it could ONLY be refused if the
	// service-wide sum includes room bytes - so this test fails if the room
	// sum is dropped from serviceWideActiveBytes.
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-small", Size: 50, ContentType: "text/html; charset=utf-8"})
	s := domain.Site{
		Slug:      "site2345",
		Identity:  "key:test",
		Manifest:  m,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RetentionWindow),
	}
	if got := s.Manifest.DedupedSize(); got != 50 {
		t.Fatalf("test setup: site deduped size = %d, want 50 (must fit alone under the cap)", got)
	}
	if err := sites.InsertWithQuotaCheck(s, s.Manifest.DedupedSize(), serviceCap, 0, now); !errors.Is(err, ErrServiceFull) {
		t.Fatalf("site deploy ignored room bytes: got %v, want ErrServiceFull", err)
	}
}
