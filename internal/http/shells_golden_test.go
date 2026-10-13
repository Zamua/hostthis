package http

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	stdhttp "net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

var updateShellGolden = flag.Bool("update-shell-golden", false, "rewrite "+shellGoldenPath)

// Shared with the Worker's tests, which hold the Worker's shells and assets to the
// same bytes and headers.
const shellGoldenPath = "../../testdata/shells-golden.json"

type goldenResponse struct {
	Status int               `json:"status"`
	Header map[string]string `json:"header"`
	Size   int               `json:"size"`
	SHA256 string            `json:"sha256"`
}

// recordResponse leaves out Content-Length: it is framing, which the Worker's
// runtime decides, and TestRead_ContentLengthIsTheDecodedSize pins its value.
func recordResponse(w *httptest.ResponseRecorder) goldenResponse {
	h := map[string]string{}
	for name, values := range w.Header() {
		if name == "Content-Length" {
			continue
		}
		h[name] = strings.Join(values, ", ")
	}
	sum := sha256.Sum256(w.Body.Bytes())
	return goldenResponse{Status: w.Code, Header: h, Size: w.Body.Len(), SHA256: hex.EncodeToString(sum[:])}
}

// shellResponses serves every shell page, every kind's raw bytes and every
// renderer asset, plus asset names that must stay unservable.
func shellResponses(t *testing.T) map[string]goldenResponse {
	t.Helper()
	got := map[string]goldenResponse{}
	src := []byte("# title\n\nsome **body**\n")
	for kind := range shells {
		got["shell/"+string(kind)] = recordResponse(serveReady(t, kind, "abc23456", keyA, src, "/p/abc23456", "text/html"))
		got["raw/"+string(kind)] = recordResponse(serveReady(t, kind, "abc23456", keyA, src, "/p/abc23456?raw=1", "text/html"))
	}
	kb := buildKBServer(t).Handler()
	for name, path := range map[string]string{
		"kb/root": "/", "kb/document": "/guide/setup.md", "kb/raw": "/guide/setup.md?raw=1", "kb/files": "/?files=1",
	} {
		got[name] = recordResponse(kbGet(t, kb, path))
	}
	got["kb/files-escapes"] = recordResponse(kbGet(t, escapingKBServer().Handler(), "/?files=1"))
	for name, status := range map[string]domain.PasteStatus{"status/pending": domain.PasteStatusPending, "status/failed": domain.PasteStatusFailed} {
		srv := &Server{ApexDomain: "paste.test", Pastes: stubPasteReader{p: domain.Paste{
			Slug: "abc23456", Status: status, Kind: domain.KindMarkdown,
			Manifest: domain.DocumentManifest(domain.ManifestEntry{Key: keyA}),
		}}}
		got[name] = recordResponse(kbGet(t, srv.Handler(), "/"))
	}
	for name, req := range map[string][2]string{
		"error/notfound":  {"GET", "/"},
		"redirect/clean":  {"GET", "/a//b?raw=1"},
		"redirect/head":   {"HEAD", "/a//b"},
		"redirect/tree":   {"GET", "/_hostthis?v=x"},
		"health/healthz":  {"GET", "/healthz"},
		"health/readyz":   {"GET", "/readyz"},
		"health/redirect": {"GET", "//healthz"},
	} {
		srv := &Server{ApexDomain: "paste.test", Pastes: stubPasteReader{}}
		r := httptest.NewRequest(req[0], req[1], nil)
		r.Host = "abc23456.paste.test"
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)
		got[name] = recordResponse(w)
	}
	mux := (&Server{ApexDomain: "paste.test"}).Handler()
	names := []string{"shell.html", "shells.json", "evil.js", "mdshell/md.js", "common/deeplink.js"}
	for name := range assetSource {
		names = append(names, name)
	}
	for _, name := range names {
		r := httptest.NewRequest("GET", "/_hostthis/"+name+"?v=x", nil)
		r.Host = "abc23456.paste.test"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		got["asset/"+name] = recordResponse(w)
	}
	return got
}

// escapingKBPaths are knowledge base paths whose file list exercises the JSON
// encoder's escapes and UTF-8 byte order.
var escapingKBPaths = []string{
	"a<b>&c.md", "line sep .md", "quote\"back\\slash.md", "tab\tnl\n.md", "ctl\x01\x1f\b\f.md",
	"del\x7f.md", "é.md", "\U0001d11e.md", ".md", "z.md",
}

func escapingKBServer() *Server {
	m := domain.NewManifest()
	for i, p := range escapingKBPaths {
		m.Add(p, domain.ManifestEntry{Key: fmt.Sprintf("key-%d", i), Size: 1})
	}
	return &Server{ApexDomain: "paste.test", Pastes: stubPasteReader{p: domain.Paste{
		Slug: "abc23456", Status: domain.PasteStatusReady, Kind: domain.KindKnowledgeBase,
		UploadID: "up-escapes", UpdatedAt: kbUpdated, Manifest: m,
	}}}
}

// Shell pages, raw responses and renderer assets keep their exact status,
// headers and bytes wherever the shell files and table live. The Worker's
// tests hold its public surface to the same records.
func TestShells_GoldenResponses(t *testing.T) {
	got := shellResponses(t)
	if *updateShellGolden {
		b, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shellGoldenPath, append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	b, err := os.ReadFile(shellGoldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-shell-golden): %v", err)
	}
	var want map[string]goldenResponse
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	slices.Sort(sorted)
	for _, k := range sorted {
		g, inGot := got[k]
		w, inWant := want[k]
		switch {
		case !inGot:
			t.Errorf("%s: golden response no longer served", k)
		case !inWant:
			t.Errorf("%s: served but absent from the golden", k)
		case g.Status != w.Status || g.Size != w.Size || g.SHA256 != w.SHA256:
			t.Errorf("%s: status/size/sha256 %d/%d/%s, want %d/%d/%s", k, g.Status, g.Size, g.SHA256, w.Status, w.Size, w.SHA256)
		default:
			for name := range mergeKeys(g.Header, w.Header) {
				if g.Header[name] != w.Header[name] {
					t.Errorf("%s: header %s %q, want %q", k, name, g.Header[name], w.Header[name])
				}
			}
		}
	}
	if len(want) == 0 || want["shell/"+string(domain.KindMarkdown)].Status != stdhttp.StatusOK {
		t.Fatal("golden holds no markdown shell, so it pins nothing")
	}
}

func mergeKeys(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}
