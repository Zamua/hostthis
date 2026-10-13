package workertest

import (
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func get(t *testing.T, base, host, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest("GET", base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	maps.Copy(req.Header, header)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, string(body)
}

// The public surface answers hosts under the apex, with the static assets and
// the oversized asset in PAYLOADS beside it, while the listener's own host
// stays the token-gated internal router.
func TestStartPublicServesBothRouters(t *testing.T) {
	rt := StartPublic(t, Public{Apex: "public.test"})

	res, body := get(t, rt.URL, "abcdefgh.public.test", "/healthz", nil)
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "no-store" || body != "ok\n" {
		t.Errorf("public /healthz = %d %q %q", res.StatusCode, res.Header.Get("Cache-Control"), body)
	}
	res, body = get(t, rt.URL, "abcdefgh.public.test", "/", nil)
	if res.StatusCode != 404 || body != "404 page not found\n" {
		t.Errorf("unknown slug = %d %q", res.StatusCode, body)
	}
	res, _ = get(t, rt.URL, "public.test", "/_hostthis/md.js", nil)
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Errorf("static asset = %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res, body = get(t, rt.URL, "public.test", "/_hostthis/duckdb-eh.wasm", nil)
	if res.StatusCode != 200 || !strings.HasPrefix(body, "\x00asm") {
		t.Errorf("oversized asset = %d, %d bytes", res.StatusCode, len(body))
	}

	res, _ = get(t, rt.URL, "", "/paste/exists?slug=abcdefgh", nil)
	if res.StatusCode != 401 {
		t.Errorf("internal route without the token = %d, want 401", res.StatusCode)
	}
	res, body = get(t, rt.URL, "", "/paste/exists?slug=abcdefgh",
		http.Header{"Authorization": {"Bearer " + rt.Token}})
	if res.StatusCode != 200 || !strings.Contains(body, "false") {
		t.Errorf("internal route with the token = %d %q", res.StatusCode, body)
	}
}

// The shared runtimes never take the public surface from the environment.
func TestSharedRuntimesDropThePublicVariables(t *testing.T) {
	t.Setenv("HOSTTHIS_LOCALRT_APEX_DOMAIN", "public.test")
	if slices.ContainsFunc(runtimeEnv(), func(kv string) bool { return strings.HasPrefix(kv, "HOSTTHIS_LOCALRT_") }) {
		t.Fatal("runtimeEnv passes HOSTTHIS_LOCALRT_* on")
	}
	res, _ := get(t, strings.TrimSuffix(Endpoint(t), "/"), "abcdefgh.public.test", "/_hostthis/md.js", nil)
	if res.StatusCode == 200 {
		t.Fatal("the shared runtime serves the public surface")
	}
}
