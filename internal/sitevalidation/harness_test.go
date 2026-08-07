// Package sitevalidation is the test-only byte-identical validation harness for
// static-site hosting. It deploys real framework-built site fixtures through the
// SAME archive pipeline an ssh tar upload hits (service.DeploySite over a real
// metadata repo and compressed blob store), fetches every built file back over the
// real HTTP serving surface, and asserts the served bytes are BYTE-IDENTICAL to
// the fixture with the content type the extension implies.
//
// Fixtures are committed build outputs under testdata/sitefixtures/<demo>/dist
// (see that dir's README). The harness never runs npm, so `go test ./...`
// exercises the full deploy + serve round-trip with no Node toolchain.
package sitevalidation

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

const apexDomain = "paste.test"

// fixtureDist resolves the absolute path to a fixture's dist/ directory. Tests
// run with the package dir as CWD, so fixtures are two levels up under
// testdata/.
func fixtureDist(t *testing.T, demo string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sitefixtures", demo, "dist"))
	if err != nil {
		t.Fatalf("resolve fixture %q: %v", demo, err)
	}
	if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
		t.Fatalf("fixture dist not found at %s (err=%v) - run `make rebuild-site-fixtures`?", p, err)
	}
	return p
}

// distFile is one file in a fixture's dist/ tree. relPath is both the manifest
// key and the URL path.
type distFile struct {
	relPath string // e.g. "index.html", "assets/index-abc123.js"
	body    []byte
}

// readDist walks dist/ and returns every regular file under its
// site-root-relative forward-slash path, the exact key the archive pipeline
// puts in the manifest. Sorted for deterministic iteration.
func readDist(t *testing.T, distDir string) []distFile {
	t.Helper()
	var files []distFile
	err := filepath.Walk(distDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(distDir, p)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, distFile{
			relPath: filepath.ToSlash(rel),
			body:    body,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk dist %s: %v", distDir, err)
	}
	if len(files) == 0 {
		t.Fatalf("fixture dist %s is empty", distDir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].relPath < files[j].relPath })
	return files
}

// tarDist packs the dist files into the gzip-tar shape a
// `tar czf - dist/ | ssh hostthis.dev` upload streams in: regular files at
// site-root-relative paths.
func tarDist(t *testing.T, files []distFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		hdr := &tar.Header{
			Name:     f.relPath,
			Mode:     0o644,
			Size:     int64(len(f.body)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %q: %v", f.relPath, err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatalf("tar body %q: %v", f.relPath, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// deployedSite is a live site: the slug it landed under plus a real HTTP
// server in front of the same repos it was deployed into.
type deployedSite struct {
	slug    domain.Slug
	httpSrv *httptest.Server
}

// get issues a request whose Host is "<slug>.paste.test", so the server's
// subdomain routing peels the slug exactly as in production.
func (d deployedSite) get(t *testing.T, urlPath string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", d.httpSrv.URL+urlPath, nil)
	if err != nil {
		t.Fatalf("new request %q: %v", urlPath, err)
	}
	req.Host = d.slug.String() + "." + apexDomain
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %q: %v", urlPath, err)
	}
	return resp
}

// deployFixture stands up real metadata repo, compressed-blob and site repos, deploys
// the fixture's dist/ through the production DeploySite use case, and fronts
// those same repos with the real HTTP handler.
func deployFixture(t *testing.T, demo string) ([]distFile, deployedSite) {
	t.Helper()
	dir := t.TempDir()

	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	pastes := storagetest.NewRepo(t)
	sites := storage.NewShaleSiteRepo(storagetest.NewRepo(t))
	deploy := service.NewDeploySite(sites, pastes, blobUnit)

	files := readDist(t, fixtureDist(t, demo))
	arc := tarDist(t, files)

	res, err := deploy.Deploy(bytes.NewReader(arc), "key:fixture")
	if err != nil {
		t.Fatalf("deploy %s fixture: %v", demo, err)
	}

	httpSrv := httptest.NewServer((&httpapi.Server{
		Pastes:     pastes,
		Sites:      sites,
		Blobs:      blobUnit,
		ApexDomain: apexDomain,
	}).Handler())
	t.Cleanup(httpSrv.Close)

	return files, deployedSite{slug: res.Site.Slug, httpSrv: httpSrv}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

func indexBytes(t *testing.T, files []distFile) []byte {
	t.Helper()
	for _, f := range files {
		if f.relPath == "index.html" {
			return f.body
		}
	}
	t.Fatalf("fixture has no root index.html")
	return nil
}

// assertByteIdentical is the core of the harness: deploy a fixture, then assert
// every built file round-trips byte-identically with the content type its
// extension implies, "/" serves the root index.html, and a missing asset 404s.
// Callers layer the SPA-route assertion on per demo.
func assertByteIdentical(t *testing.T, demo string) ([]distFile, deployedSite) {
	t.Helper()
	files, site := deployFixture(t, demo)

	for _, f := range files {
		t.Run("file/"+f.relPath, func(t *testing.T) {
			resp := site.get(t, "/"+f.relPath)
			got := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("GET /%s: status %d, want 200", f.relPath, resp.StatusCode)
			}
			if !bytes.Equal(got, f.body) {
				t.Fatalf("GET /%s: served bytes differ from fixture (got %d bytes, want %d)",
					f.relPath, len(got), len(f.body))
			}
			wantCT := domain.ContentTypeForPath(f.relPath)
			if ct := resp.Header.Get("Content-Type"); ct != wantCT {
				t.Fatalf("GET /%s content-type: got %q, want %q", f.relPath, ct, wantCT)
			}
			// Sandbox headers per the site-serving contract.
			if resp.Header.Get("X-Frame-Options") != "DENY" {
				t.Fatalf("GET /%s: missing sandbox header X-Frame-Options", f.relPath)
			}
		})
	}

	t.Run("root", func(t *testing.T) {
		resp := site.get(t, "/")
		got := readBody(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf(`GET "/": status %d, want 200`, resp.StatusCode)
		}
		if !bytes.Equal(got, indexBytes(t, files)) {
			t.Fatalf(`GET "/": did not serve the root index.html bytes`)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf(`GET "/" content-type: got %q`, ct)
		}
	})

	// A genuinely missing asset 404s: the SPA fallback must not serve
	// index.html under a .js URL.
	t.Run("missing-asset-404", func(t *testing.T) {
		resp := site.get(t, "/assets/nope.js")
		_ = readBody(t, resp)
		if resp.StatusCode != 404 {
			t.Fatalf("GET /assets/nope.js: status %d, want 404", resp.StatusCode)
		}
	})

	return files, site
}

// assertSPAFallback asserts a deep client-side route (not a real file) serves
// the root index.html via the SPA fallback.
func assertSPAFallback(t *testing.T, files []distFile, site deployedSite) {
	t.Helper()
	wantIndex := indexBytes(t, files)
	for _, route := range []string{"/about", "/users/123"} {
		t.Run("spa-route/"+route, func(t *testing.T) {
			resp := site.get(t, route)
			got := readBody(t, resp)
			if resp.StatusCode != 200 {
				t.Fatalf("GET %s: status %d, want 200 (SPA fallback)", route, resp.StatusCode)
			}
			if !bytes.Equal(got, wantIndex) {
				t.Fatalf("GET %s: did not serve the root index.html via SPA fallback", route)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
				t.Fatalf("GET %s content-type: got %q, want text/html", route, ct)
			}
		})
	}
}

func TestReactSPA_ByteIdentical(t *testing.T) {
	files, site := assertByteIdentical(t, "react")
	assertSPAFallback(t, files, site)
}

func TestVueSPA_ByteIdentical(t *testing.T) {
	files, site := assertByteIdentical(t, "vue")
	assertSPAFallback(t, files, site)
}

func TestSvelteSPA_ByteIdentical(t *testing.T) {
	files, site := assertByteIdentical(t, "svelte")
	assertSPAFallback(t, files, site)
}

// TestPlainStatic_ByteIdentical runs the round-trip for a hand-written
// multi-page site and additionally pins that /about.html is a DIRECT manifest
// hit, not the SPA fallback: both would be 200 + text/html, so only the body
// distinguishes them.
func TestPlainStatic_ByteIdentical(t *testing.T) {
	files, site := assertByteIdentical(t, "plain-static")

	var aboutBody []byte
	for _, f := range files {
		if f.relPath == "about.html" {
			aboutBody = f.body
		}
	}
	if aboutBody == nil {
		t.Fatalf("plain-static fixture missing about.html")
	}
	resp := site.get(t, "/about.html")
	got := readBody(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("GET /about.html: status %d, want 200", resp.StatusCode)
	}
	if !bytes.Equal(got, aboutBody) {
		t.Fatalf("GET /about.html: served bytes are not the about.html fixture")
	}
	if bytes.Equal(got, indexBytes(t, files)) {
		t.Fatalf("GET /about.html: served the index, not the real about.html file")
	}
}

// TestFixtureSnapshot_Matches hashes every fixture file against
// testdata/sitefixtures/SHA256SUMS. The round-trip tests only prove the pipeline
// preserves whatever dist/ is on disk; this proves those committed bytes are the
// intended ones, and fails if a fixture file is added or removed without
// regenerating the manifest (`make rebuild-site-fixtures`).
func TestFixtureSnapshot_Matches(t *testing.T) {
	sumsPath, err := filepath.Abs(filepath.Join("..", "..", "testdata", "sitefixtures", "SHA256SUMS"))
	if err != nil {
		t.Fatalf("resolve SHA256SUMS: %v", err)
	}
	f, err := os.Open(sumsPath)
	if err != nil {
		t.Fatalf("open SHA256SUMS (run `make rebuild-site-fixtures`?): %v", err)
	}
	defer func() { _ = f.Close() }()

	// Manifest lines are "<hex-sha>  <demo>/<rel-path>".
	want := make(map[string]string) // "<demo>/<rel>" -> sha
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 {
			t.Fatalf("malformed SHA256SUMS line: %q", line)
		}
		want[parts[1]] = parts[0]
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read SHA256SUMS: %v", err)
	}
	if len(want) == 0 {
		t.Fatalf("SHA256SUMS is empty")
	}

	have := make(map[string]string)
	for _, demo := range []string{"react", "vue", "svelte", "plain-static"} {
		distDir := fixtureDist(t, demo)
		for _, df := range readDist(t, distDir) {
			key := demo + "/" + df.relPath
			sum := sha256.Sum256(df.body)
			have[key] = hex.EncodeToString(sum[:])
		}
	}

	for key, gotSHA := range have {
		wantSHA, ok := want[key]
		if !ok {
			t.Errorf("fixture file %s is not in SHA256SUMS (regenerate with `make rebuild-site-fixtures`)", key)
			continue
		}
		if gotSHA != wantSHA {
			t.Errorf("fixture file %s: sha mismatch\n  on disk:  %s\n  manifest: %s", key, gotSHA, wantSHA)
		}
	}
	for key := range want {
		if _, ok := have[key]; !ok {
			t.Errorf("SHA256SUMS lists %s but it is missing from the fixture tree", key)
		}
	}
}

// TestFixtureManifest_ExpectedFiles pins the shape of each committed fixture, so
// a corruption such as a missing index.html is caught independently of the
// round-trip.
func TestFixtureManifest_ExpectedFiles(t *testing.T) {
	cases := map[string][]string{
		"react":  {"index.html"},
		"vue":    {"index.html"},
		"svelte": {"index.html"},
		"plain-static": {
			"about.html", "css/app.css", "index.html", "js/app.js",
		},
	}
	for demo, mustHave := range cases {
		t.Run(demo, func(t *testing.T) {
			files := readDist(t, fixtureDist(t, demo))
			have := make(map[string]bool, len(files))
			for _, f := range files {
				have[f.relPath] = true
			}
			for _, want := range mustHave {
				if !have[want] {
					t.Fatalf("fixture %s missing expected file %q", demo, want)
				}
			}
			// A hashed JS bundle under assets/ is the proof a framework demo
			// is a real build, not hand-faked HTML.
			if demo != "plain-static" {
				var hasAssetJS bool
				for _, f := range files {
					if strings.HasPrefix(f.relPath, "assets/") && strings.HasSuffix(f.relPath, ".js") {
						hasAssetJS = true
					}
				}
				if !hasAssetJS {
					t.Fatalf("framework fixture %s has no assets/*.js bundle", demo)
				}
			}
		})
	}
}
