// Package web holds what hostthis serves that is not Go code.
//
// shells/ holds the client render shells, one directory each: the page
// template (shell.html, never served) and the renderer assets, described by
// shells/shells.json. The Worker build reads the same files, so both
// servers render the same pages from the same versions.
package web

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Files holds shells/ as embedded at build time.
//
//go:embed shells
var Files embed.FS

const (
	// ShellsDir is the root of the shells in Files.
	ShellsDir = "shells"
	// TemplateName is a page's template in its shell's directory.
	TemplateName = "shell.html"
	tableName    = "shells.json"
)

// ShellTable is shells/shells.json.
type ShellTable struct {
	// CSP names the Content-Security-Policy variants a page is served under.
	// "shell" is the baseline: same-origin script, style and connect, no inline
	// script, no framing or form submission; images and media are open because
	// markdown embeds remote ones, and 'unsafe-inline' covers styles only.
	// "wasmWorker" adds what a WebAssembly renderer needs: 'wasm-unsafe-eval'
	// (compilation only; eval and inline script stay denied) and blob: workers,
	// which the wasm bootstraps from a generated URL. Chosen per shell, so no
	// page carries the union of every renderer's capabilities.
	CSP map[string]string `json:"csp"`
	// Kinds maps each client-rendered content kind to its shell.
	Kinds map[string]Kind `json:"kinds"`
	// Shells maps a directory under shells/ to the shell it holds.
	Shells map[string]Shell `json:"shells"`
}

// Kind is one client-rendered content kind.
type Kind struct {
	Shell string `json:"shell"`
	// Raw is the Content-Type of the kind's ?raw response.
	Raw string `json:"raw"`
}

// Shell is one directory's page and assets. Assets are served at
// /_hostthis/<name>, one flat namespace across shells, so a page can load
// another shell's file by name.
type Shell struct {
	// Version is the page's ETag and the ?v= cache buster substituted for
	// __VER__. Assets are served immutable, so bump it whenever a file changes
	// in a way visitors must re-fetch.
	Version string `json:"version"`
	// CSP names the page's policy in ShellTable.CSP. Empty for a directory
	// with no page.
	CSP string `json:"csp,omitempty"`
	// Assets whitelists the directory's servable files, with their
	// Content-Type.
	Assets map[string]string `json:"assets"`
	Note   string            `json:"note,omitempty"`
}

// LoadShellTable decodes shells/shells.json and checks it against Files.
func LoadShellTable() (ShellTable, error) {
	b, err := Files.ReadFile(ShellsDir + "/" + tableName)
	if err != nil {
		return ShellTable{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var t ShellTable
	if err := dec.Decode(&t); err != nil {
		return ShellTable{}, fmt.Errorf("%s: %w", tableName, err)
	}
	if err := t.check(); err != nil {
		return ShellTable{}, fmt.Errorf("%s: %w", tableName, err)
	}
	return t, nil
}

// check holds the table to the files: every page has a template and a known
// policy, every kind a page, every asset name one owner, and every embedded
// file is a template or a whitelisted asset.
func (t ShellTable) check() error {
	var errs []error
	owner := map[string]string{}
	for dir, sh := range t.Shells {
		if !plainName(dir) {
			errs = append(errs, fmt.Errorf("shell %q: not a directory name", dir))
			continue
		}
		if sh.Version == "" {
			errs = append(errs, fmt.Errorf("shell %q: no version", dir))
		}
		want := map[string]bool{}
		if sh.CSP != "" {
			if t.CSP[sh.CSP] == "" {
				errs = append(errs, fmt.Errorf("shell %q: unknown csp %q", dir, sh.CSP))
			}
			want[TemplateName] = true
		}
		for name, ct := range sh.Assets {
			switch {
			case !plainName(name) || name == TemplateName || name == tableName:
				errs = append(errs, fmt.Errorf("shell %q: %q cannot be an asset", dir, name))
			case ct == "":
				errs = append(errs, fmt.Errorf("shell %q: %q has no content type", dir, name))
			case owner[name] != "":
				errs = append(errs, fmt.Errorf("asset %q: claimed by %q and %q", name, owner[name], dir))
			}
			owner[name] = dir
			want[name] = true
		}
		entries, err := fs.ReadDir(Files, ShellsDir+"/"+dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("shell %q: %w", dir, err))
			continue
		}
		for _, e := range entries {
			if !want[e.Name()] {
				errs = append(errs, fmt.Errorf("shell %q: %s is neither its template nor a listed asset", dir, e.Name()))
			}
			delete(want, e.Name())
		}
		for name := range want {
			errs = append(errs, fmt.Errorf("shell %q: %s is missing", dir, name))
		}
	}
	entries, err := fs.ReadDir(Files, ShellsDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := t.Shells[e.Name()]; e.IsDir() && !ok {
			errs = append(errs, fmt.Errorf("directory %q is not in the table", e.Name()))
		}
	}
	for kind, k := range t.Kinds {
		if t.Shells[k.Shell].CSP == "" {
			errs = append(errs, fmt.Errorf("kind %q: %q is not a shell with a page", kind, k.Shell))
		}
		if k.Raw == "" {
			errs = append(errs, fmt.Errorf("kind %q: no raw content type", kind))
		}
	}
	return errors.Join(errs...)
}

func plainName(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\`)
}
