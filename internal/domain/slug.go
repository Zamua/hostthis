// Package domain holds pure types and rules - no I/O.
//
// Nothing here imports from storage, ssh, http, render, or any
// external SDK. Anything that needs a clock, a random source, or a
// database goes through an interface declared in domain and
// implemented by an adapter in internal/storage or its sibling.
package domain

import (
	"errors"
	"strings"
)

// SlugAlphabet is the lowercase, ambiguous-character-free alphabet
// used for randomly generated paste slugs. See SPEC.md "URL shape".
const SlugAlphabet = "abcdefghijkmnpqrstuvwxyz23456789"

// SlugLength is the number of characters in a generated slug.
const SlugLength = 8

// Slug is a paste identifier that lives in a URL.
//
// It's a small value-object rather than a bare string so the rest of
// the domain can require valid slugs at compile time. Construction
// validates against the alphabet + length rules; persisted slugs are
// re-validated on read by passing through ParseSlug.
type Slug string

var (
	// ErrSlugEmpty is returned when an empty string is parsed as a slug.
	ErrSlugEmpty = errors.New("slug is empty")
	// ErrSlugWrongLength is returned when a slug is the wrong number of characters.
	ErrSlugWrongLength = errors.New("slug is not exactly SlugLength characters")
	// ErrSlugBadAlphabet is returned when a slug contains characters outside SlugAlphabet.
	ErrSlugBadAlphabet = errors.New("slug contains characters outside SlugAlphabet")
)

// ParseSlug validates that s is a well-formed slug and returns it
// typed. Use this at every boundary where untrusted input becomes a
// Slug - request handlers, repo reads, CLI args.
func ParseSlug(s string) (Slug, error) {
	if s == "" {
		return "", ErrSlugEmpty
	}
	if len(s) != SlugLength {
		return "", ErrSlugWrongLength
	}
	for _, r := range s {
		if !strings.ContainsRune(SlugAlphabet, r) {
			return "", ErrSlugBadAlphabet
		}
	}
	return Slug(s), nil
}

// String returns the slug as a plain string for URL building.
func (s Slug) String() string { return string(s) }
