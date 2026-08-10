package storage

import (
	"bytes"

	"github.com/Zamua/shale/pkg/ring"
)

// shaleShardKey is the ShardKeyFn the cluster is opened with. It extracts a
// routing key from a full metadata key by parsing its family prefix, so every
// key belonging to one subject co-locates on a shard WITHOUT renaming keys.
// See docs/SPEC.md "Three shard families".
//
// Untagged so it is unit-testable on the default build: its one shale
// dependency, pkg/ring, is cgo-free and slatedb-free.
//
// Family -> shard key:
//
//	bref/{<routeShard>}/<unit>/<blobid> -> <routeShard>  (the {...} hash tag)
//
//	pastes/<slug>                  -> <slug>
//	versions/<slug>/<NNNN>         -> <slug>
//	versions_doc/<slug>            -> <slug>
//	slug_owner/<slug>              -> <slug>
//	                                               slash-free and RFC3339 has no
//	                                               '/', so this is unambiguous)
//	identity_pastes/<id>/<slug>    -> <id>
//	identity_first_seen/<id>       -> <id>
//	owner_doc/<id>                 -> <id>
//	keygate/<subnet>/<identity>    -> <subnet>    (Sybil admission)
//	keygate_id/<identity>/<subnet> -> <identity>  (identity-leading view)
//
//	sites/<slug>                   -> <slug>
//	identity_sites/<id>/<slug>     -> <id>
//
//	rooms/<app-slug>/<uuid>            -> <app-slug>
//	roomkv/<app-slug>/<uuid>/<key>     -> <app-slug>
//	roomcreate/<app-slug>/<subnet>/<ts> -> <app-slug>
//	roombytes/<app-slug>               -> <app-slug>
//
// An unrecognised key falls back to the full key as its own shard key: it
// routes deterministically and cannot collide families.
func shaleShardKey(key []byte) []byte {
	switch {
	// Blob-pointer keys (shale-internal). The route shard key lives in the
	// Redis-style hash tag {<routeShard>} that brefKey writes, so the pointer
	// co-routes with the metadata under the SAME unit; ring.ShardKey is the
	// hash-tag extractor. This MUST be the leading case so a bref/ key is never
	// mis-classified by a later family prefix. See blob-values.md section 11.5.
	case bytes.HasPrefix(key, prefixBref):
		return ring.ShardKey(key)

	// Per-slug authoritative family. All shard on the slug, the FIRST segment
	// after the family prefix.
	case bytes.HasPrefix(key, prefixPastes):
		return firstSegment(key[len(prefixPastes):])
	case bytes.HasPrefix(key, prefixVersionsAll):
		return firstSegment(key[len(prefixVersionsAll):])
	// versions_doc/<slug>: the disposable version index cache. It co-shards with
	// versions/<slug>/* and the paste head so it joins the {slug} transactions
	// that write those rows. The trailing '/' on prefixVersionsAll ("versions/")
	// keeps "versions_doc/" from matching it, so this needs its own case or it
	// would fall to the whole-key fallback and land on the wrong shard.
	case bytes.HasPrefix(key, prefixVersionsDocAll):
		return firstSegment(key[len(prefixVersionsDocAll):])
	case bytes.HasPrefix(key, prefixSlugOwner):
		return firstSegment(key[len(prefixSlugOwner):])

	// Per-slug static-site authoritative family.
	case bytes.HasPrefix(key, prefixSites):
		return firstSegment(key[len(prefixSites):])

	// Per-identity derived family. All shard on the id, the first segment
	// after the family prefix.
	case bytes.HasPrefix(key, prefixIdentityPastesAll):
		return firstSegment(key[len(prefixIdentityPastesAll):])
	case bytes.HasPrefix(key, prefixIdentityFirstSeenAll):
		return firstSegment(key[len(prefixIdentityFirstSeenAll):])
	case bytes.HasPrefix(key, prefixIntentsAll):
		return firstSegment(key[len(prefixIntentsAll):])

	// owner_doc/<id>: the single-document owner index. Shards on the id so it
	// joins the same {id}-shard CAS transactions that maintain the legacy
	// identity_* row families.
	case bytes.HasPrefix(key, prefixOwnerDocAll):
		return firstSegment(key[len(prefixOwnerDocAll):])

	// Staged-blob records: staged/<slug>/<blobid>. Shards on the SLUG, so they
	// co-shard with the paste row, the bref they describe and the blobowner
	// fence that guards them.
	case bytes.HasPrefix(key, prefixStagedAll):
		return firstSegment(key[len(prefixStagedAll):])

	// blobowner/<slug>: the fence epoch a bind checks. Shards on the SLUG so it
	// co-shards with the bref it guards - the check runs inside the bind's
	// transaction, which is pinned on the slug and cannot read another shard.
	case bytes.HasPrefix(key, prefixBlobOwnerAll):
		return firstSegment(key[len(prefixBlobOwnerAll):])

	// Shards on the identity so one key's subnet entries co-locate and the
	// lookup is a single-shard prefix scan. Without a case here the family
	// hashes the whole key and scatters, forcing a cross-shard fan-out whose
	// cost is set by unit count rather than rows matched.
	case bytes.HasPrefix(key, prefixKeygateIdentityAll):
		return firstSegment(key[len(prefixKeygateIdentityAll):])

	// Per-identity static-site derived family: shards on the id so an owner's
	// site entries co-locate and the site quota's prefix scan is single-shard.
	case bytes.HasPrefix(key, prefixIdentitySitesAll):
		return firstSegment(key[len(prefixIdentitySitesAll):])

	// Per-app room family (the app-persistence tier). All room families shard
	// on the <app-slug> so an app's rooms, values, creation ledger and byte
	// counter co-locate on ONE shard, making "write one key" / "load the whole
	// room" / "count this app's creations" single-shard ops. The <app-slug> is
	// the FIRST segment after the prefix for every room family.
	case bytes.HasPrefix(key, prefixRooms):
		return firstSegment(key[len(prefixRooms):])
	case bytes.HasPrefix(key, prefixRoomKV):
		return firstSegment(key[len(prefixRoomKV):])
	case bytes.HasPrefix(key, prefixRoomCreate):
		return firstSegment(key[len(prefixRoomCreate):])
	case bytes.HasPrefix(key, prefixRoomBytes):
		return firstSegment(key[len(prefixRoomBytes):])
	// Per-subnet Sybil-gate family.
	case bytes.HasPrefix(key, prefixKeygateAll):
		return firstSegment(key[len(prefixKeygateAll):])
	}

	// Unknown family: route by the whole key. Never returns nil so the
	// caller never has to special-case an empty shard key.
	return key
}

// Family prefixes, declared as package vars so shaleShardKey compares
// against shared byte slices rather than re-allocating on every call.
// The trailing '/' is intentional: it anchors each prefix to a full path
// segment so e.g. "rooms/" never matches "roomkv/".
var (
	// prefixBref matches shale's internal blob-pointer keys
	// (bref/{<routeShard>}/<unit>/<blobid>).
	prefixBref = []byte("bref/")

	prefixPastes      = []byte("pastes/")
	prefixVersionsAll = []byte("versions/")
	// Disposable version index cache: versions_doc/<slug>, co-sharded with the
	// version rows it copies (docs/SPEC.md "The version index cache").
	prefixVersionsDocAll       = []byte("versions_doc/")
	prefixSlugOwner            = []byte("slug_owner/")
	prefixIdentityPastesAll    = []byte("identity_pastes/")
	prefixIdentityFirstSeenAll = []byte("identity_first_seen/")

	// Owner document: one JSON value per identity replacing the row-per-paste
	// index for reads (docs/SPEC.md "The owner document (v2 owner index)").
	prefixOwnerDocAll = []byte("owner_doc/")

	// Durable-intent family: intents/<scope>/<id>. Shards on the SCOPE (the
	// owner), which is what makes listing one owner's outstanding intents a
	// single-shard prefix scan on the shard its enumeration entries already
	// live on (docs/SPEC.md "Durable intent").
	prefixIntentsAll         = []byte("intents/")
	prefixStagedAll          = []byte("staged/")
	prefixBlobOwnerAll       = []byte("blobowner/")
	prefixKeygateIdentityAll = []byte("keygate_id/")
	prefixKeygateAll         = []byte("keygate/")

	// Static-site families. identity_sites/ is the per-owner enumeration index.
	prefixSites            = []byte("sites/")
	prefixIdentitySitesAll = []byte("identity_sites/")

	// Room families (the app-persistence tier). All shard on <app-slug>,
	// co-locating an app's rooms + values + creation ledger + byte counter on
	// one shard.
	prefixRooms      = []byte("rooms/")
	prefixRoomKV     = []byte("roomkv/")
	prefixRoomCreate = []byte("roomcreate/")
	prefixRoomBytes  = []byte("roombytes/")
)

// firstSegment returns the bytes up to (but not including) the first '/' in s,
// or all of s if there is no '/'.
func firstSegment(s []byte) []byte {
	if before, _, ok := bytes.Cut(s, []byte{'/'}); ok {
		return before
	}
	return s
}

// shaleKeyKeygateIdentity is the identity-leading view of a keygate fact, so
// "which subnets has this key been seen in" is a narrow prefix scan rather than
// a scan of every keygate row. A KV store cannot derive one key ordering from
// another, so both must be materialised; subnet-leading alone still returns the
// right answer, but only by reading the whole table.
func shaleKeyKeygateIdentity(identity, subnet string) []byte {
	return []byte("keygate_id/" + identity + "/" + subnet)
}
