package ssh_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

func (s *stack) abuseOp(op, slug string) {
	s.t.Helper()
	res, err := http.Post(s.endpoint+"/abuse/"+op, "application/json",
		bytes.NewBufferString(`{"slug":"`+slug+`","reason":"test"}`))
	if err != nil || res.StatusCode != http.StatusOK {
		s.t.Fatalf("abuse %s %s: %v %v", op, slug, res, err)
	}
	_ = res.Body.Close()
}

// A quarantined paste 404s, tells only its owner why, keeps its listing entry
// through a delete attempt, and comes back whole on release.
func TestQuarantine_HidesUntilReleaseAndOnlyTheOwnerIsTold(t *testing.T) {
	g := startStack(t)
	stdout, _, code := g.run("", []byte("<!doctype html><h1>q</h1>"))
	if code != 0 {
		t.Fatalf("upload exit %d", code)
	}
	slug := extractSlug(stdout)
	g.abuseOp("quarantine", slug)

	if status, _ := getBody(t, g.httpURL+"/p/"+slug); status != http.StatusNotFound {
		t.Fatalf("quarantined read: %d, want 404", status)
	}
	_, stderr, code := g.run("delete "+slug, nil)
	if code != 4 || !strings.Contains(stderr, "quarantined by the operator") {
		t.Fatalf("owner delete: exit %d %q, want 4 and the quarantine message", code, stderr)
	}
	other, _ := newKeyClient(t, g.sshAddr)
	if _, stderr, code := g.runOn(other, "delete "+slug, nil); code != 4 || strings.Contains(stderr, "quarantined") {
		t.Fatalf("stranger delete: exit %d %q, want a plain not found", code, stderr)
	}
	if listing, _, _ := g.run("list", nil); !strings.Contains(listing, slug) {
		t.Fatalf("listing lost the quarantined slug: %q", listing)
	}

	g.abuseOp("release", slug)
	if status, body := getBody(t, g.httpURL+"/p/"+slug); status != http.StatusOK || !strings.Contains(body, "<h1>q</h1>") {
		t.Fatalf("released read: %d %q", status, body)
	}
}
