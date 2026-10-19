package parity

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// genericTargets reach every paste the same way: the flags as Go's
// URL.Query().Has reads them, paths a document does not serve, paths ServeMux
// cleans or redirects, malformed escapes, and the routes that answer on
// every host.
var genericTargets = []string{
	"/", "/?raw", "/?raw=0", "/?raw=1", "/?r%61w", "/?raw;x=1", "/?raw=%zz", "/?files=1", "/?files",
	"/?raw&files=1", "/?x=1&raw", "/?RAW",
	"/index.html", "/nope", "/nope.js", "/a/b/c", "/a/b/",
	"//", "/./", "/x/../", "/%2e%2e/", "/%2E/", "/x/%2e%2e/?raw", "/.", "/x/..",
	"/%zz", "/%2F", "/%2Fevil.com//", "/%5Cevil", "//evil.com/", "/%00",
	"/_hostthis", "/_hostthis/", "/_hostthis/md.js", "/_hostthis/mdshell/md.js", "/_hostthis/nope.js",
	"/healthz", "/readyz", "/healthz/", "/api/push/key",
}

// variants are the requests made of each target: GET, HEAD, and a method no
// read answers.
func variants(host, target string) []request {
	return []request{
		get(host, target),
		{method: "HEAD", host: host, target: target},
		{method: "POST", host: host, target: target, body: []byte("x")},
	}
}

// conditionals revalidate an answer that carried validators, with tags and
// dates that match and that do not.
func conditionals(r request, h response) []request {
	if h.status != 200 {
		return nil
	}
	var out []request
	if etag := h.header.Get("ETag"); etag != "" {
		out = append(out,
			r.with("If-None-Match", etag),
			r.with("If-None-Match", "W/"+etag),
			r.with("If-None-Match", `"other", `+etag),
			r.with("If-None-Match", `"other"`),
			r.with("If-None-Match", "*"),
		)
	}
	if lm := h.header.Get("Last-Modified"); lm != "" {
		out = append(out, r.with("If-Modified-Since", lm))
		if at, err := http.ParseTime(lm); err == nil {
			out = append(out,
				r.with("If-Modified-Since", at.Add(-time.Hour).Format(http.TimeFormat)),
				r.with("If-Modified-Since", at.Add(time.Hour).Format(http.TimeFormat)),
			)
		}
		out = append(out, r.with("If-Modified-Since", "not a date"))
		if etag := h.header.Get("ETag"); etag != "" {
			out = append(out, r.with("If-None-Match", `"other"`).with("If-Modified-Since", lm))
		}
	}
	return out
}

// ranges ask a sized answer for parts of its body, satisfiable or not, and
// with an If-Range that holds and one that does not.
func ranges(r request, h response) []request {
	if h.status != 200 || h.header.Get("Accept-Ranges") != "bytes" {
		return nil
	}
	size := h.header.Get("Content-Length")
	out := []request{
		r.with("Range", "bytes=0-0"),
		r.with("Range", "bytes=1-"),
		r.with("Range", "bytes=-3"),
		r.with("Range", "bytes=0-99999999"),
		r.with("Range", "bytes="+size+"-"),
		r.with("Range", "bytes=-0"),
		r.with("Range", "bytes=0-1,3-4"),
		r.with("Range", "bytes=5-1"),
		r.with("Range", "lines=0-1"),
		r.with("Range", "bytes=0-1").with("If-Range", `"other"`),
		{method: "HEAD", host: r.host, target: r.target, header: r.with("Range", "bytes=1-").header},
	}
	if etag := h.header.Get("ETag"); etag != "" {
		out = append(out, r.with("Range", "bytes=0-1").with("If-Range", etag))
	}
	return out
}

// checkTarget compares every variant of target and, for an answer with
// validators, its revalidations and byte ranges.
func (w *world) checkTarget(t *testing.T, host, target string) {
	t.Helper()
	for _, r := range variants(host, target) {
		h := w.check(t, r)
		if r.method == "GET" {
			rs := ranges(r, h)
			rangeChecks.Add(int64(len(rs)))
			for _, c := range slices.Concat(conditionals(r, h), rs) {
				w.check(t, c)
			}
		}
	}
}

// rangeChecks counts range requests compared, so a corpus that stops
// recording sizes cannot silently stop exercising them.
var rangeChecks atomic.Int64

func testReads(t *testing.T, w *world) {
	t.Cleanup(func() {
		if rangeChecks.Load() == 0 {
			t.Error("no read answered with Accept-Ranges, so no range was compared")
		}
	})
	for _, sc := range w.slugs {
		t.Run(sc.label, func(t *testing.T) {
			t.Parallel()
			host := w.host(sc.slug)
			for _, target := range slices.Concat(genericTargets, sc.targets) {
				w.checkTarget(t, host, target)
			}
		})
	}
	t.Run("hosts", func(t *testing.T) {
		t.Parallel()
		slug := w.slugs[0].slug
		hosts := []string{w.apex, w.host(slug) + ".", upper(w.host(slug))}
		if w.local {
			// A deployed environment's DNS need not route these to hostthisd.
			hosts = append(hosts, "www."+w.apex, "a.b."+w.apex, "abc."+w.apex, "ABCDEFG1."+w.apex, w.host(slug)+":80")
		}
		targets := []string{"/", "/x", "/healthz", "/readyz", "/_hostthis/md.js", "/p/" + slug, "/p/" + slug + "/", "/p/" + slug + "/x", "/p/nope", "/p/"}
		for _, host := range hosts {
			for _, target := range targets {
				w.checkTarget(t, host, target)
			}
		}
	})
	t.Run("legacy sizes", func(t *testing.T) {
		t.Parallel()
		testLegacy(t, w)
	})
	t.Run("assets", func(t *testing.T) {
		t.Parallel()
		for _, name := range assetNames(t) {
			w.checkTarget(t, w.apex, "/_hostthis/"+name)
			w.check(t, get(w.host(w.slugs[0].slug), "/_hostthis/"+name+"?v=1"))
		}
	})
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}

// assetNames lists every renderer asset web/shells/shells.json names.
func assetNames(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "web", "shells", "shells.json"))
	if err != nil {
		t.Fatal(err)
	}
	var table struct {
		Shells map[string]struct {
			Assets map[string]string `json:"assets"`
		} `json:"shells"`
	}
	if err := json.Unmarshal(raw, &table); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sh := range table.Shells {
		for name := range sh.Assets {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return names
}
