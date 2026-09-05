package ssh_test

import (
	"encoding/json"
	"strings"
	"testing"

	hostssh "github.com/Zamua/hostthis/internal/ssh"
)

const siteIndex = "<!doctype html><h1>home</h1>"

// A gzip-tar piped with no verb and no flag deploys a site; a plain file on
// the same server is still a paste. Site files serve with their content-type
// and the sandbox headers, route-shaped misses fall back to the root index,
// asset-shaped misses 404, an in-place redeploy keeps the URL, and a foreign
// redeploy is not-found and leaves the site untouched.
func TestSite_DeployAndServe(t *testing.T) {
	s := startStack(t, withSites())
	arc := makeSiteArchive(t, map[string]string{
		"index.html":  siteIndex,
		"about.html":  "<h1>about</h1>",
		"css/app.css": "body{color:teal}",
		"js/app.js":   "console.log('app')",
	})
	stdout, stderr, exit := s.run("", arc)
	if exit != 0 {
		t.Fatalf("deploy exit %d: stderr %q", exit, stderr)
	}
	base := strings.TrimSpace(stdout)
	slug := extractSlug(stdout)
	if !strings.HasPrefix(base, s.httpURL+"/p/") || slug == "" {
		t.Fatalf("stdout doesn't look like a URL: %q (stderr %q)", stdout, stderr)
	}
	// Only the site path narrates "site: N file(s)".
	if !strings.Contains(stderr, "site:") {
		t.Fatalf("archive should route to the site path, stderr %q", stderr)
	}

	t.Run("FilesServeWithTypeAndSandbox", func(t *testing.T) {
		for _, c := range []struct{ path, body, ctype string }{
			{"", siteIndex, "text/html; charset=utf-8"},
			{"/about.html", "<h1>about</h1>", "text/html; charset=utf-8"},
			{"/css/app.css", "body{color:teal}", "text/css; charset=utf-8"},
			{"/js/app.js", "console.log('app')", "text/javascript; charset=utf-8"},
		} {
			resp, body := httpGet(t, base+c.path)
			if code := resp.StatusCode; code != 200 || body != c.body {
				t.Fatalf("GET %s: code %d body %q, want 200 %q", c.path, code, body, c.body)
			}
			if ct := resp.Header.Get("Content-Type"); ct != c.ctype {
				t.Fatalf("GET %s content-type: got %q, want %q", c.path, ct, c.ctype)
			}
			if resp.Header.Get("X-Frame-Options") != "DENY" {
				t.Fatalf("GET %s missing sandbox header", c.path)
			}
		}
	})

	t.Run("RouteMissServesRootIndex_AssetMiss404s", func(t *testing.T) {
		// A route-shaped miss (no extension, or ".html") serves the ROOT
		// index.html with a 200 so a client-side router can render the route.
		for _, p := range []string{"/about-page", "/does-not-exist.html", "/users/42"} {
			if code, body := getBody(t, base+p); code != 200 || body != siteIndex {
				t.Fatalf("GET route %s: code %d body %q, want 200 + root index", p, code, body)
			}
		}
		// Never index.html served as JS.
		if code, _ := getBody(t, base+"/assets/nope.js"); code != 404 {
			t.Fatalf("missing asset: got %d, want 404", code)
		}
	})

	t.Run("PlainFileIsStillAPaste", func(t *testing.T) {
		out, errOut, exit := s.run("", []byte("<!doctype html><p>sibling paste</p>"))
		if exit != 0 || strings.Contains(errOut, "site:") {
			t.Fatalf("plain file should route to the paste path: exit %d stderr %q", exit, errOut)
		}
		url := strings.TrimSpace(out)
		if url == base {
			t.Fatalf("paste and site got the same slug %q", base)
		}
		resp, body := httpGet(t, url)
		if code := resp.StatusCode; code != 200 || body != "<!doctype html><p>sibling paste</p>" {
			t.Fatalf("paste GET: code %d body %q", code, body)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("paste content-type: got %q", ct)
		}
	})

	t.Run("ForeignRedeployIsNotFoundAndSiteUnchanged", func(t *testing.T) {
		other, _ := newKeyClient(t, s.sshAddr)
		hijack := makeSiteArchive(t, map[string]string{"index.html": "<h1>hijacked</h1>"})
		_, errOut, exit := s.runOn(other, slug, hijack)
		if exit != hostssh.ExitNotFound {
			t.Fatalf("foreign re-deploy: exit %d stderr %q, want not-found (%d)", exit, errOut, hostssh.ExitNotFound)
		}
		if code, body := getBody(t, base); code != 200 || body != siteIndex {
			t.Fatalf("site after rejected foreign re-deploy: code %d body %q, want unchanged", code, body)
		}
	})

	t.Run("InPlaceRedeployKeepsURLServesNewBytes", func(t *testing.T) {
		v2 := makeSiteArchive(t, map[string]string{
			"index.html":  "<!doctype html><h1>v2 CHANGED</h1>",
			"css/app.css": "body{color:green}",
			"new.html":    "<h1>brand new file</h1>",
		})
		out, errOut, exit := s.run(slug, v2)
		if exit != 0 || !strings.Contains(errOut, "site:") || strings.Contains(errOut, "not found") {
			t.Fatalf("in-place site update: exit %d stderr %q", exit, errOut)
		}
		if got := strings.TrimSpace(out); got != base {
			t.Fatalf("in-place update changed the URL: got %q, want %q", got, base)
		}
		for p, want := range map[string]string{
			"":             "<!doctype html><h1>v2 CHANGED</h1>",
			"/css/app.css": "body{color:green}",
			"/new.html":    "<h1>brand new file</h1>",
		} {
			if code, body := getBody(t, base+p); code != 200 || body != want {
				t.Fatalf("GET %s after update: code %d body %q, want %q", p, code, body, want)
			}
		}
	})
}

// The update path's gzip-magic peek leaves a plain-HTML paste update alone:
// the version still bumps, the URL is unchanged and the new body is served.
func TestSite_PasteUpdateSurvivesGzipPeek(t *testing.T) {
	s := startStack(t, withSites())
	out1, err1, exit1 := s.run("", []byte("<!doctype html><p>v1 paste</p>"))
	if exit1 != 0 || strings.Contains(err1, "site:") {
		t.Fatalf("create paste: exit %d stderr %q", exit1, err1)
	}
	base := strings.TrimSpace(out1)
	out2, err2, exit2 := s.run(extractSlug(out1), []byte("<!doctype html><p>v2 paste UPDATED</p>"))
	if exit2 != 0 || !strings.Contains(err2, "saved") {
		t.Fatalf("paste update: exit %d stderr %q", exit2, err2)
	}
	if got := strings.TrimSpace(out2); got != base {
		t.Fatalf("paste update changed the URL: got %q want %q", got, base)
	}
	if code, body := getBody(t, base); code != 200 || body != "<!doctype html><p>v2 paste UPDATED</p>" {
		t.Fatalf("paste body after update: code %d body %q", code, body)
	}
}

// An archive with no web content is rejected like any unsupported upload.
func TestSite_NoWebContentRejected(t *testing.T) {
	s := startStack(t, withSites())
	arc := makeSiteArchive(t, map[string]string{"data.json": "{}", "notes.txt": "hi"})
	_, stderr, exit := s.run("", arc)
	if exit == 0 {
		t.Fatalf("expected nonzero exit for no-web-content archive")
	}
	if !strings.Contains(stderr, "no web content") {
		t.Fatalf("stderr should explain rejection, got %q", stderr)
	}
}

// `list` shows deployed sites in both the table and -o json. A site counts
// against the same quota as pastes, so an owner who cannot see it can never
// free what it holds.
func TestSite_ListIncludesSites(t *testing.T) {
	s := startStack(t, withSites())
	s.run("", []byte("<!doctype html><p>a text paste</p>"))
	if _, stderr, _ := s.run("", makeSiteArchive(t, map[string]string{"index.html": siteIndex})); !strings.Contains(stderr, "site:") {
		t.Fatalf("expected a site deploy, stderr=%q", stderr)
	}

	stdout, _, _ := s.run("list", nil)
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 3 { // header + paste + site
		t.Fatalf("expected header + 2 rows, got %d:\n%s", len(lines), stdout)
	}
	var siteRow string
	for _, ln := range lines[1:] {
		if strings.Contains(ln, "site") {
			siteRow = ln
		}
	}
	if siteRow == "" {
		t.Fatalf("no site row (kind=site) in list output:\n%s", stdout)
	}
	// A directory is a paste, so it is versioned and shows a version like
	// any other.
	if f := strings.Fields(siteRow); f[len(f)-1] != "v1" {
		t.Fatalf("site VERS column should be 'v1', got %q in %q", f[len(f)-1], siteRow)
	}

	jsonOut, _, _ := s.run("list -o json", nil)
	var items []struct {
		Slug          string `json:"slug"`
		Kind          string `json:"kind"`
		SizeBytes     int    `json:"size_bytes"`
		ServedVersion *int   `json:"served_version"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &items); err != nil {
		t.Fatalf("list -o json not an array: %v\n%q", err, jsonOut)
	}
	var site, paste int
	for _, it := range items {
		if it.ServedVersion == nil {
			t.Fatalf("%s %s served_version should be non-null", it.Kind, it.Slug)
		}
		if it.Kind == "site" {
			site++
			if *it.ServedVersion != 1 || it.SizeBytes <= 0 {
				t.Fatalf("site row: served_version %d size_bytes %d", *it.ServedVersion, it.SizeBytes)
			}
		} else {
			paste++
		}
	}
	if site != 1 || paste != 1 {
		t.Fatalf("want 1 site + 1 paste in json, got site=%d paste=%d", site, paste)
	}
}
