package ssh

import (
	"encoding/json"
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

func TestNewPasteView_Naming(t *testing.T) {
	t.Run("unset name is empty string not dash", func(t *testing.T) {
		v := newPasteListItem(domain.Paste{Slug: "abc12345", Name: ""})
		if v.Name != "" {
			t.Fatalf("name: got %q want empty string", v.Name)
		}
	})
}

func TestNewPasteView_VersionState(t *testing.T) {
	t.Run("unpinned serves latest", func(t *testing.T) {
		v := newPasteListItem(domain.Paste{Slug: "s", PinnedVersion: 0, LatestVersion: 5})
		if *v.ServedVersion != 5 || *v.LatestVersion != 5 || *v.PinnedVersion != 0 {
			t.Fatalf("unpinned: got served=%d latest=%d pinned=%d", *v.ServedVersion, *v.LatestVersion, *v.PinnedVersion)
		}
	})

	t.Run("pinned serves the pin", func(t *testing.T) {
		v := newPasteListItem(domain.Paste{Slug: "s", PinnedVersion: 3, LatestVersion: 5})
		if *v.ServedVersion != 3 || *v.LatestVersion != 5 || *v.PinnedVersion != 3 {
			t.Fatalf("pinned: got served=%d latest=%d pinned=%d", *v.ServedVersion, *v.LatestVersion, *v.PinnedVersion)
		}
	})

	t.Run("site has null version fields + site kind", func(t *testing.T) {
		v := newSiteListItem(domain.Site{Slug: "portfolio2"})
		if v.Kind != "site" {
			t.Fatalf("site kind: got %q want site", v.Kind)
		}
		if v.ServedVersion != nil || v.LatestVersion != nil || v.PinnedVersion != nil {
			t.Fatalf("site version fields should be nil, got served=%v latest=%v pinned=%v", v.ServedVersion, v.LatestVersion, v.PinnedVersion)
		}
	})
}

func TestNewPasteViews_EmptyMarshalsToArray(t *testing.T) {
	b, err := json.Marshal(newListView(nil, nil))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Fatalf("empty list should marshal to [], got %s", b)
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

// "current" marks the version the URL serves. A truncated version list (one the
// index could not be read in full) must not move the marker onto whatever entry
// happens to be newest in it.
func TestServedVersionOf(t *testing.T) {
	v := func(n int, sha string, deleted bool) domain.Version {
		return domain.Version{VerNum: n, ContentSHA: sha, Deleted: deleted}
	}
	// Newest-first, the order ListVersions renders.
	whole := []domain.Version{v(3, "sha-v3", false), v(2, "sha-v2", false), v(1, "sha-v1", false)}
	truncated := []domain.Version{v(2, "sha-v2", false), v(1, "sha-v1", false)} // v3 missing

	cases := []struct {
		name string
		head domain.Paste
		vers []domain.Version
		want int
	}{
		{"pinned wins outright", domain.Paste{PinnedVersion: 1, ContentSHA: "sha-v1"}, whole, 1},
		{"unpinned marks the served sha", domain.Paste{ContentSHA: "sha-v3"}, whole, 3},
		{"tombstoned entries are skipped", domain.Paste{ContentSHA: "sha-v2"},
			[]domain.Version{v(3, "sha-v3", true), v(2, "sha-v2", false)}, 2},
		{"a list missing the served version marks nothing", domain.Paste{ContentSHA: "sha-v3"}, truncated, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := servedVersionOf(tc.head, tc.vers); got != tc.want {
				t.Fatalf("servedVersionOf: got v%d, want v%d", got, tc.want)
			}
		})
	}
}
