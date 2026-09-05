package domain

import (
	"errors"
	"testing"
)

func TestParseSlug(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    Slug
		wantErr error
	}{
		{name: "valid letters+digits", input: "abc23456", want: "abc23456"},
		{name: "all alphabet chars", input: "abcdefgh", want: "abcdefgh"},
		{name: "with digits", input: "23456789", want: "23456789"},
		{name: "empty", input: "", wantErr: ErrSlugEmpty},
		{name: "too short", input: "abc", wantErr: ErrSlugWrongLength},
		{name: "too long", input: "abcdefghi", wantErr: ErrSlugWrongLength},
		{name: "uppercase rejected", input: "Abcdefgh", wantErr: ErrSlugBadAlphabet},
		{name: "ambiguous l rejected", input: "abclefgh", wantErr: ErrSlugBadAlphabet},
		{name: "ambiguous 1 rejected", input: "abc1efgh", wantErr: ErrSlugBadAlphabet},
		{name: "ambiguous 0 rejected", input: "abc0efgh", wantErr: ErrSlugBadAlphabet},
		{name: "ambiguous o rejected", input: "abcoefgh", wantErr: ErrSlugBadAlphabet},
		{name: "hyphen rejected", input: "abc-efgh", wantErr: ErrSlugBadAlphabet},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseSlug(c.input)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err: got %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != c.want {
				t.Fatalf("slug: got %q, want %q", got, c.want)
			}
		})
	}
}
