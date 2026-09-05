package ssh_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestVerbDelete_Roundtrip(t *testing.T) {
	s := startStack(t)
	stdout, _, _ := s.run("", []byte("<!doctype html><p>delete me</p>"))
	slug := extractSlug(stdout)

	resp, err := http.Get(s.httpURL + "/p/" + slug)
	if err != nil {
		t.Fatalf("get before delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 before delete, got %d", resp.StatusCode)
	}

	_, stderr, exit := s.run("delete "+slug, nil)
	if exit != 0 {
		t.Fatalf("delete exit: %d (%q)", exit, stderr)
	}
	if !strings.Contains(stderr, "deleted") {
		t.Fatalf("expected 'deleted' confirmation, got %q", stderr)
	}

	resp, err = http.Get(s.httpURL + "/p/" + slug)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

func TestVerbUpdate_AppendsVersion(t *testing.T) {
	s := startStack(t)
	stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	slug := extractSlug(stdout)
	_, stderr, exit := s.run(slug, []byte("<!doctype html><p>v2</p>"))
	if exit != 0 {
		t.Fatalf("update exit: %d (%q)", exit, stderr)
	}
	if !strings.Contains(stderr, "v2") {
		t.Fatalf("expected v2 in update stderr: %q", stderr)
	}
	stdoutV, _, _ := s.run("versions "+slug, nil)
	if !strings.Contains(stdoutV, "v1") || !strings.Contains(stdoutV, "v2") {
		t.Fatalf("expected v1 + v2 in versions output: %q", stdoutV)
	}
	resp, _ := http.Get(s.httpURL + "/p/" + slug)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "v2") {
		t.Fatalf("expected v2 served: %q", body)
	}
	// Pinning v1 changes what the URL serves.
	_, _, _ = s.run("pin "+slug+" 1", nil)
	resp, _ = http.Get(s.httpURL + "/p/" + slug)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "v1") {
		t.Fatalf("expected v1 served after pin: %q", body)
	}
}

// An update that carries a rejected --name reports the failure instead of the
// success banner: the label did not change, so claiming the save succeeded
// hides a half-applied command.
func TestVerbUpdate_RejectedNameIsNotReportedAsSuccess(t *testing.T) {
	s := startStack(t)
	stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	slug := extractSlug(stdout)

	tooLong := strings.Repeat("a", 61) // validName caps a label at 60 runes
	outUpd, stderr, exit := s.run(slug+" --name "+tooLong, []byte("<!doctype html><p>v2</p>"))
	if exit == 0 {
		t.Fatalf("update with a rejected label should exit non-zero, got 0 (stdout=%q stderr=%q)", outUpd, stderr)
	}
	if !strings.Contains(stderr, "name must be") {
		t.Fatalf("expected the invalid-name message, got %q", stderr)
	}
	if strings.Contains(stderr, "saved") {
		t.Fatalf("a rejected label must not print the saved banner, got %q", stderr)
	}
	// The label really is unchanged, so the error was the truthful report.
	listOut, _, _ := s.run("list", nil)
	if strings.Contains(listOut, tooLong) {
		t.Fatalf("rejected label must not be applied: %q", listOut)
	}
}
