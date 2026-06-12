package storage_test

// Sqlite backend's entry into the backend-agnostic conformance suite.
// This file is built by the DEFAULT `go test ./...` run (no build tag,
// no cgo, no external services), so the contract is exercised against
// the production sqlite backend on every test run.

import (
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

// sqliteConformRepo unions the two sqlite structs (PasteRepo and
// KeyGateRepo) over one shared *sql.DB so they present as a single
// conformanceRepo. The slatedb / shale backends are a single struct
// and don't need this; the sqlite backend split the key-gate rows into
// their own repo type, so the suite composes them here.
type sqliteConformRepo struct {
	*storage.PasteRepo
	*storage.KeyGateRepo
}

func TestConformance_Sqlite(t *testing.T) {
	caps := conformCaps{ExpiryFreesQuotaAtReadTime: true, StrictQuotaUnderConcurrency: true}
	newRepo := func(t *testing.T) conformanceRepo {
		dir := t.TempDir()
		db, err := storage.Open(filepath.Join(dir, "conform.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return sqliteConformRepo{
			PasteRepo:   storage.NewPasteRepo(db),
			KeyGateRepo: storage.NewKeyGateRepo(db),
		}
	}
	// The site repo shares the same *sql.DB as the paste repo, so the
	// cross-quota + cross-family-slug site subtests exercise the real
	// interaction.
	newSites := func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		dir := t.TempDir()
		db, err := storage.Open(filepath.Join(dir, "conform.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		r := sqliteConformRepo{
			PasteRepo:   storage.NewPasteRepo(db),
			KeyGateRepo: storage.NewKeyGateRepo(db),
		}
		return r, storage.NewSiteRepo(db)
	}
	// The room repo shares the same *sql.DB as the paste + site repos, so the
	// cross-kind service-wide cap room subtest exercises the real interaction.
	newRooms := func(t *testing.T) roomConformanceStores {
		dir := t.TempDir()
		db, err := storage.Open(filepath.Join(dir, "conform.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return roomConformanceStores{
			Rooms: storage.NewRoomKVRepo(db),
			Paste: sqliteConformRepo{
				PasteRepo:   storage.NewPasteRepo(db),
				KeyGateRepo: storage.NewKeyGateRepo(db),
			},
			Site: storage.NewSiteRepo(db),
		}
	}
	runConformanceWithSites(t, "sqlite", caps, newRepo, newSites, newRooms)
}
