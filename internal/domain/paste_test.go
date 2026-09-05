package domain

import (
	"strings"
	"testing"
)

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
