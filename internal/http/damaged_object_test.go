package http

import (
	"bytes"
	"io"
	"math/rand/v2"
	stdhttp "net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/zstdenc"
)

// rawObjects is an in-memory object store holding stored bytes as given.
type rawObjects map[string][]byte

func (o rawObjects) Put(key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	o[key] = b
	return err
}

func (o rawObjects) GetReader(key string) (io.ReadCloser, int64, error) {
	b, ok := o[key]
	if !ok {
		return nil, 0, domain.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (o rawObjects) DeletePrefix(string) error { return nil }

// encoded returns body in the at-rest format.
func encoded(t *testing.T, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstdenc.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// noise is n bytes that do not compress, so a cut in the stored object lands
// mid-body.
func noise(n int) []byte {
	b := make([]byte, n)
	r := rand.NewChaCha8([32]byte{1})
	_, _ = r.Read(b)
	return b
}

// syncLog collects log lines from concurrent handlers.
type syncLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *syncLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, sprintf(format, args...))
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// storedHTMLServer serves one ready HTML paste whose object holds stored, read
// through the production at-rest decoding.
func storedHTMLServer(stored []byte, size int) *Server {
	const key = "uploads/u1/0"
	return &Server{
		ApexDomain: "paste.test",
		Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Status: domain.PasteStatusReady, Kind: domain.KindHTML, UpdatedAt: time.Now().UTC(),
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: key, Size: size}),
		}},
		Blobs: storage.NewCompressedBlobStore(rawObjects{key: stored}),
	}
}

// fetch makes one request over a real connection and reads the whole body.
func fetch(t *testing.T, srv *Server, method, target string) (*stdhttp.Response, []byte, error) {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	req, err := stdhttp.NewRequest(method, ts.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

// An object holding the magic and no zstd frame is damaged: a 500, not an
// empty page.
func TestRead_MagicOnlyObjectIs500(t *testing.T) {
	srv := storedHTMLServer(zstdenc.Magic[:], 0)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/p/abc23456", nil))
	if w.Code != stdhttp.StatusInternalServerError {
		t.Fatalf("status %d body %q, want 500", w.Code, w.Body.String())
	}
}

// Damage the decoder meets in the first chunk is a 500, not an empty 200.
func TestRead_DamagedFirstChunkIs500(t *testing.T) {
	stored := append(zstdenc.Magic[:], "not a zstd frame at all"...)
	resp, _, err := fetch(t, storedHTMLServer(stored, 23), "GET", "/p/abc23456")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != stdhttp.StatusInternalServerError {
		t.Fatalf("status %d, want 500", resp.StatusCode)
	}
}

// Damage found after the status is sent breaks the transfer, so no client
// mistakes the short body for the whole file, and is logged once.
func TestRead_MidStreamDamageAbortsTheResponse(t *testing.T) {
	body := noise(1 << 20)
	stored := encoded(t, body)
	checksum := bytes.Clone(stored)
	checksum[len(checksum)-1] ^= 0xff
	for name, damaged := range map[string][]byte{
		"truncated": stored[:len(stored)*3/4],
		"checksum":  checksum,
	} {
		for _, size := range []int{len(body), -1} {
			t.Run(name+"/size "+strconv.Itoa(size), func(t *testing.T) {
				srv := storedHTMLServer(damaged, size)
				var logged syncLog
				srv.Logf = logged.logf
				resp, got, err := fetch(t, srv, "GET", "/p/abc23456")
				if resp.StatusCode != stdhttp.StatusOK {
					t.Fatalf("status %d, want 200 (the damage is past the first chunk)", resp.StatusCode)
				}
				if err == nil {
					t.Fatalf("read %d of %d bytes with no error: the transfer must break", len(got), len(body))
				}
				if out := logged.String(); strings.Count(out, "abc23456") != 1 {
					t.Fatalf("want one warning naming the slug, got %q", out)
				}
			})
		}
	}
}

// A 200 that streams stored bytes declares the entry's decoded size, so a
// short body is visible to every client; a negative size is unknown.
func TestRead_ContentLengthIsTheRecordedSize(t *testing.T) {
	doc := []byte("# notes\n")
	big := noise(8 << 10)
	file := []byte("body{color:red}")
	objects := rawObjects{
		"uploads/d/0": encoded(t, doc),
		"uploads/b/0": encoded(t, big),
		"uploads/s/0": encoded(t, file),
		"uploads/s/1": encoded(t, nil),
	}
	now := time.Now().UTC()
	paste := func(kind domain.ContentKind, key string, size int) *Server {
		return &Server{Blobs: storage.NewCompressedBlobStore(objects), Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Status: domain.PasteStatusReady, Kind: kind, UpdatedAt: now,
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: key, Size: size}),
		}}}
	}
	site := domain.NewManifest()
	site.Add("style.css", domain.ManifestEntry{Key: "uploads/s/0", Size: len(file), ContentType: "text/css; charset=utf-8"})
	site.Add("empty.css", domain.ManifestEntry{Key: "uploads/s/1", Size: 0, ContentType: "text/css; charset=utf-8"})
	sites := &Server{Blobs: storage.NewCompressedBlobStore(objects), Sites: stubSiteReader{s: domain.Site{
		Slug: "abc23456", Kind: domain.KindSite, Manifest: site, UpdatedAt: now,
	}}}
	kbm := domain.NewManifest()
	kbm.Add("README.md", domain.ManifestEntry{Key: "uploads/d/0", Size: len(doc)})
	kb := &Server{Blobs: storage.NewCompressedBlobStore(objects), Pastes: stubPasteReader{p: domain.Paste{
		Slug: "abc23456", Status: domain.PasteStatusReady, Kind: domain.KindKnowledgeBase, UpdatedAt: now, Manifest: kbm,
	}}}

	cases := []struct {
		name, target string
		srv          *Server
		want         string
		body         []byte
	}{
		{"html", "/p/abc23456", paste(domain.KindHTML, "uploads/d/0", len(doc)), "8", doc},
		{"raw", "/p/abc23456?raw=1", paste(domain.KindMarkdown, "uploads/d/0", len(doc)), "8", doc},
		{"large", "/p/abc23456", paste(domain.KindHTML, "uploads/b/0", len(big)), "8192", big},
		// Past the server's own buffering, so it cannot fill the length in.
		{"unknown size", "/p/abc23456", paste(domain.KindHTML, "uploads/b/0", -1), "", big},
		{"site file", "/p/abc23456/style.css", sites, "15", file},
		{"empty site file", "/p/abc23456/empty.css", sites, "0", nil},
		{"knowledge base document", "/p/abc23456/README.md?raw=1", kb, "8", doc},
	}
	for _, c := range cases {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(c.name+"/"+method, func(t *testing.T) {
				resp, got, err := fetch(t, c.srv, method, c.target)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != stdhttp.StatusOK {
					t.Fatalf("status %d, want 200", resp.StatusCode)
				}
				if cl := resp.Header.Get("Content-Length"); cl != c.want {
					t.Fatalf("Content-Length %q, want %q", cl, c.want)
				}
				if method == "GET" && !bytes.Equal(got, c.body) {
					t.Fatalf("body %q, want %q", got, c.body)
				}
			})
		}
	}
}
