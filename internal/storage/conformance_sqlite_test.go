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
	runConformance(t, "sqlite", func(t *testing.T) conformanceRepo {
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
	})
}
