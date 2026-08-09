package storage

import "testing"

// TestShaleShardKey pins shaleShardKey's family-prefix parsing: every key in a
// family extracts to the same shard key, so a transaction over one family for
// one subject is single-shard (docs/SPEC.md "Three shard families"). A parsing
// bug here silently scatters a subject's keys and breaks the per-shard CAS.
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

		// Per-identity derived family -> shard key <id>.
		{"identity_pastes", "identity_pastes/sha256:deadbeef/abc12345", "sha256:deadbeef"},
		{"identity_first_seen", "identity_first_seen/sha256:deadbeef", "sha256:deadbeef"},
		{"owner_doc", "owner_doc/sha256:deadbeef", "sha256:deadbeef"},

		// Per-subnet Sybil-gate family -> shard key <subnet>.
		{"keygate", "keygate/10.0.0.0_24/sha256:deadbeef", "10.0.0.0_24"},

		// Static-site per-slug authoritative family -> shard key <slug>.
		{"site", "sites/abc12345", "abc12345"},

		// Static-site per-identity derived family -> shard key <id>, so an
		// owner's site entries co-locate for the single-shard quota scan.
		{"identity_sites index", "identity_sites/sha256:deadbeef/abc12345", "sha256:deadbeef"},

		// Room families -> shard key <app-slug>, the FIRST segment after the
		// prefix. The trailing-slash discipline keeps roomkv/ from matching
		// rooms/.
		{"room record", "rooms/app12345/9f8e7d6c-1234-4abc-89de-0123456789ab", "app12345"},
		{"room value", "roomkv/app12345/9f8e7d6c-1234-4abc-89de-0123456789ab/card/1", "app12345"},
		{"room create", "roomcreate/app12345/10.0.0.0_24/2026-06-05T12:00:00.000000000Z/9f8e7d6c-1234-4abc-89de-0123456789ab", "app12345"},
		{"room bytes", "roombytes/app12345", "app12345"},

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

// TestShaleShardKeyFamilyColocation asserts the property directly: a slug's
// authoritative keys all extract to one shard key, and an identity's derived
// keys to another. A regression turns single-shard transactions cross-shard.
func TestShaleShardKeyFamilyColocation(t *testing.T) {
	slug := "z9y8x7w6"
	slugKeys := []string{
		"pastes/" + slug,
		"versions/" + slug + "/0001",
		"versions/" + slug + "/0009",
		"slug_owner/" + slug,
		// A site's keys co-shard with the same slug, so the site insert's
		// cross-family paste-slug collision read is single-shard with the
		// authoritative site write.
		"sites/" + slug,
		// A bref key carries its route shard in the {...} hash tag, so BindBlob
		// co-commits with the authoritative {slug} write in one single-shard
		// transaction. The <unit>/<blobid> tail does not affect routing.
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
		// The site enumeration index co-shards with the paste {id} family, so
		// an owner's paste + site entries land on one shard and each quota
		// prefix scan is single-shard.
		"identity_sites/" + id + "/" + slug,
		// The owner doc must co-shard with the row families it is maintained
		// alongside, or the doc update turns every {id}-shard CAS cross-shard.
		"owner_doc/" + id,
	}
	for _, k := range idKeys {
		if got := string(shaleShardKey([]byte(k))); got != id {
			t.Fatalf("identity key %q sharded to %q, want %q (co-location broken)", k, got, id)
		}
	}

	// Distinct prefixes: keygate/<subnet> never aliases a slug even when the
	// two strings are equal.
	if got := string(shaleShardKey([]byte("keygate/" + slug + "/" + id))); got != slug {
		t.Fatalf("keygate subnet extraction = %q, want %q", got, slug)
	}

	// All four room families + the per-app byte counter co-shard on the app
	// slug, so a room create / write / count / cap-check is single-shard. One
	// family drifting breaks the per-app cap's single-shard CAS invariant.
	app := "app77777"
	uuid := "9f8e7d6c-1234-4abc-89de-0123456789ab"
	roomKeys := []string{
		"rooms/" + app + "/" + uuid,
		"roomkv/" + app + "/" + uuid + "/some/nested/key",
		"roomcreate/" + app + "/10.0.0.0_24/2026-06-05T12:00:00.000000000Z/" + uuid,
		"roombytes/" + app,
	}
	for _, k := range roomKeys {
		if got := string(shaleShardKey([]byte(k))); got != app {
			t.Fatalf("room key %q sharded to %q, want %q (co-location broken)", k, got, app)
		}
	}
	// roomkv/ must NOT alias rooms/: the trailing-slash discipline keeps them
	// distinct families, matched by different cases even though both shard on
	// the app slug.
	if got := string(shaleShardKey([]byte("roomkv/" + app + "/" + uuid + "/k"))); got != app {
		t.Fatalf("roomkv key sharded to %q, want %q", got, app)
	}
}

// TestShaleShardKeyBrefCoRoutes pins the wiring requirement the shale-blob path
// imposes (blob-values.md 11.5): a bref key MUST route to the SAME shard as the
// metadata it references, or BindBlob cannot co-commit with the authoritative
// {slug} write. Dropping the bref/ case, or shadowing it with a later family
// prefix, scatters the pointer off that shard. Equality is asserted against
// BOTH pastes/<slug> and sites/<slug>, which co-shard.
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

// Co-location of the identity-leading keygate index.
//
// Invisible to a correctness test: a scattered index returns the same answer
// via a cross-shard fan-out, whose cost is set by unit count rather than rows
// matched. Untagged so CI runs it, since losing a shard-key case is a pure cost
// regression with no behavioural symptom.
func TestShardKey_KeygateIdentityIndexCoLocatesPerIdentity(t *testing.T) {
	const id = "key:frank"
	subnets := []string{
		"10.4.0.0/24", "10.4.1.0/24", "10.4.2.0/24", "10.4.3.0/24",
		"2001:db8::/48", "10.4.5.0/24", "10.4.6.0/24", "10.4.7.0/24",
	}

	want := string(shaleShardKey(shaleKeyKeygateIdentity(id, subnets[0])))
	for _, subnet := range subnets[1:] {
		got := string(shaleShardKey(shaleKeyKeygateIdentity(id, subnet)))
		if got != want {
			t.Fatalf("entries for one identity must share a shard key so they co-locate on one unit: "+
				"subnet %q gave %q, want %q. Without this, SubnetsForIdentity degrades to a cross-shard "+
				"fan-out whose cost scales with unit count rather than rows matched (26.7s to return a "+
				"single entry when this was missing).", subnet, got, want)
		}
	}
	// The shard key must be the IDENTITY, not something incidental that merely
	// happens to be constant across this input set.
	if want != id {
		t.Fatalf("keygate_id/ must shard on the identity, got %q want %q", want, id)
	}

	// It must also SPREAD identities: a shard key constant for every identity
	// passes the co-location check above while piling the whole family onto
	// one unit.
	seen := make(map[string]struct{})
	for _, other := range []string{"key:g", "key:h", "key:i", "key:j"} {
		seen[string(shaleShardKey(shaleKeyKeygateIdentity(other, "10.4.0.0/24")))] = struct{}{}
	}
	if len(seen) != 4 {
		t.Fatalf("4 distinct identities produced %d distinct shard keys; the family must spread across "+
			"units, not pin every identity to one", len(seen))
	}
}
