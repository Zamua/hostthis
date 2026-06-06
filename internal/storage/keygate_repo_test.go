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
	for i := 0; i < 20; i++ {
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
	for i := 0; i < 20; i++ {
		_, _ = r.AdmitNewKey("key:"+string(rune('a'+i)), "1.2.3.0/24", now, 20, 24*time.Hour)
	}
	// A different subnet still has its own bucket.
	if _, err := r.AdmitNewKey("key:other", "5.6.7.0/24", now, 20, 24*time.Hour); err != nil {
		t.Fatalf("different subnet: %v", err)
	}
}

func TestKeyGate_DeleteOldRowsFreesSlots(t *testing.T) {
	r := newKeyGateRepo(t)
	old := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) // 4 days ago
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		_, _ = r.AdmitNewKey("key:"+string(rune('a'+i)), "1.2.3.0/24", old, 20, 24*time.Hour)
	}
	// Limit hit at `now` even though the rows are old — but only if
	// the window covers them.
	_, err := r.AdmitNewKey("key:z", "1.2.3.0/24", now, 20, 24*time.Hour)
	// Since old is 4 days before now and the window is 24h, the
	// existing rows are OUTSIDE the window — count returns 0 → admitted.
	if err != nil {
		t.Fatalf("expected admission past old rows, got %v", err)
	}
	// Pruning old rows shouldn't change behavior beyond shrinking the table.
	n, err := r.DeleteFirstSeenOlderThan(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("delete old: %v", err)
	}
	if n != 20 {
		t.Fatalf("expected 20 deletes, got %d", n)
	}
}
