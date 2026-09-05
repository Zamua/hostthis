package ssh

import (
	"reflect"
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

// TestListItem pins the list-row mapper for pastes and sites: naming, the
// served/latest/pinned triple, and SIZE as what the QUOTA CHARGED (every live
// version for a paste, StoredBytes for a site), so a list sums to the figure
// whoami shows. served_size_bytes carries the served version's own size so a
// JSON consumer never has to infer which number it holds.
func TestListItem(t *testing.T) {
	ip := func(n int) *int { return &n }
	man := domain.NewManifest()
	man.Add("index.html", domain.ManifestEntry{SHA: "a", Size: 4000})
	cases := []struct {
		name       string
		item       listItemView
		kind       string
		size       int
		servedSize *int
		multi      bool
		served     *int
		latest     *int
		pinned     *int
	}{
		{"unnamed paste keeps empty name, not the table dash",
			newPasteListItem(domain.Paste{Slug: "abc12345", Kind: "html"}),
			"html", 0, ip(0), false, ip(0), ip(0), ip(0)},
		{"unpinned serves latest",
			newPasteListItem(domain.Paste{Slug: "s", Kind: "html", LatestVersion: 5, Size: 7}),
			"html", 7, ip(7), false, ip(5), ip(5), ip(0)},
		{"pinned serves the pin",
			newPasteListItem(domain.Paste{Slug: "s", Kind: "html", PinnedVersion: 3, LatestVersion: 5, Size: 7}),
			"html", 7, ip(7), false, ip(3), ip(5), ip(3)},
		{"multi-version paste is charged every live version and flagged",
			newPasteListItem(domain.Paste{Slug: "p", Kind: "html", Size: 14266, StoredBytes: 40890, LatestVersion: 3}),
			"html", 40890, ip(14266), true, ip(3), ip(3), ip(0)},
		{"single-version paste is unflagged",
			newPasteListItem(domain.Paste{Slug: "p", Kind: "html", Size: 500, StoredBytes: 500, LatestVersion: 1}),
			"html", 500, ip(500), false, ip(1), ip(1), ip(0)},
		{"absent stored total falls back to the served size, never zero",
			newPasteListItem(domain.Paste{Slug: "p", Kind: "html", Size: 777, LatestVersion: 1}),
			"html", 777, ip(777), false, ip(1), ip(1), ip(0)},
		{"site is charged StoredBytes, not the manifest total, with null versions",
			newSiteListItem(domain.Site{Slug: "sitezzz1", Manifest: man, StoredBytes: 1500}),
			"site", 1500, nil, false, nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.item
			if got.Name != "" || got.Kind != tc.kind || got.SizeBytes != tc.size || got.multiVersion != tc.multi {
				t.Fatalf("name %q kind %q size %d multi %v, want \"\" %q %d %v",
					got.Name, got.Kind, got.SizeBytes, got.multiVersion, tc.kind, tc.size, tc.multi)
			}
			for _, f := range []struct {
				name      string
				got, want *int
			}{
				{"served_size_bytes", got.ServedSizeBytes, tc.servedSize},
				{"served_version", got.ServedVersion, tc.served},
				{"latest_version", got.LatestVersion, tc.latest},
				{"pinned_version", got.PinnedVersion, tc.pinned},
			} {
				if (f.got == nil) != (f.want == nil) || (f.got != nil && *f.got != *f.want) {
					t.Fatalf("%s: got %v want %v", f.name, deref(f.got), deref(f.want))
				}
			}
		})
	}
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
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

func TestNewWhoamiView(t *testing.T) {
	t.Run("full info", func(t *testing.T) {
		info := service.WhoamiInfo{
			Identity:   domain.IdentityKeyPrefix + "SHA256:abcd",
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
		info := service.WhoamiInfo{Identity: domain.IdentityKeyPrefix + "SHA256:x", Active: 0, QuotaBytes: 0}
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
