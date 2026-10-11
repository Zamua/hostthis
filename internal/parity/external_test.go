package parity

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// externalConfig is a deployed environment to compare, from the environment:
//
//	PARITY_HOSTTHISD_URL  hostthisd's public URL, scheme://<apex>[:port]; reads
//	                      go to <slug>.<apex>
//	PARITY_WORKER_URL     the Worker's internal endpoint, reached in forwarded
//	                      mode
//	PARITY_TOKEN_FILE     a file holding the Worker's token
//	PARITY_SLUGS          the slugs to read, separated by commas or spaces
//	PARITY_ALLOW_WRITES   1 to run the room and socket checks, which write
//	PARITY_WRITE_SLUG     the dedicated test paste those writes are confined to
//
// Without PARITY_ALLOW_WRITES=1 the run only reads.
type externalConfig struct {
	hostthisd *url.URL
	worker    *url.URL
	token     string
	slugs     []string
	writeSlug string
}

func readExternalConfig(t *testing.T) (externalConfig, bool) {
	t.Helper()
	raw := os.Getenv("PARITY_HOSTTHISD_URL")
	if raw == "" {
		return externalConfig{}, false
	}
	var cfg externalConfig
	var err error
	if cfg.hostthisd, err = url.Parse(raw); err != nil || cfg.hostthisd.Hostname() == "" {
		t.Fatalf("PARITY_HOSTTHISD_URL %q is not scheme://host[:port]", raw)
	}
	if cfg.worker, err = url.Parse(os.Getenv("PARITY_WORKER_URL")); err != nil || cfg.worker.Host == "" {
		t.Fatal("PARITY_WORKER_URL must name the Worker's internal endpoint")
	}
	tokenFile := os.Getenv("PARITY_TOKEN_FILE")
	if tokenFile == "" {
		t.Fatal("PARITY_TOKEN_FILE must name a file holding the Worker's token")
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatalf("PARITY_TOKEN_FILE: %v", err)
	}
	cfg.token = strings.TrimSpace(string(b))
	cfg.slugs = strings.FieldsFunc(os.Getenv("PARITY_SLUGS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	})
	if len(cfg.slugs) == 0 {
		t.Fatal("PARITY_SLUGS must name at least one slug to read")
	}
	for _, s := range cfg.slugs {
		if !slugPattern.MatchString(s) {
			t.Fatalf("PARITY_SLUGS: %q is not a slug", s)
		}
	}
	if os.Getenv("PARITY_ALLOW_WRITES") == "1" {
		cfg.writeSlug = os.Getenv("PARITY_WRITE_SLUG")
		if !slugPattern.MatchString(cfg.writeSlug) {
			t.Fatal("PARITY_ALLOW_WRITES=1 needs PARITY_WRITE_SLUG naming the test paste")
		}
	}
	return cfg, true
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// externalWorld compares hostthisd at its public hosts with the Worker in
// forwarded mode. Nothing it does writes outside the test paste.
func externalWorld(t *testing.T, cfg externalConfig) *world {
	t.Helper()
	w := &world{
		apex: strings.ToLower(cfg.hostthisd.Hostname()),
		hostthisd: &server{
			name: "hostthisd", via: viaHostthisd, port: port(cfg.hostthisd), tls: cfg.hostthisd.Scheme == "https",
		},
		workers: []*server{{
			name: "worker (forwarded)", via: viaForwarded, token: cfg.token,
			addr: net.JoinHostPort(cfg.worker.Hostname(), port(cfg.worker)), tls: cfg.worker.Scheme == "https",
		}},
		writable: cfg.writeSlug != "",
		writeApp: cfg.writeSlug,
	}
	for _, slug := range cfg.slugs {
		w.slugs = append(w.slugs, slugCase{slug: slug, label: slug, targets: w.discover(t, slug)})
	}
	return w
}

// A same-origin reference in a page: a path, not a scheme or another host.
var pageRef = regexp.MustCompile(`(?:src|href)="(/?[A-Za-z0-9._~%!$&'()*+,;=:@/-]+)"`)

const maxDiscovered = 200

// discover finds the paths a deployed paste serves beyond its root: a
// knowledge base's file list, and the same-origin references in its root
// page.
func (w *world) discover(t *testing.T, slug string) []string {
	t.Helper()
	var targets []string
	add := func(target string) {
		if len(targets) < maxDiscovered && !slices.Contains(targets, target) {
			targets = append(targets, target)
		}
	}
	if res, err := w.hostthisd.do(get(w.host(slug), "/?files=1")); err == nil && res.status == 200 {
		var paths []string
		if json.Unmarshal(res.body, &paths) == nil {
			for _, p := range paths {
				add(escapeTarget(strings.TrimPrefix(p, "/")))
				add(escapeTarget(strings.TrimPrefix(p, "/")) + "?raw=1")
			}
		}
	}
	if res, err := w.hostthisd.do(get(w.host(slug), "/")); err == nil && res.status == 200 &&
		strings.HasPrefix(res.header.Get("Content-Type"), "text/html") {
		for _, m := range pageRef.FindAllStringSubmatch(string(res.body), -1) {
			ref := m[1]
			if strings.Contains(ref, ":") || strings.HasPrefix(ref, "//") || strings.HasPrefix(ref, "/_hostthis/") {
				continue
			}
			add("/" + strings.TrimPrefix(strings.TrimPrefix(ref, "./"), "/"))
		}
	}
	return targets
}
