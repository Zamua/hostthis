package http

import "embed"

// mdShellFS holds the client-side markdown render assets: the fixed HTML
// shell, the bootstrap JS, the page CSS, and the vendored marked + DOMPurify
// libraries.
//
//go:embed assets/mdshell/*
var mdShellFS embed.FS

// mdShellVersion: bump whenever a file under assets/mdshell/ changes in a way
// visitors must re-fetch (see clientShell.version).
const mdShellVersion = "mdshell-v7"

// mdShellAssets is serveAsset's whitelist for this shell.
var mdShellAssets = map[string]string{
	"marked.min.js": "text/javascript; charset=utf-8",
	"purify.min.js": "text/javascript; charset=utf-8",
	"md.js":         "text/javascript; charset=utf-8",
	"md.css":        "text/css; charset=utf-8",
}
