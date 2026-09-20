package http

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// serveKnowledgeBase serves a directory of documents. The shell answers the
// root, every markdown path, and a route-shaped miss; ?raw=1 on a markdown path
// is the bytes it renders; every other file serves raw exactly as a site's
// does, so HTML keeps running as itself on its own origin instead of being
// sanitized into the shell.
func (s *Server) serveKnowledgeBase(w http.ResponseWriter, r *http.Request, slug domain.Slug,
	manifest domain.Manifest, uploadID string, updatedAt time.Time, reqPath string,
) {
	if wantsFileList(r) {
		s.serveFileList(w, r, manifest, uploadID, updatedAt, reqPath)
		return
	}

	entry, hit := manifest.Lookup(reqPath)
	// The root is the shell's even when no file resolves there: which document
	// opens is the shell's choice from the file list, not the server's.
	doc := reqPath == domain.Root || domain.IsMarkdownPath(reqPath)
	switch {
	case hit && doc && wantsRaw(r):
		s.streamDocument(w, r, slug, entry, updatedAt, reqPath)
		return
	case hit && !doc:
		s.serveFromManifest(w, r, slug, manifest, updatedAt, reqPath)
		return
	case !hit && domain.LooksLikeAsset(reqPath):
		// A missing .js or .png is a broken reference, not a place in the base.
		http.NotFound(w, r)
		return
	}

	shell := shellFor(domain.KindKnowledgeBase)
	h := w.Header()
	setSandboxHeaders(h)
	h.Set("Permissions-Policy", permissionsPolicy)
	// The shell is content-independent at every path it answers, so it caches
	// like a paste's and validates on its version.
	h.Set("Cache-Control", "public, max-age=3600")
	if notModified(w, r, `"`+shell.version+`"`, updatedAt) {
		return
	}
	h.Set("Content-Security-Policy", shell.policy())
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(shell.html(domain.KindKnowledgeBase))
}

// streamDocument writes one markdown document's raw bytes, what the shell
// fetches to render. Streamed, so a GET never buffers the whole file.
func (s *Server) streamDocument(w http.ResponseWriter, r *http.Request, slug domain.Slug,
	entry domain.ManifestEntry, updatedAt time.Time, reqPath string,
) {
	h := w.Header()
	setSandboxHeaders(h)
	h.Set("Permissions-Policy", permissionsPolicy)
	h.Set("Cache-Control", kbCacheControl(reqPath))
	if notModified(w, r, `"`+entry.Key+`"`, updatedAt) {
		return
	}
	rc, _, err := s.Blobs.Read(r.Context(), entry)
	if err != nil {
		s.logf("warn: knowledge base read 500: slug=%s document blob read: %v", slug, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rc.Close() //nolint:errcheck
	h.Set("Content-Type", rawContentType[domain.KindKnowledgeBase])
	_, _ = io.Copy(w, rc)
}

// serveFileList answers ?files=1 with the base's file paths as JSON, which is
// how the shell builds its tree and its search index without the server
// templating anything into the page. It covers the WHOLE base whichever of its
// paths carries the query, and is sorted so the shell's bounded index falls in
// a deterministic place.
func (s *Server) serveFileList(w http.ResponseWriter, r *http.Request,
	manifest domain.Manifest, uploadID string, updatedAt time.Time, reqPath string,
) {
	h := w.Header()
	setSandboxHeaders(h)
	h.Set("Permissions-Policy", permissionsPolicy)
	h.Set("Cache-Control", kbCacheControl(reqPath))
	// A deploy's manifest is immutable and its upload prefix names that deploy,
	// so the id validates the list without hashing the paths.
	if notModified(w, r, `"files-`+uploadID+`"`, updatedAt) {
		return
	}
	paths := make([]string, 0, len(manifest.Files))
	for p := range manifest.Files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	h.Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(paths)
}

// wantsFileList reports whether the request asked for the base's file list. A
// QUERY on the existing slug route, never a path of its own, so a base holding
// files.json or a _files/ directory cannot shadow it.
func wantsFileList(r *http.Request) bool { return r.URL.Query().Has("files") }

// kbCacheControl caches the ROOT's variants, which a purge covers
// (internal/cache/urls.go), and revalidates everything deeper, where a redeploy
// is visible only through the file's own object-key ETag.
func kbCacheControl(reqPath string) string {
	if reqPath == domain.Root {
		return "public, max-age=3600"
	}
	return "public, no-cache"
}
