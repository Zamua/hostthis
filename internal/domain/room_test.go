package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestNewRoomID_IsValidV4(t *testing.T) {
	for range 200 {
		id := NewRoomID()
		got, err := ParseRoomID(id.String())
		if err != nil {
			t.Fatalf("NewRoomID produced an unparseable id %q: %v", id, err)
		}
		if got != id {
			t.Fatalf("round-trip mismatch: minted %q, parsed to %q", id, got)
		}
		// Canonical shape: 36 chars, version 4, variant 8/9/a/b.
		s := id.String()
		if len(s) != 36 {
			t.Fatalf("len(%q) = %d, want 36", s, len(s))
		}
		if s[14] != '4' {
			t.Fatalf("version nibble of %q = %q, want '4'", s, s[14])
		}
		if !strings.ContainsRune("89ab", rune(s[19])) {
			t.Fatalf("variant nibble of %q = %q, want one of 8/9/a/b", s, s[19])
		}
	}
}

func TestNewRoomID_Unique(t *testing.T) {
	seen := make(map[RoomID]struct{}, 1000)
	for i := range 1000 {
		id := NewRoomID()
		if _, dup := seen[id]; dup {
			t.Fatalf("collision after %d ids: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestParseRoomID(t *testing.T) {
	valid := "f47ac10b-58cc-4372-a567-0e02b2c3d479" // canonical v4
	got, err := ParseRoomID(valid)
	if err != nil {
		t.Fatalf("valid v4 rejected: %v", err)
	}
	if got.String() != valid {
		t.Fatalf("canonical round-trip: got %q", got)
	}

	// Uppercase canonicalizes to lowercase so two spellings address one room.
	up, err := ParseRoomID(strings.ToUpper(valid))
	if err != nil {
		t.Fatalf("uppercase v4 rejected: %v", err)
	}
	if up.String() != valid {
		t.Fatalf("uppercase not canonicalized: got %q want %q", up, valid)
	}

	cases := []struct {
		name string
		in   string
		want error
	}{
		{"empty", "", ErrRoomIDEmpty},
		{"too short", "f47ac10b-58cc-4372-a567-0e02b2c3d47", ErrRoomIDMalformed},
		{"too long", valid + "0", ErrRoomIDMalformed},
		{"bad hyphen", "f47ac10b558cc-4372-a567-0e02b2c3d479", ErrRoomIDMalformed},
		{"non-hex", "z47ac10b-58cc-4372-a567-0e02b2c3d479", ErrRoomIDMalformed},
		{"version 1 not 4", "f47ac10b-58cc-1372-a567-0e02b2c3d479", ErrRoomIDMalformed},
		{"bad variant", "f47ac10b-58cc-4372-7567-0e02b2c3d479", ErrRoomIDMalformed},
		{"slug shaped", "abc12345", ErrRoomIDMalformed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParseRoomID(c.in); !errors.Is(err, c.want) {
				t.Fatalf("ParseRoomID(%q) = %v, want %v", c.in, err, c.want)
			}
		})
	}
}

func TestValidateRoomKey(t *testing.T) {
	if err := ValidateRoomKey(""); !errors.Is(err, ErrRoomKeyEmpty) {
		t.Fatalf("empty key = %v", err)
	}
	if err := ValidateRoomKey(strings.Repeat("k", MaxRoomKeyLen+1)); !errors.Is(err, ErrRoomKeyTooLong) {
		t.Fatalf("long key = %v", err)
	}
	if err := ValidateRoomKey("participants"); err != nil {
		t.Fatalf("normal key rejected: %v", err)
	}
	// A key WITH slashes is fine - "card/<id>" is a real app shape.
	if err := ValidateRoomKey("card/abc123"); err != nil {
		t.Fatalf("slashed key rejected: %v", err)
	}
}
