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
// The static-site families mirror the paste families one-for-one (sites
// reuse the paste layout, only with a site_ flavor; see docs/SPEC.md
// "Shale reuses the layout"):
//
//	sites/<slug>                       -> <slug>   (authoritative, joins the {slug} family)
//	expiry_sites/<rfc3339>/<slug>      -> <slug>   (LAST segment, like expiry/)
//	identity_sites/<id>/<slug>         -> <id>     (derived index, joins the {id} family)
//	identity_site_bytes/<id>           -> <id>     (the site quota counter)
//	identity_site_reserve/<id>/<slug>  -> <id>     (site reservation marker; co-shards with the site counter)
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

	// Per-identity static-site derived family. All shard on the id so the
	// site index, the site byte counter, and the site reservation marker
	// co-locate (the same single-shard-CAS argument as the paste {id}
	// family). identity_site_bytes/ and identity_site_reserve/ do NOT have
	// identity_sites/ as a prefix (the char after 'site' is 's' vs '_'), so
	// the trailing-slash anchoring keeps the three apart.
	case bytes.HasPrefix(key, prefixIdentitySitesAll):
		return firstSegment(key[len(prefixIdentitySitesAll):])
	case bytes.HasPrefix(key, prefixIdentitySiteBytesAll):
		return firstSegment(key[len(prefixIdentitySiteBytesAll):])
	case bytes.HasPrefix(key, prefixIdentitySiteReserveAll):
		return firstSegment(key[len(prefixIdentitySiteReserveAll):])

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

	// Static-site families (mirror the paste families).
	prefixSites                  = []byte("sites/")
	prefixExpirySitesAll         = []byte("expiry_sites/")
	prefixIdentitySitesAll       = []byte("identity_sites/")
	prefixIdentitySiteBytesAll   = []byte("identity_site_bytes/")
	prefixIdentitySiteReserveAll = []byte("identity_site_reserve/")
)

// firstSegment returns the bytes up to (but not including) the first '/'
// in s, or all of s if there is no '/'. Used to pull the leading shard
// subject out of the remainder after a family prefix has been stripped.
func firstSegment(s []byte) []byte {
	if i := bytes.IndexByte(s, '/'); i >= 0 {
		return s[:i]
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
