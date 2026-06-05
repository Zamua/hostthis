package http

import "testing"

func TestSlugFromHost(t *testing.T) {
	s := &Server{ApexDomain: "hostthis.dev"}
	cases := []struct {
		host    string
		want    string // empty when expected ok=false
		wantOK  bool
	}{
		{"abc23456.hostthis.dev", "abc23456", true},
		{"abc23456.hostthis.dev:443", "abc23456", true},
		{"hostthis.dev", "", false},
		{"hostthis.dev:443", "", false},
		// Multi-level subdomain — ignored.
		{"foo.abc23456.hostthis.dev", "", false},
		// Wrong apex — ignored.
		{"abc23456.example.com", "", false},
		// Slug too short — fails ParseSlug.
		{"abc.hostthis.dev", "", false},
		// Slug uses uppercase — fails ParseSlug.
		{"ABC23456.hostthis.dev", "", false},
		// Reserved-ish but valid slug shape — accepted (apex landing
		// blocks reserved by never generating them, not by Host check).
		{"abcdefgh.hostthis.dev", "abcdefgh", true},
	}
	for _, c := range cases {
		t.Run(c.host, func(t *testing.T) {
			got, ok := s.slugFromHost(c.host)
			if ok != c.wantOK {
				t.Fatalf("ok: got %v, want %v (slug=%q)", ok, c.wantOK, got)
			}
			if string(got) != c.want {
				t.Fatalf("slug: got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSlugFromHost_NoApexConfigured(t *testing.T) {
	// When ApexDomain is empty, never match — only path-mode works.
	s := &Server{}
	if _, ok := s.slugFromHost("abc23456.anything.com"); ok {
		t.Fatalf("should not match without ApexDomain")
	}
}
