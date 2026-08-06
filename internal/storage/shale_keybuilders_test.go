//go:build slatedb

package storage

import (
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// Every key family, built through the SAME builder production uses, routes to
// its subject. shale_shardkey_test.go asserts the router against hand-written
// literals, so a builder that spelled a family differently would still pass
// there while every key it wrote fell through shaleShardKey's whole-key
// fallback: the family scatters across shards and each single-shard CAS the
// design relies on silently becomes a cross-shard write.
func TestShaleKeyBuildersRouteToTheirSubject(t *testing.T) {
	const (
		slug     = domain.Slug("abc12345")
		app      = domain.Slug("app12345")
		identity = "key:SHA256:deadbeef"
		subnet   = "10.0.0.0/24"
	)
	roomID := domain.NewRoomID()
	at := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		key  []byte
		want string
	}{
		{"paste", shaleKeyPaste(slug), string(slug)},
		{"version", shaleKeyVersion(slug, 7), string(slug)},
		{"versions prefix", shalePrefixVersions(slug), string(slug)},
		{"slug_owner", shaleKeySlugOwner(slug), string(slug)},
		{"identity_pastes", shaleKeyIdentityPaste(identity, string(slug)), identity},
		{"identity_pastes prefix", shalePrefixIdentityPastes(identity), identity},
		{"identity_first_seen", shaleKeyIdentityFirstSeen(identity), identity},

		{"site", keySite(slug), string(slug)},
		{"identity_sites", keyIdentitySite(identity, string(slug)), identity},
		{"identity_sites prefix", prefixIdentitySites(identity), identity},

		{"room", shaleKeyRoom(app, roomID), string(app)},
		{"room value", shaleKeyRoomValue(app, roomID, "a/b"), string(app)},
		{"room values prefix", shalePrefixRoomValues(app, roomID), string(app)},
		{"room create", shaleKeyRoomCreate(app, subnet, roomID, at), string(app)},
		{"room creates prefix", shalePrefixAppRoomCreates(app), string(app)},
		{"room bytes", shaleKeyRoomBytes(app), string(app)},

		// The keygate subnet segment carries its own '/', so the router's
		// first-segment rule yields the network part. Every key for one subnet
		// still shares it, which is all co-location needs.
		{"keygate", shaleKeyKeygate(subnet, identity), "10.0.0.0"},
		{"keygate subnet prefix", shalePrefixKeygateSubnet(subnet), "10.0.0.0"},
		{"keygate_id prefix", shalePrefixKeygateIdentity(identity), identity},
	}

	for _, tc := range cases {
		got := string(shaleShardKey(tc.key))
		if got == string(tc.key) && tc.want != string(tc.key) {
			t.Errorf("%s: key %q fell through to the unknown-family fallback (whole key as shard key)", tc.name, tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: shaleShardKey(%q) = %q, want %q", tc.name, tc.key, got, tc.want)
		}
	}
}

// A room's every key family lands on ONE shard, which is what makes a room
// write a single-shard CAS.
func TestShaleRoomKeysCoLocate(t *testing.T) {
	app := domain.Slug("app12345")
	roomID := domain.NewRoomID()
	at := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	keys := [][]byte{
		shaleKeyRoom(app, roomID),
		shaleKeyRoomValue(app, roomID, "k"),
		shaleKeyRoomCreate(app, "10.0.0.0/24", roomID, at),
		shaleKeyRoomBytes(app),
	}
	for _, k := range keys {
		if got := string(shaleShardKey(k)); got != string(app) {
			t.Errorf("key %q routes to %q, want %q", k, got, app)
		}
	}
}
