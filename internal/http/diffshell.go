package http

import "embed"

// diffShellFS holds the client-side diff render assets: the fixed HTML shell,
// the bootstrap JS, the page CSS, and the vendored diff2html + highlight.js
// libraries with their themes. Embedding them lets a diff read serve a
// content-independent shell whose memory cost is constant regardless of paste
// size; the browser does the rendering. Mirrors mdShellFS.
//
//go:embed assets/diffshell/*
var diffShellFS embed.FS

// diffShellVersion tags the fixed diff-shell response's ETag AND is stamped
// into the shell's asset URLs as a ?v= cache-buster: the assets are served
// `immutable`, so a same-path change would otherwise be pinned in browser
// caches for a year. The shell is content-independent, so its ETag does NOT
// depend on the paste content. BUMP THIS whenever any file under
// assets/diffshell/ changes in a way visitors must re-fetch.
const diffShellVersion = "diffshell-v23"

// diffShellAssets is the whitelist of asset names serveAsset will serve, mapped
// to their Content-Type. Anything not in this set 404s, so no path traversal or
// arbitrary embedded-file disclosure is possible.
var diffShellAssets = map[string]string{
	"diff2html-ui-base.min.js": "text/javascript; charset=utf-8",
	"highlight.min.js":         "text/javascript; charset=utf-8",
	"diff.js":                  "text/javascript; charset=utf-8",
	"diff2html.min.css":        "text/css; charset=utf-8",
	"hljs-light.css":           "text/css; charset=utf-8",
	"hljs-dark.css":            "text/css; charset=utf-8",
	"diff.css":                 "text/css; charset=utf-8",
}
