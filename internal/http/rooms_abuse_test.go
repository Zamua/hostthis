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

// reqXFF POSTs /api/rooms with a chosen RemoteAddr and an optional
// X-Forwarded-For, so a test can probe per-IP bucketing under forged headers.
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

// TestRoomsHTTP_XFFNotTrustedByDefault pins that with HOSTTHIS_HTTP_TRUST_XFF
// unset the per-IP room-creation bucket derives from the TCP RemoteAddr alone,
// so rotating a forged X-Forwarded-For does not mint a fresh bucket per request
// and escape the cap.
func TestRoomsHTTP_XFFNotTrustedByDefault(t *testing.T) {
	// Keep an ambient opt-in from leaking in.
	t.Setenv("HOSTTHIS_HTTP_TRUST_XFF", "")
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	const slug = "appz2345"
	const realAddr = "203.0.113.5:40000"

	if w := reqXFF(t, srv, slug, realAddr, "1.2.3.4"); w.Code != http.StatusCreated {
		t.Fatalf("first create: code %d body %q", w.Code, w.Body.String())
	}
	// Same RemoteAddr, a different forged XFF: 429, not a fresh bucket.
	if w := reqXFF(t, srv, slug, realAddr, "5.6.7.8"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("XFF rotation bypassed the per-IP cap: code %d, want 429", w.Code)
	}
}

// TestRoomsHTTP_XFFTrustedWhenOptedIn pins that under
// HOSTTHIS_HTTP_TRUST_XFF=true the per-IP bucket comes from the RIGHT-MOST
// X-Forwarded-For hop (the trusted proxy's own view of the client), so two
// clients behind one proxy land in different buckets despite sharing a
// RemoteAddr.
func TestRoomsHTTP_XFFTrustedWhenOptedIn(t *testing.T) {
	t.Setenv("HOSTTHIS_HTTP_TRUST_XFF", "true")
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	const slug = "appz2345"
	const proxyAddr = "10.0.0.1:40000"

	// The left-most entry is client-claimed and must be ignored. Buckets are
	// /24-masked, so client Y below has to sit in a different /24 to be
	// distinguishable from client X.
	if w := reqXFF(t, srv, slug, proxyAddr, "spoofed, 203.0.113.10"); w.Code != http.StatusCreated {
		t.Fatalf("client X first create: code %d body %q", w.Code, w.Body.String())
	}
	if w := reqXFF(t, srv, slug, proxyAddr, "other-spoof, 203.0.113.10"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("client X second create: code %d, want 429", w.Code)
	}
	if w := reqXFF(t, srv, slug, proxyAddr, "198.51.100.7"); w.Code != http.StatusCreated {
		t.Fatalf("client Y create: code %d, want 201 (its own bucket)", w.Code)
	}
}

// TestRoomsHTTP_CreateUnknownAppIs404 pins that POST /api/rooms under a slug
// naming no live app 404s, so an attacker cannot rotate through the ~10^12 slug
// space to mint a fresh per-app budget under each one.
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

// TestRoomsHTTP_CreateLivePasteAppSucceeds pins the spec's "an app is a
// deployed static site or a paste": a live paste alone is a valid room host.
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
