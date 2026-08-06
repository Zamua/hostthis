package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Room is the aggregate for the no-auth, capability-based persistence tier
// (SPEC.md "Rooms (app persistence)"): a key-value namespace under a deployed
// static-site app, addressed by an unguessable UUIDv4. Possession of that UUID
// is the entire access model, and the storage layer namespaces every value by
// the triple (app-slug, room-uuid, key) so a room can never cross-read another
// room's or another app's data.
type Room struct {
	AppSlug   Slug   // the owning app: the static site's slug
	ID        RoomID // the UUIDv4 capability
	CreatedAt time.Time
	UpdatedAt time.Time // last write (PUT or DELETE); a read does NOT move it
}

// Per-room data caps, sized for app STATE rather than files. A write that
// would push the room past EITHER cap is rejected with the prior state intact.
const (
	// MaxRoomBytes caps the total stored value bytes across all keys in one
	// room. Keys are small metadata and are not charged.
	MaxRoomBytes = 256 << 10 // 256 KiB
	// MaxRoomKeys bounds the metadata footprint independently of the byte
	// total.
	MaxRoomKeys = 256
	// MaxRoomKeyLen keeps a pathological key from bloating the metadata
	// store. App keys are short ("participants", "card/<id>").
	MaxRoomKeyLen = 256
)

// MaxRoomValueBytes equals the whole-room byte cap: one key may fill the room,
// but no value can exceed the room budget. An app needing to host file bytes
// uses the archive/site feature, not a room value.
const MaxRoomValueBytes = MaxRoomBytes

// MaxAppRoomBytes bounds one app's rooms in aggregate so a single app cannot
// consume the whole service. Past it, new room creation and new writes for
// that app are refused until the app deletes rooms or values. Tunable default.
const MaxAppRoomBytes = 64 << 20 // 64 MiB

// Room-creation rate-limit defaults (SPEC.md "Quota and abuse"). Creation is
// gated per source IP AND per app so a script cannot spam rooms into
// existence; the verbs on an existing room ride the per-room data cap instead.
const (
	// MaxRoomsPerIPPerWindow caps fresh rooms from one source IP subnet.
	MaxRoomsPerIPPerWindow = 60
	// MaxRoomsPerAppPerWindow bounds a single popular app's blast radius.
	MaxRoomsPerAppPerWindow = 300
	// RoomCreateWindow is the rolling window both creation limits use.
	RoomCreateWindow = time.Hour
)

// RoomID is a room's UUIDv4 capability. A value object rather than a bare
// string so every boundary turning untrusted input into a room id re-validates
// it via ParseRoomID. The canonical text form is the lowercase 8-4-4-4-12
// hyphenated hex of RFC 4122.
type RoomID string

var (
	// ErrRoomIDEmpty is returned when an empty string is parsed as a RoomID.
	ErrRoomIDEmpty = errors.New("room id is empty")
	// ErrRoomIDMalformed is returned when a string is not a well-formed
	// UUIDv4. A malformed id is a 400 at the HTTP layer, distinct from the
	// 404 a well-formed but nonexistent room gets. See SPEC.md.
	ErrRoomIDMalformed = errors.New("room id is not a valid UUIDv4")
)

// NewRoomID mints a fresh random UUIDv4 using crypto/rand. It never returns an
// error: crypto/rand.Read fails only when the OS entropy source is broken,
// where a panic is the right outcome.
func NewRoomID() RoomID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("hostthis: crypto/rand failure: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10xx (RFC 4122)
	return RoomID(formatUUID(b))
}

// ParseRoomID validates that s is a well-formed UUIDv4 and returns it typed
// and lowercased. Use it at every boundary where untrusted input becomes a
// RoomID: HTTP path segments, repo reads.
//
// Validation is strict: exactly 36 chars in 8-4-4-4-12 layout, hex outside the
// hyphen positions, version nibble == 4, variant nibble in {8,9,a,b}. Tighter
// than "any UUID shape" so a forged id of the wrong version is rejected as
// malformed (400) rather than treated as a real-but-absent room (404).
func ParseRoomID(s string) (RoomID, error) {
	if s == "" {
		return "", ErrRoomIDEmpty
	}
	if len(s) != 36 {
		return "", ErrRoomIDMalformed
	}
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
		// Two nibbles per byte, indexed by position in the de-hyphenated
		// stream rather than in s.
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
	// Lowercased so two textual spellings of the same UUID address the same
	// room key.
	return RoomID(formatUUID(raw)), nil
}

// String returns the room id as a plain string.
func (id RoomID) String() string { return string(id) }

// formatUUID renders 16 bytes as canonical lowercase 8-4-4-4-12 hex.
func formatUUID(b [16]byte) string {
	const hyphen = "-"
	h := hex.EncodeToString(b[:])
	return h[0:8] + hyphen + h[8:12] + hyphen + h[12:16] + hyphen + h[16:20] + hyphen + h[20:32]
}

// hexNibble decodes one hex character, either case, to its 4-bit value. The
// bool is false for non-hex input.
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

// RoomKV is the I/O-free view of a room's key-value namespace: a flat map of
// key to opaque value bytes. A value is never parsed, only stored and returned
// verbatim; the only domain rule is the per-room cap enforced by CanPut.
type RoomKV struct {
	Values map[string][]byte

	// Seq is the room's sequence at the instant this view was materialized:
	// a dense uint64 the backend assigns at commit, +1 per committed mutation
	// (PUT or DELETE, including the idempotent delete of an absent key). A
	// snapshot stamped Seq == S reflects EXACTLY the mutations with seq <= S,
	// which is what lets a relay late-joiner splice the live stream onto the
	// snapshot: discard frames with seq <= S, apply seq > S in order. Zero for
	// a fresh room and for the zero value.
	Seq uint64
}

// NewRoomKV returns an empty namespace ready to fill.
func NewRoomKV() RoomKV { return RoomKV{Values: make(map[string][]byte)} }

// Get returns the value for key and whether it exists. A miss becomes a 404 at
// the HTTP layer.
func (kv RoomKV) Get(key string) ([]byte, bool) {
	v, ok := kv.Values[key]
	return v, ok
}

// Put records val under key. The caller must run the CanPut cap check first;
// Put only mutates the map.
func (kv RoomKV) Put(key string, val []byte) { kv.Values[key] = val }

// Delete removes key. Idempotent: deleting an absent key is a no-op.
func (kv RoomKV) Delete(key string) { delete(kv.Values, key) }

// TotalBytes is the value-byte sum charged against the per-room byte cap. Keys
// are not counted.
func (kv RoomKV) TotalBytes() int {
	var n int
	for _, v := range kv.Values {
		n += len(v)
	}
	return n
}

// KeyCount is the distinct-key count charged against the per-room key cap.
func (kv RoomKV) KeyCount() int { return len(kv.Values) }

var (
	// ErrRoomKeyEmpty is returned when a write/read targets an empty key.
	ErrRoomKeyEmpty = errors.New("room key is empty")
	// ErrRoomKeyTooLong is returned when a key exceeds MaxRoomKeyLen.
	ErrRoomKeyTooLong = errors.New("room key is too long")
	// ErrRoomFull is returned by CanPut when the write would push the room
	// past its byte or key-count cap. Surfaces as 413 at HTTP.
	ErrRoomFull = errors.New("room is at its data cap")
	// ErrRoomValueTooLarge is returned by CanPut when a single value exceeds
	// MaxRoomValueBytes, which can never fit whatever the room holds.
	ErrRoomValueTooLarge = errors.New("room value is too large")
)

// ValidateRoomKey checks a key against the empty + length rules. Use it at the
// boundary where an untrusted key (an HTTP path segment) becomes a stored key.
func ValidateRoomKey(key string) error {
	if key == "" {
		return ErrRoomKeyEmpty
	}
	if len(key) > MaxRoomKeyLen {
		return ErrRoomKeyTooLong
	}
	return nil
}

// CanPut reports whether writing val under key keeps the room within its caps,
// computing the post-write totals WITHOUT mutating the namespace so a rejected
// write leaves the prior state untouched. Overwriting an existing key charges
// only the size delta; a new key charges its full size and one key slot.
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
	// Replacing an existing key frees its old bytes.
	prior := 0
	if existing, ok := kv.Values[key]; ok {
		prior = len(existing)
	}
	if kv.TotalBytes()-prior+len(val) > MaxRoomBytes {
		return ErrRoomFull
	}
	postKeys := kv.KeyCount()
	if _, ok := kv.Values[key]; !ok {
		postKeys++
	}
	if postKeys > MaxRoomKeys {
		return ErrRoomFull
	}
	return nil
}
