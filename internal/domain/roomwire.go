package domain

import "encoding/json"

// RoomWireValue is the SINGLE encoding of a stored room value onto any
// JSON wire surface: the value is embedded as raw JSON when the stored
// bytes already parse as JSON (so a value the app PUT as a JSON object
// comes back as a NESTED object, not a JSON-string of escaped text),
// and as a JSON string of the verbatim bytes otherwise (so opaque bytes
// round-trip without corrupting the surrounding JSON object).
//
// Both consumers - the HTTP scan handler (GET /api/rooms/<uuid>) and
// the relay's snapshot / put mirror frames - MUST encode a value
// byte-identically: the client splice contract treats a relay snapshot
// and a cold-start HTTP scan as interchangeable (docs/SPEC.md "Rooms"
// and "The client splice contract"), so the encoding is defined ONCE
// here and both call it. It is a pure function of the value bytes,
// which is what makes it domain: a cross-surface product invariant,
// not a transport detail.
//
// The returned RawMessage is always valid JSON: for any non-JSON input
// json.Marshal of a Go string cannot fail (invalid UTF-8 is coerced to
// U+FFFD per encoding/json's string encoding), with a defensive "null"
// fallback kept anyway.
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

// RoomValueIsJSON is the predicate RoomWireValue passes a value through
// raw on: non-empty, syntactically valid JSON. Exported because the
// single-key GET surface labels its response `application/json` on
// exactly this predicate - the content-type decision and the wire
// encoding must agree on what "recognizably JSON" means.
func RoomValueIsJSON(v []byte) bool {
	return len(v) > 0 && json.Valid(v)
}
