// The owner document: the single-Get owner index (docs/SPEC.md "The owner
// document (v2 owner index)"). One JSON value at owner_doc/<identity> carries
// the owner's first-seen plus every cached enumeration entry, so list / count /
// quota sum / whoami are one point Get instead of a prefix scan whose range
// grows with lifetime churn. Writes maintain BOTH the doc and the legacy row
// families this release, so a rollback to the row-scanning binary loses
// nothing; readers never consult legacy rows for an owner with a doc.

package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

func shaleKeyOwnerDoc(identity string) []byte { return shaleKey(prefixOwnerDocAll, identity) }

// ownerDocPaste mirrors identityPasteRow's cached fields, tag for tag, so the
// doc renders a list row and sums quota bytes exactly as the legacy entry did.
// No Placeholder: the doc is written only by paths holding real values.
type ownerDocPaste struct {
	Name          string    `json:"name"`
	Size          int       `json:"size"`
	CreatedAt     time.Time `json:"created_at"`
	Kind          string    `json:"kind,omitempty"`
	LatestVersion int       `json:"latest_version,omitempty"`
	ServedSize    int       `json:"served_size,omitempty"`
	PinnedVersion int       `json:"pinned_version,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// ownerDocSite mirrors what the value-bearing identity_sites projection
// cached. The live site tier serves directories through the paste family, so
// no current write or read touches this map; it exists so the stored doc
// already carries the site shape the spec names for the follow-up releases.
type ownerDocSite struct {
	Name        string    `json:"name,omitempty"`
	DedupedSize int       `json:"deduped_size"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ownerDoc is the stored document, maps keyed by slug.
type ownerDoc struct {
	FirstSeen time.Time                `json:"first_seen"`
	Pastes    map[string]ownerDocPaste `json:"pastes,omitempty"`
	Sites     map[string]ownerDocSite  `json:"sites,omitempty"`
}

func ownerDocPasteFromRow(e identityPasteRow) ownerDocPaste {
	return ownerDocPaste{
		Name: e.Name, Size: e.Size, CreatedAt: e.CreatedAt,
		Kind: e.Kind, LatestVersion: e.LatestVersion, ServedSize: e.ServedSize,
		PinnedVersion: e.PinnedVersion, UpdatedAt: e.UpdatedAt,
	}
}

func (e ownerDocPaste) toRow() identityPasteRow {
	return identityPasteRow{
		Name: e.Name, Size: e.Size, CreatedAt: e.CreatedAt,
		Kind: e.Kind, LatestVersion: e.LatestVersion, ServedSize: e.ServedSize,
		PinnedVersion: e.PinnedVersion, UpdatedAt: e.UpdatedAt,
	}
}

// clone deep-copies the maps. Transact re-runs its closure on conflict, so a
// closure seeded from a candidate must never mutate the shared snapshot.
func (d ownerDoc) clone() ownerDoc {
	out := ownerDoc{
		FirstSeen: d.FirstSeen,
		Pastes:    make(map[string]ownerDocPaste, len(d.Pastes)),
		Sites:     make(map[string]ownerDocSite, len(d.Sites)),
	}
	maps.Copy(out.Pastes, d.Pastes)
	maps.Copy(out.Sites, d.Sites)
	return out
}

func (d *ownerDoc) summary() domain.OwnerSummary {
	out := domain.OwnerSummary{Active: len(d.Pastes), FirstSeen: d.FirstSeen}
	for _, e := range d.Pastes {
		out.PasteBytes += int64(e.Size)
	}
	for _, e := range d.Sites {
		out.SiteBytes += int64(e.DedupedSize)
	}
	return out
}

// listRows renders the owner's list in the order the legacy path produced:
// the scan returned slug-ascending, then the stable UpdatedAt sort, so ties
// keep slug order. A map iteration would make ties nondeterministic.
func (d *ownerDoc) listRows(owner string) []domain.Paste {
	slugs := make([]string, 0, len(d.Pastes))
	for s := range d.Pastes {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	out := make([]domain.Paste, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, d.Pastes[s].toRow().toDomain(domain.Slug(s), owner))
	}
	sortByUpdatedAtDesc(out)
	return out
}

// getOwnerDoc returns the owner's document via routed Get, or nil when none
// exists (a pre-migration owner, whose reads fall back to the legacy rows).
// An UNDECODABLE doc also reads as nil, loudly: the legacy rows are still
// dual-written and true, so failing the read would turn one corrupt value
// into a per-owner outage, and the owner's next mutation rebuilds the doc
// from those rows (txApplyOwnerDoc's repair path).
func (r *ShaleRepo) getOwnerDoc(identity string) (*ownerDoc, error) {
	payload, err := r.getRaw(shaleKeyOwnerDoc(identity))
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, nil
	}
	var doc ownerDoc
	if err := json.Unmarshal(payload, &doc); err != nil {
		r.repoLog().Printf("shale: owner doc for %s undecodable (%v); reads fall back to the legacy rows until a write rebuilds it", identity, err)
		return nil, nil
	}
	return &doc, nil
}

// ownerDocCandidate builds the heal-on-write seed for an owner with no doc
// yet: the same walk the legacy list performs (cached fields when the entry
// renders, the authoritative read-through otherwise, pruning entries whose
// row is gone or failed) plus the legacy first-seen. It runs OUTSIDE the
// transaction that applies it because scans are unsupported inside a CAS tx;
// txApplyOwnerDoc re-reads the doc inside the tx, so a racing first-write is
// settled by the shard CAS rather than by this snapshot. Returns nil when a
// DECODABLE doc already exists (nothing to seed); an undecodable doc gets a
// candidate anyway, which is what lets the write overwrite-repair it.
//
// Per-record damage fails OPEN, mirroring the quota scan: an entry that
// cannot be read is skipped and summarised, never propagated, because
// propagating would let one corrupt sibling brick the owner's entire write
// surface (and wedge the intent sweep). A skipped entry is simply absent
// from the candidate; that slug's next write restores it.
func (r *ShaleRepo) ownerDocCandidate(identity string) (*ownerDoc, error) {
	if identity == "" {
		return nil, nil
	}
	payload, err := r.getRaw(shaleKeyOwnerDoc(identity))
	if err != nil {
		return nil, err
	}
	if len(payload) > 0 {
		var doc ownerDoc
		if json.Unmarshal(payload, &doc) == nil {
			return nil, nil
		}
		// Undecodable doc: build the seed from the legacy rows so the write
		// rebuilds it in place.
	}
	cand := &ownerDoc{
		Pastes: map[string]ownerDocPaste{},
		Sites:  map[string]ownerDocSite{},
	}
	var skips scanSkips
	first, ferr := r.legacyOwnerFirstSeen(identity)
	if ferr != nil {
		skips.add(shaleKeyIdentityFirstSeen(identity), ferr)
	}
	cand.FirstSeen = first
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(identity))
	if err != nil {
		return nil, err
	}
	for _, item := range idx {
		slug := extractSlug(item.Key)
		var e identityPasteRow
		if err := json.Unmarshal(item.Value, &e); err == nil && e.renderable() {
			cand.Pastes[slug] = ownerDocPasteFromRow(e)
			continue
		}
		fresh, ok, ferr := r.freshRowFromHead(domain.Slug(slug))
		if ferr != nil {
			skips.add(item.Key, ferr)
			continue
		}
		if ok {
			cand.Pastes[slug] = ownerDocPasteFromRow(fresh)
		}
	}
	skips.reportHeal(r, identity)
	return cand, nil
}

// txApplyOwnerDoc mutates the owner's doc inside an EXISTING {id}-shard CAS
// transaction. The tx.Get always runs so the doc key joins the transaction's
// read set: two racing first-writes serialize on the shard CAS, and the
// retried loser re-reads and finds the winner's doc, at which point candidate
// (the pre-tx heal snapshot) is ignored. With no doc and no candidate the
// mutation starts from a zero-valued doc.
//
// An UNDECODABLE stored doc is rebuilt from the candidate and overwritten:
// the legacy rows the candidate was walked from are still dual-written and
// true, so the overwrite IS the repair, and aborting instead would wedge
// every write for the owner behind one corrupt value.
func (r *ShaleRepo) txApplyOwnerDoc(tx shaleKVTx, identity string, candidate *ownerDoc, mutate func(*ownerDoc)) error {
	docKey := shaleKeyOwnerDoc(identity)
	var doc ownerDoc
	raw, gerr := tx.Get(docKey)
	switch {
	case errors.Is(gerr, backend.ErrNotFound):
		if candidate != nil {
			doc = candidate.clone()
		}
	case gerr != nil:
		return fmt.Errorf("tx get %s: %w", docKey, gerr)
	default:
		if derr := decodeOwnerDoc(raw, &doc); derr != nil {
			doc = ownerDoc{}
			if candidate != nil {
				doc = candidate.clone()
			}
			r.repoLog().Printf("shale: owner doc for %s undecodable (%v); rebuilding it from the legacy rows", identity, derr)
		}
	}
	if doc.Pastes == nil {
		doc.Pastes = map[string]ownerDocPaste{}
	}
	if doc.Sites == nil {
		doc.Sites = map[string]ownerDocSite{}
	}
	mutate(&doc)
	return shaleTxPutJSON(tx, docKey, doc)
}

// decodeOwnerDoc decodes a raw stored doc value (possibly LWW-enveloped).
func decodeOwnerDoc(raw []byte, out *ownerDoc) error {
	payload, err := stripEnvelope(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, out)
}

// dropOwnerEntry removes slug from the owner's enumeration index and doc in
// one {id}-shard CAS, the shared tail of Delete and MarkFailed. Idempotent on
// a missing entry.
func (r *ShaleRepo) dropOwnerEntry(identity, slug string) error {
	indexKey := shaleKeyIdentityPaste(identity, slug)
	candidate, err := r.ownerDocCandidate(identity)
	if err != nil {
		return err
	}
	return r.cluster.Transact(indexKey, func(tx backend.Transaction) error {
		if _, err := tx.Get(indexKey); err == nil {
			if derr := tx.Delete(indexKey); derr != nil {
				return derr
			}
		} else if !errors.Is(err, backend.ErrNotFound) {
			return err
		}
		return r.txApplyOwnerDoc(tx, identity, candidate, func(doc *ownerDoc) {
			delete(doc.Pastes, slug)
		})
	})
}

// OwnerSummary answers whoami's count + first-seen + byte-sum with ONE read
// when the owner has a doc. A pre-migration owner composes the same figures
// from the legacy scans, read-only. SiteBytes stays zero on the legacy path:
// a directory is a paste, so its bytes are already in the paste sum.
func (r *ShaleRepo) OwnerSummary(owner string, now time.Time) (domain.OwnerSummary, error) {
	if owner == "" {
		return domain.OwnerSummary{}, nil
	}
	doc, err := r.getOwnerDoc(owner)
	if err != nil {
		return domain.OwnerSummary{}, err
	}
	if doc != nil {
		return doc.summary(), nil
	}
	idx, err := r.scanPrefix(shalePrefixIdentityPastes(owner))
	if err != nil {
		return domain.OwnerSummary{}, err
	}
	first, err := r.legacyOwnerFirstSeen(owner)
	if err != nil {
		return domain.OwnerSummary{}, err
	}
	used, err := r.sumActiveBytesForOwner(owner)
	if err != nil {
		return domain.OwnerSummary{}, err
	}
	return domain.OwnerSummary{Active: len(idx), FirstSeen: first, PasteBytes: used}, nil
}
