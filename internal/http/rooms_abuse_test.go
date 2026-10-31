package http

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// create POSTs /api/rooms from remoteAddr with the given X-Forwarded-For.
func create(t *testing.T, srv *Server, slug, remoteAddr, forwarded string) *httptest.ResponseRecorder {
	t.Helper()
	return req(t, srv, http.MethodPost, slug, "/api/rooms", nil, from(remoteAddr), xff(forwarded))
}

// TestRoomsHTTP_XFFNotTrustedByDefault pins that with the peer as the source the per-IP
// room-creation bucket derives from the TCP RemoteAddr alone, so rotating a
// forged X-Forwarded-For does not mint a fresh bucket per request and escape
// the cap.
func TestRoomsHTTP_XFFNotTrustedByDefault(t *testing.T) {
	srv := buildRoomServer(t)
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	const slug = "appz2345"
	const realAddr = "203.0.113.5:40000"

	if w := create(t, srv, slug, realAddr, "1.2.3.4"); w.Code != http.StatusCreated {
		t.Fatalf("first create: code %d body %q", w.Code, w.Body.String())
	}
	// Same RemoteAddr, a different forged XFF: 429, not a fresh bucket.
	if w := create(t, srv, slug, realAddr, "5.6.7.8"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("XFF rotation bypassed the per-IP cap: code %d, want 429", w.Code)
	}
}

// TestRoomsHTTP_XFFTrustedWhenOptedIn pins that under xff-rightmost the per-IP
// bucket comes from the RIGHT-MOST X-Forwarded-For hop (the trusted proxy's own
// view of the client), so two clients behind one proxy land in different
// buckets despite sharing a RemoteAddr.
func TestRoomsHTTP_XFFTrustedWhenOptedIn(t *testing.T) {
	srv := buildRoomServer(t)
	srv.ClientIP = ClientIPForwardedFor
	srv.Rooms.(*service.Rooms).MaxRoomsPerIP = 1
	const slug = "appz2345"
	const proxyAddr = "10.0.0.1:40000"

	// The left-most entry is client-claimed and must be ignored. Buckets are
	// /24-masked, so client Y below has to sit in a different /24 to be
	// distinguishable from client X.
	if w := create(t, srv, slug, proxyAddr, "spoofed, 203.0.113.10"); w.Code != http.StatusCreated {
		t.Fatalf("client X first create: code %d body %q", w.Code, w.Body.String())
	}
	if w := create(t, srv, slug, proxyAddr, "other-spoof, 203.0.113.10"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("client X second create: code %d, want 429", w.Code)
	}
	if w := create(t, srv, slug, proxyAddr, "198.51.100.7"); w.Code != http.StatusCreated {
		t.Fatalf("client Y create: code %d, want 201 (its own bucket)", w.Code)
	}
}

type clientIPVectors struct {
	XFF []struct {
		Name    string   `json:"name"`
		Lines   []string `json:"lines"`
		Address string   `json:"address"`
	} `json:"xff"`
	Subnets [][2]string `json:"subnets"`
}

// The vectors in testdata/client-ip-vectors.json, which the Worker's
// rooms-policy.test.js runs too.
func loadClientIPVectors(t *testing.T) clientIPVectors {
	t.Helper()
	raw, err := os.ReadFile("../../testdata/client-ip-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v clientIPVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.XFF) == 0 || len(v.Subnets) == 0 {
		t.Fatal("no vectors")
	}
	return v
}

// With X-Forwarded-For trusted, the creation limit counts the right-most value
// across every header line; without one, the TCP peer.
func TestXFFVectors(t *testing.T) {
	const peer = "192.0.2.1:40000"
	for _, v := range loadClientIPVectors(t).XFF {
		t.Run(v.Name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://appz2345.hostthis.test/api/rooms", nil)
			r.RemoteAddr = peer
			for _, line := range v.Lines {
				r.Header.Add("X-Forwarded-For", line)
			}
			want := "192.0.2.0/24"
			if v.Address != "" {
				want = domain.SubnetOf(net.ParseIP(v.Address))
			}
			if got := clientSubnet(r, ClientIPForwardedFor); got != want {
				t.Fatalf("lines %q: subnet %q, want %q (address %q)", v.Lines, got, want, v.Address)
			}
		})
	}
}

func TestSubnetVectors(t *testing.T) {
	for _, v := range loadClientIPVectors(t).Subnets {
		if got := domain.SubnetOf(net.ParseIP(v[0])); got != v[1] {
			t.Errorf("%q: subnet %q, want %q", v[0], got, v[1])
		}
	}
}

// TestRoomsHTTP_CreateUnknownAppIs404 pins that POST /api/rooms under a slug
// naming no live app 404s, so an attacker cannot rotate through the ~10^12 slug
// space to mint a fresh per-app budget under each one.
func TestRoomsHTTP_CreateUnknownAppIs404(t *testing.T) {
	srv := &Server{
		ApexDomain: "hostthis.test",
		Rooms:      service.NewRooms(newRoomRepo(t)),
		// No Sites, no Pastes: no slug resolves to a live app.
	}
	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusNotFound {
		t.Fatalf("create under unknown app: code %d, want 404", w.Code)
	}
}

// TestRoomsHTTP_CreateLivePasteAppSucceeds pins the spec's "an app is a
// deployed static site or a paste": a live paste alone is a valid room host.
func TestRoomsHTTP_CreateLivePasteAppSucceeds(t *testing.T) {
	now := time.Now().UTC()
	livePaste := domain.Paste{
		Slug:      "appz2345",
		Kind:      domain.KindHTML,
		UpdatedAt: now,
	}
	srv := &Server{
		ApexDomain: "hostthis.test",
		Rooms:      service.NewRooms(newRoomRepo(t)),
		Pastes:     stubPasteReader{p: livePaste},
	}
	if w := req(t, srv, http.MethodPost, "appz2345", "/api/rooms", nil); w.Code != http.StatusCreated {
		t.Fatalf("create under live paste app: code %d, want 201", w.Code)
	}
}

// Behind Cloudflare the creation limit counts CF-Connecting-IP, and only when
// the trusted proxy's hop is a Cloudflare edge: a direct client's own
// CF-Connecting-IP counts for nothing.
func TestClientSubnet_Cloudflare(t *testing.T) {
	for _, tc := range []struct {
		name, xff, cf, want string
	}{
		{"edge hop hands over to CF-Connecting-IP", "spoofed, 162.158.1.2", "203.0.113.9", "203.0.113.0/24"},
		{"IPv6 edge hop", "2606:4700::1", "2001:db8:1:2::5", "2001:db8:1::/48"},
		{"direct client cannot claim an address", "198.51.100.7", "203.0.113.9", "198.51.100.0/24"},
		{"edge hop without the header counts the edge", "162.158.1.2", "", "162.158.1.0/24"},
		{"no hop falls back to the peer", "", "203.0.113.9", "192.0.2.0/24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://appz2345.hostthis.test/api/rooms", nil)
			r.RemoteAddr = "192.0.2.1:40000"
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.cf != "" {
				r.Header.Set("CF-Connecting-IP", tc.cf)
			}
			if got := clientSubnet(r, ClientIPCloudflare); got != tc.want {
				t.Fatalf("subnet %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseClientIPSource(t *testing.T) {
	for in, want := range map[string]ClientIPSource{"": ClientIPPeer, "peer": ClientIPPeer, "xff-rightmost": ClientIPForwardedFor, "Cloudflare": ClientIPCloudflare} {
		if got, err := ParseClientIPSource(in); err != nil || got != want {
			t.Fatalf("%q = %v, %v", in, got, err)
		}
	}
	if _, err := ParseClientIPSource("xff-leftmost"); err == nil {
		t.Fatal("unknown source accepted")
	}
}
