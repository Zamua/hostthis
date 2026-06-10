package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

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
