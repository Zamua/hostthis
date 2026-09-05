package http

import "embed"

// diffShellFS holds the client-side diff render assets: the fixed HTML shell,
// the bootstrap JS, the page CSS, and the vendored diff2html + highlight.js
// libraries with their themes.
//
//go:embed assets/diffshell/*
var diffShellFS embed.FS

// diffShellVersion: bump whenever a file under assets/diffshell/ changes in a
// way visitors must re-fetch (see clientShell.version).
const diffShellVersion = "diffshell-v32"

// diffShellAssets is serveAsset's whitelist for this shell.
var diffShellAssets = map[string]string{
	"diff2html-ui-base.min.js": "text/javascript; charset=utf-8",
	"highlight.min.js":         "text/javascript; charset=utf-8",
	"diff.js":                  "text/javascript; charset=utf-8",
	"diff2html.min.css":        "text/css; charset=utf-8",
	"hljs-light.css":           "text/css; charset=utf-8",
	"hljs-dark.css":            "text/css; charset=utf-8",
	"diff.css":                 "text/css; charset=utf-8",
}
