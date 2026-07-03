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

		// Static-site per-identity derived family -> shard key <id>. All three
		// site {id} families (byte counter, reservation marker, enumeration
		// index) route on the id so they co-shard; they disambiguate on their
		// distinct identity_site_bytes/ vs identity_site_reserve/ vs
		// identity_sites/ prefixes (which diverge before the trailing slash).
		{"identity_site_bytes", "identity_site_bytes/sha256:deadbeef", "sha256:deadbeef"},
		{"identity_site_reserve", "identity_site_reserve/sha256:deadbeef/abc12345", "sha256:deadbeef"},
		// The site RELEASE marker (delete-side mirror of the reserve marker) also
		// routes on the id so it co-shards with the site counter; its
		// identity_site_release/ prefix diverges from identity_site_reserve/ at
		// 'l' vs 's' and from identity_sites/ at '_' vs 's', so it is unambiguous.
		{"identity_site_release", "identity_site_release/sha256:deadbeef/abc12345", "sha256:deadbeef"},
		{"identity_sites index", "identity_sites/sha256:deadbeef/abc12345", "sha256:deadbeef"},

		// Room families -> shard key <app-slug>. All four families (+ the
		// per-app byte counter) shard on the app slug, the FIRST segment after
		// the prefix for rooms/ + roomkv/ + roomcreate/, and the SECOND-to-last
		// segment for roomexpiry/ (between the <ts> and the trailing <uuid>).
		// The trailing-slash discipline keeps roomkv/ from matching rooms/.
		{"room record", "rooms/app12345/9f8e7d6c-1234-4abc-89de-0123456789ab", "app12345"},
		{"room value", "roomkv/app12345/9f8e7d6c-1234-4abc-89de-0123456789ab/card/1", "app12345"},
		{"room create", "roomcreate/app12345/10.0.0.0_24/2026-06-05T12:00:00.000000000Z/9f8e7d6c-1234-4abc-89de-0123456789ab", "app12345"},
		{"room bytes", "roombytes/app12345", "app12345"},
		// roomexpiry: app slug is the second-to-last segment, behind a
		// fixed-width <ts> (contains ':' + '-' + '.', no '/').
		{"room expiry", "roomexpiry/2026-06-05T12:00:00.000000000Z/app12345/9f8e7d6c-1234-4abc-89de-0123456789ab", "app12345"},

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
		// A blob pointer's bref key carries the route shard in its {...} hash
		// tag, so it co-shards with the metadata it references: the BindBlob
		// co-commits with the authoritative {slug} write in one single-shard
		// transaction. (The <unit>/<blobid> tail does not affect routing.)
		"bref/{" + slug + "}/0-3/deadbeefcafe",
		"bref/{" + slug + "}/legacy/0123456789ab",
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
		// The site byte counter, reservation marker, AND enumeration index all
		// co-shard with the identity so the confirm/delete step's read-modify
		// on all three is a single-shard CAS (mirrors the paste {id} family).
		"identity_site_bytes/" + id,
		"identity_site_reserve/" + id + "/" + slug,
		// The site release marker MUST co-shard with the site counter so
		// DeleteSite's decrement + marker-consume is a single-shard {id} CAS.
		"identity_site_release/" + id + "/" + slug,
		"identity_sites/" + id + "/" + slug,
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

	// All FOUR room families + the per-app byte counter co-shard on the app
	// slug, so a room create / write / count / cap-check is single-shard. If
	// any regresses to a different shard key, those become cross-shard and the
	// per-app cap's single-shard CAS invariant breaks.
	app := "app77777"
	uuid := "9f8e7d6c-1234-4abc-89de-0123456789ab"
	roomKeys := []string{
		"rooms/" + app + "/" + uuid,
		"roomkv/" + app + "/" + uuid + "/some/nested/key",
		"roomcreate/" + app + "/10.0.0.0_24/2026-06-05T12:00:00.000000000Z/" + uuid,
		"roombytes/" + app,
		"roomexpiry/2026-06-05T12:00:00.000000000Z/" + app + "/" + uuid,
	}
	for _, k := range roomKeys {
		if got := string(shaleShardKey([]byte(k))); got != app {
			t.Fatalf("room key %q sharded to %q, want %q (co-location broken)", k, got, app)
		}
	}
	// roomkv/ must NOT alias rooms/: the trailing-slash discipline keeps them
	// distinct families (both still shard on the app slug, but via different
	// cases - a regression that collapsed them would be a latent bug).
	if got := string(shaleShardKey([]byte("roomkv/" + app + "/" + uuid + "/k"))); got != app {
		t.Fatalf("roomkv key sharded to %q, want %q", got, app)
	}
}

// TestShaleShardKeyBrefCoRoutes pins the one wiring requirement the shale-blob
// path imposes (blob-values.md 11.5): a blob pointer's bref key MUST route to
// the SAME shard as the metadata it references, so BindBlob co-commits with the
// authoritative {slug} write. The bref key carries the route shard in its {...}
// hash tag; shaleShardKey's bref/ case defers to ring.ShardKey, which extracts
// it. A regression (the bref/ case dropped, or a later family prefix shadowing
// it) would scatter the pointer off the metadata's shard and break the co-
// commit, so this asserts strict equality with BOTH pastes/<slug> and
// sites/<slug> (pastes and sites co-shard, so a blob staged for either routes
// the same).
func TestShaleShardKeyBrefCoRoutes(t *testing.T) {
	for _, slug := range []string{"abc12345", "z9y8x7w6", "00000000"} {
		pasteShard := string(shaleShardKey([]byte("pastes/" + slug)))
		siteShard := string(shaleShardKey([]byte("sites/" + slug)))
		brefShard := string(shaleShardKey([]byte("bref/{" + slug + "}/0-7/anyblobid")))
		if brefShard != slug {
			t.Fatalf("bref shard for %q = %q, want %q", slug, brefShard, slug)
		}
		if brefShard != pasteShard {
			t.Fatalf("bref shard %q != paste shard %q (pointer would not co-route with the paste)", brefShard, pasteShard)
		}
		if brefShard != siteShard {
			t.Fatalf("bref shard %q != site shard %q (pointer would not co-route with the site)", brefShard, siteShard)
		}
	}
}
