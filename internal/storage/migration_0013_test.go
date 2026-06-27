package storage

import (
	"database/sql"
	"sort"
	"testing"
)

// applyMigrationsMatching replays the embedded migration files whose name
// satisfies pred, in lexical order, each in its own transaction - the same
// shape as migrate(). It does NOT use the _migrations bookkeeping table; the
// caller drives exactly which files run, so a populated-DB upgrade across a
// single migration can be exercised in isolation.
func applyMigrationsMatching(t *testing.T, db *sql.DB, pred func(name string) bool) {
	t.Helper()
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && pred(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			t.Fatalf("begin %s: %v", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply %s: %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
	}
}

// TestMigration0013_PreservesVersionsOnPopulatedDB is the regression test for
// the data-loss hazard in the 0013 (add 'diff' kind) table rebuild. The
// migrator runs with foreign_keys ON, so a naive rebuild of the pastes parent
// would fire versions' ON DELETE CASCADE and silently wipe all version
// history on any existing sqlite deploy. This test populates the DB through
// 0012, then applies ONLY 0013, and asserts the version rows survive (and a
// 'diff'-kind insert is now accepted, and the FK still rejects an orphan).
//
// It fails on the cascade-bug version of 0013 (versions emptied) and passes
// on the stash-and-refill fix.
func TestMigration0013_PreservesVersionsOnPopulatedDB(t *testing.T) {
	// Same connection shape as Open(): foreign_keys ON, single connection.
	db, err := sql.Open("sqlite", "file:mig0013?mode=memory&cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Bring the schema up to the pre-0013 state.
	applyMigrationsMatching(t, db, func(name string) bool { return name < "0013" })

	// Populate: one paste with two version rows (one a tombstone) - exactly
	// the history a cascade would silently destroy.
	if _, err := db.Exec(`INSERT INTO pastes (slug, identity, kind, content_sha, size, name, created_at, updated_at, expires_at, pinned_version, status)
		VALUES ('abc12345', 'id-hash', 'markdown', 'sha-v2', 10, 'note', '2026-06-01T00:00:00Z', '2026-06-02T00:00:00Z', '2026-07-02T00:00:00Z', 2, 'ready')`); err != nil {
		t.Fatalf("insert paste: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at, deleted) VALUES
		('abc12345', 1, 'markdown', 'sha-v1', 8, '2026-06-01T00:00:00Z', 1),
		('abc12345', 2, 'markdown', 'sha-v2', 10, '2026-06-02T00:00:00Z', 0)`); err != nil {
		t.Fatalf("insert versions: %v", err)
	}

	// Apply ONLY 0013.
	applyMigrationsMatching(t, db, func(name string) bool { return name == "0013_diff_kind.sql" })

	// The version history must survive the rebuild.
	var versionCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM versions WHERE slug = 'abc12345'`).Scan(&versionCount); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versionCount != 2 {
		t.Fatalf("version history lost in migration: got %d rows, want 2", versionCount)
	}
	// The tombstone flag must be preserved too (not just the row count).
	var deletedV1 int
	if err := db.QueryRow(`SELECT deleted FROM versions WHERE slug = 'abc12345' AND ver_num = 1`).Scan(&deletedV1); err != nil {
		t.Fatalf("read v1 deleted: %v", err)
	}
	if deletedV1 != 1 {
		t.Errorf("v1 tombstone flag lost: got deleted=%d, want 1", deletedV1)
	}

	// No leftover stash table.
	var stashLeft int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='versions_stash'`).Scan(&stashLeft); err != nil {
		t.Fatalf("check stash: %v", err)
	}
	if stashLeft != 0 {
		t.Errorf("versions_stash table left behind after migration")
	}

	// The relaxed CHECK now accepts a 'diff' kind on both tables.
	if _, err := db.Exec(`INSERT INTO pastes (slug, identity, kind, content_sha, size, name, created_at, updated_at, expires_at, pinned_version, status)
		VALUES ('diff0001', 'id-hash', 'diff', 'sha-d1', 12, '', '2026-06-03T00:00:00Z', '2026-06-03T00:00:00Z', '2026-07-03T00:00:00Z', 1, 'ready')`); err != nil {
		t.Fatalf("diff-kind paste insert should be accepted after 0013: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at, deleted)
		VALUES ('diff0001', 1, 'diff', 'sha-d1', 12, '2026-06-03T00:00:00Z', 0)`); err != nil {
		t.Fatalf("diff-kind version insert should be accepted after 0013: %v", err)
	}

	// The FK from versions -> pastes must still be enforced after the rebuild.
	if _, err := db.Exec(`INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at, deleted)
		VALUES ('nopaste9', 1, 'html', 'sha-x', 1, '2026-06-03T00:00:00Z', 0)`); err == nil {
		t.Errorf("orphan version insert should be rejected by the FK, but it was accepted")
	}

	// An unsupported kind is still rejected by the CHECK.
	if _, err := db.Exec(`INSERT INTO pastes (slug, identity, kind, content_sha, size, name, created_at, updated_at, expires_at, pinned_version, status)
		VALUES ('bad00001', 'id-hash', 'pdf', 'sha-b', 1, '', '2026-06-03T00:00:00Z', '2026-06-03T00:00:00Z', '2026-07-03T00:00:00Z', 1, 'ready')`); err == nil {
		t.Errorf("unsupported kind 'pdf' should be rejected by the CHECK, but it was accepted")
	}
}
