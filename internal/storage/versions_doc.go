// The version index cache: a disposable per-slug copy of the authoritative
// versions/<slug>/<N> rows (docs/SPEC.md "The version index cache"). It exists
// only to keep the hot reads off the version prefix scan; it is NEVER a source
// of truth. Every cheap read that consults it falls back to scanning the rows
// when it is absent or unreadable, and no destructive decision ever reads it -
// those decide from the rows and the head. So a stale cache is harmless and
// there is nothing to detect: no generation counter, no completeness flag, no
// staleness fingerprint.

package storage

import (
	"encoding/json"
	"errors"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

func shaleKeyVersionsDoc(slug domain.Slug) []byte {
	return shaleKey(prefixVersionsDocAll, slug.String())
}

// versionsDoc is the stored cache value: the slug's version rows verbatim
// (numbers, served descriptors, created-at, deleted flag), live and tombstoned
// alike, so a read served from it matches a rows scan tag for tag.
type versionsDoc struct {
	Versions []versionRow `json:"versions"`
}

// cachedVersionRows returns the cached version rows on a decodable hit, or
// (nil, false) when the cache is absent (a pre-cache paste) or unreadable. It
// NEVER errors: the cache is a disposable accelerator, so damage falls OPEN to
// scanning the authoritative rows rather than failing a read (docs/SPEC.md
// "Fail open on unreadable data"). The read never writes the cache, so it
// cannot race a write over it.
func (r *ShaleRepo) cachedVersionRows(slug domain.Slug) ([]versionRow, bool) {
	payload, err := r.getRaw(shaleKeyVersionsDoc(slug))
	if err != nil {
		// A truncated envelope or a Get failure: fall back to the rows.
		r.repoLog().Printf("shale: version cache for %s unreadable (%v); reads scan the rows", slug, err)
		return nil, false
	}
	if len(payload) == 0 {
		return nil, false // no cache; scan the rows
	}
	var doc versionsDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		r.repoLog().Printf("shale: version cache for %s undecodable (%v); reads scan the rows", slug, err)
		return nil, false
	}
	return doc.Versions, true
}

// versionRowsForRead returns a slug's version rows for a CHEAP read, preferring
// the cache and falling back to a rows scan when it is absent or unreadable.
// NOT for any destructive decision: those read the rows and the head directly
// (docs/SPEC.md "Destructive operations decide from the rows and the head").
func (r *ShaleRepo) versionRowsForRead(slug domain.Slug) ([]versionRow, error) {
	if rows, ok := r.cachedVersionRows(slug); ok {
		return rows, nil
	}
	return r.scanVersions(slug)
}

// txPutVersionsDoc writes the WHOLE version cache inside an existing {slug}
// transaction. A blind Put with no read-check: the document is disposable, so a
// lost or clobbered write only leaves it stale, which the next whole-document
// write (insert / append / unpin) refreshes. Adding conflict detection here
// would exist only to certify the cache fresh, which no reader needs.
func txPutVersionsDoc(tx shaleKVTx, slug domain.Slug, rows []versionRow) error {
	return shaleTxPutJSON(tx, shaleKeyVersionsDoc(slug), versionsDoc{Versions: rows})
}

// txReadVersionsDoc reads and decodes the disposable version cache inside a
// {slug} transaction, recording a read-check so a concurrent write of the cache
// conflicts and the reader upserts onto a fresh set. Present and decodable =>
// (rows, true, nil). Absent OR undecodable => (nil, false, nil): the caller
// migrates or repairs from a rows scan (fail open, the same discipline the
// cheap-read fallback keeps). Only a transport error propagates.
func txReadVersionsDoc(tx shaleKVTx, slug domain.Slug) ([]versionRow, bool, error) {
	raw, err := tx.Get(shaleKeyVersionsDoc(slug))
	if errors.Is(err, backend.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	doc, derr := decodeVersionsDoc(raw)
	if derr != nil {
		return nil, false, nil // undecodable: rebuild from the rows
	}
	return doc.Versions, true, nil
}

// txMarkVersionDeletedInCache flips v's Deleted flag in the cache when the cache
// EXISTS, inside the tombstone's {slug} transaction. A pre-cache paste (no doc)
// is left untouched: building one here would need a scan the tombstone
// deliberately avoids, and its reads fall back to the rows. An unreadable cache
// is left as-is (fail open); the next whole-document write rebuilds it.
func txMarkVersionDeletedInCache(tx shaleKVTx, slug domain.Slug, v versionRow) error {
	docKey := shaleKeyVersionsDoc(slug)
	raw, err := tx.Get(docKey)
	if errors.Is(err, backend.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	doc, derr := decodeVersionsDoc(raw)
	if derr != nil {
		return nil
	}
	v.Deleted = true
	doc.upsert(v)
	return shaleTxPutJSON(tx, docKey, doc)
}

// decodeVersionsDoc decodes a raw stored cache value (LWW-enveloped at R>1).
func decodeVersionsDoc(raw []byte) (versionsDoc, error) {
	payload, err := stripEnvelope(raw)
	if err != nil {
		return versionsDoc{}, err
	}
	var doc versionsDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		return versionsDoc{}, err
	}
	return doc, nil
}

// upsert replaces the record for v.VerNum, or appends it when the cache does
// not carry that version yet, so a stale cache missing a version is left at
// least as complete as before plus the change.
func (d *versionsDoc) upsert(v versionRow) {
	for i := range d.Versions {
		if d.Versions[i].VerNum == v.VerNum {
			d.Versions[i] = v
			return
		}
	}
	d.Versions = append(d.Versions, v)
}
