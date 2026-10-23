package ssh_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// cssAccount is an account and pod on a Community Solid Server, driven through
// its JSON account API the way its login pages drive it.
type cssAccount struct {
	t        *testing.T
	base     string
	client   *http.Client
	email    string
	password string
	webid    string
	pod      string
}

func (a *cssAccount) call(method, u string, body any) map[string]any {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, rd)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := a.client.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		a.t.Fatalf("CSS %s %s: %d %s", method, u, res.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (a *cssAccount) control(path ...string) string {
	a.t.Helper()
	node := any(a.call(http.MethodGet, a.base+".account/", nil)["controls"])
	for _, p := range path {
		node = node.(map[string]any)[p]
	}
	s, _ := node.(string)
	if s == "" {
		a.t.Fatalf("CSS control %v missing", path)
	}
	return s
}

func newCSSAccount(t *testing.T, base string) *cssAccount {
	jar, _ := cookiejar.New(nil)
	a := &cssAccount{
		t: t, base: base, email: fmt.Sprintf("hostthis-%d@example.test", time.Now().UnixNano()), password: randHex(12),
		client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	a.call(http.MethodPost, base+".account/account/", map[string]any{})
	a.call(http.MethodPost, a.control("password", "create"), map[string]string{"email": a.email, "password": a.password})
	pod := a.call(http.MethodPost, a.control("account", "pod"), map[string]string{"name": "h" + randHex(4)})
	a.webid, _ = pod["webId"].(string)
	a.pod, _ = pod["pod"].(string)
	return a
}

// follow walks redirects with the account's cookies until one leaves for
// stop, and returns that URL.
func (a *cssAccount) follow(u, stop string) string {
	a.t.Helper()
	for range 12 {
		if strings.HasPrefix(u, stop) {
			return u
		}
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("Accept", "text/html")
		res, err := a.client.Do(req)
		if err != nil {
			a.t.Fatal(err)
		}
		_ = res.Body.Close()
		loc := res.Header.Get("Location")
		if loc == "" {
			return u
		}
		next, _ := url.Parse(u)
		u = next.ResolveReference(&url.URL{Path: ""}).ResolveReference(mustParse(loc)).String()
	}
	a.t.Fatal("too many redirects")
	return ""
}

func mustParse(s string) *url.URL {
	u, _ := url.Parse(s)
	return u
}

// login signs in at CSS for an authorization request and returns the
// redirect to hostthis's callback.
func (a *cssAccount) login(authURL, callback string) string {
	a.t.Helper()
	a.follow(authURL, callback)
	steps := []struct {
		control []string
		body    any
	}{
		{[]string{"password", "login"}, map[string]any{"email": a.email, "password": a.password, "remember": false}},
		{[]string{"oidc", "webId"}, map[string]any{"webId": a.webid, "remember": false}},
		{[]string{"oidc", "consent"}, map[string]any{"remember": false}},
	}
	var last string
	for _, step := range steps {
		res := a.call(http.MethodPost, a.control(step.control...), step.body)
		if loc, _ := res["location"].(string); loc != "" {
			last = a.follow(loc, callback)
		}
	}
	if !strings.HasPrefix(last, callback) {
		a.t.Fatalf("CSS login ended at %q, not the callback", last)
	}
	return last
}

// The pod flow against a real Community Solid Server, when one is running at
// HOSTTHIS_TEST_CSS (e.g. http://localhost:3555/, in WAC or ACP mode).
func TestPodStorageAgainstCSS(t *testing.T) {
	base := os.Getenv("HOSTTHIS_TEST_CSS")
	if base == "" {
		t.Skip("HOSTTHIS_TEST_CSS not set")
	}
	s := startPodStack(t, false, false)
	acct := newCSSAccount(t, base)

	out, stderr, code := s.run("pod link "+acct.webid, nil)
	if code != 0 {
		t.Fatalf("pod link: %s", stderr)
	}
	linkURL := regexp.MustCompile(`http://\S+/_solid/link/\S+`).FindString(out)
	u, _ := url.Parse(linkURL)
	res, err := noRedirect.PostForm(u.Scheme+"://"+u.Host+u.Path, url.Values{"k": {u.Query().Get("k")}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck
	if res.StatusCode != http.StatusSeeOther {
		b, _ := io.ReadAll(res.Body)
		t.Fatalf("continue: %d %s", res.StatusCode, b)
	}
	callback := acct.login(res.Header.Get("Location"), s.httpURL+"/_solid/callback")
	st, page := httpBody(t, callback)
	if st != 200 || !strings.Contains(page, "Pod linked") {
		t.Fatalf("callback: %d\n%s", st, page)
	}

	out, stderr, code = s.run("", []byte("# hello css\n"))
	if code != 0 {
		t.Fatalf("paste: %s", stderr)
	}
	s.upload.WaitFinalize()
	if st, body := httpBody(t, strings.TrimSpace(out)+"?raw"); st != 200 || body != "# hello css\n" {
		t.Fatalf("paste read: %d %q", st, body)
	}
	big := strings.Repeat("y", 2<<20)
	out, stderr, code = s.run("", siteArchive(t, map[string]string{"index.html": "<p>css</p>", "a.css": "p{}", "big.bin": big}))
	if code != 0 {
		t.Fatalf("site: %s", stderr)
	}
	site := strings.TrimSuffix(strings.TrimSpace(out), "/")
	for path, want := range map[string]string{"/": "<p>css</p>", "/a.css": "p{}", "/big.bin": big} {
		if st, body := httpBody(t, site+path); st != 200 || body != want {
			t.Fatalf("site %s: %d, %d bytes", path, st, len(body))
		}
	}
	slug := site[strings.LastIndex(site, "/")+1:]
	if _, stderr, code := s.run("delete "+slug, nil); code != 0 {
		t.Fatalf("delete: %s", stderr)
	}
	// Nothing outside hostthis/ is reachable with hostthis's identity, and the
	// folder is not public.
	if st, _ := httpBody(t, acct.pod+"hostthis/"); st != http.StatusUnauthorized {
		t.Fatalf("anonymous read of hostthis/: %d, want 401", st)
	}
}
