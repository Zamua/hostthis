package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
)

// 0019 projects the paste/site split onto one artifact model. The suite that
// runs every migration on an EMPTY database cannot check that: with no site
// rows present, the copy is a no-op and passes vacuously. This applies the
// migrations to a database that actually holds a site.
func TestMigration0019_ProjectsSitesOntoArtifacts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := applyMigrationsUpTo(db, "0018"); err != nil {
		t.Fatalf("migrate to 0018: %v", err)
	}

	// A two-file site, as the pre-0019 world stored one.
	manifest := `{"files":{"/":{"sha":"sha-index","size":120,"ct":"text/html; charset=utf-8"},` +
		`"/app.js":{"sha":"sha-app","size":900,"ct":"text/javascript"}}}`
	if _, err := db.Exec(
		`INSERT INTO sites (slug, identity, manifest, deduped_size, created_at, updated_at)
		 VALUES ('sitea111','key:mig',?,1020,'2026-01-01T00:00:00Z','2026-01-02T00:00:00Z')`,
		manifest); err != nil {
		t.Fatalf("seed site: %v", err)
	}
	// A paste too, so the version projection is exercised alongside it.
	if _, err := db.Exec(
		`INSERT INTO pastes (slug, identity, kind, content_sha, size, name, created_at, updated_at, pinned_version, status)
		 VALUES ('pastea11','key:mig','markdown','sha-p',50,'','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z',0,'ready')`); err != nil {
		t.Fatalf("seed paste: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO versions (slug, ver_num, kind, content_sha, size, created_at, deleted)
		 VALUES ('pastea11',1,'markdown','sha-p',50,'2026-01-01T00:00:00Z',0)`); err != nil {
		t.Fatalf("seed version: %v", err)
	}

	if err := applyMigrationsUpTo(db, "9999"); err != nil {
		t.Fatalf("migrate past 0019: %v", err)
	}

	// The site is now an artifact.
	var kind, sha string
	var size int
	if err := db.QueryRow(`SELECT kind, content_sha, size FROM pastes WHERE slug='sitea111'`).
		Scan(&kind, &sha, &size); err != nil {
		t.Fatalf("the site must have become a pastes row: %v", err)
	}
	if kind != "site" || sha != "sha-index" || size != 120 {
		t.Errorf("site artifact row: got kind=%q sha=%q size=%d, want site/sha-index/120", kind, sha, size)
	}

	// Its v1 carries the WHOLE manifest, not just the root entry - that is the
	// difference between a migrated directory and a truncated one.
	var raw string
	if err := db.QueryRow(`SELECT manifest FROM versions WHERE slug='sitea111' AND ver_num=1`).Scan(&raw); err != nil {
		t.Fatalf("the site must have a v1: %v", err)
	}
	var got struct {
		Files map[string]struct {
			SHA  string `json:"sha"`
			Size int    `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("v1 manifest is not JSON: %v (%s)", err, raw)
	}
	if len(got.Files) != 2 {
		t.Fatalf("migrated manifest must keep every file: got %d entries, want 2 (%s)", len(got.Files), raw)
	}
	if got.Files["/app.js"].SHA != "sha-app" || got.Files["/app.js"].Size != 900 {
		t.Errorf("non-root entry lost in migration: %+v", got.Files["/app.js"])
	}

	// The pre-existing paste version became a one-entry manifest at Root.
	// Decoded into a FRESH value: unmarshalling into a non-nil map merges keys
	// rather than replacing them, which would silently carry the site's two
	// entries into this assertion.
	got.Files = nil
	if err := db.QueryRow(`SELECT manifest FROM versions WHERE slug='pastea11' AND ver_num=1`).Scan(&raw); err != nil {
		t.Fatalf("paste version: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("paste manifest is not JSON: %v (%s)", err, raw)
	}
	if len(got.Files) != 1 || got.Files["/"].SHA != "sha-p" {
		t.Errorf("paste must project to a one-entry manifest at /: %s", raw)
	}

	// The original site row is UNTOUCHED: the migration is additive so a
	// rollback to the previous binary still serves.
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sites WHERE slug='sitea111'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("sites row must survive for rollback: n=%d err=%v", n, err)
	}
}

// applyMigrationsUpTo runs embedded migrations whose filename sorts at or below
// stop, so a test can build a database at a HISTORICAL schema and then migrate
// it forward. It mirrors migrate() rather than calling it, because migrate()
// deliberately offers no way to stop partway.
func applyMigrationsUpTo(db *sql.DB, stop string) error {
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS _migrations (
		filename TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	)`); err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() > stop+"_zzzz" {
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
		if _, err := db.Exec(string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", e.Name(), err)
		}
		if _, err := db.Exec("INSERT INTO _migrations (filename) VALUES (?)", e.Name()); err != nil {
			return err
		}
	}
	return nil
}
