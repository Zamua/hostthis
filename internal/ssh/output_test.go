package ssh

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

func TestParseOutputFormat(t *testing.T) {
	cases := []struct {
		name       string
		argv       []string
		wantFormat outputFormat
		wantRest   []string
		wantErr    bool
	}{
		{"no flag defaults to table", []string{"abc12345"}, formatTable, []string{"abc12345"}, false},
		{"empty argv", nil, formatTable, []string{}, false},
		{"-o json", []string{"-o", "json"}, formatJSON, []string{}, false},
		{"--output json", []string{"--output", "json"}, formatJSON, []string{}, false},
		{"-o=json", []string{"-o=json"}, formatJSON, []string{}, false},
		{"--output=json", []string{"--output=json"}, formatJSON, []string{}, false},
		{"-ojson glued short form", []string{"-ojson"}, formatJSON, []string{}, false},
		{"-otable glued short form", []string{"-otable"}, formatTable, []string{}, false},
		{"-ojson glued after positional", []string{"abc12345", "-ojson"}, formatJSON, []string{"abc12345"}, false},
		{"-oyaml glued unknown value", []string{"-oyaml"}, "", nil, true},
		{"-o table explicit", []string{"-o", "table"}, formatTable, []string{}, false},
		{"flag after positional", []string{"abc12345", "-o", "json"}, formatJSON, []string{"abc12345"}, false},
		{"flag before positional", []string{"-o", "json", "abc12345"}, formatJSON, []string{"abc12345"}, false},
		{"unknown format value", []string{"-o", "yaml"}, "", nil, true},
		{"-o with no value", []string{"-o"}, "", nil, true},
		{"--output= empty value", []string{"--output="}, "", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotFormat, gotRest, err := parseOutputFormat(c.argv)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got format=%q rest=%v", gotFormat, gotRest)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotFormat != c.wantFormat {
				t.Fatalf("format: got %q want %q", gotFormat, c.wantFormat)
			}
			if !reflect.DeepEqual(gotRest, c.wantRest) {
				t.Fatalf("rest: got %v want %v", gotRest, c.wantRest)
			}
		})
	}
}

// listOutput renders the list verb for the given rows.
func listOutput(t *testing.T, format outputFormat, pastes []domain.Paste, sites []domain.Site) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if err := writeList(&stdout, &stderr, format, newListView(pastes, sites)); err != nil {
		t.Fatalf("writeList: %v", err)
	}
	return stdout.String(), stderr.String()
}

// listJSON is the list verb's -o json array, one object per row.
func listJSON(t *testing.T, pastes []domain.Paste, sites []domain.Site) []map[string]any {
	t.Helper()
	out, _ := listOutput(t, formatJSON, pastes, sites)
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("list json %q: %v", out, err)
	}
	return rows
}

// The list -o json row: naming, the served/latest/pinned triple, and
// size_bytes as what the QUOTA CHARGED (every live version for a paste,
// StoredBytes for a site), so a list sums to the figure whoami shows.
// served_size_bytes carries the served version's own size so a consumer never
// has to infer which number it holds.
func TestListJSONRow(t *testing.T) {
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{Key: "a", Size: 4000})
	cases := []struct {
		name  string
		paste *domain.Paste
		site  *domain.Site
		want  map[string]any
	}{
		{"unnamed paste keeps empty name, not the table dash",
			&domain.Paste{Slug: "abc12345", Kind: "html"}, nil,
			map[string]any{"name": "", "kind": "html", "size_bytes": 0.0, "served_size_bytes": 0.0, "served_version": 0.0, "latest_version": 0.0, "pinned_version": 0.0}},
		{"unpinned serves latest",
			&domain.Paste{Slug: "s", Kind: "html", LatestVersion: 5, Size: 7}, nil,
			map[string]any{"kind": "html", "size_bytes": 7.0, "served_size_bytes": 7.0, "served_version": 5.0, "latest_version": 5.0, "pinned_version": 0.0}},
		{"pinned serves the pin",
			&domain.Paste{Slug: "s", Kind: "html", PinnedVersion: 3, LatestVersion: 5, Size: 7}, nil,
			map[string]any{"size_bytes": 7.0, "served_version": 3.0, "latest_version": 5.0, "pinned_version": 3.0}},
		{"multi-version paste is charged every live version",
			&domain.Paste{Slug: "p", Kind: "html", Size: 14266, StoredBytes: 40890, LatestVersion: 3}, nil,
			map[string]any{"size_bytes": 40890.0, "served_size_bytes": 14266.0, "served_version": 3.0}},
		{"absent stored total falls back to the served size, never zero",
			&domain.Paste{Slug: "p", Kind: "html", Size: 777, LatestVersion: 1}, nil,
			map[string]any{"size_bytes": 777.0, "served_size_bytes": 777.0}},
		{"site is charged StoredBytes, not the manifest total, with null versions",
			nil, &domain.Site{Slug: "sitezzz1", Kind: domain.KindSite, Manifest: man, StoredBytes: 1500},
			map[string]any{"kind": "site", "size_bytes": 1500.0, "served_size_bytes": nil, "served_version": nil, "latest_version": nil, "pinned_version": nil}},
		{"a knowledge base lists under its own kind, not as a site",
			nil, &domain.Site{Slug: "kbasezz1", Kind: domain.KindKnowledgeBase, Manifest: man, StoredBytes: 900},
			map[string]any{"kind": "knowledgebase", "size_bytes": 900.0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var pastes []domain.Paste
			var sites []domain.Site
			if tc.paste != nil {
				pastes = append(pastes, *tc.paste)
			}
			if tc.site != nil {
				sites = append(sites, *tc.site)
			}
			rows := listJSON(t, pastes, sites)
			if len(rows) != 1 {
				t.Fatalf("rows = %v, want one", rows)
			}
			for k, want := range tc.want {
				got, ok := rows[0][k]
				if !ok || !reflect.DeepEqual(got, want) {
					t.Fatalf("%s = %v (present %v), want %v; row %v", k, got, ok, want, rows[0])
				}
			}
		})
	}
}

// An empty list is [] in json mode and one stderr line in table mode.
func TestListEmpty(t *testing.T) {
	if out, errOut := listOutput(t, formatJSON, nil, nil); strings.TrimSpace(out) != "[]" || errOut != "" {
		t.Fatalf("json = (%q, %q), want [] and no stderr", out, errOut)
	}
	if out, errOut := listOutput(t, formatTable, nil, nil); out != "" || errOut != "no active pastes\n" {
		t.Fatalf("table = (%q, %q)", out, errOut)
	}
}

// The table explains a SIZE above the served version only when some row is
// charged for more than one version.
func TestListTableMultiVersionNote(t *testing.T) {
	const note = "SIZE counts every live version"
	single := domain.Paste{Slug: "single12", Kind: "html", Size: 500, StoredBytes: 500, LatestVersion: 1}
	multi := domain.Paste{Slug: "multi123", Kind: "html", Size: 500, StoredBytes: 1500, LatestVersion: 3}
	if _, errOut := listOutput(t, formatTable, []domain.Paste{single}, nil); strings.Contains(errOut, note) {
		t.Fatalf("single-version list carries the note: %q", errOut)
	}
	if _, errOut := listOutput(t, formatTable, []domain.Paste{single, multi}, nil); !strings.Contains(errOut, note) {
		t.Fatalf("multi-version list lacks the note: %q", errOut)
	}
}

func TestNewVersionsView(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 6, 5, 15, 1, 0, 0, time.UTC)
	p := domain.Paste{PinnedVersion: 0}
	vers := []domain.Version{
		{VerNum: 2, CreatedAt: created, Size: 1400, Deleted: false},
		{VerNum: 1, CreatedAt: created, Size: 0, Deleted: true},
	}
	view := newVersionsView("abc12345", p, vers, 2, now)

	if view.Slug != "abc12345" || view.PinnedVersion != 0 {
		t.Fatalf("envelope: got slug=%q pinned=%d", view.Slug, view.PinnedVersion)
	}
	if len(view.Versions) != 2 {
		t.Fatalf("want 2 versions, got %d", len(view.Versions))
	}
	// v2: current, non-deleted, size present.
	if !view.Versions[0].Current || view.Versions[0].Deleted {
		t.Fatalf("v2 should be current + non-deleted: %+v", view.Versions[0])
	}
	if view.Versions[0].SizeBytes == nil || *view.Versions[0].SizeBytes != 1400 {
		t.Fatalf("v2 size: got %v want 1400", view.Versions[0].SizeBytes)
	}
	// v1: deleted → size null, not current.
	if !view.Versions[1].Deleted || view.Versions[1].Current {
		t.Fatalf("v1 should be deleted + not current: %+v", view.Versions[1])
	}
	if view.Versions[1].SizeBytes != nil {
		t.Fatalf("deleted version size_bytes should be null, got %v", *view.Versions[1].SizeBytes)
	}
}

// fingerprintOwner is the owner of a key fingerprint.
func fingerprintOwner(t *testing.T, fp string) domain.Owner {
	t.Helper()
	o, err := domain.OwnerFromKeyFingerprint(fp)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestNewWhoamiView(t *testing.T) {
	t.Run("full info", func(t *testing.T) {
		info := service.WhoamiInfo{
			Owner:      fingerprintOwner(t, "SHA256:abcd"),
			Active:     2,
			FirstSeen:  time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			UsedBytes:  1234,
			QuotaBytes: 10485760,
			Session: service.SessionInfo{
				Subnet:           "203.0.113.0/24",
				SubnetFreshCount: 1,
				SubnetCap:        5,
				IdentitySubnets:  2,
			},
		}
		v := newWhoamiView(info)
		if v.Key != "SHA256:abcd" {
			t.Fatalf("key prefix not stripped: %q", v.Key)
		}
		if v.QuotaBytes == nil || *v.QuotaBytes != 10485760 {
			t.Fatalf("quota_bytes: got %v", v.QuotaBytes)
		}
		if v.Session == nil || v.Session.Subnet != "203.0.113.0/24" {
			t.Fatalf("session: got %+v", v.Session)
		}
		if v.FirstSeen == nil {
			t.Fatalf("first_seen should be set")
		}
	})

	t.Run("no quota + no session null out", func(t *testing.T) {
		info := service.WhoamiInfo{Owner: fingerprintOwner(t, "SHA256:x"), Active: 0, QuotaBytes: 0}
		v := newWhoamiView(info)
		if v.QuotaBytes != nil {
			t.Fatalf("quota_bytes should be null when uncapped, got %v", *v.QuotaBytes)
		}
		if v.Session != nil {
			t.Fatalf("session should be null when no subnet, got %+v", v.Session)
		}
		if v.FirstSeen != nil {
			t.Fatalf("first_seen should be null when zero, got %v", *v.FirstSeen)
		}
	})
}
