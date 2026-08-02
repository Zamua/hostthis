package http

import "embed"

// mdShellFS holds the client-side markdown render assets: the fixed HTML
// shell, the bootstrap JS, the page CSS, and the vendored marked + DOMPurify
// libraries. Embedding them means a markdown read serves a content-independent
// shell whose memory cost is constant regardless of paste size; the browser
// does the rendering.
//
//go:embed assets/mdshell/*
var mdShellFS embed.FS

// mdShellVersion tags the fixed shell response's ETag AND is stamped into the
// shell's asset URLs as a ?v= cache-buster: the assets are served `immutable`,
// so a same-path change would otherwise be pinned in browser caches for a year.
// BUMP THIS whenever shell.html / md.js / md.css change in a way visitors must
// re-fetch.
const mdShellVersion = "mdshell-v7"

// mdShellAssets whitelists the asset names serveAsset will serve, mapped to
// their Content-Type. Anything outside it 404s.
var mdShellAssets = map[string]string{
	"marked.min.js": "text/javascript; charset=utf-8",
	"purify.min.js": "text/javascript; charset=utf-8",
	"md.js":         "text/javascript; charset=utf-8",
	"md.css":        "text/css; charset=utf-8",
}
