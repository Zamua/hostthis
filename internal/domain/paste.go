package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"
)

// Paste is the unit a user uploads. The current served bytes are
// addressed by ContentSHA + Kind; older versions (when we add the
// versions verb) hang off the same Slug with their own ContentSHA.
type Paste struct {
	Slug       Slug
	OwnerHash  string      // SHA256 fingerprint of the owner's ssh key, or "" for anonymous
	Kind       ContentKind // html | markdown
	ContentSHA string      // sha256 of the raw bytes; the blob store keys on this
	Size       int         // bytes
	Name       string      // optional owner-set label; empty when unset
	CreatedAt  time.Time
	UpdatedAt  time.Time
	ExpiresAt  time.Time // CreatedAt or UpdatedAt + RetentionWindow
}

// RetentionWindow is the fixed 24-hour TTL per SPEC.md. Not config.
const RetentionWindow = 24 * time.Hour

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
