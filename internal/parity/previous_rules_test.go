package parity

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeETag = `"uploads/u/0"`

var fakeModified = time.Date(2026, 6, 7, 14, 0, 0, 0, time.UTC)

const frontDoor400 = "<html><head><title>400 Bad Request</title></head><body><h1>400 Bad Request</h1></body></html>\n"

func refuse(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = io.WriteString(w, frontDoor400)
}

// page answers /page with cacheControl, deciding a revalidation as the
// previous release did (If-Modified-Since honored beside a tag that does not
// match) or as the current one does.
func page(w http.ResponseWriter, r *http.Request, cacheControl string, previous bool) {
	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set("ETag", fakeETag)
	h.Set("Last-Modified", fakeModified.Format(http.TimeFormat))
	h.Set("X-Frame-Options", "DENY")
	inm := r.Header.Get("If-None-Match")
	ims, err := http.ParseTime(r.Header.Get("If-Modified-Since"))
	dated := err == nil && !ims.Before(fakeModified)
	if inm == fakeETag || (dated && (inm == "" || previous)) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "page body")
}

func body(w http.ResponseWriter, cacheControl, text string) {
	w.Header().Set("Cache-Control", cacheControl)
	_, _ = io.WriteString(w, text)
}

// previousOrigin is the previous hostthisd release behind an ingress that
// cleans a path before hostthisd sees it.
func previousOrigin(w http.ResponseWriter, r *http.Request) {
	switch r.RequestURI {
	case "/page", "//page", "/%2e%2e/page":
		page(w, r, "public, max-age=3600", true)
	case "/broken":
		h := w.Header()
		h.Set("Cache-Control", "public, max-age=3600")
		h.Set("ETag", fakeETag)
		h.Set("Last-Modified", fakeModified.Format(http.TimeFormat))
		h.Set("X-Frame-Options", "DENY")
		http.Error(w, "internal error", http.StatusInternalServerError)
	case "/differs":
		body(w, "public, no-cache", "previous body")
	case "/status":
		http.NotFound(w, r)
	case "/cc":
		body(w, "public, max-age=60", "page body")
	case "/refused":
		body(w, "public, no-cache", "page body")
	case "/refused-both":
		http.Error(w, "400 Bad Request", http.StatusBadRequest)
	default:
		http.NotFound(w, r)
	}
}

// currentWorker is the Worker behind a front door that refuses dot segments.
func currentWorker(w http.ResponseWriter, r *http.Request) {
	switch r.RequestURI {
	case "/page":
		page(w, r, "public, no-cache", false)
	case "//page":
		w.Header().Set("Location", "/page")
		w.WriteHeader(http.StatusTemporaryRedirect)
	case "/%2e%2e/page", "/refused", "/refused-both":
		refuse(w)
	case "/broken":
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "internal error", http.StatusInternalServerError)
	case "/differs":
		body(w, "public, no-cache", "current body")
	case "/status", "/cc":
		body(w, "public, no-cache", "page body")
	default:
		http.NotFound(w, r)
	}
}

// Each previous-release rule explains exactly its change, and a status, body
// or header difference outside them still fails.
func TestParityPreviousReleaseRules(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(previousOrigin))
	t.Cleanup(old.Close)
	cur := httptest.NewServer(http.HandlerFunc(currentWorker))
	t.Cleanup(cur.Close)
	w := &world{
		apex:      "example.test",
		previous:  true,
		hostthisd: &server{name: "hostthisd", via: viaHostthisd, addr: old.Listener.Addr().String()},
		workers:   []*server{{name: "worker", via: viaDirect, addr: cur.Listener.Addr().String()}},
	}
	host := w.host("abcdefgh")
	lm := fakeModified.Format(http.TimeFormat)
	cases := []struct {
		name string
		r    request
		diff string // a difference the run reports; "" for none
	}{
		{"browser cache posture", get(host, "/page"), ""},
		{"a date beside a tag that does not match", get(host, "/page").with("If-None-Match", `"other"`).with("If-Modified-Since", lm), ""},
		{"a tag that matches", get(host, "/page").with("If-None-Match", fakeETag), ""},
		{"a bare 5xx", get(host, "/broken"), ""},
		{"a doubled slash the ingress cleaned", get(host, "//page"), ""},
		{"dot segments the front door refused", get(host, "/%2e%2e/page"), ""},
		{"a request both refuse", get(host, "/refused-both"), ""},
		{"a body", get(host, "/differs"), "body:"},
		{"a status", get(host, "/status"), "status:"},
		{"a Cache-Control the release did not change", get(host, "/cc"), "Cache-Control:"},
		{"a refusal with no dot segments", get(host, "/refused"), "status:"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			diffs := compareOnce(t, w, c.r)
			joined := strings.Join(diffs, "\n")
			switch {
			case c.diff == "" && len(diffs) > 0:
				t.Fatalf("reported:\n%s", joined)
			case c.diff != "" && !strings.Contains(joined, c.diff):
				t.Fatalf("want a %q difference, got:\n%s", c.diff, joined)
			}
		})
	}
	t.Run("off by default", func(t *testing.T) {
		w.previous = false
		t.Cleanup(func() { w.previous = true })
		if diffs := compareOnce(t, w, get(host, "/page")); !strings.Contains(strings.Join(diffs, "\n"), "Cache-Control:") {
			t.Fatalf("a run against the same release explained the posture change: %q", diffs)
		}
	})
}

func compareOnce(t *testing.T, w *world, r request) []string {
	t.Helper()
	h, err := w.hostthisd.do(r)
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.workers[0].do(r)
	if err != nil {
		t.Fatal(err)
	}
	return w.compare(r, w.workers[0], h, got)
}

// PARITY_REFERENCE=previous turns the rules on; unset leaves them off.
func TestParityReferenceConfig(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PARITY_HOSTTHISD_URL", "https://example.test")
	t.Setenv("PARITY_WORKER_URL", "https://worker.example.test")
	t.Setenv("PARITY_TOKEN_FILE", token)
	t.Setenv("PARITY_SLUGS", "abcdefgh")
	for ref, want := range map[string]bool{"": false, "previous": true} {
		t.Setenv("PARITY_REFERENCE", ref)
		cfg, ok := readExternalConfig(t)
		if !ok || cfg.previous != want {
			t.Errorf("PARITY_REFERENCE=%q: previous %v, want %v", ref, cfg.previous, want)
		}
	}
}
