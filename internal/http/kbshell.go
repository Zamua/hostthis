package http

import "embed"

// kbShellFS holds the knowledge base shell: the fixed page and its bootstrap.
// The interface it will draw (file tree, breadcrumbs, table of contents,
// search) is not built yet; what ships is the page the serving contracts hang
// off, so the server side is exercised end to end.
//
//go:embed assets/kbshell/*
var kbShellFS embed.FS

// kbShellVersion: bump whenever a file under assets/kbshell/ changes in a way
// visitors must re-fetch (see clientShell.version).
const kbShellVersion = "kbshell-v1"

var kbShellAssets = map[string]string{
	"kb.js": "text/javascript; charset=utf-8",
}
