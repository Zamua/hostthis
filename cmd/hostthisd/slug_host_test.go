package main

import (
	"io"
	"log"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestSlugHostSuffixShapesPrintedURLs(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	slug := domain.Slug("abcdefgh")
	for _, c := range []struct{ flat, want string }{
		{"", "https://abcdefgh.example.com"},
		{"-Staging.example.com", "https://abcdefgh-staging.example.com"},
	} {
		build := buildURL("https", slugHostSuffix("example.com", c.flat), "example.com", "subdomain", logger)
		if got := build(slug); got != c.want {
			t.Errorf("suffix %q: got %q, want %q", c.flat, got, c.want)
		}
	}
	if got := buildURL("https", "-staging.example.com", "example.com", "path", logger)(slug); got != "https://example.com/p/abcdefgh" {
		t.Errorf("path mode: got %q", got)
	}
}
