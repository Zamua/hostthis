// Package storage holds the infrastructure adapters behind the domain types:
// sqlite for paste metadata, a content-addressed disk store for paste bytes.
// The dependency runs one way only: storage imports internal/domain, never the
// reverse.
package storage

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/Zamua/hostthis/internal/domain"

	// Pure-Go sqlite (no CGo) so binaries cross-compile cleanly into
	// distroless containers.
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Open opens or creates the sqlite db at path, applying pending migrations from
// the embedded migrations/ dir in lexical order. ":memory:" is valid for tests.
// The returned *sql.DB is safe for concurrent use; the caller closes it.
func Open(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	// Serialize all access through one connection. sqlite has a single writer
	// regardless, and an unbounded pool lets concurrent writers race into the
	// WAL write-lock upgrade deadlock, which busy_timeout cannot resolve (it
	// returns SQLITE_BUSY immediately). Concurrent writers are real here: the
	// async-blob finalizer's status flip overlaps the insert.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", path, err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB) error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	// Applied filenames are tracked in a meta table so re-runs are idempotent.
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _migrations (
		filename TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		var seen int
		if err := db.QueryRow("SELECT COUNT(1) FROM _migrations WHERE filename = ?", e.Name()).Scan(&seen); err != nil {
			return err
		}
		if seen > 0 {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply %s: %w", e.Name(), err)
		}
		if _, err := tx.Exec("INSERT INTO _migrations (filename) VALUES (?)", e.Name()); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// ErrNotFound is returned by any repo when a lookup misses. It aliases the
// domain-owned sentinel: the error vocabulary lives in internal/domain (see
// docs/SPEC.md "The storage contract").
var ErrNotFound = domain.ErrNotFound
