package storage

import "bytes"

// shaleShardKey is the ShardKeyFn the shale cluster is opened with. It
// extracts the routing shard key from a full metadata key by parsing its
// family prefix, so that every key belonging to the same subject
// (<slug> / <id> / <subnet>) co-locates on one shard WITHOUT renaming any
// key. See docs/SPEC.md "Three shard families".
//
// It is pure and import-free (no cluster / slate deps) so it lives in an
// untagged file and is unit-testable on the default build. The tagged
// shale_repo.go references it as cluster.Config.ShardKeyFn.
//
// Family -> shard key:
//
//	pastes/<slug>                 -> <slug>   (segment after the first '/')
//	versions/<slug>/<NNNN>        -> <slug>   (first segment after the prefix)
//	slug_owner/<slug>             -> <slug>
//	expiry/<rfc3339>/<slug>       -> <slug>   (LAST segment; slugs are slash-free
//	                                           and RFC3339 contains no '/', so
//	                                           last-segment extraction is unambiguous)
//	identity_pastes/<id>/<slug>   -> <id>     (first segment after the prefix)
//	identity_first_seen/<id>      -> <id>
//	identity_bytes/<id>           -> <id>
//	identity_reserve/<id>/<slug>  -> <id>     (reservation marker; co-shards with the counter)
//	keygate/<subnet>/<identity>   -> <subnet> (first segment after the prefix)
//
// The static-site families mirror the paste families (sites reuse the paste
// layout, only with a site_ flavor; see docs/SPEC.md "Shale reuses the
// layout"). Shale keeps NO identity_sites/<id>/<slug> index (it would be
// write-only on shale; the slatedb backend has one but shale's owner sum
// reads the counter instead), so there is no routing case for it:
//
//	sites/<slug>                       -> <slug>   (authoritative, joins the {slug} family)
//	expiry_sites/<ts>/<slug>           -> <slug>   (LAST segment, like expiry/)
//	identity_site_bytes/<id>           -> <id>     (the site quota counter)
//	identity_site_reserve/<id>/<slug>  -> <id>     (site reservation marker; co-shards with the site counter)
//
// The room families (the app-persistence tier) all shard on the <app-slug>,
// co-locating an app's rooms + values + ledger + expiry + counter on one
// shard (see docs/SPEC.md "Shale reuses the layout"):
//
//	rooms/<app-slug>/<uuid>                 -> <app-slug> (first segment after the prefix)
//	roomkv/<app-slug>/<uuid>/<key>          -> <app-slug> (first segment; roomkv/ != rooms/)
//	roomcreate/<app-slug>/<subnet>/<ts>     -> <app-slug> (first segment)
//	roombytes/<app-slug>                    -> <app-slug> (the per-app room-byte counter)
//	roomexpiry/<ts>/<app-slug>/<uuid>       -> <app-slug> (SECOND-to-last segment, between <ts> and <uuid>)
//
// A key that matches no known family falls back to the full key as its
// own shard key (the safe default: it routes deterministically and never
// collides families). This mirrors shale's default hash-tagged identity
// for unrecognized shapes.
func shaleShardKey(key []byte) []byte {
	switch {
	// Per-slug authoritative family. All shard on the slug, which is the
	// FIRST segment after the family prefix for every member except
	// expiry, whose date segment sits between the prefix and the slug.
	case bytes.HasPrefix(key, prefixPastes):
		return firstSegment(key[len(prefixPastes):])
	case bytes.HasPrefix(key, prefixVersionsAll):
		return firstSegment(key[len(prefixVersionsAll):])
	case bytes.HasPrefix(key, prefixSlugOwner):
		return firstSegment(key[len(prefixSlugOwner):])
	case bytes.HasPrefix(key, prefixExpiryAll):
		// expiry/<rfc3339>/<slug>: the slug is the LAST segment. Slugs
		// are slash-free and RFC3339 has no '/', so the last '/' splits
		// the date from the slug unambiguously.
		return lastSegment(key[len(prefixExpiryAll):])

	// Per-slug static-site authoritative family (mirrors pastes/ + expiry/).
	case bytes.HasPrefix(key, prefixSites):
		return firstSegment(key[len(prefixSites):])
	case bytes.HasPrefix(key, prefixExpirySitesAll):
		// expiry_sites/<rfc3339>/<slug>: slug is the LAST segment, same as
		// expiry/. The '_sites' suffix keeps it from matching expiry/.
		return lastSegment(key[len(prefixExpirySitesAll):])

	// Per-identity derived family. All shard on the id, the first segment
	// after the family prefix.
	case bytes.HasPrefix(key, prefixIdentityPastesAll):
		return firstSegment(key[len(prefixIdentityPastesAll):])
	case bytes.HasPrefix(key, prefixIdentityFirstSeenAll):
		return firstSegment(key[len(prefixIdentityFirstSeenAll):])
	case bytes.HasPrefix(key, prefixIdentityBytesAll):
		return firstSegment(key[len(prefixIdentityBytesAll):])
	case bytes.HasPrefix(key, prefixIdentityReserveAll):
		// identity_reserve/<id>/<slug>: shard on the id (first segment) so
		// the reservation marker co-shards with identity_bytes/<id> and the
		// reserve step's read-increment-mark is a single-shard CAS.
		return firstSegment(key[len(prefixIdentityReserveAll):])

	// Per-identity static-site derived family. The site byte counter and the
	// site reservation marker shard on the id so they co-locate (the same
	// single-shard-CAS argument as the paste {id} family). Shale keeps no
	// identity_sites/<id>/<slug> index, so there is no case for it.
	case bytes.HasPrefix(key, prefixIdentitySiteBytesAll):
		return firstSegment(key[len(prefixIdentitySiteBytesAll):])
	case bytes.HasPrefix(key, prefixIdentitySiteReserveAll):
		return firstSegment(key[len(prefixIdentitySiteReserveAll):])

	// Per-app room family (the app-persistence tier). All four room
	// families shard on the <app-slug> so an app's rooms, every room value,
	// its creation ledger, its expiry entries, and its byte counter co-locate
	// on ONE shard (so "write one key" / "load the whole room" / "count this
	// app's creations" are single-shard ops). The <app-slug> is the FIRST
	// segment after the prefix for rooms/ + roomkv/ + roomcreate/, but for
	// roomexpiry it is the SECOND-to-last segment (between the <ts> and the
	// trailing <uuid>); family-aware parsing pulls it out.
	//
	// roomkv/ MUST be matched BEFORE rooms/ would (it isn't, the trailing
	// slash anchors each prefix to a full segment so "roomkv/" never matches
	// "rooms/"), and roomcreate/ + roomexpiry/ are distinct prefixes.
	case bytes.HasPrefix(key, prefixRooms):
		return firstSegment(key[len(prefixRooms):])
	case bytes.HasPrefix(key, prefixRoomKV):
		return firstSegment(key[len(prefixRoomKV):])
	case bytes.HasPrefix(key, prefixRoomCreate):
		return firstSegment(key[len(prefixRoomCreate):])
	case bytes.HasPrefix(key, prefixRoomBytes):
		// roombytes/<app-slug>: the app slug is the whole remainder (no
		// further '/').
		return firstSegment(key[len(prefixRoomBytes):])
	case bytes.HasPrefix(key, prefixRoomExpiryAll):
		// roomexpiry/<ts>/<app-slug>/<uuid>: the app slug is the
		// second-to-last segment. The <ts> is fixed-width (no '/'), the slug
		// + uuid are slash-free, so strip the leading <ts> then take the
		// first of the remaining "<app-slug>/<uuid>".
		rest := key[len(prefixRoomExpiryAll):]
		if _, after, ok := bytes.Cut(rest, []byte{'/'}); ok {
			return firstSegment(after)
		}
		return rest

	// Per-subnet Sybil-gate family. Shards on the subnet, the first
	// segment after the prefix.
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
// segment so e.g. "identity_pastes/" never matches "identity_bytes/".
var (
	prefixPastes               = []byte("pastes/")
	prefixVersionsAll          = []byte("versions/")
	prefixSlugOwner            = []byte("slug_owner/")
	prefixExpiryAll            = []byte("expiry/")
	prefixIdentityPastesAll    = []byte("identity_pastes/")
	prefixIdentityFirstSeenAll = []byte("identity_first_seen/")
	prefixIdentityBytesAll     = []byte("identity_bytes/")
	prefixIdentityReserveAll   = []byte("identity_reserve/")
	prefixKeygateAll           = []byte("keygate/")

	// Static-site families (mirror the paste families). Shale keeps no
	// identity_sites/ index, so there is no prefix var for it.
	prefixSites                  = []byte("sites/")
	prefixExpirySitesAll         = []byte("expiry_sites/")
	prefixIdentitySiteBytesAll   = []byte("identity_site_bytes/")
	prefixIdentitySiteReserveAll = []byte("identity_site_reserve/")

	// Room families (the app-persistence tier). All shard on <app-slug>,
	// co-locating an app's rooms + values + creation ledger + expiry index +
	// byte counter on one shard. The trailing '/' anchors each prefix to a
	// full segment so "roomkv/" never matches "rooms/".
	prefixRooms         = []byte("rooms/")
	prefixRoomKV        = []byte("roomkv/")
	prefixRoomCreate    = []byte("roomcreate/")
	prefixRoomBytes     = []byte("roombytes/")
	prefixRoomExpiryAll = []byte("roomexpiry/")
)

// firstSegment returns the bytes up to (but not including) the first '/'
// in s, or all of s if there is no '/'. Used to pull the leading shard
// subject out of the remainder after a family prefix has been stripped.
func firstSegment(s []byte) []byte {
	if before, _, ok := bytes.Cut(s, []byte{'/'}); ok {
		return before
	}
	return s
}

// lastSegment returns the bytes after the last '/' in s, or all of s if
// there is no '/'. Used to pull the trailing slug out of the
// expiry/<date>/<slug> shape.
func lastSegment(s []byte) []byte {
	if i := bytes.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}
