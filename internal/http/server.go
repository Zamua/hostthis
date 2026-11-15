// Package http serves hostthisd's own pages: the apex landing page, the agent
// notes beside it, and the health endpoints. Pastes, sites, rooms and the
// render shells are served by the Worker's public surface, never here.
package http

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// Server bundles the dependencies.
type Server struct {
	// Solid answers the apex /_solid/ paths (docs/SPEC.md "Pod storage");
	// nil leaves them 404.
	Solid       http.Handler
	LandingHTML []byte // optional; apex landing page bytes
	// AgentNotes is /llms.txt (and /AGENTS.md) beside the landing page; nil 404s.
	AgentNotes []byte
	// ApexDomain is the public apex, e.g. "hostthis.dev". A slug's host under it
	// is not the apex, so it gets none of the apex's pages.
	ApexDomain string
	// SlugSuffix follows the slug in an app's host; empty means ".<ApexDomain>".
	SlugSuffix string
	// Color labels the replica in blue/green deploys, echoed in the
	// X-Backend-Color header on /healthz. Empty for single-replica deploys.
	Color string
}

// Handler returns the mux the caller binds with http.ListenAndServe.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/_solid/", func(w http.ResponseWriter, r *http.Request) {
		if _, onSlug := s.slugFromHost(r.Host); onSlug || s.Solid == nil {
			http.NotFound(w, r)
			return
		}
		s.Solid.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Health endpoints answer on any Host.
		switch r.URL.Path {
		case "/healthz":
			s.serveHealthz(w, r)
			return
		case "/readyz":
			s.serveReadyz(w, r)
			return
		}
		if _, onSlug := s.slugFromHost(r.Host); onSlug {
			http.NotFound(w, r)
			return
		}
		switch {
		case r.URL.Path == "/":
			s.serveLanding(w, r)
		case (r.URL.Path == "/llms.txt" || r.URL.Path == "/AGENTS.md") && len(s.AgentNotes) > 0:
			w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
			w.Header().Set("Cache-Control", "public, max-age=300")
			_, _ = w.Write(s.AgentNotes)
		default:
			http.NotFound(w, r)
		}
	})
	return mux
}

func (s *Server) serveHealthz(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if s.Color != "" {
		h.Set("X-Backend-Color", s.Color)
	}
	_, _ = w.Write([]byte("ok\n"))
}

// slugFromHost returns (slug, true) when host is "<slug><suffix>" and the slug
// parses cleanly. The port, if present, is ignored.
func (s *Server) slugFromHost(host string) (domain.Slug, bool) {
	if s.ApexDomain == "" {
		return "", false
	}
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	suffix := s.SlugSuffix
	if suffix == "" {
		suffix = "." + s.ApexDomain
	}
	sub, ok := strings.CutSuffix(host, suffix)
	if !ok || strings.Contains(sub, ".") {
		// Not under the apex, or a multi-level subdomain ("x.y.apex").
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
		// Dev/test default; a deploy loads web/landing.html.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "hostthis - landing page not embedded.")
		return
	}
	// Short max-age: landing copy changes often, and a copy fix should be
	// visible within minutes without hitting origin per visitor.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.LandingHTML)
}
