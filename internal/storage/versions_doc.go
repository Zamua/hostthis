// The versions document: the single-Get version index (docs/SPEC.md "The
// versions document (v2 version index)"). One JSON value at
// versions_doc/<slug> carries every version WHOLE - number, created time,
// deleted flag and full served descriptor, MANIFEST included - so listing,
// numbering, the live-byte sum, blob resolution and a head roll are one point
// Get instead of a prefix scan of versions/<slug>/. Writes maintain BOTH the
// document and the legacy version rows this release, so a rollback to the
// row-scanning binary loses nothing; readers never consult the rows for a
// paste that has a document.

package storage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"

	"github.com/Zamua/hostthis/internal/domain"
)

func shaleKeyVersionsDoc(slug domain.Slug) []byte {
	return shaleKey(prefixVersionsDocAll, slug.String())
}

// errVersionsDocUnseeded is returned when the stored document was BELIEVABLE
// while the write was planned - decodable and accountable to the head - and is
// not inside the transaction, so the plan carries no heal seed to rebuild it
// from. Starting from an empty document would silently drop every version.
// Nothing is applied; the caller re-plans, and that plan walks the legacy rows.
var errVersionsDocUnseeded = errors.New("shale: versions doc undecodable with no heal seed")

// errVersionsSeedForeign is returned when a heal SEED is applied in a
// transaction whose head names a DIFFERENT paste than the seed was walked for:
// the slug was deleted and re-minted in the plan-to-tx window. The seed's
// records belong to a paste that no longer exists, so folding them into the new
// paste installs foreign versions. Nothing is applied; the caller re-plans.
var errVersionsSeedForeign = errors.New("shale: versions heal seed belongs to a re-minted paste")

// pasteIdent is a paste's IMMUTABLE identity: its owner and its creation
// instant, both fixed for the paste's life. A heal seed carries the pair it was
// walked for so the transaction that applies it can prove it is the SAME paste,
// not a re-mint that took the slug in the plan-to-tx window.
type pasteIdent struct {
	identity  string
	createdAt time.Time
}

func identOf(p pasteRow) pasteIdent {
	return pasteIdent{identity: p.Identity, createdAt: p.CreatedAt}
}

// same compares by instant rather than == because a JSON round-trip drops the
// monotonic clock the wall time would otherwise carry.
func (a pasteIdent) same(b pasteIdent) bool {
	return a.identity == b.identity && a.createdAt.Equal(b.createdAt)
}

// docFingerprint reduces a raw stored document to an envelope-independent
// identity: the stripped payload when it strips, the raw bytes when the
// envelope itself is damaged, nil when absent. Two reads of an UNCHANGED value
// fingerprint equal; any content change (including absent<->present) differs. A
// spurious inequality only forces a conservative re-heal, so it fails safe.
func docFingerprint(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	if payload, err := stripEnvelope(raw); err == nil {
		return payload
	}
	return raw
}

// versionMark is what the HEAD row says about the version set: the numbering
// floor and the generation the set is at. Both come off ONE head read, so a
// caller that already holds the head pays nothing for either oracle.
type versionMark struct {
	latest int
	gen    int
}

func markFromHead(p pasteRow) versionMark {
	return versionMark{latest: p.LatestVersion, gen: p.VersionsGen}
}

// versionGen is the generation as a transaction reads it and the one it will
// stamp. The head and the document take `next` in the SAME transaction, which
// is what makes a disagreement between them proof that something outside this
// binary wrote one of them.
type versionGen struct{ seen, next int }

func genFromHead(p pasteRow) versionGen {
	return versionGen{seen: p.VersionsGen, next: p.VersionsGen + 1}
}

// versionsDoc is the stored document. Versions holds versionRow values
// VERBATIM - the same shape the legacy rows store - so the two representations
// cannot describe a version differently, and a head roll reads the descriptor
// it needs (manifest included) out of the document alone.
type versionsDoc struct {
	Versions []versionRow `json:"versions"`

	// HighWater is the numbering mark: the next version number is one past
	// max(HighWater, the highest number held). It never lowers, so a version
	// the heal could not read still has its number reserved and its row is
	// never written over.
	HighWater int `json:"high_water"`

	// Complete is false when the document is known NOT to hold every version
	// that exists: a heal skipped a legacy row it could not read, or the head's
	// numbering mark names a version no record covers. The document is then
	// authoritative for what it HOLDS and says nothing about what it lacks, so
	// any operation that must cover EVERY version refuses instead of acting on
	// a truncated set. Absent reads as false, which is the safe direction.
	Complete bool `json:"complete"`

	// Gen is the head's version-set generation this document was built from.
	// Complete is a claim about what the document HOLDS; Gen is what makes that
	// claim accountable to the head, so a record the document holds and
	// MISDESCRIBES is caught too. Zero means the document claims no generation
	// and is trusted by nothing.
	Gen int `json:"gen,omitempty"`

	// mark and skipped are the heal walk's findings. They are NOT stored:
	// whether a skipped number is a HOLE is decided when the seed merges into a
	// document, since a document can already hold a record for a row that will
	// no longer read.
	mark    int
	skipped []int

	// planFP and planIdent tie a heal SEED to the document it read and the paste
	// it was walked for. The seed is walked OUTSIDE the transaction that applies
	// it, so a mutation racing that window can make it STALE (planFP: the
	// document the transaction reads differs from the one the walk saw) or
	// FOREIGN (planIdent: the slug was re-minted as a different paste). Neither
	// is stored; a plan-time scan snapshot cannot confer trust at a later commit.
	planFP    []byte
	planIdent pasteIdent
}

func (d *versionsDoc) find(ver int) (versionRow, bool) {
	for _, v := range d.Versions {
		if v.VerNum == ver {
			return v, true
		}
	}
	return versionRow{}, false
}

// upsert replaces the record for v.VerNum, or inserts it in number order. The
// stored order is ascending so a document round-trips byte-stably regardless
// of which path wrote it.
func (d *versionsDoc) upsert(v versionRow) {
	for i := range d.Versions {
		if d.Versions[i].VerNum == v.VerNum {
			d.Versions[i] = v
			return
		}
	}
	d.Versions = append(d.Versions, v)
	sort.Slice(d.Versions, func(i, j int) bool { return d.Versions[i].VerNum < d.Versions[j].VerNum })
}

// maxVerNum is the highest number the document holds, tombstones included.
func (d *versionsDoc) maxVerNum() int {
	max := 0
	for _, v := range d.Versions {
		if v.VerNum > max {
			max = v.VerNum
		}
	}
	return max
}

// behindMark reports whether the head's numbering mark names a version the
// document does not hold. The mark is bumped by an append that writes a version
// row, INCLUDING an append by a binary that predates the document, which
// touches the row and the mark and nothing else. A document below the mark is
// therefore missing a real, live version while looking authoritative, and that
// is indistinguishable from a heal that could not read one: both are treated as
// INCOMPLETE.
func (d *versionsDoc) behindMark(mark int) bool { return mark > d.maxVerNum() }

// accountable reports whether this document is the one the head describes. The
// mark above is a COUNTER, so it detects a version added behind the document's
// back and nothing else; a counter can never detect a MODIFICATION. The
// previous binary's per-version delete flips a legacy row's deleted flag
// without moving the mark and without touching the document, so the set keeps
// its size while a record the document holds becomes a lie - a version the
// document swears is live whose blob is already unbound. The generation moves
// on every version mutation, so it covers both.
//
// A zero on either side is a MISMATCH, not a wildcard. The previous binary
// re-marshals the head through a struct with no such field, so the field
// disappears from any row it rewrites; its absence under a document that claims
// a generation is positive evidence that such a binary touched the row.
func (d *versionsDoc) accountable(m versionMark) bool { return d.accountableTo(m.gen) }

// accountableTo is the generation half alone, for a transaction that holds the
// head's generation without the numbering mark.
func (d *versionsDoc) accountableTo(gen int) bool { return d.Gen != 0 && d.Gen == gen }

// stale reports a document that must be re-healed before anything acts on it:
// short of a version the head proves exists, or not accountable to the head's
// generation.
func (d *versionsDoc) stale(m versionMark) bool { return d.behindMark(m.latest) || !d.accountable(m) }

// applyHead folds both oracles into the completeness flag, so every gate that
// already consults Complete covers a stale document too.
func (d *versionsDoc) applyHead(m versionMark) {
	if d.stale(m) {
		d.Complete = false
	}
}

// nextVerNum is the number an append claims: one past the mark or the highest
// number held, whichever is greater. Tombstones count, so a number is never
// reused.
func (d *versionsDoc) nextVerNum() int {
	next := d.HighWater
	if held := d.maxVerNum(); held > next {
		next = held
	}
	return next + 1
}

// latestLive is the newest NON-deleted record, the version an unpinned URL
// serves. Only correct on a COMPLETE document: a truncated list's newest is
// not the newest.
func (d *versionsDoc) latestLive() (versionRow, bool) {
	var out versionRow
	found := false
	for _, v := range d.Versions {
		if v.Deleted {
			continue
		}
		if !found || v.VerNum > out.VerNum {
			out, found = v, true
		}
	}
	return out, found
}

// fillFrom supplies the versions d LACKS from a heal walk's seed, for a
// document that is ACCOUNTABLE to the head - one this binary wrote and nothing
// has modified since.
//
// It never OVERWRITES a record d holds. d is authoritative for what it holds,
// and the seed is walked OUTSIDE the transaction that applies it, so an
// overwrite could undo a tombstone that committed in between. It also means a
// record d holds for a row that will no longer decode survives the walk that
// could not read it.
func (d *versionsDoc) fillFrom(seed *versionsDoc) { d.merge(seed, false) }

// rebuildFrom replaces d's record set with the SEED's, for a document the
// head's generation says a binary without this logic modified. Filling would
// leave the misdescribed record in place and then stamp the result with a
// matching generation, which cements the lie as trustworthy and blinds the
// oracle permanently. The legacy rows the seed was walked from are the truth in
// that state, because rows are the only representation such a binary
// maintains. A record ONLY the distrusted document holds is dropped for the
// same reason: keeping it would stamp it trustworthy too, and a destructive
// gate would then act on a record nothing vouches for. A number the walk saw
// but could not read then reads as truncation, which every gate already
// refuses on; the numbering floor survives regardless, so no number is reused.
func (d *versionsDoc) rebuildFrom(seed *versionsDoc) { d.merge(seed, true) }

// merge folds a heal walk's seed into d and re-decides completeness. It is the
// ONE place Complete is set, so a document converges out of the incomplete
// state by the same rule that put it there: every number the walk skipped must
// be held, and the head's mark must name nothing the merged set lacks.
func (d *versionsDoc) merge(seed *versionsDoc, seedWins bool) {
	if seed == nil {
		return
	}
	if seedWins {
		d.Versions = nil
	}
	for _, v := range seed.Versions {
		if _, held := d.find(v.VerNum); !held {
			d.upsert(v)
		}
	}
	if seed.HighWater > d.HighWater {
		d.HighWater = seed.HighWater
	}
	d.mark, d.skipped = seed.mark, seed.skipped
	d.Complete = true
	for _, n := range seed.skipped {
		if _, held := d.find(n); !held {
			d.Complete = false
		}
	}
	if d.behindMark(seed.mark) {
		d.Complete = false
	}
}

// decodeVersionsDoc decodes a raw stored value (possibly LWW-enveloped).
func decodeVersionsDoc(raw []byte, out *versionsDoc) error {
	payload, err := stripEnvelope(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, out)
}

// getVersionsDoc returns the paste's document via routed Get, or nil when none
// exists (a pre-migration paste, whose reads fall back to the legacy rows).
// An UNDECODABLE document also reads as nil, loudly: the legacy rows are still
// dual-written and true, so failing the read would turn one corrupt value into
// a per-paste outage, and the paste's next mutation rebuilds it.
//
// Undecodable means BOTH layers. A damaged LWW envelope fails the strip before
// any JSON is parsed, and it is the shape a partially written value actually
// takes at replication factor >1; treating it as a read failure instead would
// leave the whole-paste delete - the one operation that could clear it - unable
// to run.
func (r *ShaleRepo) getVersionsDoc(slug domain.Slug) (*versionsDoc, error) {
	stored, err := r.getStored(shaleKeyVersionsDoc(slug))
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, nil
	}
	var doc versionsDoc
	if err := decodeVersionsDoc(stored, &doc); err != nil {
		r.logUndecodableVersionsDoc(slug, err)
		return nil, nil
	}
	return &doc, nil
}

// versionsDocGated is getVersionsDoc for the reads whose answer depends on the
// document describing every version CORRECTLY. It costs one extra point Get of
// the head, and none of those reads is a served request.
//
// A document the head cannot account for reads as ABSENT rather than as
// truncated, so the caller falls back to the legacy rows. Truncation says the
// document is right about what it holds; an unaccountable generation says a
// binary without this logic modified the set, so the records themselves may
// describe a version that no longer exists as described - and the rows are what
// that binary maintained.
func (r *ShaleRepo) versionsDocGated(slug domain.Slug) (*versionsDoc, error) {
	doc, err := r.getVersionsDoc(slug)
	if err != nil || doc == nil {
		return doc, err
	}
	mark := r.headVersionMark(slug)
	if !doc.accountable(mark) {
		return nil, nil
	}
	doc.applyHead(mark)
	return doc, nil
}

// logUndecodableVersionsDoc reports a document that will not decode ONCE per
// slug per process. A read path meets the same damaged value on every request,
// so logging per read would turn one corrupt value into an unbounded log stream
// (docs/SPEC.md "The log is a PER-PASS SUMMARY, never one line per row").
func (r *ShaleRepo) logUndecodableVersionsDoc(slug domain.Slug, cause error) {
	if _, seen := r.versionsDocLogged.LoadOrStore(slug.String(), struct{}{}); seen {
		return
	}
	r.repoLog().Printf("shale: versions doc for %s undecodable (%v); reads fall back to the legacy rows until a write rebuilds it", slug, cause)
}

// clearUndecodableVersionsDocLatch re-arms the report once a write has replaced
// the damaged value. Without this the latch outlives the damage it describes,
// and a LATER corruption of the same slug is silently never reported.
func (r *ShaleRepo) clearUndecodableVersionsDocLatch(slug domain.Slug) {
	r.versionsDocLogged.Delete(slug.String())
}

// versionSet is the paste's version records for a read that needs the whole
// set: the document when it exists, the legacy rows otherwise. READ-ONLY - the
// fallback never writes the document, so reads stay free of write races by
// construction.
//
// UNGATED: no head read is spent on the staleness oracle. What that admits is a
// version the document lists wrongly - short by one the head's mark would have
// named, or live when a previous binary tombstoned it - so an answer built from
// this list can be wrong in either direction. Its callers render or count from
// it: the version listing, the projection's live-byte sum, blob-id resolution.
//
// A GATE must not be built on it, and specifically must not be built on it
// while its subject comes from somewhere else. The trustworthiness of this list
// is unknown, so a guard reading here and a target read through
// versionsDocGated disagree on exactly the document neither of them judged the
// same way - which is how a destructive gate lets through the version it exists
// to protect. A caller that ACTS reads a source it has established
// (versionsDocGated, the in-transaction roll), or derives its answer from the
// head and the record it already holds.
func (r *ShaleRepo) versionSet(slug domain.Slug) ([]versionRow, error) {
	doc, err := r.getVersionsDoc(slug)
	if err != nil {
		return nil, err
	}
	if doc != nil {
		return doc.Versions, nil
	}
	return r.scanVersions(slug)
}

// versionsDocPlan reads the state a version WRITE plans from, and the heal seed
// the transaction applies with it. mark is what the head says about the version
// set, which every caller already holds.
//
// A stored document that is COMPLETE and not stale is used as-is: the common
// path spends no scan. Otherwise - absent, undecodable, incomplete, BEHIND the
// mark or not accountable to the head's generation - the walk re-runs over the
// legacy rows and its findings merge in, which is what makes a damaged document
// repairable: the flags clear the moment a walk reads everything, instead of
// latching for the life of the paste.
//
// The walk runs OUTSIDE the transaction that applies it, because a scan is
// unsupported inside a CAS transaction; the transaction re-reads the document
// key AND the head, and decides fill-versus-rebuild from those, so this
// snapshot only ever decides whether a seed is WALKED.
func (r *ShaleRepo) versionsDocPlan(slug domain.Slug, mark versionMark) (plan *versionsDoc, heal *versionsDoc, err error) {
	if hook := r.testHookVersionsDocPlanned; hook != nil {
		defer hook(slug)
	}
	// The RAW value is read so its fingerprint can be captured: the transaction
	// compares it against the document it reads to decide whether a mutation
	// raced this seed. An undecodable value keeps stored nil (the walk repairs
	// it) but still fingerprints, so a repair that leaves the SAME damaged bytes
	// reads as unchanged.
	rawStored, err := r.getStored(shaleKeyVersionsDoc(slug))
	if err != nil {
		return nil, nil, err
	}
	planFP := docFingerprint(rawStored)
	var stored *versionsDoc
	if len(rawStored) > 0 {
		var d versionsDoc
		if derr := decodeVersionsDoc(rawStored, &d); derr != nil {
			r.logUndecodableVersionsDoc(slug, derr)
		} else {
			stored = &d
		}
	}
	if stored != nil && stored.Complete && !stored.stale(mark) {
		return stored, nil, nil
	}
	seed, err := r.healVersionsDoc(slug, mark.latest)
	if err != nil {
		return nil, nil, err
	}
	merged := &versionsDoc{}
	if stored != nil {
		merged.Versions = append(merged.Versions, stored.Versions...)
		merged.HighWater = stored.HighWater
	}
	// Mirror the choice the transaction will make, so the gates a caller
	// applies to the plan see the same records the write will land.
	if stored != nil && !stored.accountable(mark) {
		merged.rebuildFrom(seed)
	} else {
		merged.fillFrom(seed)
	}
	merged.planFP = planFP
	return merged, merged, nil
}

// healVersionsDoc builds the heal seed from the legacy version rows.
//
// Per-record damage fails OPEN: a row that cannot be decoded is SKIPPED and
// summarised, never propagated, because propagating would let one damaged
// record brick the paste's entire write surface. What that leaves is a
// document missing a version while looking authoritative, so the skipped
// NUMBERS ride on the seed and fillFrom decides which of them are still holes.
//
// The scan is the TOLERANT one, so damage at the ENVELOPE layer skips the same
// way damage at the JSON layer does. A strict scan fails before any per-record
// decision, which would put the paste's whole write surface behind the one
// value this walk exists to route around.
//
// The mark is seeded from every legacy KEY the walk saw and from the head's
// latest_version, which is what makes "numbers are never reused" hold through
// damage: a version's number is in its KEY, and a key is readable even when its
// value is not.
func (r *ShaleRepo) healVersionsDoc(slug domain.Slug, mark int) (*versionsDoc, error) {
	items, err := r.scanPrefixTolerant(shalePrefixVersions(slug))
	if err != nil {
		return nil, err
	}
	cand := &versionsDoc{mark: mark}
	var skips scanSkips
	raise := func(n int) {
		if n > cand.HighWater {
			cand.HighWater = n
		}
	}
	skip := func(key []byte, num int, cause error) {
		skips.add(key, cause)
		cand.skipped = append(cand.skipped, num)
	}
	for _, item := range items {
		keyNum := verNumFromKey(item.Key)
		raise(keyNum)
		if item.Damaged != nil {
			skip(item.Key, keyNum, item.Damaged)
			continue
		}
		var v versionRow
		if derr := json.Unmarshal(item.Value, &v); derr != nil {
			skip(item.Key, keyNum, derr)
			continue
		}
		raise(v.VerNum)
		if keyNum > 0 && v.VerNum != keyNum {
			// A record that disagrees with its own key cannot be placed:
			// filing it under either number would answer confidently about a
			// version that is not there. Both numbers are already reserved.
			skip(item.Key, keyNum, fmt.Errorf("ver_num %d disagrees with its key", v.VerNum))
			continue
		}
		cand.upsert(v)
	}
	raise(mark)
	skips.reportVersionHeal(r, slug, cand.skipped)
	return cand, nil
}

// headVersionMark is what the head says about the version set, or the ZERO mark
// when the head cannot be read. The zero latest is only ever a floor for the
// next number, so an unreadable head costs the seed nothing the version keys did
// not already give it; the zero generation makes every document unaccountable,
// so a gated read falls back to the legacy rows rather than trusting a document
// nothing can vouch for.
func (r *ShaleRepo) headVersionMark(slug domain.Slug) versionMark {
	var head pasteRow
	if err := r.getJSON(shaleKeyPaste(slug), &head); err != nil {
		return versionMark{}
	}
	return markFromHead(head)
}

// verNumFromKey parses the <NNNN> tail of a versions/<slug>/<NNNN> key, or 0
// when it does not parse.
func verNumFromKey(key []byte) int {
	s := string(key)
	idx := strings.LastIndex(s, "/")
	if idx < 0 {
		return 0
	}
	n, err := strconv.Atoi(s[idx+1:])
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// txApplyVersionsDoc mutates the paste's document inside an EXISTING
// {slug}-shard CAS transaction. The tx.Get always runs so the document key
// joins the transaction's read set: two racing first-writes serialize on the
// shard CAS, and the retried loser re-reads and finds the winner's document.
//
// gen is the generation the transaction read off the HEAD; cur is that head's
// IDENTITY. Both come from the SAME in-transaction head read.
//
// A heal seed is walked OUTSIDE this transaction, so a plan-time scan snapshot
// cannot confer trust at a later commit. Two checks against in-transaction
// state guard it:
//
//   - IDENTITY. A seed whose planIdent differs from cur was walked for a paste
//     this slug no longer holds (a delete + re-mint in the plan-to-tx window).
//     Folding it would install foreign versions, so it is discarded and the
//     caller re-plans (errVersionsSeedForeign).
//   - STABILITY. A seed may STAMP the document trusted (complete + accountable)
//     ONLY when the document it read is the one this transaction reads: a
//     new-binary mutation always rewrites the document, so an unchanged
//     document proves none raced. When the document CHANGED in the window the
//     seed is a stale snapshot - the merged document is still written (the
//     rows' truth is cached, reads improve) but left INCOMPLETE and
//     UNACCOUNTABLE, so pin falls back to the rows and unpin/GetVersion refuse
//     rather than acting on records a plan-time scan vouched for. The next quiet
//     mutation re-heals it, so a healthy paste still converges.
//
// Whether the seed FILLS or REBUILDS is decided HERE from the document and the
// head as this transaction reads them. A mutation that landed since the plan
// moved both together, so it reads as accountable and its records win over the
// older seed; a binary without this logic moved neither, so its rows win.
//
// Absent or UNDECODABLE, the seed IS the document: the legacy rows it was
// walked from are still dual-written and true, so the overwrite is the repair,
// and aborting instead would wedge every write for the paste behind one corrupt
// value.
func (r *ShaleRepo) txApplyVersionsDoc(tx shaleKVTx, slug domain.Slug, heal *versionsDoc, gen versionGen, cur pasteIdent, mutate func(*versionsDoc) error) error {
	docKey := shaleKeyVersionsDoc(slug)
	if heal != nil && !heal.planIdent.same(cur) {
		return errVersionsSeedForeign
	}
	var doc versionsDoc
	var txFP []byte
	raw, gerr := tx.Get(docKey)
	switch {
	case errors.Is(gerr, backend.ErrNotFound):
		doc.fillFrom(heal)
	case gerr != nil:
		return fmt.Errorf("tx get %s: %w", docKey, gerr)
	default:
		txFP = docFingerprint(raw)
		derr := decodeVersionsDoc(raw, &doc)
		switch {
		case derr != nil && heal == nil:
			// It decoded when this write was planned, so no seed was walked.
			// An empty document would drop every version.
			return errVersionsDocUnseeded
		case derr != nil:
			doc = versionsDoc{}
			doc.fillFrom(heal)
			r.repoLog().Printf("shale: versions doc for %s undecodable (%v); rebuilding it from the legacy rows", slug, derr)
			r.clearUndecodableVersionsDocLatch(slug)
		case doc.accountableTo(gen.seen):
			doc.fillFrom(heal)
		case heal == nil:
			// Accountable when this write was planned and not now, so no seed
			// was walked and the document cannot be believed. Nothing is
			// applied; the caller re-plans, and that plan walks the rows.
			return errVersionsDocUnseeded
		default:
			doc.rebuildFrom(heal)
		}
	}
	if err := mutate(&doc); err != nil {
		return err
	}
	if heal != nil && !bytes.Equal(heal.planFP, txFP) {
		// A seed the window changed under is, for trust, the same as a heal that
		// skipped a row: written so reads improve, but vouched for by nothing.
		doc.Complete = false
		doc.Gen = 0
	} else {
		doc.Gen = gen.next // head and document move together
	}
	return shaleTxPutJSON(tx, docKey, doc)
}

// txWriteVersionsDoc writes the document OUTRIGHT inside an existing {slug}
// transaction, for the one caller that knows the WHOLE set independently of
// what is stored: insert, whose set is v1 and nothing else. No prior contents
// matter, so a document a previous paste on this slug left behind - decodable
// or not - is repaired by the overwrite. The tx.Get still runs so the key joins
// the read set.
func (r *ShaleRepo) txWriteVersionsDoc(tx shaleKVTx, slug domain.Slug, doc versionsDoc, gen versionGen) error {
	docKey := shaleKeyVersionsDoc(slug)
	if _, gerr := tx.Get(docKey); gerr != nil && !errors.Is(gerr, backend.ErrNotFound) {
		return fmt.Errorf("tx get %s: %w", docKey, gerr)
	}
	doc.Gen = gen.next
	r.clearUndecodableVersionsDocLatch(slug)
	return shaleTxPutJSON(tx, docKey, doc)
}

// txServedRefForVersion resolves the descriptor a head roll (pin, unpin) rolls
// onto the head, from INSIDE the rolling transaction so the read joins its
// read set. Document-first: one point read yields the whole descriptor,
// manifest included, so the roll never needs a second record that may be
// damaged. A number an INCOMPLETE document lacks is unavailable, not
// not-found: an absence in a truncated list is not knowledge. mark is what the
// head row the transaction already read says about the set, which is what makes
// a document BEHIND the rows or unaccountable to that head count here too.
//
// Accountability is checked BEFORE the record is taken: a document that a
// binary without this logic modified may describe a version WRONGLY rather than
// merely lack it, so rolling the head onto a record it holds is exactly the
// destructive act the check exists to stop. The legacy row below is what that
// binary maintained.
//
// A TOMBSTONED record is refused, on both paths. A tombstone unbinds the
// version's blob and queues the bytes for reclamation, so rolling the head onto
// one publishes an object that is already going away - and needs no race to
// reach: delete an unserved version, then pin it. The refusal is HERE, inside
// the rolling transaction, so a delete committing after the caller's own check
// is refused too.
func (r *ShaleRepo) txServedRefForVersion(tx shaleKVTx, slug domain.Slug, ver int, mark versionMark) (contentRef, error) {
	docKey := shaleKeyVersionsDoc(slug)
	raw, gerr := tx.Get(docKey)
	switch {
	case gerr == nil:
		var doc versionsDoc
		if derr := decodeVersionsDoc(raw, &doc); derr == nil && doc.accountable(mark) {
			if v, ok := doc.find(ver); ok {
				if v.Deleted {
					return contentRef{}, ErrVersionDeleted
				}
				return v.contentRef, nil
			}
			doc.applyHead(mark)
			if !doc.Complete {
				return contentRef{}, ErrVersionsIncomplete
			}
			return contentRef{}, ErrNotFound
		}
		// Undecodable or unaccountable: the legacy row below is still
		// dual-written and true.
	case !errors.Is(gerr, backend.ErrNotFound):
		return contentRef{}, gerr
	}
	var vr versionRow
	if err := shaleTxGetJSON(tx, shaleKeyVersion(slug, ver), &vr); err != nil {
		return contentRef{}, err
	}
	if vr.Deleted {
		return contentRef{}, ErrVersionDeleted
	}
	return vr.contentRef, nil
}

// txTombstoneLegacyVersionRow flips the legacy row's deleted flag IN PLACE,
// the dual-write half of a tombstone. A row that will not READ is left EXACTLY
// as it is and the skip is logged: rewriting it from the document would
// destroy the bytes a repair needs AND manufacture a row that reads back
// cleanly while describing content it does not have. The document is
// authoritative, so the tombstone has landed regardless.
func (r *ShaleRepo) txTombstoneLegacyVersionRow(tx shaleKVTx, slug domain.Slug, ver int) error {
	verKey := shaleKeyVersion(slug, ver)
	var v versionRow
	err := shaleTxGetJSON(tx, verKey, &v)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil // no legacy copy to flip
	case err != nil:
		r.repoLog().Printf("shale: legacy version row %s unreadable while tombstoning (%v); left untouched, the document carries the tombstone", verKey, err)
		return nil
	}
	if v.Deleted {
		return nil
	}
	v.Deleted = true
	return shaleTxPutJSON(tx, verKey, v)
}

// versionCascade is what a whole-paste delete must remove: every
// versions/<slug>/ KEY, every blob those rows and the document name, and the
// number a racing append would claim next.
//
// next is the delete's GUARD, read ExpectAbsent inside the transaction, and its
// one invariant is that it must equal the number the FIRST append committing
// after the enumeration will claim. That holds only while every observation
// that raised it is at least as old as the key scan - see deleteCascade.
type versionCascade struct {
	verKeys [][]byte
	blobIDs []string
	next    int
}

// deleteCascade enumerates the cascade from the version PREFIX rather than from
// the document, because the prefix IS every row. A document can be BEHIND the
// rows - an older binary appends by writing a row and bumping the head's mark
// without touching it, and a heal that could not read a row leaves the same
// gap - and a cascade derived from a document that is behind leaves a row and
// its bound blob alive under a deleted paste, which the next paste to mint that
// slug inherits. Correctness wins over the point read here: a whole-paste
// delete is not on any request path, so it can afford the one scan that cannot
// miss anything.
//
// A row that will not decode still has its KEY removed. The owner asked for the
// paste to be gone and nothing is manufactured by removing it, which is the one
// place the "a row that could not be read is never overwritten" rule does not
// bite. Its blob id comes from the document's record when there is one; when
// neither can be read the number is logged and the bytes stay bound, a leak of
// bytes rather than of a live version.
//
// That holds for damage at the ENVELOPE layer as well, which is why the scan is
// the TOLERANT one: a strict scan cannot get past the value, and the delete is
// the only operation that could remove it. A paste that cannot be deleted is a
// worse outcome than a blob left bound.
//
// The DOCUMENT IS READ FIRST, and the order is load-bearing. The probe number
// must equal the number a racing append claims, and an append claims one past
// everything it can see, so every observation that raises the probe has to be
// at least as old as the scan - then a row the scan missed is the very number
// the probe tests. Read the document AFTER the scan instead and an append
// committing between the two contributes its own number to the probe while
// staying out of the key set: the probe steps past the row it was meant to
// catch, the guard passes, and the delete commits leaving a live version under
// a deleted paste. The head's mark is read by the caller, earlier still, for
// the same reason.
func (r *ShaleRepo) deleteCascade(slug domain.Slug, mark int) (versionCascade, error) {
	doc, err := r.getVersionsDoc(slug)
	if err != nil {
		return versionCascade{}, err
	}
	if hook := r.testHookVersionsCascadeMidRead; hook != nil {
		hook(slug)
	}
	items, err := r.scanPrefixTolerant(shalePrefixVersions(slug))
	if err != nil {
		return versionCascade{}, err
	}
	out := versionCascade{next: mark}
	bound := map[string]bool{}
	addBlob := func(id string) {
		if id == "" || bound[id] {
			return
		}
		bound[id] = true
		out.blobIDs = append(out.blobIDs, id)
	}
	raise := func(n int) {
		if n > out.next {
			out.next = n
		}
	}
	unreadable := map[int]bool{}
	for _, item := range items {
		out.verKeys = append(out.verKeys, append([]byte(nil), item.Key...))
		keyNum := verNumFromKey(item.Key)
		raise(keyNum)
		if item.Damaged != nil {
			unreadable[keyNum] = true
			continue
		}
		var v versionRow
		if derr := json.Unmarshal(item.Value, &v); derr != nil {
			unreadable[keyNum] = true
			continue
		}
		raise(v.VerNum)
		addBlob(v.BlobID)
	}
	if doc != nil {
		raise(doc.HighWater)
		for _, v := range doc.Versions {
			raise(v.VerNum)
			addBlob(v.BlobID)
			delete(unreadable, v.VerNum)
		}
	}
	out.next++
	if len(unreadable) > 0 {
		r.repoLog().Printf("shale: whole-paste delete of %s removes %d unreadable version row%s no record names (%v); their keys go, their blobs stay bound until the operator clears them",
			slug, len(unreadable), pluralS(len(unreadable)), sortedInts(unreadable))
	}
	return out, nil
}

// sortedInts renders a set of version numbers deterministically for a log line.
func sortedInts(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}
