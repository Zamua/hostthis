package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newKeyGateRepo(t *testing.T) *KeyGateRepo {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "kg.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewKeyGateRepo(db)
}

func TestKeyGate_FirstSeenAndKnown(t *testing.T) {
	r := newKeyGateRepo(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	known, err := r.AdmitNewKey("key:abc", "1.2.3.0/24", now, 20, 24*time.Hour)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	if known {
		t.Fatalf("first sight of (key, subnet) should report known=false")
	}
	// Second admission of the same pair → known
	known2, err := r.AdmitNewKey("key:abc", "1.2.3.0/24", now.Add(time.Hour), 20, 24*time.Hour)
	if err != nil {
		t.Fatalf("admit 2: %v", err)
	}
	if !known2 {
		t.Fatalf("returning pair should report known=true")
	}
}

func TestKeyGate_LimitFires(t *testing.T) {
	r := newKeyGateRepo(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	for i := range 20 {
		_, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "1.2.3.0/24", now, 20, 24*time.Hour)
		if err != nil {
			t.Fatalf("admit %d: %v", i, err)
		}
	}
	_, err := r.AdmitNewKey("key:z", "1.2.3.0/24", now, 20, 24*time.Hour)
	if !errors.Is(err, ErrTooManyNewKeys) {
		t.Fatalf("21st should fail with ErrTooManyNewKeys, got %v", err)
	}
}

func TestKeyGate_OtherSubnetsUnaffected(t *testing.T) {
	r := newKeyGateRepo(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	for i := range 20 {
		_, _ = r.AdmitNewKey("key:"+string(rune('a'+i)), "1.2.3.0/24", now, 20, 24*time.Hour)
	}
	// A different subnet still has its own bucket.
	if _, err := r.AdmitNewKey("key:other", "5.6.7.0/24", now, 20, 24*time.Hour); err != nil {
		t.Fatalf("different subnet: %v", err)
	}
}

// Out-of-window rows free their slots AND are dropped by the admission that
// walks past them: the family stays bounded with no background prune.
func TestKeyGate_AdmissionDropsOutOfWindowRows(t *testing.T) {
	r := newKeyGateRepo(t)
	old := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) // 4 days before now
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	for i := range 20 {
		if _, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "1.2.3.0/24", old, 20, 24*time.Hour); err != nil {
			t.Fatalf("seed admit %d: %v", i, err)
		}
	}
	if n := countKeygateRows(t, r, "1.2.3.0/24"); n != 20 {
		t.Fatalf("fixture: want 20 seeded rows, got %d (the prune below could not be observed)", n)
	}

	// The 20 rows are 4 days old against a 24h window, so they count for
	// nothing and the 21st key is admitted.
	if _, err := r.AdmitNewKey("key:z", "1.2.3.0/24", now, 20, 24*time.Hour); err != nil {
		t.Fatalf("expected admission past out-of-window rows, got %v", err)
	}
	// And that same admission removed them: only the new row survives.
	if n := countKeygateRows(t, r, "1.2.3.0/24"); n != 1 {
		t.Fatalf("admission must drop the 20 out-of-window rows, leaving only the new one; got %d rows", n)
	}
}

// An admission must never drop a row still INSIDE the window: those are the
// rows the cap is counted from.
func TestKeyGate_AdmissionKeepsInWindowRows(t *testing.T) {
	r := newKeyGateRepo(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	for i := range 3 {
		if _, err := r.AdmitNewKey("key:"+string(rune('a'+i)), "1.2.3.0/24", recent, 20, 24*time.Hour); err != nil {
			t.Fatalf("seed admit %d: %v", i, err)
		}
	}
	if _, err := r.AdmitNewKey("key:z", "1.2.3.0/24", now, 20, 24*time.Hour); err != nil {
		t.Fatalf("admit: %v", err)
	}
	if n := countKeygateRows(t, r, "1.2.3.0/24"); n != 4 {
		t.Fatalf("in-window rows must survive an admission: want 4, got %d", n)
	}
}

func countKeygateRows(t *testing.T, r *KeyGateRepo, subnet string) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM key_first_seen WHERE ip_subnet = ?`, subnet).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}
