package ssh_test

import (
	"strings"
	"testing"
)

// A rendered paste's page carries its title in the markup a link unfurler
// reads: the served version's first heading. The owner's label never appears
// in the page.
func TestRenderedPasteServesItsTitle(t *testing.T) {
	s := startStack(t)
	stdout, stderr, exit := s.run("", []byte("intro\n\n# Design <notes>\n\nbody\n"))
	if exit != 0 {
		t.Fatalf("upload: exit %d (%q)", exit, stderr)
	}
	url := strings.TrimSpace(stdout)
	slug := extractSlug(stdout)
	s.upload.WaitFinalize()

	wantTitle := func(want string) {
		t.Helper()
		code, body := getBody(t, url)
		if code != 200 {
			t.Fatalf("GET %s: %d", url, code)
		}
		for _, tag := range []string{"<title>" + want + "</title>", `<meta property="og:title" content="` + want + `">`} {
			if !strings.Contains(body, tag) {
				t.Fatalf("page lacks %q:\n%s", tag, body)
			}
		}
	}
	wantTitle("Design &#60;notes&#62;")

	if _, stderr, exit := s.run("rename "+slug+" privatelabel", nil); exit != 0 {
		t.Fatalf("rename: exit %d (%q)", exit, stderr)
	}
	if _, stderr, exit := s.run(slug, []byte("# Second draft\n")); exit != 0 {
		t.Fatalf("update: exit %d (%q)", exit, stderr)
	}
	wantTitle("Second draft")
	if _, body := getBody(t, url); strings.Contains(body, "privatelabel") {
		t.Fatalf("the label reached the page:\n%s", body)
	}
}
