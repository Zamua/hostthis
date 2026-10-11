package http

import (
	"embed"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/web"
)

// A clientShell is one kind's fixed, content-independent render page: the
// browser fetches the paste's raw bytes via ?raw and renders them, so server
// memory stays constant regardless of paste size. Shells are declared in
// web/shells/shells.json; adding a kind is a table entry there.
type clientShell struct {
	// version tags the response ETag and is substituted for __VER__ in the
	// shell's asset URLs as a ?v= cache-buster.
	version string
	fs      embed.FS
	dir     string
	// assets whitelists the names serveAsset will serve from dir, mapped to
	// their Content-Type. A name outside every shell's whitelist 404s, so no
	// path traversal or arbitrary embedded-file disclosure is possible.
	assets map[string]string
	// csp is the page's Content-Security-Policy; empty for a directory with no
	// page.
	csp string
}

// policy returns the Content-Security-Policy for this shell.
func (s *clientShell) policy() string { return s.csp }

// html returns the shell page with its version stamped into the asset URLs and
// the kind stamped into the root element. The kind is passed rather than
// sniffed client-side: one shell serves both csv and json, and a viewer that
// re-derives the kind can disagree with the stored one on an ambiguous file (a
// JSON array of arrays is also a table).
func (s *clientShell) html(kind domain.ContentKind) []byte {
	b, err := s.fs.ReadFile(s.dir + "/" + web.TemplateName)
	if err != nil {
		// The template is compiled into the binary, so a read failure means
		// the build is broken; that surfaces in tests.
		return nil
	}
	out := strings.ReplaceAll(string(b), "__VER__", s.version)
	return []byte(strings.ReplaceAll(out, "__KIND__", string(kind)))
}

var (
	// shellsByDir holds one clientShell per directory, so kinds sharing a
	// shell (csv and json) share one pointer and cannot drift apart.
	shellsByDir = map[string]*clientShell{}
	// shells maps each client-rendered kind to its shell. A kind absent here
	// is served directly (HTML) or is not a paste at all (site). The knowledge
	// base is reached through the directory path, since its shell answers at
	// many paths rather than at one.
	shells = map[domain.ContentKind]*clientShell{}
	// rawContentType is the Content-Type of each client-rendered kind's ?raw
	// response, served from the blob verbatim.
	rawContentType = map[domain.ContentKind]string{}
	// assetSource resolves a flat asset name to the shell that owns it.
	assetSource = map[string]*clientShell{}
)

func init() {
	table, err := web.LoadShellTable()
	if err != nil {
		// Embedded at build time: a bad table is a broken build.
		panic(err)
	}
	for dir, sh := range table.Shells {
		cs := &clientShell{
			version: sh.Version,
			fs:      web.Files,
			dir:     web.ShellsDir + "/" + dir,
			assets:  sh.Assets,
			csp:     table.CSP[sh.CSP],
		}
		shellsByDir[dir] = cs
		for name := range sh.Assets {
			assetSource[name] = cs
		}
	}
	for kind, k := range table.Kinds {
		shells[domain.ContentKind(kind)] = shellsByDir[k.Shell]
		rawContentType[domain.ContentKind(kind)] = k.Raw
	}
}

// shellFor returns the shell for kind, or nil when the kind renders without one.
func shellFor(kind domain.ContentKind) *clientShell { return shells[kind] }

// serveAsset serves a single whitelisted client-render asset under
// /_hostthis/<name>. The assets are immutable (pinned by the shell version in
// their URLs), so they get a year-long immutable cache.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	sh, ok := assetSource[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := sh.fs.Open(sh.dir + "/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	h := w.Header()
	h.Set("Content-Type", sh.assets[name])
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = io.Copy(w, f)
}
