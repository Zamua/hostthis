// The package's shared vocabulary: the errors callers match on, the result
// types they read, and the manifest codec every backend stores.
//
// The errors are ALIASES of the domain's, not new values, so a caller may match
// with errors.Is against either spelling and a backend cannot invent a
// look-alike that fails the comparison.

package storage

import (
	"encoding/json"
	"fmt"

	"github.com/Zamua/hostthis/internal/domain"
)

var (
	ErrNotFound       = domain.ErrNotFound
	ErrServiceFull    = domain.ErrServiceFull
	ErrTooManyNewKeys = domain.ErrTooManyNewKeys
	ErrSlugTaken      = domain.ErrSlugTaken
	ErrAppRoomsFull   = domain.ErrAppRoomsFull
	ErrRoomDataFull   = domain.ErrRoomDataFull
	ErrOverUserQuota  = domain.ErrOverUserQuota
)

type AppendResult = domain.AppendResult

// sortableTimeFormat makes a timestamp key sort lexicographically in time
// order, which is what lets a time range be read as a key range.
//
// Deliberately NOT time.RFC3339Nano, which drops trailing fractional zeros:
// under that variable-width format "...00.5Z" sorts BEFORE "...00Z" (because
// '.' < 'Z'), so a range scan would cut up to ~1s on the wrong side.
const sortableTimeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// The stored manifest document. Field names are the on-disk contract: every
// backend writes this shape, so a site deployed by one is servable by the next.
type manifestJSON struct {
	Files map[string]entryJSON `json:"files"`
}

type entryJSON struct {
	SHA  string `json:"sha"`
	Size int    `json:"size"`
	CT   string `json:"ct"`
	// Kind is the RENDER kind, strictly richer than CT: several kinds share
	// text/plain and are told apart by sniffing, not by extension. Omitted for
	// an ordinary file inside a directory, which is served raw.
	Kind string `json:"kind,omitempty"`
	// CompSize is the stored post-compression size, the quota basis. Omitted
	// when unknown, where the uncompressed Size is the fallback.
	CompSize int `json:"csize,omitempty"`
}

func encodeManifest(m domain.Manifest) (string, error) {
	mj := manifestJSON{Files: make(map[string]entryJSON, len(m.Files))}
	for p, e := range m.Files {
		mj.Files[p] = entryJSON{SHA: e.SHA, Size: e.Size, CT: e.ContentType, Kind: e.Kind, CompSize: e.CompressedSize}
	}
	b, err := json.Marshal(mj)
	if err != nil {
		return "", fmt.Errorf("encode manifest: %w", err)
	}
	return string(b), nil
}

func decodeManifest(s string) (domain.Manifest, error) {
	var mj manifestJSON
	if err := json.Unmarshal([]byte(s), &mj); err != nil {
		return domain.Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	m := domain.NewManifest()
	for p, e := range mj.Files {
		m.Add(p, domain.ManifestEntry{SHA: e.SHA, Size: e.Size, ContentType: e.CT, Kind: e.Kind, CompressedSize: e.CompSize})
	}
	return m, nil
}
