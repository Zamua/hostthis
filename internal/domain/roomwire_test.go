package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// TestRoomWireValue pins the room-value wire encoding BYTE-FOR-BYTE.
// This is the single shared implementation behind both the HTTP scan
// handler and the relay snapshot/mirror frames; the client splice
// contract depends on the two surfaces encoding a value identically,
// so the exact output bytes are contract, not implementation detail.
func TestRoomWireValue(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		// Valid JSON passes through VERBATIM - the exact stored bytes,
		// whitespace and all, not a re-marshaled canonical form.
		{"json object passthrough", []byte(`{"a":1,"b":[true,null]}`), `{"a":1,"b":[true,null]}`},
		{"json object with stored whitespace", []byte(`{ "a" : 1 }`), `{ "a" : 1 }`},
		{"json number passthrough", []byte(`42`), `42`},
		{"json string passthrough", []byte(`"already a json string"`), `"already a json string"`},
		{"json null passthrough", []byte(`null`), `null`},
		{"json array passthrough", []byte(`[1,2,3]`), `[1,2,3]`},

		// Non-JSON encodes as a JSON string of the verbatim bytes.
		{"plain text", []byte(`hello world`), `"hello world"`},
		{"almost-json", []byte(`{"unterminated`), `"{\"unterminated"`},
		{"whitespace only", []byte(" "), `" "`},

		// Empty is NOT valid JSON: it encodes as the empty JSON string.
		{"empty", []byte{}, `""`},
		{"nil", nil, `""`},

		// Control chars escape per encoding/json's string rules.
		{"control chars", []byte{0x00, 0x01, 'a', '\n'}, "\"\\u0000\\u0001a\\n\""},
		// Invalid UTF-8 is coerced to U+FFFD, emitted as its escape
		// sequence (encoding/json string rule).
		{"invalid utf8", []byte{0xff, 0xfe}, `"\ufffd\ufffd"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := domain.RoomWireValue(tc.in)
			if string(got) != tc.want {
				t.Fatalf("RoomWireValue(%q) = %s, want %s", tc.in, got, tc.want)
			}
			// The output must always itself be valid JSON: it is embedded
			// verbatim into a surrounding JSON object by both surfaces.
			if !json.Valid(got) {
				t.Fatalf("RoomWireValue(%q) = %s is not valid JSON", tc.in, got)
			}
		})
	}
}
