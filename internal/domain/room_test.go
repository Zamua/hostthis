package domain

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNewRoomID_IsValidV4(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := NewRoomID()
		// Must parse cleanly as its own canonical form.
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
	for i := 0; i < 1000; i++ {
		id := NewRoomID()
		if _, dup := seen[id]; dup {
			t.Fatalf("collision after %d ids: %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestParseRoomID(t *testing.T) {
	valid := "f47ac10b-58cc-4372-a567-0e02b2c3d479" // a canonical v4
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

func TestRoomKV_PutGetDeleteScan(t *testing.T) {
	kv := NewRoomKV()
	if _, ok := kv.Get("missing"); ok {
		t.Fatal("empty namespace returned a hit")
	}
	kv.Put("a", []byte("alpha"))
	kv.Put("b", []byte("beta"))
	if v, ok := kv.Get("a"); !ok || !bytes.Equal(v, []byte("alpha")) {
		t.Fatalf("get a = %q,%v", v, ok)
	}
	if kv.KeyCount() != 2 {
		t.Fatalf("key count = %d, want 2", kv.KeyCount())
	}
	if kv.TotalBytes() != len("alpha")+len("beta") {
		t.Fatalf("total bytes = %d", kv.TotalBytes())
	}
	// Overwrite replaces, doesn't add a key.
	kv.Put("a", []byte("AA"))
	if kv.KeyCount() != 2 {
		t.Fatalf("overwrite changed key count to %d", kv.KeyCount())
	}
	if kv.TotalBytes() != len("AA")+len("beta") {
		t.Fatalf("overwrite total bytes = %d", kv.TotalBytes())
	}
	// Delete is idempotent.
	kv.Delete("a")
	kv.Delete("a")
	if _, ok := kv.Get("a"); ok {
		t.Fatal("a still present after delete")
	}
	if kv.KeyCount() != 1 {
		t.Fatalf("key count after delete = %d, want 1", kv.KeyCount())
	}
}

func TestRoomKV_CanPut_ByteCap(t *testing.T) {
	kv := NewRoomKV()
	// One value at exactly the cap fits.
	atCap := make([]byte, MaxRoomBytes)
	if err := kv.CanPut("big", atCap); err != nil {
		t.Fatalf("value at cap rejected: %v", err)
	}
	kv.Put("big", atCap)
	// Any new byte over the cap is rejected; prior state stays intact.
	if err := kv.CanPut("more", []byte("x")); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("over-cap write = %v, want ErrRoomFull", err)
	}
	// Overwriting "big" with something smaller is fine (delta is negative).
	if err := kv.CanPut("big", []byte("small")); err != nil {
		t.Fatalf("shrinking overwrite rejected: %v", err)
	}
}

func TestRoomKV_CanPut_ValueTooLarge(t *testing.T) {
	kv := NewRoomKV()
	tooBig := make([]byte, MaxRoomValueBytes+1)
	if err := kv.CanPut("x", tooBig); !errors.Is(err, ErrRoomValueTooLarge) {
		t.Fatalf("oversize value = %v, want ErrRoomValueTooLarge", err)
	}
}

func TestRoomKV_CanPut_KeyCountCap(t *testing.T) {
	kv := NewRoomKV()
	for i := 0; i < MaxRoomKeys; i++ {
		k := keyN(i)
		if err := kv.CanPut(k, []byte("v")); err != nil {
			t.Fatalf("key %d rejected under cap: %v", i, err)
		}
		kv.Put(k, []byte("v"))
	}
	// One more distinct key tips over the key-count cap.
	if err := kv.CanPut("overflow", []byte("v")); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("over-key-count write = %v, want ErrRoomFull", err)
	}
	// But overwriting an EXISTING key at the cap is allowed (count unchanged).
	if err := kv.CanPut(keyN(0), []byte("v2")); err != nil {
		t.Fatalf("overwrite at key cap rejected: %v", err)
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

func keyN(i int) string {
	const digits = "0123456789"
	return "k" + string(digits[i/100%10]) + string(digits[i/10%10]) + string(digits[i%10])
}
