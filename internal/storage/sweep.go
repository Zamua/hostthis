package storage

import (
	"fmt"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// ExpiredPastes returns one reference per paste whose expires_at is at or
// before now. The scan reads the pastes table itself (there is no standalone
// expiry index to fall out of sync with the records), so IndexRef is always
// empty and a returned slug always named a live row at scan time.
func (r *PasteRepo) ExpiredPastes(now time.Time) ([]domain.ExpiredPaste, error) {
	rows, err := r.db.Query(`SELECT slug FROM pastes WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("expired pastes: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []domain.ExpiredPaste
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, domain.ExpiredPaste{Slug: domain.Slug(s)})
	}
	return out, rows.Err()
}

// DeleteExpired processes one expired reference with the same full-cascade
// delete as Delete, reporting whether a paste row was actually removed. There
// is no expiry-index entry to clean here, so a missing row is a no-op that
// returns false (docs/SPEC.md "The storage contract", Expiry).
func (r *PasteRepo) DeleteExpired(ref domain.ExpiredPaste) (bool, error) {
	res, err := r.db.Exec(`DELETE FROM pastes WHERE slug = ?`, ref.Slug.String())
	if err != nil {
		return false, fmt.Errorf("delete expired %q: %w", ref.Slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete expired %q: rows affected: %w", ref.Slug, err)
	}
	return n > 0, nil
}

// ReferencedBlobSHAs returns the content-SHAs the sweep must keep. Every blob
// NOT in the set is GC'd, so an empty set while pastes exist would delete every
// blob; the sweep guards against that by aborting on zero refs.
//
// This impl also keeps a TOMBSTONED version's SHA referenced (its query has no
// deleted filter), diverging from the canonical rule that a tombstoned
// version's blob is GC-able.
func (r *PasteRepo) ReferencedBlobSHAs() ([]string, error) {
	// Materialised in memory rather than a sqlite "NOT IN (subselect)": that
	// subselect has no index to lean on and is slow on big tables, while the
	// set stays a few MB even at 100k pastes.
	refs, err := r.referencedSHAs()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for sha := range refs {
		out = append(out, sha)
	}
	return out, nil
}

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
				rows.Close() //nolint:errcheck
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
