package http

import "embed"

// mermaidShellFS holds the client-side Mermaid render assets. The renderer is
// large (~3.4 MB), which is why the MARKDOWN shell loads it lazily and only
// when a fetched document actually contains a mermaid fence; a prose paste
// never pays for it. This shell, serving a bare diagram, always needs it.
//
//go:embed assets/mermaidshell/*
var mermaidShellFS embed.FS

const mermaidShellVersion = "mermaidshell-v1"

var mermaidShellAssets = map[string]string{
	// Owned here but shared: the markdown shell fetches this same URL when it
	// finds a fence. The asset namespace is flat, so it resolves either way.
	"mermaid.min.js": "text/javascript; charset=utf-8",
	"mermaid.js":     "text/javascript; charset=utf-8",
	"mermaid.css":    "text/css; charset=utf-8",
}

// pdfShellFS holds pdf.js and the viewer bootstrap. pdf.js is loaded as an ES
// module and spawns its own worker, so both files are served from the flat
// asset namespace.
//
//go:embed assets/pdfshell/*
var pdfShellFS embed.FS

const pdfShellVersion = "pdfshell-v4"

var pdfShellAssets = map[string]string{
	"pdf.min.mjs":        "text/javascript; charset=utf-8",
	"pdf.worker.min.mjs": "text/javascript; charset=utf-8",
	"pdf.js":             "text/javascript; charset=utf-8",
	"pdf.css":            "text/css; charset=utf-8",
}

// dataShellFS holds the tabular/tree viewer for csv and json. ONE shell serves
// both kinds: the table, the stats, the deep-link handling, and the SQL console
// are identical, and the two differ only in how bytes become rows. Splitting
// them would duplicate all of that to vary a parser.
//
// duckdb-eh.wasm is 35 MB and is fetched ONLY when the SQL console is opened.
// Sorting, filtering, and column stats are computed in plain JS and never touch
// it, so the default view stays as fast as any other shell.
//
//go:embed assets/datashell/*
var dataShellFS embed.FS

const dataShellVersion = "datashell-v7"

var dataShellAssets = map[string]string{
	"data.js":                     "text/javascript; charset=utf-8",
	"data.css":                    "text/css; charset=utf-8",
	"duckdb-browser.mjs":          "text/javascript; charset=utf-8",
	"duckdb-browser-eh.worker.js": "text/javascript; charset=utf-8",
	"duckdb-eh.wasm":              "application/wasm",
	// duckdb's ESM ships bare specifiers ("apache-arrow") that no browser can
	// resolve and that script-src 'self' would refuse from a CDN. They are
	// vendored here and their specifiers rewritten to this flat namespace at
	// vendor time, so the shell needs no inline import map.
	"apache-arrow.js": "text/javascript; charset=utf-8",
	"flatbuffers.js":  "text/javascript; charset=utf-8",
	"tslib.js":        "text/javascript; charset=utf-8",
	// CodeMirror is a single esbuild bundle on purpose: loading its packages
	// separately gives each one its own @codemirror/state, and CodeMirror
	// throws on duplicate state instances. Lazy-loaded with the console.
	"codemirror.min.js": "text/javascript; charset=utf-8",
}

// flameShellFS holds the flame graph renderer for folded stack profiles. No
// profiling library is vendored: the format is one line per stack, and the
// renderer is a prefix-tree aggregation plus a rectangle layout.
//
//go:embed assets/flameshell/*
var flameShellFS embed.FS

const flameShellVersion = "flameshell-v1"

var flameShellAssets = map[string]string{
	"flame.js":  "text/javascript; charset=utf-8",
	"flame.css": "text/css; charset=utf-8",
}
