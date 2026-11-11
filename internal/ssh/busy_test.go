package ssh_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/worker"
)

// busyAppendRepo refuses every append because another change is settling.
type busyAppendRepo struct{ *worker.PasteRepo }

func (busyAppendRepo) AppendVersionWithQuotaCheck(context.Context, domain.Slug, string, domain.ContentKind,
	string, domain.Manifest, int, int64, time.Time,
) (domain.AppendResult, error) {
	return domain.AppendResult{}, domain.ErrBusy
}

// A busy paste tells the user to retry, without the storage sentinel's prefix.
func TestUpdate_BusyPasteSaysRetry(t *testing.T) {
	s := startStack(t, withManageRepo(func(r *worker.PasteRepo) service.PasteAdmin { return busyAppendRepo{r} }))
	stdout, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	slug := extractSlug(stdout)

	_, stderr, exit := s.run(slug, []byte("<!doctype html><p>v2</p>"))
	if exit != hostssh.ExitErr {
		t.Fatalf("exit = %d, want %d (stderr %q)", exit, hostssh.ExitErr, stderr)
	}
	if want := "hostthis: another change to this paste is still settling; retry shortly\n"; stderr != want {
		t.Fatalf("stderr = %q, want %q", stderr, want)
	}
}

// busyBlobs refuses writes with the store-busy sentinel once busy is set.
type busyBlobs struct {
	storage.InnerBlobStore
	busy *atomic.Bool
}

func (b busyBlobs) Put(key string, r io.Reader, size int64) error {
	if b.busy.Load() {
		return fmt.Errorf("blob put %s: %w", key, domain.ErrStoreBusy)
	}
	return b.InnerBlobStore.Put(key, r, size)
}

// A write the blob store could not admit in time tells the user to retry, on
// every synchronous write path.
func TestUpload_StoreBusySaysTryAgain(t *testing.T) {
	var busy atomic.Bool
	s := startStack(t, withSites(), withRawBlobs(func(raw storage.InnerBlobStore) storage.InnerBlobStore {
		return busyBlobs{InnerBlobStore: raw, busy: &busy}
	}))
	pasteOut, _, _ := s.run("", []byte("<!doctype html><p>v1</p>"))
	arc := makeSiteArchive(t, map[string]string{"index.html": "<h1>v1</h1>"})
	siteOut, _, _ := s.run("", arc)
	s.upload.WaitFinalize()
	busy.Store(true)

	for _, tc := range []struct {
		name  string
		slug  string
		stdin []byte
	}{
		{"paste update", extractSlug(pasteOut), []byte("<!doctype html><p>v2</p>")},
		{"site deploy", "", arc},
		{"site redeploy", extractSlug(siteOut), arc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, exit := s.run(tc.slug, tc.stdin)
			if exit != hostssh.ExitErr {
				t.Fatalf("exit = %d, want %d (stderr %q)", exit, hostssh.ExitErr, stderr)
			}
			if want := "hostthis: busy storing other uploads; try again in a minute\n"; stderr != want {
				t.Fatalf("stderr = %q, want %q", stderr, want)
			}
		})
	}
}

// A new paste that the blob store cannot admit is refused before any URL is
// handed out, and a paste whose URL was handed out is written however busy the
// store has become since.
func TestUpload_NewPasteIsAdmittedBeforeItsURL(t *testing.T) {
	blobs := newSlowBlobs()
	s := startStack(t, withRawBlobs(func(raw storage.InnerBlobStore) storage.InnerBlobStore {
		gate, err := storage.NewGatedBlobStore(blobs.wrap(raw), 64, 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		return gate
	}))
	t.Cleanup(blobs.release)
	blobs.park.Store(true)

	// The first paste gets its URL; its write then holds the whole budget.
	first := runOnce(s.keyed, "", strings.NewReader("<!doctype html><p>first</p>"))
	if first.err != nil || first.exit != hostssh.ExitOK {
		t.Fatalf("first paste: exit %d, %v (stderr %q)", first.exit, first.err, first.stderr)
	}
	waitFor(t, func() bool { return blobs.parked.Load() == 1 })

	second := runOnce(s.keyed, "", strings.NewReader("<!doctype html><p>second</p>"))
	if second.exit != hostssh.ExitErr || second.stdout != "" {
		t.Fatalf("second paste: exit %d stdout %q, want %d and no URL", second.exit, second.stdout, hostssh.ExitErr)
	}
	if want := "hostthis: busy storing other uploads; try again in a minute\n"; second.stderr != want {
		t.Fatalf("stderr = %q, want %q", second.stderr, want)
	}

	blobs.release()
	s.upload.WaitFinalize()
	if code, body := getBody(t, strings.TrimSpace(first.stdout)); code != 200 || !strings.Contains(body, "first") {
		t.Fatalf("first paste after the store freed: %d %q", code, body)
	}
}
