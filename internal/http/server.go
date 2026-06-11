// Package http serves the apex landing + the paste read surface.
//
// The router accepts both URL shapes simultaneously so the binary
// works whether the operator runs in subdomain mode (`<slug>.apex`)
// or path mode (`apex/p/<slug>`). The actual mode is set by what URL
// the SSH server emits after upload; the HTTP side just doesn't care.
package http

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/render"
	"github.com/Zamua/hostthis/internal/storage"
)

// PasteReader is the read-side interface - same shape the service
// layer uses, intentionally narrow (this package doesn't need
// Insert).
type PasteReader interface {
	Get(domain.Slug) (domain.Paste, error)
}

// SiteReader is the read-side interface for static sites. Optional:
// nil disables site serving (the slug then resolves only as a paste).
// internal/storage.SiteRepo satisfies it.
type SiteReader interface {
	Get(domain.Slug) (domain.Site, error)
}

// BlobReader fetches paste bytes by content sha.
type BlobReader interface {
	Get(sha string) ([]byte, error)
}

// Server bundles the dependencies.
type Server struct {
	Pastes      PasteReader
	Sites       SiteReader // optional; nil disables static-site serving
	Blobs       BlobReader
	LandingHTML []byte // optional - apex landing page bytes embedded at build
	ApexDomain  string // e.g. "hostthis.dev" - used to peel slug subdomains
	// Color labels the replica in blue/green deploys. Echoed in the
	// X-Backend-Color response header on /healthz so operators can verify
	// which backend is responding. Empty for single-replica deploys.
	Color string
	Now   func() time.Time
}

func (s *Server) nowOrTime() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Handler returns the mux that the caller binds with http.ListenAndServe.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 0. Health endpoint - apex only, no Host-based routing. Used by
		// load balancers / haproxy / nginx to decide if this backend is
		// ready to take traffic. Cheap: just confirms the HTTP server is
		// up. Container startup opens the db + verifies blob backend
		// before binding the http listener, so an HTTP response from
		// here means the backend is healthy enough to serve.
		if r.URL.Path == "/healthz" {
			s.serveHealthz(w, r)
			return
		}
		// 1. Subdomain mode: Host like "<slug>.<apex>".
		//
		// A slug can resolve to a SITE (a directory served off its whole
		// path space) or a single-file PASTE (served only at "/"). We try
		// the site first - if a site owns the slug, every path on the
		// subdomain routes into its manifest. Otherwise we fall back to
		// the paste, which serves ONLY at "/" (any other path 404s, so a
		// browser's automatic favicon fetch doesn't get the full paste
		// HTML labeled text/html and hang the loading indicator).
		if slug, ok := s.slugFromHost(r.Host); ok {
			if s.serveSiteIfExists(w, r, slug, r.URL.Path) {
				return
			}
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			s.servePasteSlug(w, r, slug)
			return
		}
		// 2. Path mode: /p/<slug> (paste) or /p/<slug>/<path...> (site)
		// on the apex.
		if strings.HasPrefix(r.URL.Path, "/p/") {
			rest := strings.TrimPrefix(r.URL.Path, "/p/")
			// Split the first segment (the slug) from the remaining site
			// path, if any. "/p/abc12345" → slug "abc12345", path "/".
			// "/p/abc12345/css/x.css" → slug "abc12345", path "/css/x.css".
			slugStr := rest
			sitePath := "/"
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				slugStr = rest[:i]
				sitePath = rest[i:]
			}
			slug, err := domain.ParseSlug(slugStr)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			if s.serveSiteIfExists(w, r, slug, sitePath) {
				return
			}
			// Not a site: a paste serves only at the bare slug path.
			if sitePath != "/" {
				http.NotFound(w, r)
				return
			}
			s.servePasteSlug(w, r, slug)
			return
		}
		// 3. Apex root → landing.
		if r.URL.Path == "/" {
			s.serveLanding(w, r)
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

func (s *Server) serveHealthz(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if s.Color != "" {
		h.Set("X-Backend-Color", s.Color)
	}
	_, _ = w.Write([]byte("ok\n"))
}

// slugFromHost returns (slug, true) when host is "<slug>.<apex>" and
// the slug parses cleanly. Otherwise (_, false). Strips the port if
// present.
func (s *Server) slugFromHost(host string) (domain.Slug, bool) {
	if s.ApexDomain == "" {
		return "", false
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	suffix := "." + s.ApexDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	sub := strings.TrimSuffix(host, suffix)
	if strings.Contains(sub, ".") {
		// Multi-level subdomain (e.g. "x.y.apex") - not a slug, ignore.
		return "", false
	}
	slug, err := domain.ParseSlug(sub)
	if err != nil {
		return "", false
	}
	return slug, true
}

func (s *Server) serveLanding(w http.ResponseWriter, _ *http.Request) {
	if len(s.LandingHTML) == 0 {
		// Dev/test default - operator can override at startup with the
		// real bytes from web/landing.html.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "hostthis - landing page not embedded.")
		return
	}
	// Landing changes more often than pastes (rare edits, new copy);
	// 5-min cache balances "operators can ship a copy fix and see it
	// in minutes" against not hammering origin for every visitor.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.LandingHTML)
}

// servePasteSlug serves the paste for the given slug, with all the
// sandboxing headers. Both the subdomain and the path entry points
// funnel through here.
func (s *Server) servePasteSlug(w http.ResponseWriter, r *http.Request, slug domain.Slug) {
	p, err := s.Pastes.Get(slug)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.nowOrTime()
	if !now.Before(p.ExpiresAt) {
		// Past the retention window. The background sweep will delete this
		// shortly; we 404 in the meantime so visitors don't see
		// content that's technically expired.
		http.NotFound(w, r)
		return
	}

	// Sandboxing headers per SPEC.md HTML-sandboxing section.
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")

	// Cache-Control: 1h max-age is the sweet spot for hostthis. Active
	// changes (update/delete) fire an explicit purge via CachePurger;
	// passive expiry gets at most 1h of staleness which is acceptable.
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("Last-Modified", p.UpdatedAt.UTC().Format(http.TimeFormat))

	// ETag is the content SHA for HTML - content-addressed, byte-stable.
	// For markdown the rendered output depends on the renderer version,
	// so we mix that in so that a renderer bump invalidates the cache
	// without us having to manually purge.
	etag := `"` + p.ContentSHA + `"`
	if p.Kind == domain.KindMarkdown {
		etag = `"` + p.ContentSHA + "-" + render.MarkdownRendererVersion + `"`
	}
	h.Set("ETag", etag)

	// Conditional GET: 304 if either If-None-Match or If-Modified-Since says so.
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if since, err := http.ParseTime(ims); err == nil && !p.UpdatedAt.UTC().Truncate(time.Second).After(since) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	body, err := s.Blobs.Get(p.ContentSHA)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	switch p.Kind {
	case domain.KindHTML:
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	case domain.KindMarkdown:
		// Render markdown → sanitized HTML on every read. The render
		// is pure and cheap (~1ms for typical docs); a cache keyed
		// on (ContentSHA, render.MarkdownRendererVersion) can land
		// later if cold renders become hot.
		rendered, err := render.Markdown(body)
		if err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(rendered)
	default:
		http.Error(w, "unsupported kind", http.StatusInternalServerError)
	}
}

// serveSiteIfExists tries to serve reqPath from the static site owning
// slug. Returns true if it handled the request (served a file, 404'd a
// path inside an existing site, or 404'd an expired site) - the caller
// must then return. Returns false ONLY when no site owns the slug, so
// the caller can fall through to the paste path. This keeps the slug
// namespace unified: one slug is either a site or a paste, never both.
func (s *Server) serveSiteIfExists(w http.ResponseWriter, r *http.Request, slug domain.Slug, reqPath string) bool {
	if s.Sites == nil {
		return false
	}
	site, err := s.Sites.Get(slug)
	if err != nil {
		// Not a site (or a read error) - fall through to the paste path.
		// A storage error here is indistinguishable from "no such site"
		// on purpose: we let the paste path try, and it will surface its
		// own 404 / 500 if the slug isn't a paste either.
		return false
	}
	now := s.nowOrTime()
	if !now.Before(site.ExpiresAt) {
		// Expired: 404 here (we own the slug) rather than falling through.
		http.NotFound(w, r)
		return true
	}

	entry, ok := site.Manifest.Lookup(reqPath)
	if !ok {
		// A path that doesn't match any manifest entry is a clean 404.
		// No SPA fallback in this version (an unmatched route does NOT
		// serve the root index.html).
		http.NotFound(w, r)
		return true
	}

	// Same sandbox headers + cache posture as an HTML paste read: files
	// are served RAW, secured by per-subdomain origin isolation, not by
	// sanitizing the bytes.
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("Last-Modified", site.UpdatedAt.UTC().Format(http.TimeFormat))

	// ETag is the file's content SHA - content-addressed, byte-stable.
	etag := `"` + entry.SHA + `"`
	h.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if since, err := http.ParseTime(ims); err == nil && !site.UpdatedAt.UTC().Truncate(time.Second).After(since) {
			w.WriteHeader(http.StatusNotModified)
			return true
		}
	}

	body, err := s.Blobs.Get(entry.SHA)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	ct := entry.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	_, _ = w.Write(body)
	return true
}

// etagMatches checks if the client's If-None-Match header lists our
// etag. Supports the comma-separated form and the "*" wildcard.
func etagMatches(ifNoneMatch, etag string) bool {
	if ifNoneMatch == "" {
		return false
	}
	if strings.TrimSpace(ifNoneMatch) == "*" {
		return true
	}
	for _, candidate := range strings.Split(ifNoneMatch, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}
