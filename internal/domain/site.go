package domain

import (
	"errors"
	"path"
	"sort"
	"strings"
	"time"
)

// Site is the aggregate for a static-site upload: a directory of files served
// off a single slug, sharing Paste's slug shape, identity, and retention clock.
//
// The served bytes are addressed indirectly: the Manifest maps each safe
// relative path to the SHA256 of its uncompressed blob, so the
// content-addressed BlobStore dedupes identical files across deploys and
// across sites for free.
type Site struct {
	Slug     Slug
	Identity Identity // owner; "key:<fp>" - quota AND ownership gate
	Manifest Manifest // path -> blob ref (sha + size + content-type)
	// StoredBytes is what the quota charged for this site: the deduped total of
	// the STORED (post-zstd) blob sizes, recorded at deploy.
	//
	// Carried on the Site rather than recomputed from Manifest because the
	// per-entry compressed sizes are not persisted, so a loaded manifest cannot
	// reproduce this figure. Zero on a Site that has not been read from storage.
	StoredBytes int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time // UpdatedAt + Retention window (or NeverExpires)
}

// ManifestEntry is one file in a site. ContentType is a function of the
// path's extension alone, no I/O.
type ManifestEntry struct {
	SHA            string // sha256 of the file's uncompressed bytes
	Size           int    // uncompressed bytes (for display)
	CompressedSize int    // stored post-zstd bytes; the quota basis (matches how pastes charge)
	ContentType    string // by extension; see contentTypeByExt
}

// Manifest maps each safe, site-root-relative path to its blob ref. Pure
// value object: every operation on it is I/O-free.
//
// Paths are always cleaned, slash-separated, and relative (never leading "/"),
// enforced by the safe-untar that produces them.
type Manifest struct {
	Files map[string]ManifestEntry
}

// Limits on a single site deploy. These bound the untar so a hostile archive
// cannot exhaust file descriptors, inodes, or metadata-store space even when
// each file is tiny.
const (
	// MaxSiteFiles caps the number of regular-file entries in one site.
	MaxSiteFiles = 5000
	// MaxSitePathLen caps a single entry's cleaned path length, so a
	// pathological deep/long name can't bloat the manifest.
	MaxSitePathLen = 1024
	// MaxManifestBytes bounds the total size of all path strings in a
	// manifest, a guard on metadata-store footprint independent of the file
	// count.
	MaxManifestBytes = 1 << 20 // 1 MiB of path text
	// MaxSiteBytes caps the total UNCOMPRESSED bytes a single site may extract
	// to. The decompression-bomb guard aborts the untar the instant the running
	// total would exceed this OR the identity's available quota, whichever is
	// smaller.
	//
	// Declared as its own value rather than an alias of UserQuotaBytes because
	// that is a var (test-shrinkable) and a const cannot reference one. The two
	// must stay equal; MaxSiteBytesMatchesQuota pins that so a quota change
	// cannot silently leave the untar guard behind.
	MaxSiteBytes = 100 << 20 // 100 MiB, tracking UserQuotaBytes
)

// Errors the safe-untar surfaces. All abort the whole deploy: a
// half-extracted site is never persisted.
var (
	// ErrUnsafeArchive is the zip-slip / tar-traversal guard: a tar entry that
	// is not a regular file or directory (symlink, hardlink, device, FIFO), or
	// whose path is absolute, contains "..", or otherwise escapes the site root.
	ErrUnsafeArchive = errors.New("archive contains an unsafe entry (symlink, traversal, or non-regular file)")
	// ErrArchiveTooLarge is the decompression-bomb guard: the running
	// uncompressed total would exceed the site/quota byte cap mid-stream.
	ErrArchiveTooLarge = errors.New("archive expands beyond the allowed size")
	// ErrTooManyFiles is returned when the entry count would exceed
	// MaxSiteFiles or the manifest path text would exceed MaxManifestBytes.
	ErrTooManyFiles = errors.New("archive has too many files")
	// ErrNoWebContent is returned when an archive safe-untars cleanly but
	// holds no web content (no index.html and no .html/.css/.js file).
	ErrNoWebContent = errors.New("archive has no web content (need an index.html or .html/.css/.js file)")
)

// NewManifest returns an empty manifest.
func NewManifest() Manifest { return Manifest{Files: make(map[string]ManifestEntry)} }

// Add records one file at the cleaned relative path. The caller (the
// safe-untar) is responsible for having cleaned + validated the path.
func (m Manifest) Add(p string, e ManifestEntry) { m.Files[p] = e }

// commonLeadingDir returns the single top-level directory shared by EVERY
// file, or "" when the files live at the root or span more than one top-level
// directory.
func (m Manifest) commonLeadingDir() string {
	prefix := ""
	first := true
	for p := range m.Files {
		before, _, ok := strings.Cut(p, "/")
		if !ok {
			return "" // a file sits at the root; nothing to strip
		}
		top := before
		if first {
			prefix, first = top, false
		} else if top != prefix {
			return "" // files span multiple top-level dirs
		}
	}
	return prefix
}

// StripCommonLeadingDir removes a single shared top-level directory from every
// path when ALL files live under it, so the natural `tar czf - site/` serves
// index.html at the root instead of 404ing there. No-op when files are already
// at the root or span multiple top-level directories. Stripping a shared
// prefix preserves distinctness, so it can never collide two entries onto one
// key.
func (m *Manifest) StripCommonLeadingDir() {
	dir := m.commonLeadingDir()
	if dir == "" {
		return
	}
	stripped := make(map[string]ManifestEntry, len(m.Files))
	for p, e := range m.Files {
		stripped[strings.TrimPrefix(p, dir+"/")] = e
	}
	m.Files = stripped
}

// Lookup resolves a request path to a manifest entry, applying the
// directory-index rule:
//
//   - "" or "/" or any path ending in "/" resolves to "<dir>index.html"
//     if that entry exists.
//   - an exact path match serves that file.
//   - a path that names a directory (its "<p>/index.html" exists) also
//     resolves to that index, so "/blog" and "/blog/" both work.
//
// No SPA fallback and no traversal here: an unmatched path is a clean miss the
// HTTP layer turns into a 404.
func (m Manifest) Lookup(reqPath string) (ManifestEntry, bool) {
	clean := strings.TrimPrefix(reqPath, "/")

	if clean == "" || strings.HasSuffix(clean, "/") {
		idx := clean + "index.html"
		if e, ok := m.Files[idx]; ok {
			return e, true
		}
		return ManifestEntry{}, false
	}

	if e, ok := m.Files[clean]; ok {
		return e, true
	}

	// Bare directory name (no trailing slash) with an index.html under it.
	if e, ok := m.Files[clean+"/index.html"]; ok {
		return e, true
	}

	return ManifestEntry{}, false
}

// assetExtensions is the set of file extensions the SPA fallback treats as
// REAL static assets: a manifest miss ending in one of these 404s, a miss with
// any other extension (or none) is a client-side ROUTE and gets the root
// index.html. See SPEC.md "SPA fallback (route vs. asset)".
//
// Enumerating the ASSET set rather than the route set is what makes a novel
// route shape default to the SPA index instead of a 404. ".html" is
// deliberately absent: a missing ".html" path is a pre-rendered route the
// build did not emit, so it routes through the SPA too.
var assetExtensions = map[string]struct{}{
	".js": {}, ".mjs": {}, ".cjs": {}, ".css": {}, ".json": {}, ".map": {},
	".xml": {}, ".txt": {}, ".csv": {}, ".pdf": {}, ".wasm": {}, ".webmanifest": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".webp": {},
	".avif": {}, ".svg": {}, ".ico": {}, ".bmp": {},
	".woff": {}, ".woff2": {}, ".ttf": {}, ".otf": {}, ".eot": {},
	// Media: a missing one must 404, not get served the index.html as text/html.
	".mp4": {}, ".webm": {}, ".mov": {}, ".m4v": {}, ".ogv": {},
	".mp3": {}, ".wav": {}, ".ogg": {}, ".flac": {}, ".m4a": {}, ".aac": {},
	// Pre-compressed bundles + common data/binary assets.
	".gz": {}, ".br": {}, ".zip": {}, ".bin": {}, ".dat": {},
}

// looksLikeAsset reports whether reqPath's LAST segment has a known
// static-asset extension, so "/users/123/edit" is a route and "/img/logo.png"
// is an asset.
func looksLikeAsset(reqPath string) bool {
	ext := strings.ToLower(path.Ext(path.Base(reqPath)))
	_, ok := assetExtensions[ext]
	return ok
}

// LookupWithSPAFallback resolves reqPath like Lookup, but on a miss applies
// the SPA fallback: a path that looks like a client-side ROUTE (no extension,
// or a ".html" one) resolves to the site's ROOT index.html, while a missing
// static ASSET stays a miss.
//
// Three outcomes encoded in two bools:
//   - hit && !viaFallback: a direct manifest entry, served normally.
//   - hit && viaFallback: the root index.html for a client-side route; the
//     HTTP layer still responds 200, exactly as if "/" were requested.
//   - !hit (viaFallback always false): a 404. Covers a missing asset AND a
//     route on a site with no root index.html to fall back to.
func (m Manifest) LookupWithSPAFallback(reqPath string) (entry ManifestEntry, hit, viaFallback bool) {
	if e, ok := m.Lookup(reqPath); ok {
		return e, true, false
	}
	if looksLikeAsset(reqPath) {
		return ManifestEntry{}, false, false
	}
	if e, ok := m.Files["index.html"]; ok {
		return e, true, true
	}
	return ManifestEntry{}, false, false
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

// DedupedSize returns the total UNCOMPRESSED bytes the manifest's DISTINCT
// blobs occupy: two paths pointing at the same blob (identical file content)
// count once. Display figure; CompressedDedupedSize is the quota basis.
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

// CompressedDedupedSize is the distinct-blob total of the STORED (post-zstd)
// sizes, the number charged against the per-identity quota (matching how
// pastes charge). Dedup is by SHA, so a file referenced N times counts its
// compressed size once.
func (m Manifest) CompressedDedupedSize() int {
	seen := make(map[string]int, len(m.Files))
	for _, e := range m.Files {
		seen[e.SHA] = e.CompressedSize
	}
	var total int
	for _, sz := range seen {
		total += sz
	}
	return total
}

// PathTextBytes returns the total byte length of all path keys, so the untar
// can bound manifest metadata footprint (MaxManifestBytes).
func (m Manifest) PathTextBytes() int {
	var n int
	for p := range m.Files {
		n += len(p)
	}
	return n
}

// SHASet returns the set of distinct blob SHAs the manifest references. The
// storage layer uses it for GC accounting: which blobs a live site keeps alive.
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

// contentTypeByExt maps a file extension to a content-type, purely by name.
// Anything unknown gets application/octet-stream so an unexpected extension is
// served as a download, never mislabeled as text/html (which would let
// arbitrary bytes run as script on the origin).
//
// The ONE place content-type is decided for site files: it is a domain
// decision (a property of the name), not an infrastructure one.
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
		// Served raw, not rendered (rendering is the single-file paste path).
		// Plain text so it isn't run as markup.
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

// ContentTypeForPath exposes contentTypeByExt so the mapping has exactly one
// definition.
func ContentTypeForPath(p string) string { return contentTypeByExt(p) }
