package ssh_test

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	xssh "golang.org/x/crypto/ssh"

	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

// updateStack is a full real stack (sqlite + blob store + http surface +
// ssh server) with BOTH the paste-manage path and the site-deploy path
// wired, PLUS persistent keyed clients so two sessions from the same
// fixedClient share one owner (the fingerprint of one key). This is what
// the in-place site-update e2e needs: re-deploying to the same slug must
// come from the SAME identity, which the per-call fresh-key helper in
// site_regression_test.go cannot express.
type updateStack struct {
	t       *testing.T
	httpURL string
	sshAddr string
	owner   *xssh.Client // identity A
	other   *xssh.Client // identity B (a different key -> different owner)
}

func newUpdateStack(t *testing.T) *updateStack {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	blobUnit := service.NewStandaloneBlobUnit(blobs)
	repo := storage.NewPasteRepo(db)
	sites := storage.NewSiteRepo(db)

	httpSrv := httptest.NewServer((&httpapi.Server{
		Pastes: repo, Sites: sites, Blobs: blobUnit, ApexDomain: "paste.test",
	}).Handler())
	t.Cleanup(httpSrv.Close)

	l := mustListen(t)
	addr := l.Addr().String()
	_ = l.Close()

	upload := service.NewUpload(repo, blobUnit)
	t.Cleanup(upload.WaitFinalize)
	sshSrv := &hostssh.Server{
		Addr:       addr,
		ApexDomain: "paste.test",
		Upload:     upload,
		Manage:     service.NewManage(repo, blobUnit),
		Pastes:     repo,
		Deploy:     service.NewDeploySite(sites, repo, blobUnit),
		BuildURL:   func(s domain.Slug) string { return httpSrv.URL + "/p/" + s.String() },
		Logger:     log.New(io.Discard, "", 0),
	}
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, addr)

	owner, _ := newKeyClient(t, addr)
	other, _ := newKeyClient(t, addr)
	return &updateStack{t: t, httpURL: httpSrv.URL, sshAddr: addr, owner: owner, other: other}
}

// run drives one ssh command over the given persistent client and returns
// (stdout, stderr, exit). A persistent client keeps the same owner across
// sessions, which is the whole point of the in-place-update tests.
func (s *updateStack) run(cli *xssh.Client, cmd string, body []byte) (string, string, int) {
	s.t.Helper()
	sess, err := cli.NewSession()
	if err != nil {
		s.t.Fatalf("session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	var stdout, stderr bytes.Buffer
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if body != nil {
		sess.Stdin = bytes.NewReader(body)
	}
	exit := 0
	if err := sess.Run(cmd); err != nil {
		var ee *xssh.ExitError
		if asExitErr(err, &ee) {
			exit = ee.ExitStatus()
		} else {
			s.t.Fatalf("run %q: %v\nstderr: %s", cmd, err, stderr.String())
		}
	}
	return stdout.String(), stderr.String(), exit
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(got)
}

// TestSiteUpdateInPlace_SameSlugNewContent is the headline regression for
// the confirmed bug: piping a gzip-tar to UPDATE an existing SITE slug
// returned "service: not found" (exit 4) because the update path
// unconditionally called the PASTE-update service. This pins the spec'd
// behavior: a re-deploy to an OWNED site slug succeeds (exit 0), keeps the
// SAME slug/URL, and serves the NEW content byte-for-byte.
func TestSiteUpdateInPlace_SameSlugNewContent(t *testing.T) {
	st := newUpdateStack(t)

	// 1. Deploy a site -> slug S at URL U.
	v1 := makeSiteArchive(t, map[string]string{
		"index.html":  "<!doctype html><h1>v1</h1>",
		"css/app.css": "body{color:red}",
	})
	out1, err1, exit1 := st.run(st.owner, "", v1)
	if exit1 != 0 {
		t.Fatalf("initial deploy exit %d: stderr %q", exit1, err1)
	}
	base := strings.TrimSpace(out1)
	slug := extractSlug(out1)
	if slug == "" {
		t.Fatalf("could not extract slug from %q", out1)
	}
	if !strings.Contains(err1, "site:") {
		t.Fatalf("initial archive should route to site path, stderr %q", err1)
	}
	// Sanity: v1 content is served.
	if code, body := getBody(t, base); code != 200 || body != "<!doctype html><h1>v1</h1>" {
		t.Fatalf("v1 index before update: code %d body %q", code, body)
	}

	// 2. Re-deploy a DIFFERENT archive to slug S (`tar czf - | ssh apex S`).
	v2 := makeSiteArchive(t, map[string]string{
		"index.html":  "<!doctype html><h1>v2 CHANGED</h1>",
		"css/app.css": "body{color:green}",
		"new.html":    "<h1>brand new file</h1>",
	})
	out2, err2, exit2 := st.run(st.owner, slug, v2)

	// The bug: this used to fail with "service: not found" and exit 4.
	if exit2 != 0 {
		t.Fatalf("in-place site update should succeed, got exit %d stderr %q", exit2, err2)
	}
	if strings.Contains(err2, "not found") {
		t.Fatalf("in-place site update wrongly hit the paste-update not-found path: stderr %q", err2)
	}
	if !strings.Contains(err2, "site:") {
		t.Fatalf("in-place update should report the site path, stderr %q", err2)
	}

	// 3. SAME slug, SAME URL.
	if got := strings.TrimSpace(out2); got != base {
		t.Fatalf("in-place update changed the URL: got %q, want %q (same slug)", got, base)
	}

	// 4. The NEW content is served, byte-identical, at the SAME URL.
	if code, body := getBody(t, base); code != 200 || body != "<!doctype html><h1>v2 CHANGED</h1>" {
		t.Fatalf("index after update: code %d body %q, want v2", code, body)
	}
	if code, body := getBody(t, base+"/css/app.css"); code != 200 || body != "body{color:green}" {
		t.Fatalf("css after update: code %d body %q, want green", code, body)
	}
	// A file added in v2 is now reachable.
	if code, body := getBody(t, base+"/new.html"); code != 200 || body != "<h1>brand new file</h1>" {
		t.Fatalf("new file after update: code %d body %q", code, body)
	}
}

// TestSiteUpdateInPlace_ForeignOwnerNotFound pins ownership: a SECOND
// identity re-deploying to someone else's site slug gets not-found (exit
// 4) and the site is UNCHANGED. Not-found and not-yours collapse to the
// same shape so a non-owner cannot probe existence or ownership.
func TestSiteUpdateInPlace_ForeignOwnerNotFound(t *testing.T) {
	st := newUpdateStack(t)

	// owner deploys a site.
	v1 := makeSiteArchive(t, map[string]string{"index.html": "<!doctype html><h1>mine</h1>"})
	out1, _, exit1 := st.run(st.owner, "", v1)
	if exit1 != 0 {
		t.Fatalf("owner deploy exit %d", exit1)
	}
	base := strings.TrimSpace(out1)
	slug := extractSlug(out1)

	// other (a different key) tries to re-deploy onto owner's slug.
	v2 := makeSiteArchive(t, map[string]string{"index.html": "<!doctype html><h1>hijacked</h1>"})
	_, errOut, exit2 := st.run(st.other, slug, v2)
	if exit2 != hostssh.ExitNotFound {
		t.Fatalf("foreign re-deploy should be not-found (exit %d), got exit %d stderr %q",
			hostssh.ExitNotFound, exit2, errOut)
	}

	// The site is unchanged: still serving owner's original content.
	if code, body := getBody(t, base); code != 200 || body != "<!doctype html><h1>mine</h1>" {
		t.Fatalf("site after rejected foreign re-deploy: code %d body %q, want unchanged", code, body)
	}
}

// TestPasteUpdateSurvivesGzipPeek is the no-regression guard for the fix:
// the gzip-magic peek added to the UPDATE path must not break a normal
// HTML paste update. A plain-HTML re-upload to a PASTE slug still bumps
// the paste version and serves the new body.
func TestPasteUpdateSurvivesGzipPeek(t *testing.T) {
	st := newUpdateStack(t)

	// Create a paste (plain HTML, no slug).
	out1, err1, exit1 := st.run(st.owner, "", []byte("<!doctype html><p>v1 paste</p>"))
	if exit1 != 0 {
		t.Fatalf("create paste exit %d: stderr %q", exit1, err1)
	}
	if strings.Contains(err1, "site:") {
		t.Fatalf("plain HTML wrongly routed to site path: stderr %q", err1)
	}
	base := strings.TrimSpace(out1)
	slug := extractSlug(out1)

	// Update the SAME paste slug with new HTML. The peek sees no gzip magic
	// and must fall through to the paste-update path unchanged.
	out2, err2, exit2 := st.run(st.owner, slug, []byte("<!doctype html><p>v2 paste UPDATED</p>"))
	if exit2 != 0 {
		t.Fatalf("paste update should succeed, got exit %d stderr %q", exit2, err2)
	}
	if !strings.Contains(err2, "saved") {
		t.Fatalf("paste update should report a saved version, stderr %q", err2)
	}
	if got := strings.TrimSpace(out2); got != base {
		t.Fatalf("paste update changed the URL: got %q want %q", got, base)
	}
	if code, body := getBody(t, base); code != 200 || body != "<!doctype html><p>v2 paste UPDATED</p>" {
		t.Fatalf("paste body after update: code %d body %q", code, body)
	}
}

// TestNewSiteUploadStillWorks is the second no-regression guard: uploading
// a NEW site (no slug positional) still mints a fresh random slug via the
// create path, unaffected by the new update-path routing.
func TestNewSiteUploadStillWorks(t *testing.T) {
	st := newUpdateStack(t)
	arc := makeSiteArchive(t, map[string]string{
		"index.html": "<!doctype html><h1>fresh site</h1>",
	})
	out, errOut, exit := st.run(st.owner, "", arc)
	if exit != 0 {
		t.Fatalf("new site upload exit %d: stderr %q", exit, errOut)
	}
	if !strings.Contains(errOut, "site:") {
		t.Fatalf("new site upload should route to site path, stderr %q", errOut)
	}
	base := strings.TrimSpace(out)
	if !strings.HasPrefix(base, st.httpURL+"/p/") {
		t.Fatalf("new site upload should return a fresh /p/ URL, got %q", base)
	}
	if code, body := getBody(t, base); code != 200 || body != "<!doctype html><h1>fresh site</h1>" {
		t.Fatalf("new site index: code %d body %q", code, body)
	}
}
