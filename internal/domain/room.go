package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Room is the aggregate for the no-auth, capability-based persistence
// tier (see SPEC.md "Rooms (app persistence)"). A room is a key-value
// namespace created under a deployed static-site app and addressed by
// an unguessable UUIDv4. Possession of that UUID is the entire access
// model: holding it grants full read / write / delete to the room's
// data, and the storage layer namespaces every value by the triple
// (app-slug, room-uuid, key) so a room can never cross-read another
// room's or another app's data.
//
// Room sits alongside Paste and Site in the domain and imports nothing
// from infrastructure. The retention clock is the same shape pastes and
// sites use (UpdatedAt + a fixed window), just a longer window because a
// room backs a live app whose participants return over weeks, not the
// "share this link this week" lifetime of a paste.
type Room struct {
	AppSlug   Slug   // the owning app: the static site's slug
	ID        RoomID // the UUIDv4 capability
	CreatedAt time.Time
	UpdatedAt time.Time // last write (PUT or DELETE); a read does NOT move it
	ExpiresAt time.Time // UpdatedAt + RoomRetentionWindow
}

// RoomRetentionWindow is the fixed TTL for a room: it expires this long
// after its last write. The same 30-day window as pastes and sites -
// long enough that a room backing a live app whose participants return
// over weeks (a poll open for a month, a retro board a team revisits)
// survives normal use. Still finite, still no user-facing knob, still
// swept automatically; a product opinion, a starting point, not config.
const RoomRetentionWindow = 30 * 24 * time.Hour

// Per-room data caps. A single room is sized for app STATE (a poll's
// votes, a retro board's cards, a when2meet's availability grid) -
// generous for those, tight enough that a room cannot be turned into a
// free file host. A write that would push the room past EITHER cap is
// rejected and the prior state is left intact.
const (
	// MaxRoomBytes caps the total stored value bytes across all keys in
	// one room. Keys themselves are small metadata and are not charged.
	MaxRoomBytes = 256 << 10 // 256 KiB
	// MaxRoomKeys caps the number of distinct keys in one room, a second
	// guard on metadata footprint independent of the byte total.
	MaxRoomKeys = 256
	// MaxRoomKeyLen bounds a single key's length so a pathological key
	// cannot bloat the metadata store. App keys are short strings
	// ("participants", "card/<id>", "slot/2026-06-11"); 256 is generous.
	MaxRoomKeyLen = 256
)

// MaxRoomValueBytes is the largest single value a room key may hold. It
// equals the whole-room byte cap: one key may fill the room, but no
// value can exceed the room budget. Rooms store small JSON/bytes app
// STATE, not files - an app that needs to host file bytes uses the
// archive/site feature, not a room value.
const MaxRoomValueBytes = MaxRoomBytes

// Per-app aggregate cap. A popular app's rooms in aggregate are bounded
// so one app cannot consume the whole service - the room-tier analogue
// of the per-identity paste quota. Past this, new room creation and new
// writes for that app are refused until rooms expire and free space. A
// starting default, flagged as tunable.
const MaxAppRoomBytes = 64 << 20 // 64 MiB

// Room-creation rate-limit defaults (see SPEC.md "Quota and abuse").
// POST /api/rooms is gated per source IP AND per app so a script cannot
// spam rooms into existence. The read/write verbs on an existing room
// are NOT room-creation and ride the per-room data cap instead.
const (
	// MaxRoomsPerIPPerWindow caps fresh rooms from one source IP subnet.
	MaxRoomsPerIPPerWindow = 60
	// MaxRoomsPerAppPerWindow caps fresh rooms under one app, bounding a
	// single popular app's blast radius.
	MaxRoomsPerAppPerWindow = 300
	// RoomCreateWindow is the rolling window both creation limits use.
	RoomCreateWindow = time.Hour
)

// RoomID is a room's UUIDv4 capability. It is a value object rather than
// a bare string so every boundary that turns untrusted input into a room
// id re-validates it (ParseRoomID), and the domain can require a
// well-formed id at compile time. The canonical text form is the
// lowercase 8-4-4-4-12 hyphenated hex of RFC 4122.
type RoomID string

var (
	// ErrRoomIDEmpty is returned when an empty string is parsed as a RoomID.
	ErrRoomIDEmpty = errors.New("room id is empty")
	// ErrRoomIDMalformed is returned when a string is not a well-formed
	// UUIDv4 (wrong length, bad hyphen placement, non-hex characters, or
	// the version/variant nibbles are not v4 / RFC-4122). A malformed id
	// is a 400 at the HTTP layer, distinct from the 404 a well-formed but
	// nonexistent room gets - see SPEC.md.
	ErrRoomIDMalformed = errors.New("room id is not a valid UUIDv4")
)

// NewRoomID mints a fresh random UUIDv4 using crypto/rand. The version
// nibble (4) and variant bits (10xx) are set per RFC 4122. Like
// NewRandomSlug this never returns an error: crypto/rand.Read on
// linux/darwin only fails in the impossible "OS is dying" case, where a
// panic is the right outcome.
func NewRoomID() RoomID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("hostthis: crypto/rand failure: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return RoomID(formatUUID(b))
}

// ParseRoomID validates that s is a well-formed UUIDv4 and returns it
// typed and canonicalized to lowercase. Use this at every boundary where
// untrusted input becomes a RoomID - HTTP path segments, repo reads.
//
// Validation is strict: exactly 36 chars in 8-4-4-4-12 layout, all hex
// outside the hyphen positions, version nibble == 4, and variant nibble
// in {8,9,a,b}. This is deliberately tighter than "any UUID shape" so a
// forged or guessed id of the wrong version is rejected as malformed
// (400) rather than treated as a real-but-absent room (404).
func ParseRoomID(s string) (RoomID, error) {
	if s == "" {
		return "", ErrRoomIDEmpty
	}
	if len(s) != 36 {
		return "", ErrRoomIDMalformed
	}
	// Hyphens at the fixed positions; hex everywhere else.
	var raw [16]byte
	ri := 0
	for i := range 36 {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return "", ErrRoomIDMalformed
			}
			continue
		}
		hi, ok := hexNibble(c)
		if !ok {
			return "", ErrRoomIDMalformed
		}
		// Pack two nibbles per byte. The "high" nibble is the even index
		// within the de-hyphenated stream; track it via ri.
		if ri%2 == 0 {
			raw[ri/2] = hi << 4
		} else {
			raw[ri/2] |= hi
		}
		ri++
	}
	// version 4
	if raw[6]>>4 != 0x4 {
		return "", ErrRoomIDMalformed
	}
	// variant 10xx
	if raw[8]>>6 != 0x2 {
		return "", ErrRoomIDMalformed
	}
	// Canonicalize to lowercase so two textual spellings of the same
	// UUID address the same room key.
	return RoomID(formatUUID(raw)), nil
}

// String returns the room id as a plain string for key building + URLs.
func (id RoomID) String() string { return string(id) }

// formatUUID renders 16 bytes as canonical lowercase 8-4-4-4-12 hex.
func formatUUID(b [16]byte) string {
	const hyphen = "-"
	h := hex.EncodeToString(b[:])
	return h[0:8] + hyphen + h[8:12] + hyphen + h[12:16] + hyphen + h[16:20] + hyphen + h[20:32]
}

// hexNibble decodes one lowercase- or uppercase-hex character to its
// 4-bit value. The bool is false for non-hex input.
func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// RoomKV is the in-memory, I/O-free view of a room's key-value
// namespace: a flat map of key -> opaque value bytes. It is a pure value
// object - putting, getting, deleting, scanning, and computing the
// room's total byte size + key count for the cap check are all I/O-free
// domain operations. The storage layer materializes this from persisted
// rows and the service layer uses the pure cap math here before it
// writes.
//
// hostthis never parses a value: a value is app STATE, stored and
// returned verbatim. The only domain rule is the per-room cap (total
// bytes + key count + per-key length), enforced by CanPut.
type RoomKV struct {
	Values map[string][]byte

	// Seq is the room's per-room sequence at the instant this view was
	// materialized: the dense uint64 counter the storage backend assigns at
	// commit, +1 per committed mutation (PUT or DELETE, including the
	// idempotent delete of an absent key). A snapshot stamped with Seq == S
	// reflects EXACTLY the mutations with seq <= S - no more, no fewer -
	// which is what lets a relay late-joiner splice the live stream onto
	// the snapshot (discard frames with seq <= S, apply seq > S in order).
	// Zero for a fresh room and for the zero value.
	Seq uint64
}

// NewRoomKV returns an empty namespace ready to fill.
func NewRoomKV() RoomKV { return RoomKV{Values: make(map[string][]byte)} }

// Get returns the value for key and whether it exists. A miss is
// (nil, false) - the HTTP layer turns it into a 404.
func (kv RoomKV) Get(key string) ([]byte, bool) {
	v, ok := kv.Values[key]
	return v, ok
}

// Put records val under key (create or overwrite). Caller is responsible
// for the cap check (CanPut) first; Put only mutates the map.
func (kv RoomKV) Put(key string, val []byte) { kv.Values[key] = val }

// Delete removes key. Idempotent: deleting an absent key is a no-op, so
// the post-condition "the key is gone" holds either way.
func (kv RoomKV) Delete(key string) { delete(kv.Values, key) }

// TotalBytes is the sum of all value bytes in the namespace - the figure
// charged against the per-room byte cap. Keys are small metadata and are
// not counted.
func (kv RoomKV) TotalBytes() int {
	var n int
	for _, v := range kv.Values {
		n += len(v)
	}
	return n
}

// KeyCount is the number of distinct keys - charged against the per-room
// key cap.
func (kv RoomKV) KeyCount() int { return len(kv.Values) }

var (
	// ErrRoomKeyEmpty is returned when a write/read targets an empty key.
	ErrRoomKeyEmpty = errors.New("room key is empty")
	// ErrRoomKeyTooLong is returned when a key exceeds MaxRoomKeyLen.
	ErrRoomKeyTooLong = errors.New("room key is too long")
	// ErrRoomFull is returned by CanPut when accepting the write would
	// push the room past its byte or key-count cap. The prior state is
	// left intact (the caller does not Put). Surfaces as 413 at HTTP.
	ErrRoomFull = errors.New("room is at its data cap")
	// ErrRoomValueTooLarge is returned by CanPut when a single value
	// exceeds MaxRoomValueBytes - a value larger than the whole-room
	// budget can never fit and is rejected up front.
	ErrRoomValueTooLarge = errors.New("room value is too large")
)

// ValidateRoomKey checks a key against the empty + length rules. Use it
// at the boundary where an untrusted key (an HTTP path segment) becomes
// a stored key.
func ValidateRoomKey(key string) error {
	if key == "" {
		return ErrRoomKeyEmpty
	}
	if len(key) > MaxRoomKeyLen {
		return ErrRoomKeyTooLong
	}
	return nil
}

// CanPut reports whether writing val under key keeps the room within its
// caps. It is pure: it computes the post-write totals against the
// current namespace WITHOUT mutating it, so the caller can reject a write
// and leave the prior state untouched. Overwriting an existing key
// charges only the delta (new size minus the size being replaced); a
// new key charges its full size and one key slot.
//
// Returns:
//   - ErrRoomValueTooLarge if val alone exceeds MaxRoomValueBytes
//   - ErrRoomFull          if the post-write byte total > MaxRoomBytes
//     OR the post-write key count > MaxRoomKeys
//   - nil                  if the write fits
func (kv RoomKV) CanPut(key string, val []byte) error {
	if len(val) > MaxRoomValueBytes {
		return ErrRoomValueTooLarge
	}
	// Byte delta: replacing an existing key frees its old bytes.
	prior := 0
	if existing, ok := kv.Values[key]; ok {
		prior = len(existing)
	}
	if kv.TotalBytes()-prior+len(val) > MaxRoomBytes {
		return ErrRoomFull
	}
	// Key count only grows when key is new.
	postKeys := kv.KeyCount()
	if _, ok := kv.Values[key]; !ok {
		postKeys++
	}
	if postKeys > MaxRoomKeys {
		return ErrRoomFull
	}
	return nil
}
