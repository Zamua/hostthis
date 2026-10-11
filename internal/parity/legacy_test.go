package parity

import (
	"bytes"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/zstdenc"
)

// legacyCase is a read of an entry written before DecodedSize existed, whose
// Size is wrong, and the file's own bytes.
type legacyCase struct {
	slug, target string
	body         []byte
}

// buildLegacy writes rows the way write paths did before they recorded a
// decoded size: each entry's Size is 0, or its object's stored length.
func (w *world) buildLegacy(t *testing.T) {
	t.Helper()
	big := bytes.Repeat([]byte("a legacy line that compresses well\n"), 400)
	small := []byte("<!doctype html><p>a legacy page</p>\n")
	tiny := []byte("8 bytes\n")
	sizeZero := func(_, _ int) int { return 0 }
	storedSize := func(_, stored int) int { return stored }

	markdown := w.cell.legacyPaste(t, domain.KindMarkdown, map[string][]byte{domain.Root: big}, sizeZero)
	html := w.cell.legacyPaste(t, domain.KindHTML, map[string][]byte{domain.Root: small}, sizeZero)
	site := w.cell.legacyPaste(t, domain.KindSite, map[string][]byte{
		"index.html": small, "app.js": big, "tiny.txt": tiny,
	}, storedSize)
	w.slugs = append(w.slugs,
		slugCase{slug: markdown, label: "legacy size 0 markdown"},
		slugCase{slug: html, label: "legacy size 0 html"},
		slugCase{slug: site, label: "legacy stored sizes site", targets: []string{
			"/app.js", "/app.js?raw=1", "/tiny.txt", "/index.html", "/some/route",
		}},
	)
	w.legacy = append(w.legacy,
		legacyCase{markdown, "/?raw=1", big},
		legacyCase{html, "/", small},
		legacyCase{site, "/", small},
		legacyCase{site, "/some/route", small},
		legacyCase{site, "/app.js", big},
		legacyCase{site, "/tiny.txt", tiny},
	)
}

// legacyPaste writes a ready row holding files in the at-rest format, with
// each entry's Size from size(decoded, stored) and no decoded size.
func (c *cellAPI) legacyPaste(t *testing.T, kind domain.ContentKind, files map[string][]byte, size func(decoded, stored int) int) string {
	t.Helper()
	slug := domain.NewRandomSlug().String()
	upload := domain.NewUploadID()
	m := domain.NewManifest()
	charged := 0
	for i, name := range sortedKeys(files) {
		key := domain.UploadObjectKey(upload, i)
		stored := atRest(t, files[name])
		c.call(t, "PUT", "/blob/"+key, bytes.NewReader(stored))
		e := domain.ManifestEntry{Key: key, Size: size(len(files[name]), len(stored)), CompressedSize: len(stored) - len(zstdenc.Magic)}
		if kind.IsDirectory() {
			e.ContentType = domain.ContentTypeForPath(name)
		} else {
			e.Kind = string(kind)
		}
		m.Add(name, e)
		charged += e.CompressedSize
	}
	now := time.Now().UnixMilli()
	body, err := json.Marshal(map[string]any{
		"generation": "parity-legacy-" + slug, "fingerprint": "parity",
		"row": map[string]any{
			"slug": slug, "identity": "key:SHA256:parity-forced", "status": "ready", "kind": string(kind),
			"size": charged, "createdAt": now, "updatedAt": now, "uploadId": upload, "manifest": m,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("DecodedSize")) {
		t.Fatalf("a legacy row records a decoded size: %s", body)
	}
	c.call(t, "POST", "/paste/put?slug="+slug, bytes.NewReader(body))
	return slug
}

func atRest(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw, err := zstdenc.NewWriter(&out)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// netHTTPFrames is the largest body net/http frames itself, with the length
// it wrote, when its handler declares none.
const netHTTPFrames = 2048

// testLegacy holds every legacy read to the file's own bytes: two servers
// that both trusted the wrong Size would still agree with each other. Each
// answers the whole file, and states no length but the true one; past what
// net/http frames itself, none at all.
func testLegacy(t *testing.T, w *world) {
	for _, lc := range w.legacy {
		r := get(w.host(lc.slug), lc.target)
		for _, s := range append([]*server{w.hostthisd}, w.workers...) {
			res, err := s.do(r)
			if err != nil {
				t.Errorf("%s: %s: %v", r, s.name, err)
				continue
			}
			if res.status != 200 || res.bodyErr != nil || !bytes.Equal(res.body, lc.body) {
				t.Errorf("%s: %s answered %d with %d of %d bytes (body error %v), want the whole file",
					r, s.name, res.status, len(res.body), len(lc.body), res.bodyErr)
			}
			cl := res.header.Get("Content-Length")
			if cl != "" && (len(lc.body) > netHTTPFrames || cl != strconv.Itoa(len(lc.body))) {
				t.Errorf("%s: %s states Content-Length %s for a %d byte file with no decoded size", r, s.name, cl, len(lc.body))
			}
		}
	}
}
