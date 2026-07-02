package ssh_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// End-to-end coverage for `-o json` over a real SSH session, complementing
// the pure mapper/parser unit tests in output_test.go. These decode stdout
// into TEST-LOCAL structs (a consumer's view of the published wire
// contract), deliberately NOT the internal view types, so the test fails
// if the on-the-wire JSON keys/shape ever drift.

type jsonPaste struct {
	Slug             string  `json:"slug"`
	Name             string  `json:"name"`
	SizeBytes        int     `json:"size_bytes"`
	Kind             string  `json:"kind"`
	ExpiresAt        *string `json:"expires_at"`
	ExpiresInSeconds *int64  `json:"expires_in_seconds"`
	ServedVersion    int     `json:"served_version"`
	LatestVersion    int     `json:"latest_version"`
	PinnedVersion    int     `json:"pinned_version"`
}

type jsonVersions struct {
	Slug          string  `json:"slug"`
	PinnedVersion int     `json:"pinned_version"`
	ExpiresAt     *string `json:"expires_at"`
	Versions      []struct {
		Version   int    `json:"version"`
		CreatedAt string `json:"created_at"`
		SizeBytes *int   `json:"size_bytes"`
		Deleted   bool   `json:"deleted"`
		Current   bool   `json:"current"`
	} `json:"versions"`
}

type jsonWhoami struct {
	Key          string  `json:"key"`
	FirstSeen    *string `json:"first_seen"`
	ActivePastes int     `json:"active_pastes"`
	UsedBytes    int     `json:"used_bytes"`
	QuotaBytes   *int    `json:"quota_bytes"`
	Session      *struct {
		Subnet string `json:"subnet"`
	} `json:"session"`
}

func TestListJSON_E2E(t *testing.T) {
	s := startStack(t)
	_, _, _ = s.run(`--name "demo"`, []byte("<!doctype html><p>a</p>"))
	_, _, _ = s.run("", []byte("# md\nbody"))

	stdout, stderr, exit := s.run("list -o json", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr %q)", exit, stderr)
	}
	var views []jsonPaste
	if err := json.Unmarshal([]byte(stdout), &views); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%q", err, stdout)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 pastes, got %d: %q", len(views), stdout)
	}
	names := map[string]bool{}
	for _, v := range views {
		names[v.Name] = true
		if v.Slug == "" || v.Kind == "" {
			t.Fatalf("missing slug/kind in view: %+v", v)
		}
		if v.SizeBytes <= 0 {
			t.Fatalf("size_bytes should be a positive int: %+v", v)
		}
		if v.ServedVersion < 1 {
			t.Fatalf("served_version should be >= 1: %+v", v)
		}
	}
	if !names["demo"] {
		t.Fatalf("named paste 'demo' missing from json: %q", stdout)
	}
	// The unnamed paste is the empty string, NOT the "-" table sentinel.
	if !names[""] {
		t.Fatalf("unnamed paste should have empty-string name, got names=%v", names)
	}
}

func TestListJSON_GluedShortForm(t *testing.T) {
	s := startStack(t)
	_, _, _ = s.run("", []byte("<!doctype html><p>a</p>"))

	// The glued short form `-ojson` (no space, no =) must work over the
	// real ssh path, same as `-o json`.
	stdout, stderr, exit := s.run("list -ojson", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr %q)", exit, stderr)
	}
	var views []jsonPaste
	if err := json.Unmarshal([]byte(stdout), &views); err != nil {
		t.Fatalf("list -ojson did not produce a JSON array: %v\n%q", err, stdout)
	}
	if len(views) != 1 {
		t.Fatalf("want 1 paste, got %d: %q", len(views), stdout)
	}
}

func TestListJSON_EmptyIsArrayNotNarration(t *testing.T) {
	s := startStack(t)
	// The default keyed client uploaded nothing.
	stdout, _, exit := s.run("list -o json", nil)
	if exit != 0 {
		t.Fatalf("exit: %d", exit)
	}
	if strings.TrimSpace(stdout) != "[]" {
		t.Fatalf("empty list -o json should be [] on stdout, got %q", stdout)
	}
}

func TestVersionsJSON_E2E(t *testing.T) {
	s := startStack(t)
	up, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	slug := extractSlug(up)
	_, _, _ = s.run(slug, []byte("<!doctype html><p>v2</p>"))

	stdout, stderr, exit := s.run("versions "+slug+" -o json", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr %q)", exit, stderr)
	}
	var view jsonVersions
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("stdout is not the versions JSON object: %v\n%q", err, stdout)
	}
	if view.Slug != slug {
		t.Fatalf("slug: got %q want %q", view.Slug, slug)
	}
	if view.PinnedVersion != 0 {
		t.Fatalf("fresh paste should be unpinned, got pinned_version=%d", view.PinnedVersion)
	}
	if len(view.Versions) != 2 {
		t.Fatalf("want 2 versions, got %d: %q", len(view.Versions), stdout)
	}
	// Newest first: v2 is current (unpinned -> latest).
	if view.Versions[0].Version != 2 || !view.Versions[0].Current {
		t.Fatalf("v2 should be first + current: %+v", view.Versions[0])
	}
}

func TestWhoamiJSON_E2E(t *testing.T) {
	s := startStack(t)
	_, _, _ = s.run("", []byte("<!doctype html><p>a</p>"))

	stdout, stderr, exit := s.run("whoami -o json", nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr %q)", exit, stderr)
	}
	var view jsonWhoami
	if err := json.Unmarshal([]byte(stdout), &view); err != nil {
		t.Fatalf("stdout is not the whoami JSON object: %v\n%q", err, stdout)
	}
	if view.Key == "" {
		t.Fatalf("whoami json missing key: %q", stdout)
	}
	if view.ActivePastes != 1 {
		t.Fatalf("active_pastes: got %d want 1", view.ActivePastes)
	}
}

func TestOutputFormat_UnknownIsUsageError(t *testing.T) {
	s := startStack(t)
	stdout, stderr, exit := s.run("list -o yaml", nil)
	if exit != 2 { // ExitUsage
		t.Fatalf("unknown format should exit ExitUsage(2), got %d", exit)
	}
	if !strings.Contains(stderr, "unknown output format") {
		t.Fatalf("stderr should explain the bad format, got %q", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Fatalf("no data should print on stdout for a usage error, got %q", stdout)
	}
}
