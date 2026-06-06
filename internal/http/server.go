// Package http serves the apex landing + the paste read surface.
//
// The router accepts both URL shapes simultaneously so the binary
// works whether the operator runs in subdomain mode (`<slug>.apex`)
// or path mode (`apex/p/<slug>`). The actual mode is set by what URL
// the SSH server emits after upload; the HTTP side just doesn't care.
package http

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/render"
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
	ApexDomain  string // e.g. "hostthis.dev" — used to peel slug subdomains
	Now         func() time.Time
}

func (s *Server) nowOrTime() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Handler returns the mux that the caller binds with http.ListenAndServe.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 1. Subdomain mode: Host like "<slug>.<apex>" → serve paste,
		// but ONLY at path "/". Any other path on the slug subdomain
		// (favicon.ico, /style.css, /wp-login.php, etc.) returns 404
		// — otherwise the browser's automatic favicon fetch sees the
		// full paste HTML labeled text/html and keeps the loading
		// indicator spinning in some clients.
		if slug, ok := s.slugFromHost(r.Host); ok {
			if r.URL.Path != "/" {
				http.NotFound(w, r)
				return
			}
			s.servePasteSlug(w, r, slug)
			return
		}
		// 2. Path mode: /p/<slug> on the apex.
		if strings.HasPrefix(r.URL.Path, "/p/") {
			slugStr := strings.TrimPrefix(r.URL.Path, "/p/")
			slug, err := domain.ParseSlug(slugStr)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			s.servePasteSlug(w, r, slug)
			return
		}
		// 3. Apex root → landing.
		if r.URL.Path == "/" {
			s.serveLanding(w, r)
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

// slugFromHost returns (slug, true) when host is "<slug>.<apex>" and
// the slug parses cleanly. Otherwise (_, false). Strips the port if
// present.
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
		// Multi-level subdomain (e.g. "x.y.apex") — not a slug, ignore.
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
		// Dev/test default — operator can override at startup with the
		// real bytes from web/landing.html.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "hostthis — landing page not embedded.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(s.LandingHTML)
}

// servePasteSlug serves the paste for the given slug, with all the
// sandboxing headers. Both the subdomain and the path entry points
// funnel through here.
func (s *Server) servePasteSlug(w http.ResponseWriter, r *http.Request, slug domain.Slug) {
	p, err := s.Pastes.Get(slug)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	now := s.nowOrTime()
	if !now.Before(p.ExpiresAt) {
		// Past the retention window. The background sweep will delete this
		// shortly; we 404 in the meantime so visitors don't see
		// content that's technically expired.
		http.NotFound(w, r)
		return
	}
	body, err := s.Blobs.Get(p.ContentSHA)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Sandboxing headers per SPEC.md HTML-sandboxing section.
	// No Content-Security-Policy — origin isolation (subdomain-per-paste)
	// is the actual security boundary. Same posture as codepen et al.
	// We do keep the headers that don't restrict what the paste's JS
	// can DO inside its own origin:
	//   - X-Frame-Options DENY: no clickjacking embed
	//   - Referrer-Policy no-referrer: visitor's referrer doesn't leak
	//   - Permissions-Policy: deny camera/mic/geo/usb/payment (the
	//     categories that need explicit user grant; we don't want
	//     malicious pastes triggering those prompts)
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), payment=()")

	switch p.Kind {
	case domain.KindHTML:
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(body)
	case domain.KindMarkdown:
		// Render markdown → sanitized HTML on every read. The render
		// is pure and cheap (~1ms for typical docs); a cache keyed
		// on (ContentSHA, render.MarkdownRendererVersion) can land
		// later if cold renders become hot.
		rendered, err := render.Markdown(body)
		if err != nil {
			http.Error(w, "render error", http.StatusInternalServerError)
			return
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(rendered)
	default:
		http.Error(w, "unsupported kind", http.StatusInternalServerError)
	}
}
