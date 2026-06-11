package http

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// liveAppSiteReader is a test SiteReader that treats EVERY parseable slug
// as a live (non-expired) site. It stands in for "the operator has
// provisioned an app at this slug," which is the precondition room
// creation now requires (see createRoom's appExists gate). Tests that
// exercise room behavior under a provisioned app wire this so the slug
// they POST under resolves to a live app; the unknown-app 404 path has
// its own dedicated test that does NOT wire it.
type liveAppSiteReader struct{}

func (liveAppSiteReader) Get(slug domain.Slug) (domain.Site, error) {
	now := time.Now().UTC()
	return domain.Site{
		Slug:      slug,
		Identity:  "key:test",
		Manifest:  domain.NewManifest(),
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RetentionWindow),
	}, nil
}

// buildRoomServer wires a Server with the real Rooms service over a real
// sqlite repo, in subdomain mode. End-to-end: a request through the mux
// hits the service hits the storage layer. A liveAppSiteReader is wired so
// any slug the test creates rooms under resolves to a live app (the
// existence precondition room creation requires).
func buildRoomServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rooms := service.NewRooms(storage.NewRoomKVRepo(db))
	return &Server{
		ApexDomain: "hostthis.test",
		Rooms:      rooms,
		Sites:      liveAppSiteReader{},
	}
}

// req issues a request against the app subdomain <slug>.hostthis.test.
func req(t *testing.T, srv *Server, method, slug, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, "http://"+slug+".hostthis.test"+path, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "http://"+slug+".hostthis.test"+path, nil)
	}
	r.RemoteAddr = "203.0.113.5:40000"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// createRoomID POSTs a room and returns the minted id.
func createRoomID(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	w := req(t, srv, http.MethodPost, slug, "/api/rooms", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("create room: code %d, body %q", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if _, err := domain.ParseRoomID(out.ID); err != nil {
		t.Fatalf("created id not a valid v4: %q (%v)", out.ID, err)
	}
	return out.ID
}

func TestRoomsHTTP_CreateGetPutDeleteScan(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)

	// PUT a value.
	w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/name", []byte("alice"))
	if w.Code != http.StatusNoContent {
		t.Fatalf("put: code %d", w.Code)
	}
	// GET it back.
	w = req(t, srv, http.MethodGet, slug, "/api/rooms/"+id+"/name", nil)
	if w.Code != http.StatusOK || w.Body.String() != "alice" {
		t.Fatalf("get: code %d body %q", w.Code, w.Body.String())
	}
	// PUT a JSON value, then scan returns it as embedded JSON.
	w = req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/votes", []byte(`{"a":3}`))
	if w.Code != http.StatusNoContent {
		t.Fatalf("put json: code %d", w.Code)
	}
	w = req(t, srv, http.MethodGet, slug, "/api/rooms/"+id, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("scan: code %d", w.Code)
	}
	var scan map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &scan); err != nil {
		t.Fatalf("scan decode: %v (%q)", err, w.Body.String())
	}
	if string(scan["votes"]) != `{"a":3}` {
		t.Fatalf("scanned votes = %s, want JSON object", scan["votes"])
	}
	if string(scan["name"]) != `"alice"` {
		t.Fatalf("scanned name = %s, want JSON string", scan["name"])
	}
	// DELETE it.
	w = req(t, srv, http.MethodDelete, slug, "/api/rooms/"+id+"/name", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: code %d", w.Code)
	}
	w = req(t, srv, http.MethodGet, slug, "/api/rooms/"+id+"/name", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("get after delete: code %d, want 404", w.Code)
	}
}

func TestRoomsHTTP_NonexistentRoomIs404(t *testing.T) {
	srv := buildRoomServer(t)
	id := domain.NewRoomID().String()
	// A well-formed but nonexistent room: GET value, scan, PUT, DELETE
	// all 404 (no existence leak), NOT 400.
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/rooms/" + id + "/k"},
		{http.MethodGet, "/api/rooms/" + id},
		{http.MethodDelete, "/api/rooms/" + id + "/k"},
	} {
		w := req(t, srv, tc.method, "appz2345", tc.path, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s %s: code %d, want 404", tc.method, tc.path, w.Code)
		}
	}
	w := req(t, srv, http.MethodPut, "appz2345", "/api/rooms/"+id+"/k", []byte("v"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("put to missing room: code %d, want 404", w.Code)
	}
}

func TestRoomsHTTP_MalformedUUIDIs400(t *testing.T) {
	srv := buildRoomServer(t)
	// Distinct from the 404 a well-formed-but-nonexistent room gets.
	for _, bad := range []string{"not-a-uuid", "abc12345", "12345"} {
		w := req(t, srv, http.MethodGet, "appz2345", "/api/rooms/"+bad, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("malformed id %q: code %d, want 400", bad, w.Code)
		}
	}
}

func TestRoomsHTTP_CrossRoomIsolation(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	a := createRoomID(t, srv, slug)
	b := createRoomID(t, srv, slug)
	// Write under A.
	if w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+a+"/secret", []byte("from-A")); w.Code != http.StatusNoContent {
		t.Fatalf("put A: %d", w.Code)
	}
	// B's UUID cannot read A's key - 404.
	if w := req(t, srv, http.MethodGet, slug, "/api/rooms/"+b+"/secret", nil); w.Code != http.StatusNotFound {
		t.Fatalf("B read A's key: code %d, want 404", w.Code)
	}
	// B's scan is empty.
	w := req(t, srv, http.MethodGet, slug, "/api/rooms/"+b, nil)
	var scan map[string]json.RawMessage
	_ = json.Unmarshal(w.Body.Bytes(), &scan)
	if len(scan) != 0 {
		t.Fatalf("B scan leaked %d keys", len(scan))
	}
}

func TestRoomsHTTP_CrossAppIsolation(t *testing.T) {
	srv := buildRoomServer(t)
	// Same room id minted under app1; app2 must not see it via the same id.
	id := createRoomID(t, srv, "appz2222")
	if w := req(t, srv, http.MethodPut, "appz2222", "/api/rooms/"+id+"/k", []byte("app1")); w.Code != http.StatusNoContent {
		t.Fatalf("put app1: %d", w.Code)
	}
	// app2 addressing the SAME id is a different keyspace -> 404 room.
	if w := req(t, srv, http.MethodGet, "appz3333", "/api/rooms/"+id+"/k", nil); w.Code != http.StatusNotFound {
		t.Fatalf("app2 read app1 id: code %d, want 404", w.Code)
	}
}

func TestRoomsHTTP_PerRoomCapIs413(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)
	atCap := bytes.Repeat([]byte("x"), domain.MaxRoomBytes)
	if w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/big", atCap); w.Code != http.StatusNoContent {
		t.Fatalf("at-cap put: %d", w.Code)
	}
	w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/more", []byte("y"))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap put: code %d, want 413", w.Code)
	}
}

// TestRoomsHTTP_OversizeBodyIs413 confirms a SINGLE value bigger than the
// per-room cap is rejected (413), not silently truncated-and-accepted by
// the request-body limit reader.
func TestRoomsHTTP_OversizeBodyIs413(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	id := createRoomID(t, srv, slug)
	huge := bytes.Repeat([]byte("z"), domain.MaxRoomValueBytes+100)
	w := req(t, srv, http.MethodPut, slug, "/api/rooms/"+id+"/huge", huge)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize body: code %d, want 413", w.Code)
	}
	// The key was NOT created (the write was rejected, prior state intact).
	g := req(t, srv, http.MethodGet, slug, "/api/rooms/"+id+"/huge", nil)
	if g.Code != http.StatusNotFound {
		t.Fatalf("rejected oversize key present: code %d, want 404", g.Code)
	}
}

func TestRoomsHTTP_CreateRateLimitIs429(t *testing.T) {
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 2
	const slug = "appz2345"
	createRoomID(t, srv, slug)
	createRoomID(t, srv, slug)
	// 3rd from the same IP is 429 with Retry-After.
	w := req(t, srv, http.MethodPost, slug, "/api/rooms", nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd create: code %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatalf("429 missing Retry-After")
	}
}

func TestRoomsHTTP_MethodNotAllowed(t *testing.T) {
	srv := buildRoomServer(t)
	// POST on a room value path is not allowed.
	id := createRoomID(t, srv, "appz2345")
	w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms/"+id+"/k", []byte("v"))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST value: code %d, want 405", w.Code)
	}
	if w.Header().Get("Allow") == "" {
		t.Fatalf("405 missing Allow header")
	}
}

func TestRoomsHTTP_PathModeRouting(t *testing.T) {
	srv := buildRoomServer(t)
	// In path mode the same routes live under /p/<slug>/api/rooms/...
	r := httptest.NewRequest(http.MethodPost, "http://hostthis.test/p/appz2345/api/rooms", nil)
	r.RemoteAddr = "203.0.113.5:40000"
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("path-mode create: code %d, body %q", w.Code, w.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	// Round-trip a value via the path-mode URL.
	put := httptest.NewRequest(http.MethodPut, "http://hostthis.test/p/appz2345/api/rooms/"+out.ID+"/k", bytes.NewReader([]byte("v")))
	pw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(pw, put)
	if pw.Code != http.StatusNoContent {
		t.Fatalf("path-mode put: code %d", pw.Code)
	}
	get := httptest.NewRequest(http.MethodGet, "http://hostthis.test/p/appz2345/api/rooms/"+out.ID+"/k", nil)
	gw := httptest.NewRecorder()
	srv.Handler().ServeHTTP(gw, get)
	bodyBytes, _ := io.ReadAll(gw.Body)
	if gw.Code != http.StatusOK || string(bodyBytes) != "v" {
		t.Fatalf("path-mode get: code %d body %q", gw.Code, bodyBytes)
	}
}

// TestRoomsHTTP_APIPrefixNotShadowedBySite confirms the /api/rooms
// carve-out wins over a static site that owns the same slug: even when a
// site is wired and could serve a file, the API prefix routes to the
// rooms handler.
func TestRoomsHTTP_APIPrefixNotShadowedBySite(t *testing.T) {
	srv := buildRoomServer(t)
	const slug = "appz2345"
	// Wire a site that has a file literally at api/rooms (a hostile or
	// accidental name). The API must still win.
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-i", Size: 1, ContentType: "text/html; charset=utf-8"})
	m.Add("api/rooms", domain.ManifestEntry{SHA: "sha-x", Size: 1, ContentType: "text/html; charset=utf-8"})
	srv.Sites = stubSiteReader{s: siteAt(slug, m)}
	srv.Blobs = stubBlobMap{m: map[string][]byte{"sha-i": []byte("i"), "sha-x": []byte("SHADOW")}}

	// POST /api/rooms hits the API (201), not the manifest file.
	w := req(t, srv, http.MethodPost, slug, "/api/rooms", nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("api shadowed by site: code %d body %q", w.Code, w.Body.String())
	}
	// A normal site path still serves the site.
	w = req(t, srv, http.MethodGet, slug, "/", nil)
	if w.Code != http.StatusOK || w.Body.String() != "i" {
		t.Fatalf("site root: code %d body %q", w.Code, w.Body.String())
	}
}

func siteAt(slug string, m domain.Manifest) domain.Site {
	now := time.Now().UTC()
	return domain.Site{
		Slug:      domain.Slug(slug),
		Identity:  "key:test",
		Manifest:  m,
		CreatedAt: now,
		UpdatedAt: now,
		ExpiresAt: now.Add(domain.RetentionWindow),
	}
}
