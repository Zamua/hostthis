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

func TestNewPasteView_ExpiryAndNaming(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	t.Run("unset name is empty string not dash", func(t *testing.T) {
		v := newPasteView(domain.Paste{Slug: "abc12345", Name: ""}, now)
		if v.Name != "" {
			t.Fatalf("name: got %q want empty string", v.Name)
		}
	})

	t.Run("never-expires nulls both expiry fields", func(t *testing.T) {
		v := newPasteView(domain.Paste{Slug: "abc12345", ExpiresAt: domain.NeverExpires}, now)
		if v.ExpiresAt != nil || v.ExpiresInSeconds != nil {
			t.Fatalf("never-expires should null expiry: got at=%v in=%v", v.ExpiresAt, v.ExpiresInSeconds)
		}
	})

	t.Run("normal expiry renders RFC3339 + seconds", func(t *testing.T) {
		exp := now.Add(2 * time.Hour)
		v := newPasteView(domain.Paste{Slug: "abc12345", ExpiresAt: exp}, now)
		if v.ExpiresAt == nil || *v.ExpiresAt != exp.Format(time.RFC3339) {
			t.Fatalf("expires_at: got %v want %s", v.ExpiresAt, exp.Format(time.RFC3339))
		}
		if v.ExpiresInSeconds == nil || *v.ExpiresInSeconds != 7200 {
			t.Fatalf("expires_in_seconds: got %v want 7200", v.ExpiresInSeconds)
		}
	})

	t.Run("already-expired clamps seconds to zero", func(t *testing.T) {
		exp := now.Add(-5 * time.Minute)
		v := newPasteView(domain.Paste{Slug: "abc12345", ExpiresAt: exp}, now)
		if v.ExpiresInSeconds == nil || *v.ExpiresInSeconds != 0 {
			t.Fatalf("expires_in_seconds: got %v want 0", v.ExpiresInSeconds)
		}
	})
}

func TestNewPasteView_VersionState(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

	t.Run("unpinned serves latest", func(t *testing.T) {
		v := newPasteView(domain.Paste{Slug: "s", PinnedVersion: 0, LatestVersion: 5, ExpiresAt: domain.NeverExpires}, now)
		if v.ServedVersion != 5 || v.LatestVersion != 5 || v.PinnedVersion != 0 {
			t.Fatalf("unpinned: got served=%d latest=%d pinned=%d", v.ServedVersion, v.LatestVersion, v.PinnedVersion)
		}
	})

	t.Run("pinned serves the pin", func(t *testing.T) {
		v := newPasteView(domain.Paste{Slug: "s", PinnedVersion: 3, LatestVersion: 5, ExpiresAt: domain.NeverExpires}, now)
		if v.ServedVersion != 3 || v.LatestVersion != 5 || v.PinnedVersion != 3 {
			t.Fatalf("pinned: got served=%d latest=%d pinned=%d", v.ServedVersion, v.LatestVersion, v.PinnedVersion)
		}
	})
}

func TestNewPasteViews_EmptyMarshalsToArray(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	b, err := json.Marshal(newPasteViews(nil, now))
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
	p := domain.Paste{PinnedVersion: 0, ExpiresAt: now.Add(24 * time.Hour)}
	vers := []domain.Version{
		{VerNum: 2, CreatedAt: created, Size: 1400, Deleted: false},
		{VerNum: 1, CreatedAt: created, Size: 0, Deleted: true},
	}
	view := newVersionsView("abc12345", p, vers, 2, now)

	if view.Slug != "abc12345" || view.PinnedVersion != 0 {
		t.Fatalf("envelope: got slug=%q pinned=%d", view.Slug, view.PinnedVersion)
	}
	if view.ExpiresAt == nil {
		t.Fatalf("expires_at should be set for an expiring paste")
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
