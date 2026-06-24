package http

import (
	"embed"
	"io"
	"net/http"
	"path"
	"strings"
)

// mdShellFS holds the client-side markdown render assets: the fixed HTML
// shell, the bootstrap JS, the page CSS, and the vendored marked +
// DOMPurify libraries. They are embedded so a markdown read serves a
// content-independent shell whose memory cost is constant regardless of
// paste size; the browser does the rendering.
//
//go:embed assets/mdshell/*
var mdShellFS embed.FS

// mdShellVersion tags the fixed shell response's ETag AND is stamped into
// the shell's asset URLs as a ?v= cache-buster (the assets are served
// `immutable`, so a same-path change would otherwise be pinned in browser
// caches for a year). The shell is content-independent, so its ETag does
// NOT depend on the paste content. BUMP THIS whenever shell.html / md.js /
// md.css change in a way visitors must re-fetch (e.g. a style change).
//
// v2: restyled markdown to a GitHub-like system-sans look.
const mdShellVersion = "mdshell-v2"

// shellHTML returns the fixed markdown render shell with mdShellVersion
// substituted into the asset URLs' ?v= cache-buster.
func shellHTML() []byte {
	b, err := mdShellFS.ReadFile("assets/mdshell/shell.html")
	if err != nil {
		// Embedded asset is compiled into the binary; a read failure here
		// means the build is broken, which surfaces in tests.
		return nil
	}
	return []byte(strings.ReplaceAll(string(b), "__VER__", mdShellVersion))
}

// mdShellAssets is the whitelist of asset names serveAsset will serve,
// mapped to their Content-Type. Anything not in this set 404s, so no
// path traversal or arbitrary embedded-file disclosure is possible.
var mdShellAssets = map[string]string{
	"marked.min.js": "text/javascript; charset=utf-8",
	"purify.min.js": "text/javascript; charset=utf-8",
	"md.js":         "text/javascript; charset=utf-8",
	"md.css":        "text/css; charset=utf-8",
}

// serveAsset serves a single whitelisted client-render asset under
// /_hostthis/<name>. The assets are immutable (vendored libs pinned by
// version, the bootstrap + CSS tied to mdShellVersion), so they get a
// year-long immutable cache. Any name outside the whitelist 404s.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := path.Base(r.URL.Path)
	ct, ok := mdShellAssets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := mdShellFS.Open("assets/mdshell/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = io.Copy(w, f)
}
