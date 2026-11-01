package ssh_test

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// fakeScreener records what it was sent and answers deny when deny says so.
type fakeScreener struct {
	mu   sync.Mutex
	seen []map[string]string // one file map per request
	deny func(files map[string]string) string
	err  error
	// message is returned with every refusal.
	message string
}

func (f *fakeScreener) Screen(_ context.Context, req domain.ScreenRequest) (domain.ScreenVerdict, error) {
	files := map[string]string{}
	if req.Shape == domain.ScreenDocument {
		b, _ := io.ReadAll(req.Body)
		files[""] = string(b)
	} else {
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			return domain.ScreenVerdict{}, err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(tr)
			files[h.Name] = string(b)
		}
	}
	f.mu.Lock()
	f.seen = append(f.seen, files)
	f.mu.Unlock()
	if f.err != nil {
		return domain.ScreenVerdict{}, f.err
	}
	if rule := f.deny(files); rule != "" {
		return domain.ScreenVerdict{Allow: false, Rule: rule, Message: f.message}, nil
	}
	return domain.ScreenVerdict{Allow: true}, nil
}

func denyContaining(marker string) func(map[string]string) string {
	return func(files map[string]string) string {
		for _, body := range files {
			if strings.Contains(body, marker) {
				return "test:" + marker
			}
		}
		return ""
	}
}

// A refused upload exits 8, is never listed, and is never reachable.
func TestScreen_RefusedPasteIsNeverPublished(t *testing.T) {
	f := &fakeScreener{deny: denyContaining("EVIL")}
	g := startStack(t, withScreen(&service.ScreenGate{Screener: f}))
	stdout, stderr, code := g.run("", []byte("<!doctype html><h1>EVIL</h1>"))
	if code != 8 || stdout != "" || !strings.Contains(stderr, "upload refused: it matched a content rule (test:EVIL)") {
		t.Fatalf("refused create: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if f.seen[0][""] != "<!doctype html><h1>EVIL</h1>" {
		t.Fatalf("screener saw %q, want the decoded body", f.seen[0][""])
	}
	if listing, _, _ := g.run("list", nil); strings.Contains(listing, "hostthis.") || strings.Contains(listing, "/p/") {
		t.Fatalf("a refused upload was listed: %q", listing)
	}
	out, _, code := g.run("", []byte("<!doctype html><h1>fine</h1>"))
	if code != 0 {
		t.Fatalf("allowed create: exit %d", code)
	}
	slug := extractSlug(out)
	if _, stderr, code := g.run(slug, []byte("<h1>EVIL</h1>")); code != 8 {
		t.Fatalf("refused update: exit %d %q", code, stderr)
	}
	if status, body := getBody(t, g.httpURL+"/p/"+slug); status != 200 || !strings.Contains(body, "fine") {
		t.Fatalf("a refused update changed the page: %d %q", status, body)
	}
}

// A site bundle carries text files whole and binaries by their head, and a
// refused deploy publishes nothing.
func TestScreen_SiteDeploySendsABundleAndARefusalPublishesNothing(t *testing.T) {
	f := &fakeScreener{deny: denyContaining("EVIL")}
	g := startStack(t, withSites(), withScreen(&service.ScreenGate{Screener: f}))
	binary := "\x7fELF" + strings.Repeat("x", 10000)
	arc := makeSiteArchive(t, map[string]string{"index.html": "<h1>ok</h1>", "tool.bin": binary})
	if _, stderr, code := g.run("", arc); code != 0 {
		t.Fatalf("allowed deploy: exit %d %q", code, stderr)
	}
	bundle := f.seen[0]
	if bundle["index.html"] != "<h1>ok</h1>" || bundle["tool.bin"] != binary[:4096] {
		t.Fatalf("bundle: index %q, tool.bin %d bytes", bundle["index.html"], len(bundle["tool.bin"]))
	}
	bad := makeSiteArchive(t, map[string]string{"index.html": "<h1>EVIL</h1>"})
	stdout, stderr, code := g.run("", bad)
	if code != 8 || stdout != "" {
		t.Fatalf("refused deploy: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// An unreachable screener refuses the upload, and an exempt key skips it.
func TestScreen_FailsClosedAndExemptKeysSkipIt(t *testing.T) {
	f := &fakeScreener{err: errors.New("down"), deny: denyContaining("never")}
	gate := &service.ScreenGate{Screener: f, Exempt: map[string]bool{}}
	g := startStack(t, withScreen(gate))
	_, stderr, code := g.run("", []byte("<h1>hi</h1>"))
	if code != 1 || !strings.Contains(stderr, "content screening is unavailable") {
		t.Fatalf("screener down: exit %d %q", code, stderr)
	}
	gate.Exempt[g.keyedOwner] = true
	if _, stderr, code := g.run("", []byte("<h1>hi</h1>")); code != 0 {
		t.Fatalf("exempt key: exit %d %q", code, stderr)
	}
}

// A screener's message reaches the uploader as one line of plain text.
func TestScreen_RefusalCarriesTheScreenersMessage(t *testing.T) {
	f := &fakeScreener{deny: denyContaining("PAY"), message: "for personal use\x1b[31m only\n"}
	g := startStack(t, withScreen(&service.ScreenGate{Screener: f}))
	_, stderr, code := g.run("", []byte("<h1>PAY me</h1>"))
	if code != 8 || !strings.Contains(stderr, "hostthis: upload refused: for personal use[31m only (test:PAY)") {
		t.Fatalf("exit %d stderr %q", code, stderr)
	}
}
