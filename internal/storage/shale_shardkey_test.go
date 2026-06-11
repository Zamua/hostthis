package storage

import "testing"

// TestShaleShardKey pins shaleShardKey's family-prefix parsing: every key
// in a family must extract to the same shard key (the family's subject),
// so a transaction touching one family for one subject is single-shard.
// This is the routing contract docs/SPEC.md "Three shard families"
// depends on; a parsing bug here would silently scatter a subject's keys
// across shards and break the per-shard CAS invariants.
func TestShaleShardKey(t *testing.T) {
	cases := []struct {
		name string
		key  string
		want string
	}{
		// Per-slug authoritative family -> shard key <slug>.
		{"paste", "pastes/abc12345", "abc12345"},
		{"version", "versions/abc12345/0001", "abc12345"},
		{"version high num", "versions/abc12345/0042", "abc12345"},
		{"slug_owner", "slug_owner/abc12345", "abc12345"},
		// expiry's slug is the LAST segment, behind an RFC3339 date that
		// itself contains ':' and '-' but no '/'.
		{"expiry", "expiry/2026-06-05T12:00:00Z/abc12345", "abc12345"},
		{"expiry nano", "expiry/2026-06-05T12:00:00.123456789Z/abc12345", "abc12345"},

		// Per-identity derived family -> shard key <id>.
		{"identity_pastes", "identity_pastes/sha256:deadbeef/abc12345", "sha256:deadbeef"},
		{"identity_first_seen", "identity_first_seen/sha256:deadbeef", "sha256:deadbeef"},
		{"identity_bytes", "identity_bytes/sha256:deadbeef", "sha256:deadbeef"},
		{"identity_reserve", "identity_reserve/sha256:deadbeef/abc12345", "sha256:deadbeef"},

		// Per-subnet Sybil-gate family -> shard key <subnet>.
		{"keygate", "keygate/10.0.0.0_24/sha256:deadbeef", "10.0.0.0_24"},

		// Static-site per-slug authoritative family -> shard key <slug>.
		{"site", "sites/abc12345", "abc12345"},
		// expiry_sites' slug is the LAST segment (like expiry/), and the
		// '_sites' suffix must NOT make it route as a plain expiry/ key.
		{"expiry_site", "expiry_sites/2026-06-05T12:00:00Z/abc12345", "abc12345"},
		{"expiry_site nano", "expiry_sites/2026-06-05T12:00:00.123456789Z/abc12345", "abc12345"},

		// Static-site per-identity derived family -> shard key <id>. The
		// three families share the identity_site prefix but disambiguate on
		// the char after 'site' ('s' for identity_sites, '_' for the other
		// two): identity_site_bytes and identity_site_reserve do NOT route as
		// identity_sites.
		{"identity_sites", "identity_sites/sha256:deadbeef/abc12345", "sha256:deadbeef"},
		{"identity_site_bytes", "identity_site_bytes/sha256:deadbeef", "sha256:deadbeef"},
		{"identity_site_reserve", "identity_site_reserve/sha256:deadbeef/abc12345", "sha256:deadbeef"},

		// Unknown family routes by the whole key.
		{"unknown", "weird/key/shape", "weird/key/shape"},
		{"no slash", "bareword", "bareword"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(shaleShardKey([]byte(tc.key)))
			if got != tc.want {
				t.Fatalf("shaleShardKey(%q) = %q, want %q", tc.key, got, tc.want)
			}
		})
	}
}

// TestShaleShardKeyFamilyColocation asserts the load-bearing property
// directly: all of a single slug's authoritative keys, and all of a
// single identity's derived keys, extract to one shard key each. If this
// regresses, single-shard transactions silently become cross-shard.
func TestShaleShardKeyFamilyColocation(t *testing.T) {
	slug := "z9y8x7w6"
	slugKeys := []string{
		"pastes/" + slug,
		"versions/" + slug + "/0001",
		"versions/" + slug + "/0009",
		"slug_owner/" + slug,
		"expiry/2026-06-05T12:00:00Z/" + slug,
		// A site's authoritative + expiry keys co-shard with the same slug,
		// so the cross-family paste-slug collision read in the site insert is
		// single-shard with the authoritative site write.
		"sites/" + slug,
		"expiry_sites/2026-06-05T12:00:00Z/" + slug,
	}
	for _, k := range slugKeys {
		if got := string(shaleShardKey([]byte(k))); got != slug {
			t.Fatalf("slug key %q sharded to %q, want %q (co-location broken)", k, got, slug)
		}
	}

	id := "sha256:cafebabe"
	idKeys := []string{
		"identity_pastes/" + id + "/" + slug,
		"identity_first_seen/" + id,
		"identity_bytes/" + id,
		// The reservation marker MUST co-shard with identity_bytes so the
		// reserve step's read-increment-mark is a single-shard CAS.
		"identity_reserve/" + id + "/" + slug,
		// The site index, the site byte counter, and the site reservation
		// marker all co-shard with the identity so the site reserve step's
		// read-increment-mark is single-shard (mirrors the paste {id} family).
		"identity_sites/" + id + "/" + slug,
		"identity_site_bytes/" + id,
		"identity_site_reserve/" + id + "/" + slug,
	}
	for _, k := range idKeys {
		if got := string(shaleShardKey([]byte(k))); got != id {
			t.Fatalf("identity key %q sharded to %q, want %q (co-location broken)", k, got, id)
		}
	}

	// The two families must NOT collide: a slug and an id that happen to
	// be equal strings still land on the correct family key, but the
	// distinct prefixes mean keygate/<subnet> never aliases a slug.
	if got := string(shaleShardKey([]byte("keygate/" + slug + "/" + id))); got != slug {
		t.Fatalf("keygate subnet extraction = %q, want %q", got, slug)
	}
}
