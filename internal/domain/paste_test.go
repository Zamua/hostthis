package domain

import (
	"strings"
	"testing"
)

func TestHashContent_Deterministic(t *testing.T) {
	a := HashContent([]byte("hello"))
	b := HashContent([]byte("hello"))
	if a != b {
		t.Fatalf("hash differs for same bytes: %s vs %s", a, b)
	}
}

func TestHashContent_DiffersForDiffBytes(t *testing.T) {
	a := HashContent([]byte("hello"))
	b := HashContent([]byte("hello!"))
	if a == b {
		t.Fatalf("hash is the same for different bytes: %s", a)
	}
}

func TestHashContent_Sha256Hex(t *testing.T) {
	// echo -n hello | sha256sum
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got := HashContent([]byte("hello"))
	if got != want {
		t.Fatalf("hash: got %s, want %s", got, want)
	}
}

func TestNewRandomSlug_ShapeAndAlphabet(t *testing.T) {
	for range 256 {
		s := NewRandomSlug()
		if len(s) != SlugLength {
			t.Fatalf("slug length: got %d, want %d (slug=%q)", len(s), SlugLength, s)
		}
		for _, r := range s {
			if !strings.ContainsRune(SlugAlphabet, r) {
				t.Fatalf("slug %q contains char %q outside SlugAlphabet", s, r)
			}
		}
	}
}

func TestNewRandomSlug_Uniqueness(t *testing.T) {
	// 32^8 = 1.1e12 possibilities; collisions in 1000 samples are
	// vanishingly unlikely. Birthday math: ~10^-7 per pair.
	seen := make(map[Slug]struct{}, 1000)
	for range 1000 {
		s := NewRandomSlug()
		if _, dup := seen[s]; dup {
			t.Fatalf("collision in 1000 samples: %q", s)
		}
		seen[s] = struct{}{}
	}
}

func TestNormalizeStatus(t *testing.T) {
	cases := map[string]PasteStatus{
		"pending": PasteStatusPending,
		"ready":   PasteStatusReady,
		"failed":  PasteStatusFailed,
		"":        PasteStatusReady, // legacy row with no status field
		"bogus":   PasteStatusReady, // unknown value falls back to ready
	}
	for in, want := range cases {
		if got := NormalizeStatus(in); got != want {
			t.Errorf("NormalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}
