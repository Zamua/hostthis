package http

import (
	"embed"
	"strings"
)

// diffShellFS holds the client-side diff render assets: the fixed HTML
// shell, the bootstrap JS, the page CSS, and the vendored diff2html +
// highlight.js libraries (and their highlight themes). They are embedded
// so a diff read serves a content-independent shell whose memory cost is
// constant regardless of paste size; the browser does the rendering.
// Mirrors mdShellFS.
//
//go:embed assets/diffshell/*
var diffShellFS embed.FS

// diffShellVersion tags the fixed diff-shell response's ETag AND is
// stamped into the shell's asset URLs as a ?v= cache-buster (the assets
// are served `immutable`, so a same-path change would otherwise be pinned
// in browser caches for a year). The shell is content-independent, so its
// ETag does NOT depend on the paste content. BUMP THIS whenever any file
// under assets/diffshell/ changes in a way visitors must re-fetch.
//
// v1: initial diff2html + highlight.js render shell.
// v2: fix the line-number gutter bleeding under horizontally-scrolled code.
// v3: make that gutter a sticky, opaque column so the numbers stay in view
//
//	(pinned left, GitHub-style) on horizontal scroll instead of scrolling
//	away - and never show the code through a translucent background.
//
// v4: make the sticky gutter a COMPLETE opaque block - kill diff2html's
//
//	translucent cell side-borders and bridge the sub-pixel gaps between
//	stacked cells (opaque box-shadow halo) so no colour leaks through.
const diffShellVersion = "diffshell-v4"

// diffShellHTML returns the fixed diff render shell with diffShellVersion
// substituted into the asset URLs' ?v= cache-buster.
func diffShellHTML() []byte {
	b, err := diffShellFS.ReadFile("assets/diffshell/shell.html")
	if err != nil {
		// Embedded asset is compiled into the binary; a read failure here
		// means the build is broken, which surfaces in tests.
		return nil
	}
	return []byte(strings.ReplaceAll(string(b), "__VER__", diffShellVersion))
}

// diffShellAssets is the whitelist of asset names serveDiffAsset will
// serve, mapped to their Content-Type. Anything not in this set 404s, so
// no path traversal or arbitrary embedded-file disclosure is possible.
var diffShellAssets = map[string]string{
	"diff2html-ui-base.min.js": "text/javascript; charset=utf-8",
	"highlight.min.js":         "text/javascript; charset=utf-8",
	"diff.js":                  "text/javascript; charset=utf-8",
	"diff2html.min.css":        "text/css; charset=utf-8",
	"hljs-light.css":           "text/css; charset=utf-8",
	"hljs-dark.css":            "text/css; charset=utf-8",
	"diff.css":                 "text/css; charset=utf-8",
}
