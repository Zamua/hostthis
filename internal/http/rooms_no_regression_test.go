package http

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// buildFullStackServer wires ALL THREE read surfaces on one Server: a
// single-file paste (one slug), a static site / archive (another slug),
// and the live rooms API (a third slug, over a real sqlite repo). It is
// the fixture for the no-regression checks: adding rooms must not change
// the paste or the site/archive read behavior, since rooms are a purely
// additive surface (SPEC: "Rooms are additive ... they introduce no
// change to the paste or the site/archive read-and-write behavior").
func buildFullStackServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "full.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()

	// A single-file paste at its own slug.
	paste := domain.Paste{
		Slug:       "pastenyz",
		Kind:       domain.KindHTML,
		ContentSHA: "sha-paste",
		UpdatedAt:  now,
		ExpiresAt:  now.Add(domain.RetentionWindow),
	}

	// A static site (archive) at a different slug.
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-site-index", Size: 12, ContentType: "text/html; charset=utf-8"})
	m.Add("css/app.css", domain.ManifestEntry{SHA: "sha-site-css", Size: 8, ContentType: "text/css; charset=utf-8"})
	site := domain.Site{
		Slug:      "sitewxyz",
		Identity:  "key:test",
		Manifest:  m,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RetentionWindow),
	}

	return &Server{
		ApexDomain: "hostthis.test",
		Pastes:     stubPasteReader{p: paste},
		Sites:      stubSiteReader{s: site},
		Rooms:      service.NewRooms(storage.NewRoomKVRepo(db)),
		Blobs: stubBlobMap{m: map[string][]byte{
			"sha-paste":      []byte("<!doctype html><h1>a paste</h1>"),
			"sha-site-index": []byte("<h1>site home</h1>"),
			"sha-site-css":   []byte("body{color:teal}"),
		}},
	}
}

// TestRoomsHTTP_NoRegression_PasteUnchanged confirms that with the rooms
// surface wired alongside it, a single-file paste still serves byte-exact
// at the slug root with its text/html content type and sandbox headers,
// and still 404s any non-root path. Rooms are additive; the paste path is
// untouched.
func TestRoomsHTTP_NoRegression_PasteUnchanged(t *testing.T) {
	srv := buildFullStackServer(t)

	w := req(t, srv, http.MethodGet, "pastenyz", "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("paste root: code %d", w.Code)
	}
	if w.Body.String() != "<!doctype html><h1>a paste</h1>" {
		t.Fatalf("paste body changed: %q", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("paste content-type: got %q", ct)
	}
	// The paste sandbox headers are still present (the rooms wiring did not
	// strip them).
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("paste lost X-Frame-Options: %q", w.Header().Get("X-Frame-Options"))
	}

	// A non-root path on a paste slug still 404s (the favicon-hang guard),
	// even though /api/rooms is now a live prefix on OTHER slugs.
	if w := req(t, srv, http.MethodGet, "pastenyz", "/style.css", nil); w.Code != http.StatusNotFound {
		t.Fatalf("paste non-root path: code %d, want 404", w.Code)
	}
}

// TestRoomsHTTP_NoRegression_SiteUnchanged confirms that with rooms wired,
// a static-site archive still serves its files via the manifest with the
// recorded content types, the index fallback still works, and an unmatched
// path still 404s with no SPA fallback. The rooms carve-out does not
// intercept ordinary site paths.
func TestRoomsHTTP_NoRegression_SiteUnchanged(t *testing.T) {
	srv := buildFullStackServer(t)

	cases := []struct {
		path, body, ctype string
	}{
		{"/", "<h1>site home</h1>", "text/html; charset=utf-8"},
		{"/index.html", "<h1>site home</h1>", "text/html; charset=utf-8"},
		{"/css/app.css", "body{color:teal}", "text/css; charset=utf-8"},
	}
	for _, c := range cases {
		w := req(t, srv, http.MethodGet, "sitewxyz", c.path, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("site %s: code %d", c.path, w.Code)
		}
		if w.Body.String() != c.body {
			t.Fatalf("site %s body: got %q want %q", c.path, w.Body.String(), c.body)
		}
		if ct := w.Header().Get("Content-Type"); ct != c.ctype {
			t.Fatalf("site %s ctype: got %q want %q", c.path, ct, c.ctype)
		}
	}
	// An unmatched site path 404s (no SPA fallback) - unchanged by rooms.
	if w := req(t, srv, http.MethodGet, "sitewxyz", "/nope/missing.js", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing site path: code %d, want 404", w.Code)
	}
}

// TestRoomsHTTP_NoRegression_RoomsLiveAlongsidePasteAndSite confirms the
// rooms surface itself works on its OWN slug in the same server, and that
// a slug serving a paste or a site is independent of the rooms slug. The
// three surfaces coexist; none shadows another.
func TestRoomsHTTP_NoRegression_RoomsLiveAlongsidePasteAndSite(t *testing.T) {
	srv := buildFullStackServer(t)

	// Rooms work under a fresh app slug.
	const appSlug = "appz9999"
	id := createRoomID(t, srv, appSlug)
	if w := req(t, srv, http.MethodPut, appSlug, "/api/rooms/"+id+"/k", []byte("v")); w.Code != http.StatusNoContent {
		t.Fatalf("room put: code %d", w.Code)
	}
	if w := req(t, srv, http.MethodGet, appSlug, "/api/rooms/"+id+"/k", nil); w.Code != http.StatusOK || w.Body.String() != "v" {
		t.Fatalf("room get: code %d body %q", w.Code, w.Body.String())
	}

	// And the paste + site slugs still serve their content (re-checked in
	// the same server instance to confirm the room writes did not disturb
	// the other surfaces).
	if w := req(t, srv, http.MethodGet, "pastenyz", "/", nil); w.Code != http.StatusOK || w.Body.String() != "<!doctype html><h1>a paste</h1>" {
		t.Fatalf("paste after room writes: code %d body %q", w.Code, w.Body.String())
	}
	if w := req(t, srv, http.MethodGet, "sitewxyz", "/css/app.css", nil); w.Code != http.StatusOK || w.Body.String() != "body{color:teal}" {
		t.Fatalf("site after room writes: code %d body %q", w.Code, w.Body.String())
	}

	// The paste slug also gets the rooms API (a paste-only slug can host
	// rooms - the create path records rooms keyed by that slug). This is
	// the spec's "a paste-only slug can host rooms too" note: the surface
	// is additive, never removed.
	r := httptest.NewRequest(http.MethodPost, "http://pastenyz.hostthis.test/api/rooms", nil)
	r.RemoteAddr = "203.0.113.5:40000"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("rooms on a paste slug: code %d body %q", w.Code, w.Body.String())
	}
	// And the paste itself STILL serves at root afterward.
	if pw := req(t, srv, http.MethodGet, "pastenyz", "/", nil); pw.Code != http.StatusOK {
		t.Fatalf("paste root after room create on same slug: code %d", pw.Code)
	}
}
