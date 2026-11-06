package http

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// docs/SPEC.md "WebRTC": stored bytes, and the 304 that revalidates them, let
// the page capture on its own origin; a shell or a file list never does.
const (
	wantCapture = "camera=(self), microphone=(self), display-capture=(self), geolocation=(), usb=(), payment=()"
	wantStrict  = "camera=(), microphone=(), geolocation=(), usb=(), payment=()"
)

func TestPermissionsPolicy_StoredBytesMayCapture(t *testing.T) {
	pasteServer := func(kind domain.ContentKind, body string) *Server {
		const key = "uploads/u1/0"
		return &Server{
			Pastes: stubPasteReader{p: domain.Paste{
				Slug: "abc23456", Kind: kind, UpdatedAt: time.Now().UTC(),
				Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: key}),
			}},
			Blobs: &recordingBlobs{bodies: map[string]string{key: body}},
		}
	}
	htmlPaste := pasteServer(domain.KindHTML, "<h1>call</h1>")
	mdPaste := pasteServer(domain.KindMarkdown, "# notes")

	cases := []struct {
		name        string
		srv         *Server
		target, inm string
		code        int
		want        string
	}{
		{"html paste", htmlPaste, "/p/abc23456", "", 200, wantCapture},
		{"html paste 304", htmlPaste, "/p/abc23456", storedETag("uploads/u1/0"), 304, wantCapture},
		{"markdown shell", mdPaste, "/p/abc23456", "", 200, wantStrict},
		{"markdown raw", mdPaste, "/p/abc23456?raw=1", "", 200, wantCapture},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := serveOne(c.srv, c.target, c.inm)
			if w.Code != c.code {
				t.Fatalf("code: got %d, want %d", w.Code, c.code)
			}
			if got := w.Header().Get("Permissions-Policy"); got != c.want {
				t.Fatalf("Permissions-Policy: got %q, want %q", got, c.want)
			}
		})
	}

	site := buildSiteServer(t).Handler()
	for _, path := range []string{"/", "/css/style.css", "/a/client/route"} {
		if got := kbGet(t, site, path).Header().Get("Permissions-Policy"); got != wantCapture {
			t.Fatalf("site %s: Permissions-Policy %q, want %q", path, got, wantCapture)
		}
	}

	kb := buildKBServer(t).Handler()
	for path, want := range map[string]string{
		"/":                     wantStrict,
		"/guide/setup.md":       wantStrict,
		"/?files=1":             wantStrict,
		"/guide/setup.md?raw=1": wantCapture,
		"/report.html":          wantCapture,
	} {
		if got := kbGet(t, kb, path).Header().Get("Permissions-Policy"); got != want {
			t.Fatalf("knowledge base %s: Permissions-Policy %q, want %q", path, got, want)
		}
	}
}
