// Package http serves the apex landing + the paste read surface.
//
// The router accepts both URL shapes simultaneously so the binary
// works whether the operator runs in subdomain mode (`<slug>.apex`)
// or path mode (`apex/p/<slug>`). The actual mode is set by what URL
// the SSH server emits after upload; the HTTP side just doesn't care.
package http

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
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

// BlobReader is the read side of the per-record blob seam, narrowed to
// what the http serve paths need. Read streams the bytes so every serve
// path (HTML, site files, and raw markdown) can io.Copy straight to the
// client without a full-payload allocation per GET. ReadAll buffers the
// whole blob; it is retained on the interface for callers that still need
// a buffered read, but the live serve paths use Read. Both take the
// record's slug (the route key) + its content sha; the standalone backend
// keys by sha alone and ignores the slug, the transactional shale backend
// uses the slug to route. service.BlobUnit satisfies this.
type BlobReader interface {
	ReadAll(ctx context.Context, slug, sha string) ([]byte, error)
	Read(ctx context.Context, slug, sha string) (io.ReadCloser, int64, error)
}

// Server bundles the dependencies.
type Server struct {
	Pastes      PasteReader
	Sites       SiteReader  // optional; nil disables static-site serving
	Rooms       RoomService // optional; nil disables the /api/rooms surface
	Relay       RoomRelay   // optional; nil disables the /api/rooms/<uuid>/ws relay
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
	// Client-render assets for markdown pastes. Registered as a fixed
	// prefix so ServeMux's longest-prefix match routes /_hostthis/<name>
	// here ahead of the "/" catch-all, on any Host. The handler
	// whitelists the asset names, so this prefix cannot be used to reach
	// any other path.
	mux.HandleFunc("/_hostthis/", s.serveAsset)
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
			// The /api/rooms surface is carved out of the site's path space
			// and handled BEFORE the static-file lookup, so a manifest file
			// can never shadow the API (and the API is served even for a
			// paste-only slug that owns no site).
			if rest, ok := roomAPIPath(r.URL.Path); ok {
				s.handleRoomsAPI(w, r, slug, rest)
				return
			}
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
		if after, ok := strings.CutPrefix(r.URL.Path, "/p/"); ok {
			rest := after
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
			// Dev path mode mirrors the subdomain carve-out: the rooms API
			// lives under /p/<slug>/api/rooms/... and is handled before the
			// static-file lookup so a manifest file never shadows it.
			if rest, ok := roomAPIPath(sitePath); ok {
				s.handleRoomsAPI(w, r, slug, rest)
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

	// Lifecycle status gate (docs/SPEC.md "Paste lifecycle status"). A
	// pending paste's blob has not landed yet: serve a loading page that
	// auto-refreshes until the finalizer flips it to ready. A failed paste
	// serves an error page. Only a ready paste falls through to the normal
	// content serve below.
	switch p.Status {
	case domain.PasteStatusPending:
		s.servePending(w, r)
		return
	case domain.PasteStatusFailed:
		s.serveFailed(w, r)
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

	// For the client-rendered kinds (markdown, diff) we serve one of two
	// things depending on what the client asks for: the RAW bytes (when
	// ?raw or a non-text/html Accept) or the fixed client-render shell.
	// The raw branch is content-addressed (ETag = content SHA, byte-stable).
	// The shell is content-INDEPENDENT, so its ETag is the shell version,
	// NOT the paste content - two different markdown/diff pastes yield the
	// same shell ETag. Decide here so the conditional-GET 304 below uses
	// the right validator.
	clientRendered := p.Kind == domain.KindMarkdown || p.Kind == domain.KindDiff
	rawWanted := clientRendered && wantsRaw(r)

	// ETag is the content SHA for HTML and raw markdown/diff - content-
	// addressed, byte-stable. Each shell uses its own shell version instead.
	etag := `"` + p.ContentSHA + `"`
	if clientRendered && !rawWanted {
		switch p.Kind {
		case domain.KindMarkdown:
			etag = `"` + mdShellVersion + `"`
		case domain.KindDiff:
			etag = `"` + diffShellVersion + `"`
		}
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

	switch p.Kind {
	case domain.KindHTML:
		// Stream the (decompressed) blob straight to the client. Avoids
		// buffering up to ~10 MiB per GET; the spike scaled with
		// concurrency on the small VPS. Headers above are already set;
		// the body is byte-identical to a buffered Get + Write.
		rc, _, err := s.Blobs.Read(r.Context(), string(slug), p.ContentSHA)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer rc.Close()
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.Copy(w, rc)
	case domain.KindMarkdown:
		// No server-side render. Either stream the raw bytes (the client
		// asked for them) or serve the fixed shell that renders in the
		// browser. Both keep server memory constant regardless of paste
		// size, mirroring the HTML path.
		if rawWanted {
			// Stream the raw markdown straight to the client - same
			// streaming shape as the HTML case, so no full-payload
			// allocation per GET.
			rc, _, err := s.Blobs.Read(r.Context(), string(slug), p.ContentSHA)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			defer func() { _ = rc.Close() }()
			h.Set("Content-Type", "text/markdown; charset=utf-8")
			_, _ = io.Copy(w, rc)
			return
		}
		// Fixed client-render shell. The shell loads marked + DOMPurify
		// and fetches the raw bytes itself. A tight CSP locks the page
		// down: only same-origin scripts/styles/connects, no inline
		// script, no framing.
		//
		// no-cache (override the shared max-age=3600): the shell is
		// content-independent and updated occasionally (a restyle bumps
		// mdShellVersion + the asset ?v=). Revalidating via the ETag each
		// view - a cheap 304 when unchanged - means a shell/style change is
		// seen on the next navigation instead of being pinned for an hour.
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", shellCSP)
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(shellHTML())
	case domain.KindDiff:
		// Identical shape to the markdown path: no server-side diffing.
		// Either stream the raw diff bytes (the client asked for them) or
		// serve the fixed shell that renders in the browser via diff2html +
		// highlight.js. Both keep server memory constant regardless of
		// paste size.
		if rawWanted {
			rc, _, err := s.Blobs.Read(r.Context(), string(slug), p.ContentSHA)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			defer func() { _ = rc.Close() }()
			// Serve the diff as plain text so a curl / non-browser client
			// sees it inline (the diff shell fetches these bytes itself).
			h.Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.Copy(w, rc)
			return
		}
		// Fixed client-render shell. Same CSP as the markdown shell - only
		// same-origin scripts/styles/connects (the vendored diff2html +
		// highlight.js + bootstrap, and the ?raw fetch), no inline script,
		// no framing. no-cache so a shell/style change (a diffShellVersion
		// bump) is seen on the next navigation rather than pinned for an hour.
		h.Set("Cache-Control", "no-cache")
		h.Set("Content-Security-Policy", shellCSP)
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(diffShellHTML())
	default:
		http.Error(w, "unsupported kind", http.StatusInternalServerError)
	}
}

// loadingPageHTML is the body served for a pending paste. It
// auto-refreshes every second (meta refresh, no JS required) until the
// finalizer flips the paste to ready and a refresh lands on the content.
// Kept tiny + on-brand: a centered monospace "preparing your paste" with
// a subtle pulse. The 200 status + no-store cache make every refresh hit
// the origin so the transition is seen promptly.
const loadingPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="1">
<title>preparing your paste</title>
<style>
  :root { color-scheme: light dark; }
  html, body { height: 100%; margin: 0; }
  body {
    display: flex; align-items: center; justify-content: center;
    font: 15px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
    background: #0e0e10; color: #e6e6e6;
  }
  .card { text-align: center; padding: 2rem; }
  .dot {
    display: inline-block; width: .6rem; height: .6rem; margin: 0 .15rem;
    border-radius: 50%; background: currentColor; opacity: .25;
    animation: pulse 1s infinite ease-in-out;
  }
  .dot:nth-child(2) { animation-delay: .15s; }
  .dot:nth-child(3) { animation-delay: .3s; }
  @keyframes pulse { 0%,100% { opacity: .25; } 50% { opacity: 1; } }
  .muted { color: #8a8a8a; margin-top: .75rem; font-size: 13px; }
</style>
</head>
<body>
  <div class="card">
    <div><span class="dot"></span><span class="dot"></span><span class="dot"></span></div>
    <p>preparing your paste</p>
    <p class="muted">this page refreshes automatically</p>
  </div>
</body>
</html>
`

// failedPageHTML is the body served for a failed paste (the blob write
// did not complete - object-store error or pod death mid-write). Same
// on-brand monospace shell as the loading page, no auto-refresh.
const failedPageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>paste unavailable</title>
<style>
  :root { color-scheme: light dark; }
  html, body { height: 100%; margin: 0; }
  body {
    display: flex; align-items: center; justify-content: center;
    font: 15px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
    background: #0e0e10; color: #e6e6e6;
  }
  .card { text-align: center; padding: 2rem; max-width: 28rem; }
  h1 { font-size: 1.1rem; margin: 0 0 .5rem; }
  .muted { color: #8a8a8a; font-size: 13px; }
</style>
</head>
<body>
  <div class="card">
    <h1>this paste could not be saved</h1>
    <p class="muted">the upload did not finish writing to storage. try uploading it again.</p>
  </div>
</body>
</html>
`

// servePending serves the loading page for a pending paste. 200 + no-store
// so the meta-refresh always re-checks the origin (a cached 200 would
// freeze the loading screen even after the paste went ready).
func (s *Server) servePending(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", "1")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, loadingPageHTML)
}

// serveFailed serves the error page for a failed paste. 410 Gone: the
// slug existed but its content will never arrive, which is exactly what
// Gone means, and it keeps the failed paste out of any naive success
// cache. no-store so a later re-upload to a (different) slug is unaffected.
func (s *Server) serveFailed(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusGone)
	_, _ = io.WriteString(w, failedPageHTML)
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

	// SPA fallback: a direct manifest hit serves that file; a miss that
	// looks like a client-side ROUTE (no extension or ".html") serves the
	// site's ROOT index.html with a 200 so the SPA's JS loads and routes;
	// a miss that looks like a missing static ASSET (a known asset
	// extension) stays a 404. The decision is a pure domain function -
	// see domain.Manifest.LookupWithSPAFallback + SPEC.md "SPA fallback
	// (route vs. asset)". A fallback hit is served byte-identically to
	// requesting "/" (same root index.html bytes, content-type, ETag, and
	// 200 status); only the request path differs.
	entry, hit, _ := site.Manifest.LookupWithSPAFallback(reqPath)
	if !hit {
		http.NotFound(w, r)
		return true
	}

	// Same sandbox headers as an HTML paste read (files served RAW, secured
	// by per-subdomain origin isolation, not by sanitizing the bytes).
	//
	// Cache posture differs from a single-file paste: a site is multi-file,
	// and a browser serves a site's sub-resources (its js/css) from cache
	// without revalidating while they are fresh under max-age - so a
	// re-deploy would not show until each asset's max-age expired (the
	// classic SPA "stale bundle after deploy" trap). no-cache makes every
	// site file revalidate via its content-SHA ETag on each load: a cheap
	// 304 when the SHA is unchanged, fresh bytes when it changed. So a
	// re-deploy is visible on the next normal reload, with no version-busting.
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")
	h.Set("Cache-Control", "public, no-cache")
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

	// Stream the (decompressed) site file straight to the client rather
	// than buffering the whole asset per GET. Headers above are already
	// set; the body is byte-identical to a buffered Get + Write.
	rc, _, err := s.Blobs.Read(r.Context(), string(slug), entry.SHA)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	defer rc.Close()
	ct := entry.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	_, _ = io.Copy(w, rc)
	return true
}

// shellCSP is set on every client-render shell response (markdown and
// diff). It locks the shell down: no default sources, scripts/styles/
// connects only from the same origin (the vendored libs + bootstrap + the
// ?raw fetch), images and media from anywhere (markdown can embed remote
// images), no inline script, no framing, no form submission.
// 'unsafe-inline' is allowed for styles only so the markdown's own inline
// styles (which DOMPurify keeps) and the diff renderer's injected styles
// render; scripts get no such escape hatch.
const shellCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: http: https:; media-src 'self' data: http: https:; font-src 'self' data: https:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// wantsRaw reports whether the client asked for the raw paste bytes rather
// than the client-render shell (markdown or diff). True when ?raw is present
// or when the Accept header does not include text/html (e.g. a curl default
// of */*, or an explicit text/markdown). A browser navigation sends
// Accept: text/html,... and so gets the shell.
func wantsRaw(r *http.Request) bool {
	return r.URL.Query().Has("raw") || !strings.Contains(r.Header.Get("Accept"), "text/html")
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
	for candidate := range strings.SplitSeq(ifNoneMatch, ",") {
		if strings.TrimSpace(candidate) == etag {
			return true
		}
	}
	return false
}
