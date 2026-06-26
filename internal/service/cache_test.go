package service_test

import (
	"bytes"
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// recordingPurger captures the slugs it's asked to purge. It implements
// service.CachePurger so it can stand in for a real CDN adapter.
type recordingPurger struct {
	mu       sync.Mutex
	slugs    []domain.Slug
	failNext bool
}

func (r *recordingPurger) PurgePaste(slug domain.Slug) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slugs = append(r.slugs, slug)
	if r.failNext {
		r.failNext = false
		return assertErr("purge failed")
	}
	return nil
}

func (r *recordingPurger) calls() []domain.Slug {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Slug(nil), r.slugs...)
}

func (r *recordingPurger) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slugs = nil
}

type assertErr string

func (e assertErr) Error() string { return string(e) }

const testOwner = "key:test-id"

// newPaste uploads a fresh HTML paste and returns its slug.
func newPaste(t *testing.T, upload *service.Upload) domain.Slug {
	t.Helper()
	res, err := upload.Create(bytes.NewReader(htmlBody(200)), testOwner, "", "")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	return res.Paste.Slug
}

// The decorator fires exactly one purge, naming the mutated slug, after
// each successful mutating verb.
func TestCacheInvalidating_MutationsPurge(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := service.NewCacheInvalidating(manage, purger)

	t.Run("update", func(t *testing.T) {
		purger.reset()
		slug := newPaste(t, upload)
		if _, err := mgr.Update(slug, testOwner, bytes.NewReader(htmlBody(300)), ""); err != nil {
			t.Fatalf("update: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("update purge: got %v, want [%s]", got, slug)
		}
	})
	t.Run("pin then unpin", func(t *testing.T) {
		slug := newPaste(t, upload)
		if _, err := mgr.Update(slug, testOwner, bytes.NewReader(htmlBody(300)), ""); err != nil {
			t.Fatalf("update: %v", err)
		}
		purger.reset()
		if _, err := mgr.Pin(slug, testOwner, 1); err != nil {
			t.Fatalf("pin: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("pin purge: got %v, want [%s]", got, slug)
		}
		purger.reset()
		if err := mgr.Unpin(slug, testOwner); err != nil {
			t.Fatalf("unpin: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("unpin purge: got %v, want [%s]", got, slug)
		}
	})
	t.Run("delete", func(t *testing.T) {
		purger.reset()
		slug := newPaste(t, upload)
		if err := mgr.Delete(slug, testOwner); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if got := purger.calls(); len(got) != 1 || got[0] != slug {
			t.Fatalf("delete purge: got %v, want [%s]", got, slug)
		}
	})
}

// rename never changes the bytes served at the public URL, so the
// decorator must NOT purge it (matches the spec).
func TestCacheInvalidating_RenameDoesNotPurge(t *testing.T) {
	upload, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := service.NewCacheInvalidating(manage, purger)

	slug := newPaste(t, upload)
	if err := mgr.Rename(slug, testOwner, "a-label"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := purger.calls(); len(got) != 0 {
		t.Fatalf("rename must not purge, got %v", got)
	}
}

// A failed mutation must NOT purge - there is nothing stale to flush, and
// purging a slug whose served bytes didn't change is wasteful.
func TestCacheInvalidating_FailedMutationDoesNotPurge(t *testing.T) {
	_, manage, _ := newStack(t)
	purger := &recordingPurger{}
	mgr := service.NewCacheInvalidating(manage, purger)

	// Delete of a slug that doesn't exist (or isn't owned) errors.
	if err := mgr.Delete(domain.Slug("nope1234"), testOwner); err == nil {
		t.Fatalf("expected delete of missing slug to error")
	}
	if got := purger.calls(); len(got) != 0 {
		t.Fatalf("failed delete must not purge, got %v", got)
	}
}

// A purge error is best-effort: it is swallowed so the underlying
// mutation still reports success (the paste IS deleted on origin).
func TestCacheInvalidating_PurgeErrorDoesNotFailOp(t *testing.T) {
	upload, manage, repo := newStack(t)
	purger := &recordingPurger{failNext: true}
	mgr := service.NewCacheInvalidating(manage, purger)

	slug := newPaste(t, upload)
	if err := mgr.Delete(slug, testOwner); err != nil {
		t.Fatalf("delete returned %v despite purge failure - purge errors must be swallowed", err)
	}
	if _, err := repo.Get(slug); err == nil {
		t.Fatalf("paste should be deleted, but Get returned nil error")
	}
}

// With no CDN configured (nil purger), the decorator unwraps to the inner
// service so there is zero overhead and the wrap is invisible.
func TestCacheInvalidating_NilPurgerUnwraps(t *testing.T) {
	_, manage, _ := newStack(t)
	if got := service.NewCacheInvalidating(manage, nil); got != service.PasteManager(manage) {
		t.Fatalf("nil purger should return the inner manager unwrapped, got %T", got)
	}
}
