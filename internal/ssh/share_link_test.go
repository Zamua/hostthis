package ssh_test

import (
	"path"
	"strings"
	"testing"
)

// qrGlyphs are the half-block runes qrterminal emits in HalfBlocks mode
// (BLACK/WHITE combinations). At least one must appear for a rendered QR
// to be present; none may appear on stdout (the URL must stay clean).
const qrGlyphs = "█▀▄"

// slugFromURL pulls the trailing slug off a path-mode URL
// (httpURL + "/p/<slug>"). The e2e stack builds URLs that way.
func slugFromURL(t *testing.T, url string) string {
	t.Helper()
	s := path.Base(strings.TrimSpace(url))
	if s == "" || s == "." || s == "/" {
		t.Fatalf("could not extract slug from URL %q", url)
	}
	return s
}

// TestCreate_QROnStderr_URLOnStdout pins the create contract: the URL is
// the ONLY thing on stdout (no QR glyphs leak there, preserving the
// `slug=$(... | ssh -T host)` capture), while the QR renders on stderr
// alongside the expiry narration.
func TestCreate_QROnStderr_URLOnStdout(t *testing.T) {
	s := startStack(t)
	stdout, stderr, exit := s.run("", []byte("<!doctype html><h1>qr</h1>"))
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
	}
	// stdout is exactly the URL line - one line, no QR characters.
	url := strings.TrimSpace(stdout)
	if !strings.Contains(url, "/p/") {
		t.Fatalf("stdout should be a paste URL, got %q", stdout)
	}
	if strings.Count(strings.TrimRight(stdout, "\n"), "\n") != 0 {
		t.Fatalf("stdout should be a single URL line, got %q", stdout)
	}
	if strings.ContainsAny(stdout, qrGlyphs) {
		t.Fatalf("QR glyphs leaked onto stdout: %q", stdout)
	}
	// stderr carries the narration AND the QR block.
	if !strings.Contains(stderr, "expires in 30 days") {
		t.Fatalf("stderr should mention expiry, got %q", stderr)
	}
	if !strings.ContainsAny(stderr, qrGlyphs) {
		t.Fatalf("stderr should contain a rendered QR code, got %q", stderr)
	}
}

// TestVerbURL_Existing: `url <slug>` prints just the URL on stdout, no
// QR, exit 0. The URL must match what create returned (same builder).
func TestVerbURL_Existing(t *testing.T) {
	s := startStack(t)
	createOut, _, _ := s.run("", []byte("<!doctype html><h1>u</h1>"))
	wantURL := strings.TrimSpace(createOut)
	slug := slugFromURL(t, wantURL)

	stdout, stderr, exit := s.run("url "+slug, nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
	}
	if got := strings.TrimSpace(stdout); got != wantURL {
		t.Fatalf("url stdout = %q, want %q", got, wantURL)
	}
	if strings.ContainsAny(stdout, qrGlyphs) || strings.ContainsAny(stderr, qrGlyphs) {
		t.Fatalf("url verb must not render a QR (stdout=%q stderr=%q)", stdout, stderr)
	}
}

// TestVerbQR_Existing: `qr <slug>` mirrors create - URL on stdout, QR on
// stderr.
func TestVerbQR_Existing(t *testing.T) {
	s := startStack(t)
	createOut, _, _ := s.run("", []byte("<!doctype html><h1>q</h1>"))
	wantURL := strings.TrimSpace(createOut)
	slug := slugFromURL(t, wantURL)

	stdout, stderr, exit := s.run("qr "+slug, nil)
	if exit != 0 {
		t.Fatalf("exit: %d (stderr: %q)", exit, stderr)
	}
	if got := strings.TrimSpace(stdout); got != wantURL {
		t.Fatalf("qr stdout = %q, want %q", got, wantURL)
	}
	if strings.ContainsAny(stdout, qrGlyphs) {
		t.Fatalf("QR glyphs leaked onto stdout: %q", stdout)
	}
	if !strings.ContainsAny(stderr, qrGlyphs) {
		t.Fatalf("qr verb should render a QR on stderr, got %q", stderr)
	}
}

// TestVerbURLQR_Missing: a well-formed but nonexistent slug returns the
// standard not-found (exit 4) for both verbs - same shape as every other
// missing-slug path, so existence isn't probeable.
func TestVerbURLQR_Missing(t *testing.T) {
	s := startStack(t)
	const ghost = "abcdefgh" // valid slug shape, never uploaded
	for _, verb := range []string{"url", "qr"} {
		stdout, stderr, exit := s.run(verb+" "+ghost, nil)
		if exit != 4 {
			t.Fatalf("%s missing: exit = %d, want 4 (stderr %q)", verb, exit, stderr)
		}
		if !strings.Contains(stderr, "not found") {
			t.Fatalf("%s missing: stderr = %q, want not-found", verb, stderr)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("%s missing: stdout should be empty, got %q", verb, stdout)
		}
	}
}

// TestVerbURL_BadSlug: a malformed slug is a usage error (exit 2), not a
// not-found - it never reaches the existence lookup.
func TestVerbURL_BadSlug(t *testing.T) {
	s := startStack(t)
	_, stderr, exit := s.run("url not-a-valid-slug", nil)
	if exit != 2 {
		t.Fatalf("bad slug: exit = %d, want 2 (stderr %q)", exit, stderr)
	}
}
