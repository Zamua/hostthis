//go:build slatedb

package storage

import (
	"bytes"

	"github.com/Zamua/hostthis/internal/domain"
)

// WalkPastes visits every paste row in the cluster.
//
// A GLOBAL scan, and therefore explicitly not for a request path: the
// no-scans-on-the-request-path rule stands, and this exists for offline work -
// a backend-to-backend migration, or an audit - where walking everything is the
// entire point. It is exported for exactly that, and kept out of any interface
// a service depends on so it cannot be reached by accident.
//
// Tolerant of an undecodable row: it is skipped rather than aborting the walk,
// because a migration that stops dead on one damaged record is worse than one
// that reports it and carries the rest.
func (r *ShaleRepo) WalkPastes(fn func(domain.Paste) error) error {
	// ScanPrefixAllUnits, NOT the ordinary scan. "pastes/" names no shard token,
	// so a routed scan reaches ONE unit and returns its share as if it were the
	// whole keyspace - 3 of 38 pastes in the rehearsal that caught this, with no
	// error to hint at the other 35.
	return r.cluster.ScanPrefixAllUnits(prefixPastes, func(key, _ []byte) error {
		slug := domain.Slug(bytes.TrimPrefix(key, prefixPastes))
		if slug == "" {
			return nil
		}
		// Re-read through Get rather than decoding the scanned value: Get owns
		// the row's decode and its version resolution, and duplicating that here
		// is how the two drift.
		p, err := r.Get(slug)
		if err != nil {
			return nil
		}
		return fn(p)
	})
}
