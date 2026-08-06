package storage

import (
	"database/sql"
	"testing"
)

// Migration 0018 drops expires_at from a POPULATED database: the columns were
// NOT NULL and indexed, so the drop has to survive real rows and real indexes,
// not just an empty schema.
func TestMigration0018_DropsExpiryOnPopulatedDB(t *testing.T) {
	db, err := sql.Open("sqlite", "file:mig0018?mode=memory&cache=shared&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	// Replay everything BEFORE 0018 so the schema still carries expires_at.
	applyMigrationsMatching(t, db, func(n string) bool { return n < "0018" })

	if _, err := db.Exec(`INSERT INTO pastes
		(slug, identity, kind, content_sha, size, name, created_at, updated_at, expires_at, pinned_version, status)
		VALUES ('abc12345','key:x','html','sha1',10,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-02-01T00:00:00Z',0,'ready')`); err != nil {
		t.Fatalf("seed paste: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sites
		(slug, identity, manifest, deduped_size, created_at, updated_at, expires_at)
		VALUES ('site1234','key:x','{}',10,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-02-01T00:00:00.000000000Z')`); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO rooms
		(app_slug, room_id, created_at, updated_at, expires_at)
		VALUES ('app12345','r1','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-02-01T00:00:00.000000000Z')`); err != nil {
		t.Fatalf("seed room: %v", err)
	}

	applyMigrationsMatching(t, db, func(n string) bool { return n == "0018_drop_expiry.sql" })

	for _, table := range []string{"pastes", "sites", "rooms"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = 'expires_at'`, table).Scan(&n); err != nil {
			t.Fatalf("pragma %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("%s.expires_at survived migration 0018", table)
		}
	}
	// The rows survive the column drop.
	var slug string
	if err := db.QueryRow(`SELECT slug FROM pastes`).Scan(&slug); err != nil || slug != "abc12345" {
		t.Fatalf("paste row lost by the migration: slug=%q err=%v", slug, err)
	}
	// And an INSERT with no expires_at now works, which is what the repos do.
	if _, err := db.Exec(`INSERT INTO pastes
		(slug, identity, kind, content_sha, size, name, created_at, updated_at, pinned_version, status)
		VALUES ('def45678','key:x','html','sha2',10,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',0,'ready')`); err != nil {
		t.Fatalf("post-migration insert: %v", err)
	}
}
