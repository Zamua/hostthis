package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// PasteStatus is the lifecycle state of a paste's content blob. It
// exists so the slow blob write (to object storage) can run in the
// background after the metadata is committed: Create returns the URL
// while the paste is still PasteStatusPending, a background finalizer
// flips it to PasteStatusReady once the bytes land (or
// PasteStatusFailed if the write fails). See docs/SPEC.md "Paste
// lifecycle status (async blob write)".
type PasteStatus string

const (
	// PasteStatusPending: metadata is committed (slug reserved, quota
	// charged) but the content blob has not finished landing in the
	// object store. A read serves a loading page.
	PasteStatusPending PasteStatus = "pending"
	// PasteStatusReady: the blob write succeeded; the paste serves its
	// content normally. Terminal success state.
	PasteStatusReady PasteStatus = "ready"
	// PasteStatusFailed: the background blob write failed (or the
	// handling pod died mid-write and the reconciler aged the paste
	// out). A read serves an error page; the reservation is released.
	PasteStatusFailed PasteStatus = "failed"
)

// NormalizeStatus maps a persisted status string to a PasteStatus,
// defaulting an empty value to PasteStatusReady. A row written before
// the lifecycle existed has no status field; "absent" means "written
// before the lifecycle, therefore complete." This keeps the status a
// pure additive migration with no flag day.
func NormalizeStatus(s string) PasteStatus {
	switch PasteStatus(s) {
	case PasteStatusPending:
		return PasteStatusPending
	case PasteStatusFailed:
		return PasteStatusFailed
	default:
		// "" (legacy row) or any unknown value -> ready.
		return PasteStatusReady
	}
}

// Paste is the unit a user uploads. The "currently served" bytes are
// addressed by ContentSHA + Kind. Older versions live in a parallel
// versions table, addressed by (Slug, VerNum).
//
// There's no Published flag or per-paste secret - the URL slug
// itself is the secret (8 chars / 32^8 ≈ 10^12 possibilities), so
// "share the URL with whoever you want to see it" is the access
// model.
type Paste struct {
	Slug          Slug
	Identity      Identity    // "key:<fp>" or "ip:<subnet>" - quota AND capability gate
	Status        PasteStatus // pending | ready | failed (blob-write lifecycle)
	Kind          ContentKind // html | markdown of the currently-served version
	ContentSHA    string      // sha256 of the currently-served bytes
	Size          int         // bytes (currently-served)
	Name          string      // optional owner-set label; empty when unset
	PinnedVersion int         // explicit pin; 0 means unpinned (follow latest)
	LatestVersion int         // MAX(ver_num) - what an `update` would advance from; 0 if not loaded
	CreatedAt     time.Time
	UpdatedAt     time.Time
	ExpiresAt     time.Time // UpdatedAt + RetentionWindow; only `update` moves it
}

// Version is a snapshot in a paste's history. v1 is the initial
// upload; each subsequent `update` writes a new row with ver_num+1.
//
// Deleted=true marks a tombstone - the row stays so version numbers
// don't get reused and `versions` shows the history, but the blob
// bytes are gone (quota SUMs skip it; serving falls back to MAX of
// non-deleted ver_num). Set by `delete <slug> <ver>`.
type Version struct {
	Slug       Slug
	VerNum     int
	Kind       ContentKind
	ContentSHA string
	Size       int
	CreatedAt  time.Time
	Deleted    bool
}

// RetentionWindow is the fixed 7-day TTL per SPEC.md. Not config.
const RetentionWindow = 7 * 24 * time.Hour

// HashContent returns the canonical content hash used to address blobs
// and detect dedupable uploads. The same bytes always produce the
// same hex digest.
func HashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NewRandomSlug returns a fresh random Slug drawn from SlugAlphabet
// using crypto/rand. Callers handle collision with the persistence
// layer (retry on uniqueness violation); this function never returns
// an error because crypto/rand.Read on darwin/linux only fails in
// the impossible case.
func NewRandomSlug() Slug {
	buf := make([]byte, SlugLength)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand on linux/darwin reads from getrandom(2)/arc4random;
		// the only failure mode is "OS is dying." Panic is appropriate.
		panic("hostthis: crypto/rand failure: " + err.Error())
	}
	out := make([]byte, SlugLength)
	for i, b := range buf {
		out[i] = SlugAlphabet[int(b)%len(SlugAlphabet)]
	}
	return Slug(out)
}
