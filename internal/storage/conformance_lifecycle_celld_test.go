package storage_test

// The celld entry into the paste lifecycle conformance.
//
// This is the first celld adapter whose operation spans TWO cells with no
// transaction between them - the row is addressed by slug, the quota and index
// by identity - so it is where the intent log stops being a justified design
// and has to actually carry a create. Passing the same assertions the shale
// backend passes is the only evidence that it does.
//
// Skipped unless CELLD_TEST_ENDPOINT names a running fleet.
//
// Each subtest gets its own owner and slug prefix. celld cells are durable with
// no teardown hook, so isolation comes from naming: a rerun that reused an
// owner would inherit the previous run's quota reservations and read as a
// spurious over-quota failure.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/celld"
	"github.com/Zamua/hostthis/internal/domain"
)

var celldLifecycleSeq atomic.Int64

// namespacedRepo rewrites slugs and owners into a per-run namespace, so the
// suite's fixed identifiers do not collide across runs against a durable fleet.
type namespacedRepo struct {
	inner  *celld.PasteRepo
	prefix string
}

func (n namespacedRepo) slug(s domain.Slug) domain.Slug {
	return domain.Slug(n.prefix + string(s))
}

func (n namespacedRepo) owner(o string) string { return n.prefix + o }

func (n namespacedRepo) InsertWithQuotaCheck(ctx context.Context, p domain.Paste, userCap int64, now time.Time) error {
	p.Slug = n.slug(p.Slug)
	p.Identity = domain.Identity(n.owner(p.Identity.String()))
	return n.inner.InsertWithQuotaCheck(ctx, p, userCap, now)
}

func (n namespacedRepo) Get(s domain.Slug) (domain.Paste, error) {
	got, err := n.inner.Get(n.slug(s))
	if err != nil {
		return domain.Paste{}, err
	}
	// Hand back the identifiers the caller used, or the suite's assertions
	// would be reading the harness rather than the adapter.
	got.Slug = s
	return got, nil
}

func (n namespacedRepo) MarkReady(s domain.Slug) error  { return n.inner.MarkReady(n.slug(s)) }
func (n namespacedRepo) MarkFailed(s domain.Slug) error { return n.inner.MarkFailed(n.slug(s)) }

func (n namespacedRepo) SumActiveBytesByOwner(o string, at time.Time) (int, error) {
	return n.inner.SumActiveBytesByOwner(n.owner(o), at)
}

func (n namespacedRepo) ListByOwner(o string) ([]domain.Paste, error) {
	got, err := n.inner.ListByOwner(n.owner(o))
	if err != nil {
		return nil, err
	}
	// Hand back the identifiers the caller used, or the suite reads the harness.
	for i := range got {
		got[i].Slug = domain.Slug(strings.TrimPrefix(string(got[i].Slug), n.prefix))
		got[i].Identity = domain.Identity(o)
	}
	return got, nil
}

func (n namespacedRepo) CountByOwner(o string) (int, error) {
	return n.inner.CountByOwner(n.owner(o))
}

func (n namespacedRepo) OwnerFirstSeen(o string) (time.Time, error) {
	return n.inner.OwnerFirstSeen(n.owner(o))
}

func (n namespacedRepo) DropStaleOwnerEntry(s domain.Slug, o string) (bool, error) {
	return n.inner.DropStaleOwnerEntry(n.slug(s), n.owner(o))
}

func TestOwnerIndexConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld owner-index conformance")
	}
	runOwnerIndexConformance(t, "celld", func(t *testing.T) ownerIndexRepo {
		return namespacedRepo{
			inner:  celld.NewPasteRepo(base, nil),
			prefix: fmt.Sprintf("c%d", celldLifecycleSeq.Add(1)),
		}
	})
}

func TestLifecycleConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld lifecycle conformance")
	}
	runLifecycleConformance(t, "celld", func(t *testing.T) lifecycleRepo {
		return namespacedRepo{
			inner:  celld.NewPasteRepo(base, nil),
			prefix: fmt.Sprintf("c%d", celldLifecycleSeq.Add(1)),
		}
	})
}
