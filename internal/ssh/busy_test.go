package ssh_test

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
	hostssh "github.com/Zamua/hostthis/internal/ssh"
	"github.com/Zamua/hostthis/internal/storage"
)

// busyAppendRepo refuses every append because another change is settling.
type busyAppendRepo struct{ *celld.PasteRepo }

func (busyAppendRepo) AppendVersionWithQuotaCheck(context.Context, domain.Slug, string, domain.ContentKind,
	string, domain.Manifest, int, int64, time.Time,
) (domain.AppendResult, error) {
	return domain.AppendResult{}, domain.ErrBusy
}

// A busy paste tells the user to retry, without the storage sentinel's prefix.
func TestUpdate_BusyPasteSaysRetry(t *testing.T) {
	s := startStack(t, withManageRepo(func(r *celld.PasteRepo) service.PasteAdmin { return busyAppendRepo{r} }))
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
