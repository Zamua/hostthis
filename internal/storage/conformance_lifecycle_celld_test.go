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
	"github.com/Zamua/hostthis/internal/storage"
)

var celldLifecycleSeq atomic.Int64

// A per-PROCESS nonce, not just a counter. The counter restarts at 1 every run
// while the cells it names are durable, so two runs of the same suite share a
// namespace and the second one inherits the first's admissions and quota - which
// surfaces as a never-seen key reporting knownAlready, an error that looks like
// an adapter bug and is not.
var celldRunNonce = fmt.Sprintf("%x", time.Now().UnixNano()&0xffffff)

func celldNamespace() string {
	return fmt.Sprintf("c%s%d", celldRunNonce, celldLifecycleSeq.Add(1))
}

// namespacedRepo rewrites slugs and owners into a per-run namespace, so the
// suite's fixed identifiers do not collide across runs against a durable fleet.
type namespacedRepo struct {
	inner  *celld.PasteRepo
	prefix string
	// The paste and keygate surfaces are separate types in the adapter, and the
	// full conformanceRepo wants both. Composed here rather than in the adapter:
	// production wires them as separate fields, so merging them for the suite's
	// convenience would be the test shaping the design.
	kg namespacedKeygate
}

func (n namespacedRepo) AdmitNewKey(identity, subnet string, now time.Time, limit int,
	window time.Duration,
) (bool, error) {
	return n.kg.AdmitNewKey(identity, subnet, now, limit, window)
}

func (n namespacedRepo) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	return n.kg.SubnetSnapshot(subnet, now, window)
}

func (n namespacedRepo) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	return n.kg.SubnetsForIdentity(identity, now, window)
}

// The suite's site half needs a repo whose Sites view shares the SAME cells as
// its paste view, or the cross-quota and slug-collision subtests exercise two
// unrelated stores. Namespacing both through one prefix is what keeps them the
// same store.
func (n namespacedRepo) AppendManifestVersion(ctx context.Context, slug domain.Slug, m domain.Manifest,
	root domain.ManifestEntry, size int, userCap int64, now time.Time,
) (storage.AppendResult, error) {
	return n.inner.AppendManifestVersion(ctx, n.slug(slug), m, root, size, userCap, now)
}

func (n namespacedRepo) PreClaimSlug(ctx context.Context, slug domain.Slug, owner string, now time.Time) error {
	return n.inner.PreClaimSlug(ctx, n.slug(slug), n.owner(owner), now)
}

func (n namespacedRepo) ReleaseSlugClaim(ctx context.Context, slug domain.Slug, owner string) error {
	return n.inner.ReleaseSlugClaim(ctx, n.slug(slug), n.owner(owner))
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
	// Hand back BOTH identifiers the caller used, or the suite's assertions
	// would be reading the harness rather than the adapter. The identity matters
	// as much as the slug: storage.Sites compares the stored owner against the
	// caller's on every replace, so a prefix left on one side of that comparison
	// turns an owned site into a not-found.
	got.Slug = s
	got.Identity = domain.Identity(strings.TrimPrefix(got.Identity.String(), n.prefix))
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

func (n namespacedRepo) AppendVersionWithQuotaCheck(ctx context.Context, s domain.Slug,
	kind domain.ContentKind, sha string, size int, cap int64, at time.Time,
) (domain.AppendResult, error) {
	return n.inner.AppendVersionWithQuotaCheck(ctx, n.slug(s), kind, sha, size, cap, at)
}

func (n namespacedRepo) DeleteVersion(s domain.Slug, ver int) error {
	return n.inner.DeleteVersion(n.slug(s), ver)
}

func (n namespacedRepo) Delete(s domain.Slug, want domain.Identity, at time.Time) error {
	return n.inner.Delete(n.slug(s), domain.Identity(n.owner(want.String())), at)
}

func (n namespacedRepo) SetName(s domain.Slug, name string, want domain.Identity, at time.Time) error {
	return n.inner.SetName(n.slug(s), name, domain.Identity(n.owner(want.String())), at)
}

func TestOwnerIndexConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld owner-index conformance")
	}
	runOwnerIndexConformance(t, "celld", func(t *testing.T) ownerIndexRepo {
		return newNamespacedCelld(base)
	})
}

func TestLifecycleConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld lifecycle conformance")
	}
	runLifecycleConformance(t, "celld", func(t *testing.T) lifecycleRepo {
		return newNamespacedCelld(base)
	})
}

// namespacedKeygate keeps each run's identities and subnets distinct, for the
// same reason namespacedRepo does: celld cells are durable with no teardown, so
// a rerun that reused a subnet would inherit the previous run's admissions and
// read as a spurious cap refusal.
type namespacedKeygate struct {
	inner  *celld.KeyGateRepo
	prefix string
}

func (n namespacedKeygate) AdmitNewKey(identity, subnet string, now time.Time, limit int,
	window time.Duration,
) (bool, error) {
	return n.inner.AdmitNewKey(n.prefix+identity, n.prefix+subnet, now, limit, window)
}

func (n namespacedKeygate) SubnetSnapshot(subnet string, now time.Time, window time.Duration) (int, time.Time, error) {
	return n.inner.SubnetSnapshot(n.prefix+subnet, now, window)
}

func (n namespacedKeygate) SubnetsForIdentity(identity string, now time.Time, window time.Duration) (int, error) {
	return n.inner.SubnetsForIdentity(n.prefix+identity, now, window)
}

func TestKeygateConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld keygate conformance")
	}
	runKeygateConformance(t, "celld", func(t *testing.T) keygateRepo {
		return namespacedKeygate{
			inner:  celld.NewKeyGateRepo(base, nil),
			prefix: celldNamespace() + "-",
		}
	})
}

// The site surface needs no celld-specific adapter: storage.Sites is pure
// vocabulary over the paste repo, so the celld repo drops straight into it.
// That this compiles at all is the claim being made.
var _ storage.SiteBackingRepo = (*celld.PasteRepo)(nil)

// newNamespacedCelld builds a paste+keygate view sharing ONE namespace, so the
// site suite's cross-quota subtests see a single store.
func newNamespacedCelld(base string) namespacedRepo {
	ns := celldNamespace()
	return namespacedRepo{
		inner:  celld.NewPasteRepo(base, nil),
		prefix: ns,
		kg:     namespacedKeygate{inner: celld.NewKeyGateRepo(base, nil), prefix: ns + "-"},
	}
}

func TestSiteConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld site conformance")
	}
	// A cell decides inside one event, so both byte caps hold exactly under
	// concurrency - unlike shale, whose per-identity check is a scan outside the
	// write and admits a bounded overshoot.
	caps := conformCaps{StrictQuotaUnderConcurrency: true, StrictIdentityQuotaUnderConcurrency: true}
	runSiteConformance(t, "celld", caps, func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		r := newNamespacedCelld(base)
		return r, storage.NewSites(r)
	})
}

// The remaining PasteAdmin surface, forwarded through the namespace. Mechanical
// by nature: the suite's fixed slugs have to reach the same cells the rest of
// the wrapper addresses.
func (n namespacedRepo) GetVersion(s domain.Slug, ver int) (domain.Version, error) {
	got, err := n.inner.GetVersion(n.slug(s), ver)
	if err != nil {
		return domain.Version{}, err
	}
	got.Slug = s
	return got, nil
}

func (n namespacedRepo) ListVersions(s domain.Slug) ([]domain.Version, error) {
	got, err := n.inner.ListVersions(n.slug(s))
	for i := range got {
		got[i].Slug = s
	}
	return got, err
}

func (n namespacedRepo) IsVersionServed(s domain.Slug, ver int) (bool, error) {
	return n.inner.IsVersionServed(n.slug(s), ver)
}

func (n namespacedRepo) SetPinnedVersion(s domain.Slug, v domain.Version) error {
	v.Slug = n.slug(s)
	return n.inner.SetPinnedVersion(n.slug(s), v)
}

func (n namespacedRepo) Unpin(s domain.Slug) error { return n.inner.Unpin(n.slug(s)) }

func (n namespacedRepo) OwnerSummary(o string, at time.Time) (domain.OwnerSummary, error) {
	return n.inner.OwnerSummary(n.owner(o), at)
}
