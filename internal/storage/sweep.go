package storage

import (
	"fmt"
)

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
