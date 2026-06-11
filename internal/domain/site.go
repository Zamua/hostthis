package domain

import (
	"errors"
	"path"
	"sort"
	"strings"
	"time"
)

// Site is the aggregate for a static-site upload: a directory of files
// served off a single slug. It lives alongside Paste and shares the
// same slug shape, identity, and 7-day retention clock - a site is
// "a paste that happens to be a directory."
//
// The served bytes are addressed indirectly: the Manifest maps each
// safe relative path to the SHA256 of its (uncompressed) blob, so the
// content-addressed BlobStore dedupes identical files across deploys
// and across sites for free.
type Site struct {
	Slug      Slug
	Identity  Identity // owner; "key:<fp>" - quota AND ownership gate
	Manifest  Manifest // path -> blob ref (sha + size + content-type)
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time // UpdatedAt + RetentionWindow
}

// ManifestEntry is one file in a site: the blob it points at, the
// uncompressed byte size, and the content-type derived purely from
// the path's extension. No I/O - the type is a function of the name.
type ManifestEntry struct {
	SHA         string // sha256 of the file's uncompressed bytes
	Size        int    // uncompressed bytes
	ContentType string // by extension; see contentTypeByExt
}

// Manifest maps each safe, site-root-relative path to its blob ref.
// It is a pure value object: building it, looking a path up, computing
// the deduped storage total, and resolving directory index files are
// all I/O-free.
//
// Paths are always cleaned, slash-separated, and relative (never
// leading "/"), enforced by the safe-untar that produces them.
type Manifest struct {
	Files map[string]ManifestEntry
}

// Limits on a single site deploy. These bound the untar so a hostile
// archive cannot exhaust file descriptors, inodes, or metadata-store
// space even when each file is tiny. Tuned generously for real static
// sites (a few thousand files is already a large site) while keeping
// a "million tiny files" archive cheaply rejectable.
const (
	// MaxSiteFiles caps the number of regular-file entries in one site.
	MaxSiteFiles = 5000
	// MaxSitePathLen caps a single entry's cleaned path length, so a
	// pathological deep/long name can't bloat the manifest.
	MaxSitePathLen = 1024
	// MaxManifestBytes bounds the total size of all path strings in a
	// manifest, a second guard on metadata-store footprint independent
	// of the file count.
	MaxManifestBytes = 1 << 20 // 1 MiB of path text
	// MaxSiteBytes caps the total UNCOMPRESSED bytes a single site may
	// extract to. The decompression-bomb guard aborts the untar the
	// instant the running total would exceed this OR the identity's
	// available quota, whichever is smaller. A site is text/web content,
	// so this sits at the per-identity quota - a site can fill a user's
	// whole budget but never exceed it.
	MaxSiteBytes = UserQuotaBytes
)

// Errors the safe-untar surfaces. All abort the whole deploy: a
// half-extracted site is never persisted.
var (
	// ErrUnsafeArchive is returned when a tar entry is unsafe: not a
	// regular file or directory (symlink, hardlink, device, FIFO), or a
	// path that is absolute, contains "..", or otherwise escapes the
	// site root. The zip-slip / tar-traversal guard.
	ErrUnsafeArchive = errors.New("archive contains an unsafe entry (symlink, traversal, or non-regular file)")
	// ErrArchiveTooLarge is returned when the running uncompressed total
	// would exceed the site/quota byte cap mid-stream. The decompression-
	// bomb guard.
	ErrArchiveTooLarge = errors.New("archive expands beyond the allowed size")
	// ErrTooManyFiles is returned when the entry count would exceed
	// MaxSiteFiles or the manifest path text would exceed MaxManifestBytes.
	ErrTooManyFiles = errors.New("archive has too many files")
	// ErrNoWebContent is returned when an archive safe-untars cleanly but
	// holds no web content (no index.html and no .html/.css/.js file).
	// Routed to the same rejection as any unsupported upload.
	ErrNoWebContent = errors.New("archive has no web content (need an index.html or .html/.css/.js file)")
)

// NewManifest returns an empty, ready-to-fill manifest.
func NewManifest() Manifest { return Manifest{Files: make(map[string]ManifestEntry)} }

// Add records one file at the cleaned relative path. Caller (the
// safe-untar) is responsible for having cleaned + validated the path;
// Add only stores it.
func (m Manifest) Add(p string, e ManifestEntry) { m.Files[p] = e }

// Lookup resolves a request path to a manifest entry, applying the
// directory-index rule:
//
//   - "" or "/" or any path ending in "/" resolves to "<dir>index.html"
//     if that entry exists.
//   - an exact path match serves that file.
//   - a path that names a directory (its "<p>/index.html" exists) also
//     resolves to that index, so "/blog" and "/blog/" both work.
//
// Returns (entry, true) on a hit, (zero, false) on a miss. Pure: no
// SPA fallback, no traversal - an unmatched path is a clean miss the
// HTTP layer turns into a 404.
func (m Manifest) Lookup(reqPath string) (ManifestEntry, bool) {
	clean := strings.TrimPrefix(reqPath, "/")

	// Directory root or trailing-slash directory: serve its index.html.
	if clean == "" || strings.HasSuffix(clean, "/") {
		idx := clean + "index.html"
		if e, ok := m.Files[idx]; ok {
			return e, true
		}
		return ManifestEntry{}, false
	}

	// Exact file match.
	if e, ok := m.Files[clean]; ok {
		return e, true
	}

	// Bare directory name (no trailing slash) with an index.html under it.
	if e, ok := m.Files[clean+"/index.html"]; ok {
		return e, true
	}

	return ManifestEntry{}, false
}

// HasWebContent reports whether the manifest holds at least one piece
// of web content: an index.html anywhere, or any .html / .css / .js
// file. An archive with none of these is not a site (see ErrNoWebContent).
func (m Manifest) HasWebContent() bool {
	for p := range m.Files {
		base := path.Base(p)
		if base == "index.html" {
			return true
		}
		switch strings.ToLower(path.Ext(p)) {
		case ".html", ".htm", ".css", ".js", ".mjs":
			return true
		}
	}
	return false
}

// DedupedSize returns the total UNCOMPRESSED bytes the manifest's
// DISTINCT blobs occupy. Two manifest paths pointing at the same blob
// (identical file content) count once - this is the number charged
// against the per-identity quota, matching the "dedupe for free"
// storage property.
func (m Manifest) DedupedSize() int {
	seen := make(map[string]int, len(m.Files))
	for _, e := range m.Files {
		seen[e.SHA] = e.Size
	}
	var total int
	for _, sz := range seen {
		total += sz
	}
	return total
}

// PathTextBytes returns the total byte length of all path keys. Used
// by the untar to bound manifest metadata footprint (MaxManifestBytes).
func (m Manifest) PathTextBytes() int {
	var n int
	for p := range m.Files {
		n += len(p)
	}
	return n
}

// SHASet returns the set of distinct blob SHAs the manifest references.
// The storage layer uses it for quota/GC accounting (which blobs a live
// site keeps alive).
func (m Manifest) SHASet() []string {
	seen := make(map[string]struct{}, len(m.Files))
	for _, e := range m.Files {
		seen[e.SHA] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for sha := range seen {
		out = append(out, sha)
	}
	sort.Strings(out) // deterministic order for stable serialization/tests
	return out
}

// contentTypeByExt maps a file extension to a content-type, purely by
// name. The set is the common static-site palette; anything unknown
// gets application/octet-stream so an unexpected extension is served as
// a download, never mislabeled as text/html (which would let arbitrary
// bytes run as script on the origin).
//
// This is the ONE place content-type is decided for site files - it is
// a domain decision (a property of the name), not an infrastructure one.
func contentTypeByExt(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".map":
		return "application/json; charset=utf-8"
	case ".xml":
		return "application/xml; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".md", ".markdown":
		// Served raw as text in a site (NOT rendered - rendering is the
		// single-file paste path). Plain text so it isn't run as markup.
		return "text/plain; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".eot":
		return "application/vnd.ms-fontobject"
	case ".webmanifest":
		return "application/manifest+json; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	case ".pdf":
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

// ContentTypeForPath exposes contentTypeByExt for callers building a
// manifest entry. Kept thin so the mapping has exactly one definition.
func ContentTypeForPath(p string) string { return contentTypeByExt(p) }
