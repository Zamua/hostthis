// Package http serves the apex landing + the paste read surface.
// Phase 1 supports path-mode only (`/p/<slug>`); subdomain-mode
// routing comes when the wildcard cert is in place.
package http

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// PasteReader is the read-side interface — same shape the service
// layer uses, intentionally narrow (this package doesn't need
// Insert).
type PasteReader interface {
	Get(domain.Slug) (domain.Paste, error)
}

// BlobReader fetches paste bytes by content sha.
type BlobReader interface {
	Get(sha string) ([]byte, error)
}

// Server bundles the dependencies.
type Server struct {
	Pastes      PasteReader
	Blobs       BlobReader
	LandingHTML []byte // optional — apex landing page bytes embedded at build
}

// Handler returns the mux that the caller binds with http.ListenAndServe.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Apex landing (path-mode dev: this is reached at "/")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Anything under /p/ is a paste lookup; everything else under /
		// is either the apex or a 404.
		if strings.HasPrefix(r.URL.Path, "/p/") {
			s.servePaste(w, r)
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		s.serveLanding(w, r)
	})

	return mux
}

func (s *Server) serveLanding(w http.ResponseWriter, _ *http.Request) {
	if len(s.LandingHTML) == 0 {
		// Dev/test default — operator can override at startup with the
		// real bytes from web/landing.html.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "hostthis — landing page not embedded.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.LandingHTML)
}

func (s *Server) servePaste(w http.ResponseWriter, r *http.Request) {
	slugStr := strings.TrimPrefix(r.URL.Path, "/p/")
	slug, err := domain.ParseSlug(slugStr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, err := s.Pastes.Get(slug)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	body, err := s.Blobs.Get(p.ContentSHA)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Sandboxing headers per SPEC.md HTML-sandboxing section.
	// These apply even in path-mode dev — origin isolation isn't
	// real in path-mode, but defense-in-depth headers still keep
	// the browser from doing surprising things with the bytes.
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")
	h.Set("Content-Security-Policy",
		"default-src 'self' data: blob:; "+
			"script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; "+
			"img-src 'self' data: blob: https:; "+
			"font-src 'self' data: https:; "+
			"connect-src 'self'; "+
			"frame-ancestors 'none'")

	switch p.Kind {
	case domain.KindHTML:
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	case domain.KindMarkdown:
		// Phase 1: serve raw markdown as text/plain. Server-side
		// rendering to sanitized HTML lands in Phase 2 when we wire
		// goldmark + bluemonday.
		h.Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(body)
	default:
		http.Error(w, "unsupported kind", http.StatusInternalServerError)
	}
}
