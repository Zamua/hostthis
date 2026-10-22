package ssh_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	httpapi "github.com/Zamua/hostthis/internal/http"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/worker"
	"github.com/Zamua/hostthis/internal/workertest"
)

func solidKeyJWK(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	point, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	d, err := key.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	raw, _ := json.Marshal(map[string]string{
		"kty": "EC", "crv": "P-256", "kid": "test",
		"x": enc(point[1:33]), "y": enc(point[33:]), "d": enc(d),
	})
	return string(raw)
}

// podStack is a deployment whose Worker has pod support, with its apex
// /_solid/ paths served through hostthisd as in production.
type podStack struct {
	*stack
	pod *fakePod
}

func startPodStack(t *testing.T, acp bool) *podStack {
	t.Helper()
	pod := newFakePod(t, acp)

	// The public origin is known before the Worker starts, so the WebID it
	// signs for is the one hostthisd serves.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://localhost:" + fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
	rt := workertest.StartPublic(t, workertest.Public{
		Apex: "localhost", URLMode: "path", Scheme: "http",
		SolidKey: solidKeyJWK(t), SolidOrigin: origin,
	})
	authed, err := worker.Authorize(nil, rt.URL, rt.Token)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: authed}
	rawBlobs, err := storage.NewWorkerBlobStore(rt.URL, client)
	if err != nil {
		t.Fatal(err)
	}
	blobUnit := storage.NewCompressedBlobStore(rawBlobs)
	repo := worker.NewPasteRepo(rt.URL, client)
	pods := service.NewPods(repo)
	upload := service.NewUpload(repo, blobUnit)
	upload.UploadIDs = pods.UploadID
	t.Cleanup(upload.WaitFinalize)
	manage := service.NewManage(repo, blobUnit)
	manage.UploadIDs = pods.UploadID
	sites := storage.NewSites(repo)
	deploy := service.NewDeploySite(sites, repo, blobUnit)
	deploy.UploadIDs = pods.UploadID
	upload.Archive = service.ArchiveAdapter{Deployer: deploy}
	solid, err := worker.NewSolidProxy(rt.URL, "localhost", authed)
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &httpapi.Server{Pastes: repo, Blobs: blobUnit, Sites: sites, ApexDomain: "localhost", Solid: solid}
	hs := httptest.NewUnstartedServer(httpSrv.Handler())
	_ = hs.Listener.Close()
	hs.Listener = l
	hs.Start()
	t.Cleanup(hs.Close)

	sshSrv := &hostssh.Server{
		ApexDomain: "localhost",
		Upload:     upload,
		Deploy:     deploy,
		Sites:      sites,
		Manage:     manage,
		Pastes:     repo,
		Pods:       pods,
		PodLinkURL: func(id, secret string) string { return origin + "/_solid/link/" + id + "?k=" + secret },
		BuildURL:   func(s domain.Slug) string { return origin + "/p/" + s.String() },
		Logger:     log.New(io.Discard, "", 0),
	}
	sl := mustListen(t)
	addr := sl.Addr().String()
	_ = sl.Close()
	sshSrv.Addr = addr
	go func() { _ = sshSrv.ListenAndServe() }()
	waitForSSH(t, addr)
	s := &stack{t: t, httpURL: origin, sshAddr: addr, repo: repo, upload: upload}
	s.keyed, s.keyedOwner = newKeyClient(t, addr)
	return &podStack{stack: s, pod: pod}
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// link runs `pod link` and completes it the way a browser would.
func (s *podStack) link(t *testing.T) {
	t.Helper()
	out, stderr, code := s.run("pod link "+s.pod.webid(), nil)
	if code != 0 {
		t.Fatalf("pod link: exit %d: %s", code, stderr)
	}
	if !strings.ContainsAny(stderr, qrGlyphs) || strings.ContainsAny(out, qrGlyphs) {
		t.Fatalf("pod link: want the QR on stderr only")
	}
	linkURL := regexp.MustCompile(`http://\S+/_solid/link/\S+`).FindString(out)
	if linkURL == "" {
		t.Fatalf("pod link printed no URL: %q", out)
	}
	res, err := http.Get(linkURL)
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	fp := strings.TrimPrefix(s.keyedOwner, domain.IdentityKeyPrefix)
	if res.StatusCode != http.StatusOK || !strings.Contains(string(page), fp) {
		t.Fatalf("link page: %d, names the key: %v\n%s", res.StatusCode, strings.Contains(string(page), fp), page)
	}
	u, _ := url.Parse(linkURL)
	// Continue pressed twice: the first sign-in must still complete.
	var first string
	for i := range 2 {
		res, err = noRedirect.PostForm(u.Scheme+"://"+u.Host+u.Path, url.Values{"k": {u.Query().Get("k")}})
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("continue: %d, want 303 to the provider", res.StatusCode)
		}
		if i == 0 {
			first = res.Header.Get("Location")
		}
	}
	// The provider approves at once and redirects to the callback.
	res, err = noRedirect.Get(first)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("provider: %d", res.StatusCode)
	}
	res, err = http.Get(res.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	done, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(done), "Pod linked") {
		t.Fatalf("callback: %d\n%s", res.StatusCode, done)
	}
	// The used URL is spent.
	res, err = http.Get(linkURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Fatalf("reused link URL: %d, want 410", res.StatusCode)
	}
}

func siteArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func httpBody(t *testing.T, u string) (int, string) {
	t.Helper()
	res, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close() //nolint:errcheck
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func podUploads(keys []string) map[string]bool {
	ids := map[string]bool{}
	for _, k := range keys {
		parts := strings.Split(k, "/")
		if len(parts) == 3 && parts[0] == "uploads" {
			ids[parts[1]] = true
		}
	}
	return ids
}

// A linked key's pastes and sites store their bytes in the pod and read back
// unchanged; deletes remove them; unlinking sends new uploads to the bucket.
func TestPodStorageEndToEnd(t *testing.T) {
	for _, mode := range []struct {
		name string
		acp  bool
	}{{"wac", false}, {"acp", true}} {
		t.Run(mode.name, func(t *testing.T) {
			s := startPodStack(t, mode.acp)
			if out, _, _ := s.run("pod status", nil); !strings.Contains(out, "no pod linked.") {
				t.Fatalf("status before link: %q", out)
			}
			// Bytes uploaded before the link stay in the bucket and keep serving.
			out, stderr, code := s.run("", []byte("# before the link\n"))
			if code != 0 {
				t.Fatalf("paste before link: %d %s", code, stderr)
			}
			beforeURL := strings.TrimSpace(out)
			s.upload.WaitFinalize()
			s.link(t)
			if st, body := httpBody(t, beforeURL+"?raw"); st != 200 || body != "# before the link\n" {
				t.Fatalf("pre-link paste after link: %d %q", st, body)
			}
			if len(s.pod.podKeys()) != 0 {
				t.Fatalf("linking moved bytes into the pod: %v", s.pod.podKeys())
			}
			if rule := s.pod.rules[s.pod.aclOf("/alice/hostthis/")]; !strings.Contains(rule, s.pod.webid()) || !strings.Contains(rule, "/_solid/id#me") {
				t.Fatalf("share rule names the owner and hostthis: %q", rule)
			}
			out, _, code = s.run("pod status -o json", nil)
			var status struct{ Pod, WebID string }
			if code != 0 || json.Unmarshal([]byte(out), &status) != nil || status.Pod != s.pod.storage()+"hostthis/" || status.WebID != s.pod.webid() {
				t.Fatalf("status: %d %q", code, out)
			}

			// A paste.
			out, stderr, code = s.run("", []byte("# hello pod\n"))
			if code != 0 {
				t.Fatalf("paste: %d %s", code, stderr)
			}
			pasteURL := strings.TrimSpace(out)
			s.upload.WaitFinalize()
			if st, body := httpBody(t, pasteURL+"?raw"); st != 200 || body != "# hello pod\n" {
				t.Fatalf("paste read: %d %q", st, body)
			}
			if len(podUploads(s.pod.podKeys())) != 1 {
				t.Fatalf("pod holds %v after one paste", s.pod.podKeys())
			}

			// A site with a pack and a standalone file.
			big := strings.Repeat("x", 2<<20)
			out, stderr, code = s.run("", siteArchive(t, map[string]string{"index.html": "<h1>pod site</h1>", "a.js": "console.log(1)", "big.bin": big}))
			if code != 0 {
				t.Fatalf("site: %d %s", code, stderr)
			}
			siteURL := strings.TrimSpace(out)
			for path, want := range map[string]string{"/": "<h1>pod site</h1>", "/a.js": "console.log(1)", "/big.bin": big} {
				if st, body := httpBody(t, strings.TrimSuffix(siteURL, "/")+path); st != 200 || body != want {
					t.Fatalf("site %s: %d, %d bytes", path, st, len(body))
				}
			}
			if len(podUploads(s.pod.podKeys())) != 2 {
				t.Fatalf("pod holds %v after a paste and a site", s.pod.podKeys())
			}

			// Deleting the site removes its objects from the pod.
			slug := siteURL[strings.LastIndex(strings.TrimSuffix(siteURL, "/"), "/")+1:]
			if _, stderr, code := s.run("delete "+strings.TrimSuffix(slug, "/"), nil); code != 0 {
				t.Fatalf("delete: %s", stderr)
			}
			if len(podUploads(s.pod.podKeys())) != 1 {
				t.Fatalf("pod holds %v after deleting the site", s.pod.podKeys())
			}

			// A refused write fails the deploy with a message naming the pod.
			s.pod.setDeny(true)
			_, stderr, code = s.run("", siteArchive(t, map[string]string{"index.html": "x"}))
			if code == 0 || !strings.Contains(stderr, "your pod refused the write (403)") {
				t.Fatalf("refused deploy: exit %d %q", code, stderr)
			}
			s.pod.setDeny(false)

			// After unlink, new bytes go to the bucket; the pod's paste still reads.
			if out, _, _ := s.run("pod unlink", nil); !strings.HasPrefix(out, "unlinked.") {
				t.Fatalf("unlink: %q", out)
			}
			if out, _, code = s.run("", []byte("# bucket\n")); code != 0 {
				t.Fatal("paste after unlink failed")
			}
			s.upload.WaitFinalize()
			if st, body := httpBody(t, strings.TrimSpace(out)+"?raw"); st != 200 || body != "# bucket\n" {
				t.Fatalf("bucket paste read: %d %q", st, body)
			}
			if len(podUploads(s.pod.podKeys())) != 1 {
				t.Fatalf("an upload after unlink reached the pod: %v", s.pod.podKeys())
			}
			if st, body := httpBody(t, pasteURL+"?raw"); st != 200 || body != "# hello pod\n" {
				t.Fatalf("pod paste after unlink: %d %q", st, body)
			}
		})
	}
}

// Without the Solid key the verb says pods are unavailable.
func TestPodUnavailableWithoutKey(t *testing.T) {
	s := startStack(t)
	_, stderr, code := s.run("pod link https://alice.example/profile/card#me", nil)
	if code == 0 || !strings.Contains(stderr, "pods are not available") {
		t.Fatalf("exit %d %q", code, stderr)
	}
}
