package storage_test

// The delete verb over the REAL repo on the lost-tail damage shape
// (docs/SPEC.md "Delete heals its own lost tail"): whoami/list counts drop,
// the CDN purges exactly once, the second delete is a plain not-found, and a
// foreign live slug stays not-found unpurged. Lives here rather than with the
// service tests because planting the damage needs the storage package's
// test-only raw seams.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/service"
)

// healPurger records CDN purge calls; a service.CachePurger stand-in.
type healPurger struct {
	mu    sync.Mutex
	slugs []domain.Slug
}

func (p *healPurger) PurgePaste(slug domain.Slug) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slugs = append(p.slugs, slug)
	return nil
}

func (p *healPurger) calls() []domain.Slug {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]domain.Slug(nil), p.slugs...)
}

func (p *healPurger) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.slugs = nil
}

func TestDeleteVerb_HealsStrandedIndexEntry(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	owner := "key:heal-verb"

	// A nil BlobUnit pins that the heal path never touches blobs: the {slug}
	// transaction that committed already unbound them, and a call here would
	// nil-panic.
	manage := service.NewManage(repo, nil)
	manage.Now = func() time.Time { return now }
	purger := &healPurger{}
	mgr := service.NewCacheInvalidating(manage, purger)

	p := pasteOf("healvrb1", owner, 48)
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	strandDeleteTail(t, repo, p.Slug)

	// The damage is visible before the heal, or a pass proves nothing.
	pre, err := manage.Whoami(owner, "")
	if err != nil {
		t.Fatalf("whoami pre-heal: %v", err)
	}
	if pre.Active != 1 || pre.UsedBytes != 48 {
		t.Fatalf("fixture: whoami pre-heal = %+v, want the phantom counted (1 active, 48 bytes)", pre)
	}

	if err := mgr.Delete(p.Slug, owner); err != nil {
		t.Fatalf("delete on the damage shape must succeed, got: %v", err)
	}
	// The healed delete purges like any successful delete, exactly once.
	if got := purger.calls(); len(got) != 1 || got[0] != p.Slug {
		t.Fatalf("purges after healed delete = %v, want [%s]", got, p.Slug)
	}

	post, err := manage.Whoami(owner, "")
	if err != nil {
		t.Fatalf("whoami post-heal: %v", err)
	}
	if post.Active != 0 || post.UsedBytes != 0 {
		t.Fatalf("whoami post-heal = %+v, want 0 active / 0 bytes", post)
	}
	list, err := manage.List(owner)
	if err != nil {
		t.Fatalf("list post-heal: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list post-heal = %+v, want empty", list)
	}

	// Idempotent: the residue is gone, so the second delete is a plain
	// not-found and spends no purge.
	purger.reset()
	if err := mgr.Delete(p.Slug, owner); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
	if got := purger.calls(); len(got) != 0 {
		t.Fatalf("second delete purged %v, want none", got)
	}
}

// Existence collapse holds through the heal: a live slug owned by someone
// else answers exactly not-found, is left untouched, and spends no purge.
func TestDeleteVerb_ForeignLiveSlugStaysNotFound(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

	manage := service.NewManage(repo, nil)
	manage.Now = func() time.Time { return now }
	purger := &healPurger{}
	mgr := service.NewCacheInvalidating(manage, purger)

	p := pasteOf("healfrn1", "key:someone-else", 200)
	if err := repo.InsertWithQuotaCheck(context.Background(), p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	if err := mgr.Delete(p.Slug, "key:heal-caller"); !errors.Is(err, service.ErrNotFound) {
		t.Fatalf("delete of a foreign live slug = %v, want ErrNotFound", err)
	}
	if got := purger.calls(); len(got) != 0 {
		t.Fatalf("foreign delete purged %v, want none", got)
	}
	if _, err := repo.Get(p.Slug); err != nil {
		t.Fatalf("foreign paste must be untouched: %v", err)
	}
}
