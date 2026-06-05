package storage

import (
	"fmt"
	"time"
)

// ExpiredSlugs returns the slugs of pastes whose expires_at is at or
// before `now`. The sweep job uses this to know what to delete; the
// HTTP read path uses a similar check inline to 404 expired-but-not-
// yet-deleted rows.
func (r *PasteRepo) ExpiredSlugs(now time.Time) ([]string, error) {
	rows, err := r.db.Query(`SELECT slug FROM pastes WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("expired slugs: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UnreferencedBlobSHAs returns sha256s whose content is not pointed
// at by any paste OR any version row. The sweep job uses this to
// garbage-collect orphan blobs after row deletions.
func (r *PasteRepo) UnreferencedBlobSHAs() ([]string, error) {
	// A blob is "referenced" if it appears as a paste's content_sha
	// OR as a version row's content_sha. The blob store only knows
	// shas that have ever been written; the sweep walks the disk and
	// passes each sha through this check.
	//
	// Implementation note: we materialize the set of referenced
	// content_shas in memory because sqlite "NOT IN (subselect)"
	// against a non-existent index is slow on big tables. Even at
	// 100k pastes, the referenced set is ~100k strings = ~6MB.
	// Sweep runs every 10 minutes so this is fine.
	refs, err := r.referencedSHAs()
	if err != nil {
		return nil, err
	}
	// The caller walks the disk and asks `isReferenced(sha)` for each
	// blob file. We return the set as a map embedded in a helper.
	out := make([]string, 0, len(refs))
	for sha := range refs {
		out = append(out, sha)
	}
	return out, nil
}

// referencedSHAs returns a set of all SHAs referenced by either the
// pastes or versions tables. Used by the blob GC pass.
func (r *PasteRepo) referencedSHAs() (map[string]struct{}, error) {
	out := make(map[string]struct{}, 1024)
	for _, q := range []string{
		`SELECT DISTINCT content_sha FROM pastes`,
		`SELECT DISTINCT content_sha FROM versions`,
	} {
		rows, err := r.db.Query(q)
		if err != nil {
			return nil, fmt.Errorf("referenced shas (%s): %w", q, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			out[s] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
