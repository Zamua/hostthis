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
