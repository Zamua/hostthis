package http

import "embed"

// kbShellFS holds the knowledge base shell: the fixed page, its stylesheet, and
// the three scripts that draw the interface - the file tree, the search index,
// and the controller that routes between documents and renders them. marked and
// DOMPurify are the markdown shell's: the asset namespace is flat, so a shell
// loads another's vendored library by name rather than embedding a second copy.
//
//go:embed assets/kbshell/*
var kbShellFS embed.FS

// kbShellVersion: bump whenever a file under assets/kbshell/ changes in a way
// visitors must re-fetch (see clientShell.version).
const kbShellVersion = "kbshell-v2"

var kbShellAssets = map[string]string{
	"kb.css":       "text/css; charset=utf-8",
	"kb.js":        "text/javascript; charset=utf-8",
	"kb-tree.js":   "text/javascript; charset=utf-8",
	"kb-search.js": "text/javascript; charset=utf-8",
}
