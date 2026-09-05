package domain

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
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

// NewRoomID mints a fresh random UUIDv4 from crypto/rand. It never returns an
// error: the entropy source failing is a broken host, where a panic is the
// right outcome.
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
// Validation is strict: exactly the 36-char 8-4-4-4-12 hex layout, version 4,
// RFC 4122 variant. Tighter than "any UUID shape" so a forged id of the wrong
// version or form is malformed (400) rather than a real-but-absent room (404).
func ParseRoomID(s string) (RoomID, error) {
	if s == "" {
		return "", ErrRoomIDEmpty
	}
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return "", ErrRoomIDMalformed
	}
	var raw [16]byte
	if _, err := hex.Decode(raw[:], []byte(s[0:8]+s[9:13]+s[14:18]+s[19:23]+s[24:36])); err != nil {
		return "", ErrRoomIDMalformed
	}
	if raw[6]>>4 != 0x4 || raw[8]>>6 != 0x2 {
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
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
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

// KeyCount is the distinct-key count charged against the per-room key cap.
func (kv RoomKV) KeyCount() int { return len(kv.Values) }

var (
	// ErrRoomKeyEmpty is returned when a write/read targets an empty key.
	ErrRoomKeyEmpty = errors.New("room key is empty")
	// ErrRoomKeyTooLong is returned when a key exceeds MaxRoomKeyLen.
	ErrRoomKeyTooLong = errors.New("room key is too long")
)

// Reserved room path segments: the room API serves these as something other
// than data (the relay upgrade, the push surface), so no value may live there.
const (
	RoomKeyWS   = "ws"
	RoomKeyPush = "push"
)

// IsReservedRoomKey reports whether key is "ws", "push", or under "push/".
// Keys of other shapes ("push:2026-09-06", "pushy") are ordinary data.
func IsReservedRoomKey(key string) bool {
	return key == RoomKeyWS || key == RoomKeyPush || strings.HasPrefix(key, RoomKeyPush+"/")
}

// ValidateRoomKey checks a key against the empty, length and reserved rules.
// Use it at the boundary where an untrusted key (an HTTP path segment) becomes
// a stored key.
func ValidateRoomKey(key string) error {
	if key == "" {
		return ErrRoomKeyEmpty
	}
	if len(key) > MaxRoomKeyLen {
		return ErrRoomKeyTooLong
	}
	if IsReservedRoomKey(key) {
		return ErrRoomKeyReserved
	}
	return nil
}
