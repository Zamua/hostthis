package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// buildFullStackServer wires all three read surfaces on one Server, each at its
// own slug: a single-file paste, a static-site archive, and the rooms API over a
// real sqlite repo. Fixture for the no-regression checks, which pin that rooms
// are purely additive (SPEC: "Rooms are additive ... they introduce no change to
// the paste or the site/archive read-and-write behavior").
func buildFullStackServer(t *testing.T) *Server {
	t.Helper()

	now := time.Now().UTC()

	paste := domain.Paste{
		Slug:       "pastenyz",
		Kind:       domain.KindHTML,
		ContentSHA: "sha-paste",
		UpdatedAt:  now,
	}

	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-site-index", Size: 12, ContentType: "text/html; charset=utf-8"})
	m.Add("css/app.css", domain.ManifestEntry{SHA: "sha-site-css", Size: 8, ContentType: "text/css; charset=utf-8"})
	site := domain.Site{
		Slug:      "sitewxyz",
		Identity:  "key:test",
		Manifest:  m,
		CreatedAt: now,
		UpdatedAt: now,
	}

	return &Server{
		ApexDomain: "hostthis.test",
		Pastes:     stubPasteReader{p: paste},
		Sites:      stubSiteReader{s: site},
		Rooms:      service.NewRooms(storage.NewShaleRoomRepo(storagetest.NewRepo(t))),
		Blobs: stubBlobMap{m: map[string][]byte{
			"sha-paste":      []byte("<!doctype html><h1>a paste</h1>"),
			"sha-site-index": []byte("<h1>site home</h1>"),
			"sha-site-css":   []byte("body{color:teal}"),
		}},
	}
}

// TestRoomsHTTP_NoRegression_PasteUnchanged pins that with rooms wired
// alongside it, a paste still serves byte-exact at the slug root with its
// content type and sandbox headers, and still 404s any non-root path.
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
	if w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("paste lost X-Frame-Options: %q", w.Header().Get("X-Frame-Options"))
	}

	// A non-root path on a paste slug still 404s (the favicon-hang guard),
	// even though /api/rooms is a live prefix on other slugs.
	if w := req(t, srv, http.MethodGet, "pastenyz", "/style.css", nil); w.Code != http.StatusNotFound {
		t.Fatalf("paste non-root path: code %d, want 404", w.Code)
	}
}

// TestRoomsHTTP_NoRegression_SiteUnchanged pins that the rooms carve-out does
// not intercept ordinary site paths: manifest files still serve with their
// recorded content types, the index fallback still works, and a missing asset
// still 404s.
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
	// A missing asset still 404s rather than falling back to the index.
	if w := req(t, srv, http.MethodGet, "sitewxyz", "/nope/missing.js", nil); w.Code != http.StatusNotFound {
		t.Fatalf("missing site path: code %d, want 404", w.Code)
	}
}

// TestRoomsHTTP_NoRegression_RoomsLiveAlongsidePasteAndSite pins that the three
// surfaces coexist in one server and none shadows another.
func TestRoomsHTTP_NoRegression_RoomsLiveAlongsidePasteAndSite(t *testing.T) {
	srv := buildFullStackServer(t)

	// Room creation requires the slug to name a live app (the existence gate),
	// so the room is hosted under the site the fixture deployed.
	const appSlug = "sitewxyz"
	id := createRoomID(t, srv, appSlug)
	if w := req(t, srv, http.MethodPut, appSlug, "/api/rooms/"+id+"/k", []byte("v")); w.Code != http.StatusNoContent {
		t.Fatalf("room put: code %d", w.Code)
	}
	if w := req(t, srv, http.MethodGet, appSlug, "/api/rooms/"+id+"/k", nil); w.Code != http.StatusOK || w.Body.String() != "v" {
		t.Fatalf("room get: code %d body %q", w.Code, w.Body.String())
	}

	// The room writes did not disturb the other two surfaces.
	if w := req(t, srv, http.MethodGet, "pastenyz", "/", nil); w.Code != http.StatusOK || w.Body.String() != "<!doctype html><h1>a paste</h1>" {
		t.Fatalf("paste after room writes: code %d body %q", w.Code, w.Body.String())
	}
	if w := req(t, srv, http.MethodGet, "sitewxyz", "/css/app.css", nil); w.Code != http.StatusOK || w.Body.String() != "body{color:teal}" {
		t.Fatalf("site after room writes: code %d body %q", w.Code, w.Body.String())
	}

	// A paste-only slug can host rooms too (SPEC), and still serves its paste
	// at root afterward.
	r := httptest.NewRequest(http.MethodPost, "http://pastenyz.hostthis.test/api/rooms", nil)
	r.RemoteAddr = "203.0.113.5:40000"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("rooms on a paste slug: code %d body %q", w.Code, w.Body.String())
	}
	if pw := req(t, srv, http.MethodGet, "pastenyz", "/", nil); pw.Code != http.StatusOK {
		t.Fatalf("paste root after room create on same slug: code %d", pw.Code)
	}
}
