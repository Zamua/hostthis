package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// PasteStatus is the lifecycle state of a paste's content blob. It lets the
// slow blob write run in the background after the metadata commits: Create
// returns the URL while the paste is still pending. See docs/SPEC.md "Paste
// lifecycle status (async blob write)".
type PasteStatus string

const (
	// PasteStatusPending: metadata committed (slug reserved, quota charged),
	// blob not yet landed. A read serves a loading page.
	PasteStatusPending PasteStatus = "pending"
	// PasteStatusReady: terminal success; the paste serves its content.
	PasteStatusReady PasteStatus = "ready"
	// PasteStatusFailed: the blob write failed, or the handling pod died
	// mid-write and the reconciler aged the paste out. A read serves an error
	// page; the reservation is released.
	PasteStatusFailed PasteStatus = "failed"
)

// NormalizeStatus maps a persisted status string to a PasteStatus. An absent
// or unrecognized value reads as ready: a row carrying no status field is
// complete, which keeps the field a purely additive migration with no flag day.
func NormalizeStatus(s string) PasteStatus {
	switch PasteStatus(s) {
	case PasteStatusPending:
		return PasteStatusPending
	case PasteStatusFailed:
		return PasteStatusFailed
	default:
		return PasteStatusReady
	}
}

// Paste is the unit a user uploads. The currently-served bytes are addressed
// by ContentSHA + Kind; older versions live in a parallel versions table,
// addressed by (Slug, VerNum).
//
// There is no Published flag or per-paste secret: the slug IS the secret
// (8 chars over a 32-char alphabet), so the access model is "share the URL".
type Paste struct {
	Slug       Slug
	Identity   Identity    // "key:<fp>" or "ip:<subnet>" - quota AND capability gate
	Status     PasteStatus // pending | ready | failed (blob-write lifecycle)
	Kind       ContentKind // html | markdown of the currently-served version
	ContentSHA string      // sha256 of the currently-served bytes
	Size       int         // bytes (currently-served version)
	// StoredBytes is what the quota charges for this paste: the sum across every
	// live version, not just the served one. Populated on list reads; zero
	// elsewhere, where Size is the only figure available.
	StoredBytes   int
	Name          string // optional owner-set label; empty when unset
	PinnedVersion int    // explicit pin; 0 means unpinned (follow latest)
	LatestVersion int    // MAX(ver_num) - what an `update` would advance from; 0 if not loaded
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Version is a snapshot in a paste's history. v1 is the initial upload; each
// `update` writes a new row with ver_num+1.
//
// Deleted=true is a tombstone: the row stays so version numbers are never
// reused and `versions` still shows the history, but the blob bytes are gone
// (quota SUMs skip it; serving falls back to MAX of non-deleted ver_num).
type Version struct {
	Slug       Slug
	VerNum     int
	Kind       ContentKind
	ContentSHA string
	Size       int
	CreatedAt  time.Time
	Deleted    bool
}

// HashContent returns the canonical content hash blobs are addressed by, and
// which dedupable uploads are detected with.
func HashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NewRandomSlug returns a fresh random Slug drawn from SlugAlphabet using
// crypto/rand. Collisions are the caller's problem: retry on the persistence
// layer's uniqueness violation.
func NewRandomSlug() Slug {
	buf := make([]byte, SlugLength)
	if _, err := rand.Read(buf); err != nil {
		// getrandom(2)/arc4random only fails if the OS is dying.
		panic("hostthis: crypto/rand failure: " + err.Error())
	}
	out := make([]byte, SlugLength)
	for i, b := range buf {
		out[i] = SlugAlphabet[int(b)%len(SlugAlphabet)]
	}
	return Slug(out)
}
