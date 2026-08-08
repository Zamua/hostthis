// Package http serves the apex landing + the paste read surface.
//
// The router accepts both URL shapes at once, subdomain (`<slug>.apex`) and
// path (`apex/p/<slug>`), so the serving side is independent of which URL
// the SSH server emits after upload.
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
)

// PasteReader is the read side of the paste repo, narrowed to what serving
// needs.
type PasteReader interface {
	Get(domain.Slug) (domain.Paste, error)
}

// SiteReader is the read side for static sites. Optional: nil disables site
// serving, so a slug then resolves only as a paste. internal/storage.SiteRepo
// satisfies it.
type SiteReader interface {
	Get(domain.Slug) (domain.Site, error)
}

// BlobReader is the read side of the per-record blob seam. Read streams, so a
// serve path can io.Copy without a full-payload allocation per GET; ReadAll
// buffers. Both take the record's slug plus its content sha: the standalone
// backend keys by sha alone and ignores the slug, the transactional shale
// backend routes on it. service.BlobUnit satisfies this.
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
	LandingHTML []byte // optional; apex landing page bytes embedded at build
	ApexDomain  string // e.g. "hostthis.dev"; used to peel slug subdomains
	// Color labels the replica in blue/green deploys, echoed in the
	// X-Backend-Color header on /healthz. Empty for single-replica deploys.
	Color string
	// Readiness gates /readyz (docs/SPEC.md "Readiness vs liveness") with the
	// metadata backend's readiness predicate. Optional; nil means always
	// ready, for backends with no mount concept whose open failures already
	// fail startup.
	Readiness ReadinessProber
	Now       func() time.Time
	// Logf, when set, receives one warn line per 5xx served by the paste/site
	// read path (docs/SPEC.md "5xx observability on the read surface"): slug +
	// underlying error, so a read 500 is attributable from the logs while the
	// response body stays a generic "internal error". Nil disables.
	Logf func(format string, args ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Handler returns the mux the caller binds with http.ListenAndServe.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Fixed prefix so ServeMux's longest-prefix match routes /_hostthis/<name>
	// here ahead of the "/" catch-all, on any Host. serveAsset whitelists the
	// asset names, so the prefix cannot reach any other path.
	mux.HandleFunc("/_hostthis/", s.serveAsset)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /healthz and /readyz answer on any Host, ungated, and ask different
		// questions: /healthz is liveness (process up, a restart signal),
		// /readyz applies the metadata backend's readiness predicate so a
		// rollout stalls on a pod that cannot mount its storage instead of
		// replacing the fleet. See docs/SPEC.md "Readiness vs liveness".
		if r.URL.Path == "/healthz" {
			s.serveHealthz(w, r)
			return
		}
		if r.URL.Path == "/readyz" {
			s.serveReadyz(w, r)
			return
		}
		// Subdomain mode: Host like "<slug>.<apex>". A slug resolves to a SITE
		// (owning its whole path space) or a single-file PASTE. Site wins;
		// the paste fallback serves ONLY at "/" so a browser's automatic
		// favicon fetch does not receive the paste HTML labeled text/html.
		if slug, ok := s.slugFromHost(r.Host); ok {
			// The /api/rooms surface is handled BEFORE the static-file lookup
			// so a manifest file can never shadow the API, and the API is
			// served even for a paste-only slug that owns no site.
			if rest, ok := roomAPIPath(r.URL.Path); ok {
				s.handleRoomsAPI(w, r, slug, rest)
				return
			}
			s.serveSlug(w, r, slug, r.URL.Path)
			return
		}
		// Path mode: /p/<slug> (paste) or /p/<slug>/<path...> (site) on the
		// apex.
		if after, ok := strings.CutPrefix(r.URL.Path, "/p/"); ok {
			rest := after
			// "/p/abc12345" -> slug "abc12345", path "/".
			// "/p/abc12345/css/x.css" -> slug "abc12345", path "/css/x.css".
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
			// Same carve-out as subdomain mode: the rooms API is handled
			// before the static-file lookup so a manifest file never shadows
			// it.
			if rest, ok := roomAPIPath(sitePath); ok {
				s.handleRoomsAPI(w, r, slug, rest)
				return
			}
			s.serveSlug(w, r, slug, sitePath)
			return
		}
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

// slugFromHost returns (slug, true) when host is "<slug>.<apex>" and the slug
// parses cleanly. The port, if present, is ignored.
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
		// Multi-level subdomain ("x.y.apex") is not a slug.
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
		// Dev/test default; a deploy embeds web/landing.html.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "hostthis - landing page not embedded.")
		return
	}
	// Short max-age: landing copy changes more often than a paste, and a
	// copy fix should be visible within minutes without hitting origin per
	// visitor.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.LandingHTML)
}

// servePasteSlug serves a paste with its sandboxing headers. Both the
// subdomain and the path routes funnel through here.
// servePaste resolves reqPath against the paste owning slug.
//
// ONE head read decides everything: the head carries the served version's whole
// descriptor, including its manifest, so a directory's file lookup and a
// single file's render both answer from it. There is one family: a slug with no
// paste is not found.
func (s *Server) serveSlug(w http.ResponseWriter, r *http.Request, slug domain.Slug, reqPath string) {
	// A deploy may wire either surface alone, so neither reader is assumed
	// present; the same nil-safety the site reader has always had.
	if s.Pastes == nil {
		if !s.serveSiteIfExists(w, r, slug, reqPath) {
			http.NotFound(w, r)
		}
		return
	}
	p, err := s.Pastes.Get(slug)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			if s.serveSiteIfExists(w, r, slug, reqPath) {
				return
			}
			http.NotFound(w, r)
			return
		}
		s.logf("warn: paste read 500: slug=%s metadata get: %v", slug, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The SHAPE is declared by the paste's kind, never inferred from how
	// many entries its manifest holds: a directory of one file is still a
	// directory, and serving it as a document would render it instead of
	// handing back the bytes.
	if p.Kind == domain.KindSite {
		s.serveFromManifest(w, r, slug, p.Manifest, p.UpdatedAt, reqPath)
		return
	}
	// A document answers only at its own URL; deeper paths belong to a
	// directory.
	if reqPath != "/" {
		http.NotFound(w, r)
		return
	}
	s.servePaste(w, r, slug, p)
}

// servePaste serves a resolved single-file paste with its sandboxing headers.
func (s *Server) servePaste(w http.ResponseWriter, r *http.Request, slug domain.Slug, p domain.Paste) {
	// Lifecycle status gate (docs/SPEC.md "Paste lifecycle status"). A pending
	// paste's blob has not landed yet, so it gets a self-refreshing loading
	// page; only a ready paste reaches the content serve below.
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

	// An update/delete fires an explicit purge via CachePurger, so max-age
	// only bounds the staleness of passive expiry.
	h.Set("Cache-Control", "public, max-age=3600")
	h.Set("Last-Modified", p.UpdatedAt.UTC().Format(http.TimeFormat))

	// A client-rendered kind (markdown, diff) serves either the raw bytes,
	// only under an explicit ?raw query, or the fixed client-render shell at
	// the bare URL. There is no Accept negotiation, so each URL is a SINGLE
	// representation and safe to edge-cache under the max-age set above. See
	// docs/SPEC.md "The bare URL always serves the shell (no Accept
	// negotiation)".
	shell := shellFor(p.Kind)
	rawWanted := shell != nil && wantsRaw(r)

	// ETag is the content SHA for HTML and for any raw body. The shell is
	// content-INDEPENDENT, so it validates on its shell version instead: two
	// different pastes yield the same shell ETag, and a shell change
	// propagates within max-age or immediately via the deploy-time purge.
	etag := `"` + p.ContentSHA + `"`
	if shell != nil && !rawWanted {
		etag = `"` + shell.version + `"`
	}
	h.Set("ETag", etag)

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

	// streamBlob copies the stored bytes out under ct. Streamed so a GET never
	// buffers the whole payload; the body is byte-identical to a buffered read
	// + write, and server memory stays constant regardless of paste size.
	streamBlob := func(ct, what string) {
		rc, _, err := s.Blobs.Read(r.Context(), string(slug), p.ContentSHA)
		if err != nil {
			s.logf("warn: paste read 500: slug=%s %s blob read: %v", slug, what, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer func() { _ = rc.Close() }()
		h.Set("Content-Type", ct)
		_, _ = io.Copy(w, rc)
	}

	// HTML is the one kind served as itself: the stored bytes ARE the page.
	if p.Kind == domain.KindHTML {
		streamBlob("text/html; charset=utf-8", "html")
		return
	}

	// Every other kind is client-rendered: no server-side render, just the raw
	// bytes (under an explicit ?raw) or the fixed shell that fetches and
	// renders them. The vendored libraries and the ?raw fetch are all
	// same-origin under shellCSP.
	if shell == nil {
		s.logf("warn: paste read 500: slug=%s unsupported stored kind %q", slug, p.Kind)
		http.Error(w, "unsupported kind", http.StatusInternalServerError)
		return
	}
	if rawWanted {
		ct := rawContentType[p.Kind]
		if ct == "" {
			ct = "text/plain; charset=utf-8"
		}
		streamBlob(ct, "raw "+string(p.Kind))
		return
	}
	h.Set("Content-Security-Policy", shell.policy())
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(shell.html(p.Kind))
}

// loadingPageHTML is the body served for a pending paste. The meta refresh
// (no JS required) re-checks every second until the finalizer flips the paste
// to ready.
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

// failedPageHTML is the body served for a failed paste, one whose blob write
// never completed. No auto-refresh: the content will never arrive.
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

// servePending serves the loading page for a pending paste. no-store is
// required: a cached 200 freezes the loading screen even after the paste goes
// ready.
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

// serveFailed serves the error page for a failed paste. 410 Gone: the slug
// existed but its content will never arrive, which also keeps the failed
// paste out of any naive success cache.
func (s *Server) serveFailed(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusGone)
	_, _ = io.WriteString(w, failedPageHTML)
}

// serveSiteIfExists serves reqPath from the static site owning slug. True
// means the request was handled and the caller must return. False is returned
// ONLY when no site owns the slug, so the caller falls through to the paste
// path: one slug is either a site or a paste, never both.
func (s *Server) serveSiteIfExists(w http.ResponseWriter, r *http.Request, slug domain.Slug, reqPath string) bool {
	if s.Sites == nil {
		return false
	}
	site, err := s.Sites.Get(slug)
	if err != nil {
		// A read error is deliberately indistinguishable from "no such site":
		// the paste path tries next and surfaces its own 404 or 500.
		return false
	}
	s.serveFromManifest(w, r, slug, site.Manifest, site.UpdatedAt, reqPath)
	return true
}

// serveFromManifest resolves reqPath against a manifest and writes the file.
// The one implementation both paste shapes go through: a directory
// paste's manifest is the same value whatever its cardinality.
func (s *Server) serveFromManifest(w http.ResponseWriter, r *http.Request, slug domain.Slug, manifest domain.Manifest, updatedAt time.Time, reqPath string) {
	// SPA fallback: a manifest miss that looks like a client-side ROUTE (no
	// extension, or ".html") serves the site's root index.html with a 200 so
	// the SPA's JS loads and routes; a miss that looks like a static ASSET
	// stays a 404. The decision is a pure domain function; see
	// domain.Manifest.LookupWithSPAFallback + SPEC.md "SPA fallback (route
	// vs. asset)". A fallback hit is byte-identical to requesting "/".
	entry, hit, _ := manifest.LookupWithSPAFallback(reqPath)
	if !hit {
		http.NotFound(w, r)
		return
	}

	// Site files are served RAW, secured by per-subdomain origin isolation
	// rather than by sanitizing the bytes, so they carry the same sandbox
	// headers as an HTML paste.
	//
	// no-cache, unlike a paste's max-age: under max-age a browser serves a
	// site's sub-resources from cache without revalidating, so a re-deploy
	// stays invisible until each asset expires (the SPA stale-bundle trap).
	// no-cache revalidates every file against its content-SHA ETag: a cheap
	// 304 when unchanged, fresh bytes when not.
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")
	h.Set("Cache-Control", "public, no-cache")
	h.Set("Last-Modified", updatedAt.UTC().Format(http.TimeFormat))

	etag := `"` + entry.SHA + `"`
	h.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if since, err := http.ParseTime(ims); err == nil && !updatedAt.UTC().Truncate(time.Second).After(since) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	// Streamed so a GET never buffers the whole asset.
	rc, _, err := s.Blobs.Read(r.Context(), string(slug), entry.SHA)
	if err != nil {
		s.logf("warn: site read 500: slug=%s file blob read: %v", slug, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer rc.Close() //nolint:errcheck
	ct := entry.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h.Set("Content-Type", ct)
	_, _ = io.Copy(w, rc)
}

// shellCSP locks down every client-render shell response: no default sources,
// scripts/styles/connects same-origin only, no inline script, no framing, no
// form submission. Images and media are unrestricted because markdown can
// embed remote images. 'unsafe-inline' covers styles only, so the markdown's
// own inline styles (which DOMPurify keeps) and the diff renderer's injected
// styles render; scripts get no such escape hatch.
const shellCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: http: https:; media-src 'self' data: http: https:; font-src 'self' data: https:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// wantsRaw reports whether the client asked for the raw paste bytes rather
// than the client-render shell. True ONLY for an explicit ?raw query: there is
// no Accept negotiation, so the bare URL is a single representation (the
// shell, for every client) and safe to edge-cache.
func wantsRaw(r *http.Request) bool {
	return r.URL.Query().Has("raw")
}

// etagMatches reports whether an If-None-Match header lists etag. Handles the
// comma-separated form and the "*" wildcard.
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
