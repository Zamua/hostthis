// Offline per-identity counter audit for the shale metadata backend.
//
// AuditCounters recomputes each owner's byte counters (the paste counter
// identity_bytes/<id> AND the site counter identity_site_bytes/<id>) from
// AUTHORITATIVE truth and, with apply=true, overwrites each stored counter
// with the recomputed value. It is the belt-and-suspenders backstop that
// corrects drift already banked before the crash-durable release marker
// existed, or from an operator's manual surgery (docs/SPEC.md "Offline audit
// that recomputes the counter from authoritative truth"). It is the ONLY
// place an absolute counter overwrite is permitted, because it is sound ONLY
// with writes QUIESCED: with no concurrent upload / delete / reserve in
// flight, the cross-shard scan and the counter write have no conflict window.
// The identical scan-then-write is FORBIDDEN online - it is the racy recompute
// that under-counts across the scan-to-write gap and was deliberately removed
// from the reconciler. AuditCounters therefore never runs in a background loop;
// it is invoked once, offline, via the `hostthisd audit-counters` subcommand.
//
// What it recomputes, per identity:
//
//   - identity_site_bytes/<id> = the sum of DedupedSize over that owner's live
//     sites/<slug> rows, plus any site reservation marker still WITHIN grace
//     whose site row is ABSENT (a legitimately-pending deploy whose bytes the
//     live counter already holds; a marker whose row is PRESENT is a leaked
//     confirm whose bytes are already summed from the row, so it is skipped to
//     avoid double-counting).
//   - identity_bytes/<id> = the sum of live (non-tombstoned) version sizes over
//     that owner's pastes, plus in-grace paste reservation markers whose paste
//     row is absent (same pending-vs-leaked split; an <slug>#append marker maps
//     to the base <slug>).
//
// Past-grace reserve markers and ALL release markers are excluded (they stand
// for bytes that are either abandoned or already freed); a full audit resolves
// them via the reconciler first, then runs this to correct any residual.
//
// Decode policy: FAIL CLOSED. Unlike the reconciler (Policy 1: skip + log +
// continue, safe because it only ever applies marker-driven DELTAS), the audit
// writes an ABSOLUTE value. Skipping an undecodable authoritative row would
// omit its bytes from the sum and set the counter too LOW - an under-count,
// the one direction the ranking invariant forbids (it would let the owner
// exceed the cap). So any undecodable authoritative row / marker / counter
// aborts the whole audit and writes NOTHING; the operator fixes the row and
// re-runs. This is Policy 2's fail-closed posture applied to an absolute
// overwrite (docs/SPEC.md "Decode tolerance is per-scan-semantics").

//go:build slatedb

package storage

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Zamua/shale/pkg/backend"
)

// CounterAuditFinding is one owner+kind whose STORED counter differs from the
// value recomputed from authoritative truth. Stored is what the counter key
// currently holds (0 when absent); Computed is the authoritative recompute.
type CounterAuditFinding struct {
	Identity string
	Kind     string // "paste" (identity_bytes) or "site" (identity_site_bytes)
	Stored   int64
	Computed int64
}

// CounterAuditResult summarizes an AuditCounters run: the drift findings
// (stored != computed), the number of distinct owners examined, and how many
// counters were actually overwritten (0 unless apply was true).
type CounterAuditResult struct {
	Apply    bool
	Findings []CounterAuditFinding // sorted by identity, then kind
	Owners   int                   // distinct owners examined
	Applied  int                   // counters overwritten (0 on dry-run)
}

// auditKindPaste / auditKindSite label a finding's counter family.
const (
	auditKindPaste = "paste"
	auditKindSite  = "site"
)

// AuditCounters is the offline recompute-and-optionally-set audit. See the
// file header for the invariant and the quiesced-writes precondition. now +
// reserveGrace classify reservation markers (in-grace bytes are folded into
// the recompute exactly as the live counter would hold them); apply=false is a
// dry-run that reports drift and writes nothing, apply=true overwrites each
// drifted counter with a single Put of the recomputed sum.
func (r *ShaleRepo) AuditCounters(now time.Time, reserveGrace time.Duration, apply bool) (CounterAuditResult, error) {
	pasteComputed := make(map[string]int64)
	siteComputed := make(map[string]int64)
	pasteStored := make(map[string]int64)
	siteStored := make(map[string]int64)
	owners := make(map[string]struct{})
	register := func(owner string) {
		if owner != "" {
			owners[owner] = struct{}{}
		}
	}

	// --- 1. Paste rows: slug -> owner, and the live-paste-slug set. ----------
	pasteItems, err := r.aggregatePrefix(prefixPastes)
	if err != nil {
		return CounterAuditResult{}, fmt.Errorf("audit: scan pastes: %w", err)
	}
	slugOwner := make(map[string]string, len(pasteItems))
	livePasteSlugs := make(map[string]struct{}, len(pasteItems))
	for _, item := range pasteItems {
		slug := strings.TrimPrefix(string(item.Key), "pastes/")
		var p pasteRow
		if err := json.Unmarshal(item.Value, &p); err != nil {
			// Fail closed: an undecodable authoritative paste would drop its
			// version bytes from the owner's sum -> under-count.
			return CounterAuditResult{}, fmt.Errorf("audit: decode paste %s: %w", item.Key, err)
		}
		if p.Identity == "" {
			return CounterAuditResult{}, fmt.Errorf("audit: paste %s has empty identity", slug)
		}
		slugOwner[slug] = p.Identity
		livePasteSlugs[slug] = struct{}{}
		register(p.Identity)
	}

	// --- 2. Versions: sum non-deleted sizes into each owner's paste sum. -----
	// One cross-shard scan of versions/, attributed to owners via slugOwner.
	// A version whose paste row is ABSENT is an orphan (a half-deleted / never-
	// confirmed remnant) charged to nobody, so it is skipped WITHOUT decoding
	// (its slug alone decides ownership) - a corrupt orphan version therefore
	// cannot abort an audit it does not affect. A version whose paste IS present
	// must decode cleanly (fail closed), because skipping it would under-count.
	verItems, err := r.aggregatePrefix(prefixVersionsAll)
	if err != nil {
		return CounterAuditResult{}, fmt.Errorf("audit: scan versions: %w", err)
	}
	for _, item := range verItems {
		rest := strings.TrimPrefix(string(item.Key), "versions/")
		slug, _, ok := strings.Cut(rest, "/")
		if !ok {
			continue // malformed key (no version segment); nothing to charge
		}
		owner, owned := slugOwner[slug]
		if !owned {
			continue // orphan version: not charged to any owner
		}
		var v versionRow
		if err := json.Unmarshal(item.Value, &v); err != nil {
			return CounterAuditResult{}, fmt.Errorf("audit: decode version %s: %w", item.Key, err)
		}
		if v.Deleted {
			continue // tombstoned: its bytes are freed, not counted
		}
		pasteComputed[owner] += int64(v.Size)
	}

	// --- 3. Site rows: sum DedupedSize into each owner's site sum. -----------
	siteItems, err := r.aggregatePrefix(prefixSites)
	if err != nil {
		return CounterAuditResult{}, fmt.Errorf("audit: scan sites: %w", err)
	}
	liveSiteSlugs := make(map[string]struct{}, len(siteItems))
	for _, item := range siteItems {
		slug := strings.TrimPrefix(string(item.Key), "sites/")
		var row siteRow
		if err := json.Unmarshal(item.Value, &row); err != nil {
			return CounterAuditResult{}, fmt.Errorf("audit: decode site %s: %w", item.Key, err)
		}
		if row.Identity == "" {
			return CounterAuditResult{}, fmt.Errorf("audit: site %s has empty identity", slug)
		}
		liveSiteSlugs[slug] = struct{}{}
		siteComputed[row.Identity] += int64(row.DedupedSize)
		register(row.Identity)
	}

	// --- 4. In-grace reservation markers (paste + site). ---------------------
	// An in-grace marker's bytes are held by the live counter; fold them in so
	// a dry-run does not spuriously flag a healthy pending insert, and an apply
	// does not strip its reserved bytes. Gate on the row being ABSENT: a marker
	// whose row is present is a leaked confirm whose bytes are already summed
	// from the row (steps 2/3), so folding it in too would double-count. Past-
	// grace markers are excluded entirely (the reconciler resolves them).
	if err := r.auditFoldPasteMarkers(now, reserveGrace, livePasteSlugs, pasteComputed, register); err != nil {
		return CounterAuditResult{}, err
	}
	if err := r.auditFoldSiteMarkers(now, reserveGrace, liveSiteSlugs, siteComputed, register); err != nil {
		return CounterAuditResult{}, err
	}

	// --- 5. Stored counters: fold in owners that hold a counter with no rows -
	// (the residual case: a markerless over-count whose row is long gone). Its
	// computed sum is 0, so the audit sets it to 0 under apply.
	pcItems, err := r.aggregatePrefix([]byte("identity_bytes/"))
	if err != nil {
		return CounterAuditResult{}, fmt.Errorf("audit: scan paste counters: %w", err)
	}
	for _, item := range pcItems {
		owner := strings.TrimPrefix(string(item.Key), "identity_bytes/")
		n, err := parseCounter(item.Value)
		if err != nil {
			return CounterAuditResult{}, fmt.Errorf("audit: decode paste counter %s: %w", item.Key, err)
		}
		pasteStored[owner] = n
		register(owner)
	}
	scItems, err := r.aggregatePrefix([]byte("identity_site_bytes/"))
	if err != nil {
		return CounterAuditResult{}, fmt.Errorf("audit: scan site counters: %w", err)
	}
	for _, item := range scItems {
		owner := strings.TrimPrefix(string(item.Key), "identity_site_bytes/")
		n, err := parseCounter(item.Value)
		if err != nil {
			return CounterAuditResult{}, fmt.Errorf("audit: decode site counter %s: %w", item.Key, err)
		}
		siteStored[owner] = n
		register(owner)
	}

	// --- 6. Compare + (optionally) set. --------------------------------------
	sortedOwners := make([]string, 0, len(owners))
	for owner := range owners {
		sortedOwners = append(sortedOwners, owner)
	}
	sort.Strings(sortedOwners)

	result := CounterAuditResult{Apply: apply, Owners: len(sortedOwners)}
	for _, owner := range sortedOwners {
		// Paste counter.
		if stored, computed := pasteStored[owner], pasteComputed[owner]; stored != computed {
			result.Findings = append(result.Findings, CounterAuditFinding{
				Identity: owner, Kind: auditKindPaste, Stored: stored, Computed: computed,
			})
			if apply {
				if err := r.setCounterAbsolute(shaleKeyIdentityBytes(owner), computed); err != nil {
					return result, fmt.Errorf("audit: set paste counter %s: %w", owner, err)
				}
				result.Applied++
			}
		}
		// Site counter.
		if stored, computed := siteStored[owner], siteComputed[owner]; stored != computed {
			result.Findings = append(result.Findings, CounterAuditFinding{
				Identity: owner, Kind: auditKindSite, Stored: stored, Computed: computed,
			})
			if apply {
				if err := r.setCounterAbsolute(shaleKeyIdentitySiteBytes(owner), computed); err != nil {
					return result, fmt.Errorf("audit: set site counter %s: %w", owner, err)
				}
				result.Applied++
			}
		}
	}
	return result, nil
}

// auditFoldPasteMarkers folds each in-grace paste reservation marker whose base
// slug (the <slug> of an <slug> or <slug>#append key) has NO authoritative
// paste into that owner's recomputed paste sum. See AuditCounters step 4.
func (r *ShaleRepo) auditFoldPasteMarkers(now time.Time, reserveGrace time.Duration, livePasteSlugs map[string]struct{}, pasteComputed map[string]int64, register func(string)) error {
	markers, err := r.aggregatePrefix([]byte("identity_reserve/"))
	if err != nil {
		return fmt.Errorf("audit: scan paste reservations: %w", err)
	}
	for _, item := range markers {
		m, err := parseReservationMarker(item.Value)
		if err != nil {
			return fmt.Errorf("audit: decode paste reservation %s: %w", item.Key, err)
		}
		if !markerInGrace(m, now, reserveGrace) {
			continue // past grace: abandoned or leaked; the reconciler resolves it
		}
		baseSlug := strings.TrimSuffix(markerSlugFromKey(item.Key), "#append")
		if _, present := livePasteSlugs[baseSlug]; present {
			continue // leaked confirm: bytes already summed from the row
		}
		owner := markerOwnerFromKey(item.Key)
		pasteComputed[owner] += m.Bytes
		register(owner)
	}
	return nil
}

// auditFoldSiteMarkers is the site twin of auditFoldPasteMarkers over
// identity_site_reserve/ markers against the recomputed site sum.
func (r *ShaleRepo) auditFoldSiteMarkers(now time.Time, reserveGrace time.Duration, liveSiteSlugs map[string]struct{}, siteComputed map[string]int64, register func(string)) error {
	markers, err := r.aggregatePrefix([]byte("identity_site_reserve/"))
	if err != nil {
		return fmt.Errorf("audit: scan site reservations: %w", err)
	}
	for _, item := range markers {
		m, err := parseReservationMarker(item.Value)
		if err != nil {
			return fmt.Errorf("audit: decode site reservation %s: %w", item.Key, err)
		}
		if !markerInGrace(m, now, reserveGrace) {
			continue
		}
		slug := siteMarkerSlugFromKey(item.Key)
		if _, present := liveSiteSlugs[slug]; present {
			continue
		}
		owner := siteMarkerOwnerFromKey(item.Key)
		siteComputed[owner] += m.Bytes
		register(owner)
	}
	return nil
}

// setCounterAbsolute overwrites a counter key with an exact value in one
// unconditional single-shard Put. Sound ONLY offline (quiesced writes), which
// is why it is reachable only from AuditCounters and never the reconciler. The
// Transact pins the counter's own key (its shard), so the write routes to the
// owning replica; with no reader/writer racing, the blind Put is exact and may
// safely move the counter DOWN. formatCounter clamps a negative to 0, but the
// recomputed sum is always >= 0.
func (r *ShaleRepo) setCounterAbsolute(key []byte, value int64) error {
	return r.cluster.Transact(key, func(tx backend.Transaction) error {
		return tx.Put(key, formatCounter(value))
	})
}
