package http

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
)

// reqXFF issues a POST /api/rooms with a chosen RemoteAddr and an optional
// X-Forwarded-For header, so a test can probe the per-IP rate-limit
// bucketing under client-supplied headers.
func reqXFF(t *testing.T, srv *Server, slug, remoteAddr, xff string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://"+slug+".hostthis.test/api/rooms", nil)
	r.RemoteAddr = remoteAddr
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// TestRoomsHTTP_XFFNotTrustedByDefault is the P1-XFF regression guard: by
// default the per-IP room-creation rate limit must derive the bucket from
// the TCP RemoteAddr ONLY, ignoring a client-supplied X-Forwarded-For. An
// attacker rotating XFF per request must NOT escape the per-IP cap.
//
// With HOSTTHIS_HTTP_TRUST_XFF unset, two POSTs from the same RemoteAddr
// carrying DIFFERENT forged XFF headers share one bucket, so the cap (set
// to 1 here) trips on the second. If XFF were trusted by default, each
// forged header would be a fresh bucket and both would succeed - the
// bypass this test pins shut.
func TestRoomsHTTP_XFFNotTrustedByDefault(t *testing.T) {
	// Make sure no ambient opt-in leaks in.
	t.Setenv("HOSTTHIS_HTTP_TRUST_XFF", "")
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	const slug = "appz2345"
	const realAddr = "203.0.113.5:40000"

	// First create from the real IP with one forged XFF: succeeds.
	if w := reqXFF(t, srv, slug, realAddr, "1.2.3.4"); w.Code != http.StatusCreated {
		t.Fatalf("first create: code %d body %q", w.Code, w.Body.String())
	}
	// Second create from the SAME real IP with a DIFFERENT forged XFF. If
	// XFF were trusted, this would be a fresh bucket (201). It must be 429:
	// the bucket is the real RemoteAddr, unchanged by the forged header.
	if w := reqXFF(t, srv, slug, realAddr, "5.6.7.8"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("XFF rotation bypassed the per-IP cap: code %d, want 429", w.Code)
	}
}

// TestRoomsHTTP_XFFTrustedWhenOptedIn confirms that when the operator
// explicitly opts in (HOSTTHIS_HTTP_TRUST_XFF=true, the analogue of the
// SSH PROXY-protocol flag), the per-IP bucket comes from the RIGHT-MOST
// X-Forwarded-For hop - the trusted proxy's view of the client - so two
// requests the proxy recorded from DIFFERENT clients fall into DIFFERENT
// buckets even though the proxy's own RemoteAddr is identical.
func TestRoomsHTTP_XFFTrustedWhenOptedIn(t *testing.T) {
	t.Setenv("HOSTTHIS_HTTP_TRUST_XFF", "true")
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	const slug = "appz2345"
	// Behind a proxy, every request's RemoteAddr is the proxy's IP. The
	// real client IP is the right-most XFF entry the proxy appended.
	const proxyAddr = "10.0.0.1:40000"

	// Client X (the proxy recorded it as the last hop). The left-most entry
	// is whatever the client claimed; it must be ignored. Client Y is in a
	// DIFFERENT /24 so the subnet-masked buckets are distinct.
	if w := reqXFF(t, srv, slug, proxyAddr, "spoofed, 203.0.113.10"); w.Code != http.StatusCreated {
		t.Fatalf("client X first create: code %d body %q", w.Code, w.Body.String())
	}
	// Client X again, same right-most real IP: 429 (its own bucket is full).
	if w := reqXFF(t, srv, slug, proxyAddr, "other-spoof, 203.0.113.10"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("client X second create: code %d, want 429", w.Code)
	}
	// Client Y, a real IP in a DIFFERENT /24 behind the same proxy: 201 (a
	// different bucket). The proxy's shared RemoteAddr did not collapse them
	// into one bucket the way default RemoteAddr-only would.
	if w := reqXFF(t, srv, slug, proxyAddr, "198.51.100.7"); w.Code != http.StatusCreated {
		t.Fatalf("client Y create: code %d, want 201 (its own bucket)", w.Code)
	}
}

// TestRoomsHTTP_CreateUnknownAppIs404 is the P2-APP regression guard: a
// POST /api/rooms under a slug that names NO live app (no site, no paste)
// must 404, so an attacker cannot rotate through the ~10^12 slug space to
// mint a fresh per-app budget under each one. Here no read surface is
// wired at all, so every slug is unknown.
func TestRoomsHTTP_CreateUnknownAppIs404(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	srv := &Server{
		ApexDomain: "hostthis.test",
		Rooms:      service.NewRooms(storage.NewRoomKVRepo(db)),
		// No Sites, no Pastes: no slug resolves to a live app.
	}
	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusNotFound {
		t.Fatalf("create under unknown app: code %d, want 404", w.Code)
	}
}

// TestRoomsHTTP_CreateExpiredAppIs404 confirms the existence gate treats an
// EXPIRED site/paste as gone: the slug once named an app but no longer
// names a LIVE one, so room creation under it 404s, the same line the read
// handlers draw for an expired site.
func TestRoomsHTTP_CreateExpiredAppIs404(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	expiredSite := domain.Site{
		Slug:      "appz2345",
		Identity:  "key:test",
		Manifest:  domain.NewManifest(),
		CreatedAt: now.Add(-2 * domain.RetentionWindow),
		UpdatedAt: now.Add(-2 * domain.RetentionWindow),
		ExpiresAt: now.Add(-time.Hour), // already expired
	}
	srv := &Server{
		ApexDomain: "hostthis.test",
		Rooms:      service.NewRooms(storage.NewRoomKVRepo(db)),
		Sites:      stubSiteReader{s: expiredSite},
	}
	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusNotFound {
		t.Fatalf("create under expired app: code %d, want 404", w.Code)
	}
}

// TestRoomsHTTP_CreateLivePasteAppSucceeds confirms a slug that names a
// LIVE paste (no site) is a valid app to host rooms under - the spec's
// "an app is a deployed static site or a paste."
func TestRoomsHTTP_CreateLivePasteAppSucceeds(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	livePaste := domain.Paste{
		Slug:       "appz2345",
		Kind:       domain.KindHTML,
		ContentSHA: "sha",
		UpdatedAt:  now,
		ExpiresAt:  now.Add(domain.RetentionWindow),
	}
	srv := &Server{
		ApexDomain: "hostthis.test",
		Rooms:      service.NewRooms(storage.NewRoomKVRepo(db)),
		Pastes:     stubPasteReader{p: livePaste},
	}
	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusCreated {
		t.Fatalf("create under live paste app: code %d, want 201", w.Code)
	}
}
