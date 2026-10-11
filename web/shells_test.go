package web

import (
	"maps"
	"strings"
	"testing"
)

// The embedded table loads and agrees with the embedded files.
func TestLoadShellTable(t *testing.T) {
	table, err := LoadShellTable()
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Kinds) == 0 || len(table.Shells) == 0 {
		t.Fatal("empty table")
	}
	if table.Shells["datashell"].Assets["duckdb-eh.wasm"] != "application/wasm" {
		t.Fatal("datashell does not list its wasm")
	}
}

// check refuses each way the table can disagree with the files or itself.
func TestShellTable_CheckRefuses(t *testing.T) {
	base, err := LoadShellTable()
	if err != nil {
		t.Fatal(err)
	}
	clone := func() ShellTable {
		c := ShellTable{CSP: maps.Clone(base.CSP), Kinds: maps.Clone(base.Kinds), Shells: map[string]Shell{}}
		for dir, sh := range base.Shells {
			sh.Assets = maps.Clone(sh.Assets)
			c.Shells[dir] = sh
		}
		return c
	}
	cases := map[string]struct {
		edit func(*ShellTable)
		want string
	}{
		"asset claimed twice": {func(t *ShellTable) { t.Shells["textshell"].Assets["md.js"] = "text/javascript" }, "claimed by"},
		"unlisted file":       {func(t *ShellTable) { delete(t.Shells["mdshell"].Assets, "md.js") }, "neither its template"},
		"missing file":        {func(t *ShellTable) { t.Shells["mdshell"].Assets["gone.js"] = "text/javascript" }, "missing"},
		"template as asset":   {func(t *ShellTable) { t.Shells["mdshell"].Assets[TemplateName] = "text/html" }, "cannot be an asset"},
		"no content type":     {func(t *ShellTable) { t.Shells["mdshell"].Assets["md.js"] = "" }, "no content type"},
		"unknown csp": {func(t *ShellTable) {
			sh := t.Shells["mdshell"]
			sh.CSP = "loose"
			t.Shells["mdshell"] = sh
		}, "unknown csp"},
		"kind without a page":  {func(t *ShellTable) { t.Kinds["markdown"] = Kind{Shell: "common", Raw: "text/plain"} }, "not a shell with a page"},
		"kind without raw":     {func(t *ShellTable) { t.Kinds["markdown"] = Kind{Shell: "mdshell"} }, "no raw content type"},
		"directory not listed": {func(t *ShellTable) { delete(t.Shells, "flameshell") }, "not in the table"},
		"no version": {func(t *ShellTable) {
			sh := t.Shells["textshell"]
			sh.Version = ""
			t.Shells["textshell"] = sh
		}, "no version"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			table := clone()
			c.edit(&table)
			err := table.check()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("check: %v, want an error containing %q", err, c.want)
			}
		})
	}
}
