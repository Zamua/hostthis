package domain

import "encoding/json"

// RoomWireValue is the SINGLE encoding of a stored room value onto any JSON
// wire surface. Bytes that already parse as JSON are embedded raw, so a value
// the app PUT as an object comes back NESTED rather than as a string of escaped
// text; anything else becomes a JSON string of the verbatim bytes, so opaque
// bytes round-trip without corrupting the surrounding object.
//
// The HTTP scan handler and the relay's snapshot / put mirror frames MUST
// encode a value byte-identically: the client splice contract treats a relay
// snapshot and a cold-start HTTP scan as interchangeable (docs/SPEC.md "Rooms",
// "The client splice contract"). Defining it once, as a pure function of the
// value bytes, is what makes it a domain invariant rather than transport detail.
//
// The returned RawMessage is always valid JSON: json.Marshal of a Go string
// cannot fail (invalid UTF-8 is coerced to U+FFFD), and the "null" fallback is
// defensive.
func RoomWireValue(v []byte) json.RawMessage {
	if RoomValueIsJSON(v) {
		return json.RawMessage(v)
	}
	encoded, err := json.Marshal(string(v))
	if err != nil {
		// Unreachable for any []byte; fall back to a JSON null.
		return json.RawMessage("null")
	}
	return json.RawMessage(encoded)
}

// RoomValueIsJSON is the predicate RoomWireValue passes a value through raw on.
// Exported because the single-key GET surface labels its response
// `application/json` on exactly this predicate: the content-type decision and
// the wire encoding must agree on what counts as JSON.
func RoomValueIsJSON(v []byte) bool {
	return len(v) > 0 && json.Valid(v)
}
