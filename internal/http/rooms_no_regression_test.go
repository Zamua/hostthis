package http

import (
	"net/http"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

// A paste, a site, and the rooms API on one Server, each under its own slug:
// room writes disturb neither read surface, and a paste-only slug hosts rooms
// while still serving its paste at root.
func TestRoomsHTTP_RoomsLiveAlongsidePasteAndSite(t *testing.T) {
	now := time.Now().UTC()
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{SHA: "sha-site", Size: 18, ContentType: "text/html; charset=utf-8"})
	srv := &Server{
		ApexDomain: "hostthis.test",
		Pastes:     stubPasteReader{p: domain.Paste{Slug: "pastenyz", Kind: domain.KindHTML, ContentSHA: "sha-paste", UpdatedAt: now}},
		Sites:      stubSiteReader{s: siteAt("sitewxyz", m)},
		Rooms:      service.NewRooms(storage.NewMemRoomRepo(storagetest.NewRepo(t))),
		Blobs: stubBlobMap{m: map[string][]byte{
			"sha-paste": []byte("<h1>a paste</h1>"),
			"sha-site":  []byte("<h1>site home</h1>"),
		}},
	}

	id := createRoomID(t, srv, "sitewxyz")
	if w := req(t, srv, http.MethodPut, "sitewxyz", "/api/rooms/"+id+"/k", []byte("v")); w.Code != http.StatusNoContent {
		t.Fatalf("room put: code %d", w.Code)
	}
	if w := req(t, srv, http.MethodGet, "sitewxyz", "/api/rooms/"+id+"/k", nil); w.Code != http.StatusOK || w.Body.String() != "v" {
		t.Fatalf("room get: code %d body %q", w.Code, w.Body.String())
	}
	if w := req(t, srv, http.MethodGet, "sitewxyz", "/", nil); w.Code != http.StatusOK || w.Body.String() != "<h1>site home</h1>" {
		t.Fatalf("site after room writes: code %d body %q", w.Code, w.Body.String())
	}

	createRoomID(t, srv, "pastenyz")
	if w := req(t, srv, http.MethodGet, "pastenyz", "/", nil); w.Code != http.StatusOK || w.Body.String() != "<h1>a paste</h1>" {
		t.Fatalf("paste after room create on its slug: code %d body %q", w.Code, w.Body.String())
	}
}
