package http

import (
	"embed"
	"io"
	"net/http"
	"path"
	"strings"

	"github.com/Zamua/hostthis/internal/domain"
)

// A clientShell is one kind's fixed, content-independent render page: the
// browser fetches the paste's raw bytes via ?raw and renders them, so server
// memory stays constant regardless of paste size.
//
// Every field a shell needs is declared here rather than in per-kind functions
// so adding a kind is a table entry. The asset namespace (/_hostthis/<name>) is
// FLAT and shared across shells, which is what lets the markdown shell
// lazy-load the mermaid renderer that the mermaid shell owns.
type clientShell struct {
	// version tags the response ETag and is substituted for __VER__ in the
	// shell's asset URLs as a ?v= cache-buster. The assets are served
	// immutable, so a same-path change is otherwise pinned in caches for a
	// year. BUMP IT whenever a file under the shell's dir changes in a way
	// visitors must re-fetch.
	version string
	fs      embed.FS
	dir     string
	// assets whitelists the names serveAsset will serve from dir, mapped to
	// their Content-Type. A name outside every shell's whitelist 404s, so no
	// path traversal or arbitrary embedded-file disclosure is possible.
	assets map[string]string
	// csp overrides shellCSP for shells whose renderer needs a capability the
	// baseline denies. Empty means the baseline.
	//
	// Per-shell rather than one widened global policy: the markdown and HTML
	// shells must keep the strictest policy that works, and a single policy
	// permissive enough for every renderer would hand every paste the union of
	// what any renderer needs.
	csp string
}

// policy returns the Content-Security-Policy for this shell.
func (s *clientShell) policy() string {
	if s.csp != "" {
		return s.csp
	}
	return shellCSP
}

// wasmWorkerCSP is shellCSP plus the two capabilities a WebAssembly renderer
// needs: compiling the module, and running it on a worker thread.
//
// 'wasm-unsafe-eval' permits WebAssembly compilation ONLY; it does not restore
// eval() or inline script, so the baseline's protection against injected
// JavaScript is intact. blob: is required on worker-src because the wasm
// bootstraps its worker from a generated blob URL.
const wasmWorkerCSP = "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; " +
	"worker-src 'self' blob:; child-src 'self' blob:; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: http: https:; media-src 'self' data: http: https:; " +
	"font-src 'self' data: https:; connect-src 'self' blob:; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'none'"

// html returns the shell page with its version stamped into the asset URLs and
// the kind stamped into the root element.
//
// The kind is passed to the page rather than left for the script to sniff:
// one shell serves both csv and json, the server already classified the bytes
// at upload, and a viewer that re-derives that can disagree with the stored
// kind on a file that is ambiguous (a JSON array of arrays is also a table).
func (s *clientShell) html(kind domain.ContentKind) []byte {
	b, err := s.fs.ReadFile(s.dir + "/shell.html")
	if err != nil {
		// The asset is compiled into the binary, so a read failure means the
		// build is broken; that surfaces in tests.
		return nil
	}
	out := strings.ReplaceAll(string(b), "__VER__", s.version)
	return []byte(strings.ReplaceAll(out, "__KIND__", string(kind)))
}

// dataShell is ONE shell shared by csv and json, declared once so the two kinds
// are the same pointer rather than two structs that can drift apart.
var dataShell = &clientShell{
	version: dataShellVersion, fs: dataShellFS, dir: "assets/datashell",
	assets: dataShellAssets, csp: wasmWorkerCSP,
}

// shells maps each client-rendered kind to its shell. A kind absent from this
// map is served directly (HTML) or is not a paste at all (site).
var shells = map[domain.ContentKind]*clientShell{
	domain.KindMarkdown:   {version: mdShellVersion, fs: mdShellFS, dir: "assets/mdshell", assets: mdShellAssets},
	domain.KindDiff:       {version: diffShellVersion, fs: diffShellFS, dir: "assets/diffshell", assets: diffShellAssets},
	domain.KindMermaid:    {version: mermaidShellVersion, fs: mermaidShellFS, dir: "assets/mermaidshell", assets: mermaidShellAssets},
	domain.KindPDF:        {version: pdfShellVersion, fs: pdfShellFS, dir: "assets/pdfshell", assets: pdfShellAssets, csp: wasmWorkerCSP},
	domain.KindCSV:        dataShell,
	domain.KindJSON:       dataShell,
	domain.KindFlamegraph: {version: flameShellVersion, fs: flameShellFS, dir: "assets/flameshell", assets: flameShellAssets},
	domain.KindText:       {version: textShellVersion, fs: textShellFS, dir: "assets/textshell", assets: textShellAssets},
	domain.KindLog:        {version: logShellVersion, fs: logShellFS, dir: "assets/logshell", assets: logShellAssets},
}

// rawContentType is the Content-Type each client-rendered kind's ?raw response
// carries. A kind here is served from the blob verbatim; the shell fetches it.
//
// PDF is the one kind whose "raw" IS the representation a browser can use
// directly, so its type has to be right for the viewer and for a plain curl.
var rawContentType = map[domain.ContentKind]string{
	domain.KindMarkdown:   "text/markdown; charset=utf-8",
	domain.KindDiff:       "text/plain; charset=utf-8",
	domain.KindMermaid:    "text/plain; charset=utf-8",
	domain.KindCSV:        "text/plain; charset=utf-8",
	domain.KindJSON:       "application/json; charset=utf-8",
	domain.KindPDF:        "application/pdf",
	domain.KindFlamegraph: "text/plain; charset=utf-8",
	domain.KindText:       "text/plain; charset=utf-8",
	domain.KindLog:        "application/x-ndjson; charset=utf-8",
}

// shellFor returns the shell for kind, or nil when the kind renders without one.
func shellFor(kind domain.ContentKind) *clientShell { return shells[kind] }

// commonShell is not a kind's shell: it holds assets EVERY shell loads. Deep
// link resolution lives here because it is a property of the client-render
// architecture (see docs/SPEC.md "The load order rule") rather than of any one
// viewer, and copying it per shell is how one of them ends up subtly different.
//
//go:embed assets/common/*
var commonShellFS embed.FS

var commonShell = &clientShell{
	version: commonVersion,
	fs:      commonShellFS,
	dir:     "assets/common",
	assets: map[string]string{
		"deeplink.js":    "text/javascript; charset=utf-8",
		"linegutter.js":  "text/javascript; charset=utf-8",
		"linegutter.css": "text/css; charset=utf-8",
	},
}

// commonVersion busts the cache for the shared assets. Every shell stamps its
// OWN version into its asset URLs, so bumping this alone is not enough: bump
// the shells that must re-fetch too.
const commonVersion = "common-v6"

// assetSource resolves a flat asset name to the shell that owns it. Built once
// from the shell table so a new shell needs no change here.
//
// Kinds sharing ONE shell (csv and json) are the same pointer, so they
// contribute identical entries and cannot conflict.
var assetSource = func() map[string]*clientShell {
	m := map[string]*clientShell{}
	for _, sh := range shells {
		for name := range sh.assets {
			m[name] = sh
		}
	}
	for name := range commonShell.assets {
		m[name] = commonShell
	}
	return m
}()

// serveAsset serves a single whitelisted client-render asset under
// /_hostthis/<name>. The assets are immutable (vendored libs pinned by version,
// the bootstrap + CSS tied to the shell version), so they get a year-long
// immutable cache.
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
