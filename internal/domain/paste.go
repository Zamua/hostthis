package domain

import (
	"crypto/rand"
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

// Paste is the unit a user uploads. The currently-served bytes are addressed
// by the served manifest; older versions live in a parallel versions table,
// addressed by (Slug, VerNum).
//
// There is no Published flag or per-paste secret: the slug IS the secret
// (8 chars over a 32-char alphabet), so the access model is "share the URL".
type Paste struct {
	Slug       Slug
	Generation string      // opaque incarnation fence; changes when a slug is re-minted
	Identity   Identity    // "key:<fp>" or "ip:<subnet>" - quota AND capability gate
	Status     PasteStatus // pending | ready | failed (blob-write lifecycle)
	Kind       ContentKind // html | markdown of the currently-served version
	ContentSHA string      // a legacy served version's root sha; empty for an upload-keyed one
	// UploadID names the served version's object prefix. Empty for a legacy
	// version, whose content-addressed objects may be shared.
	UploadID string
	Size     int // bytes (currently-served version)
	// StoredBytes is what the quota charges for this paste: the sum across every
	// live version, not just the served one. Populated on list reads; zero
	// elsewhere, where Size is the only figure available.
	StoredBytes   int
	Name          string // optional owner-set label; empty when unset
	PinnedVersion int    // explicit pin; 0 means unpinned (follow latest)
	LatestVersion int    // MAX(ver_num) - what an `update` would advance from; 0 if not loaded
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Manifest is the SERVED version's content in full. A document is one
	// entry at Root, a directory is N, so one lookup resolves a request path
	// for either (docs/SPEC.md "Serving a directory"). Empty when the stored
	// row carries no manifest; the flat fields above describe the one blob.
	Manifest Manifest
}

// Version is a whole-MANIFEST snapshot in a paste's history. v1 is the
// initial upload; each `update` or redeploy writes a new row with ver_num+1.
// The manifest is what makes ONE paste type enough for both a document (one
// entry at Root) and a directory (N entries); nothing downstream distinguishes
// them (docs/SPEC.md "One paste, not two aggregates"). Kind/Size describe the
// ROOT entry; ContentSHA is a legacy version's root sha.
//
// Deleted=true is a tombstone: the row stays so version numbers are never
// reused and `versions` still shows the history, but the content is gone
// (quota SUMs skip it; serving falls back to MAX of non-deleted ver_num).
type Version struct {
	Slug       Slug
	VerNum     int
	Kind       ContentKind
	ContentSHA string
	UploadID   string // prefix holding this version's objects; empty for a legacy version
	Size       int
	CreatedAt  time.Time
	Deleted    bool

	// Manifest is the version's content. Empty when the stored row carries no
	// manifest; the flat fields describe it then.
	Manifest Manifest
}

// Root is the manifest path a single-document paste serves at.
const Root = "/"

// DocumentManifest is the one-entry manifest a single document is stored as.
func DocumentManifest(e ManifestEntry) Manifest {
	return Manifest{Files: map[string]ManifestEntry{Root: e}}
}

// RootEntry is a document's served file: its manifest's root entry, or, for a
// legacy row without one, the flat descriptor's sha.
func (p Paste) RootEntry() ManifestEntry {
	if e, ok := p.Manifest.Files[Root]; ok {
		return e
	}
	return ManifestEntry{SHA: p.ContentSHA, Kind: string(p.Kind)}
}

// NewPasteGeneration returns an opaque token that identifies one slug incarnation.
func NewPasteGeneration() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("hostthis: crypto/rand failure: " + err.Error())
	}
	return hex.EncodeToString(raw[:])
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
