package storage_test

// The celld entry into the paste lifecycle conformance.
//
// This adapter spans two cells without a cross-cell transaction: the row is
// addressed by slug, while quota and index state are addressed by identity.
// The conformance suite proves that the intent protocol preserves lifecycle
// behavior across that boundary.
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
func (n namespacedRepo) AppendManifestVersion(ctx context.Context, slug domain.Slug, generation string,
	m domain.Manifest, root domain.ManifestEntry, size int, userCap int64, now time.Time,
) (storage.AppendResult, error) {
	return n.inner.AppendManifestVersion(ctx, n.slug(slug), generation, m, root, size, userCap, now)
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

func (n namespacedRepo) MarkReady(p domain.Paste) error {
	p.Slug = n.slug(p.Slug)
	p.Identity = domain.Identity(n.owner(p.Identity.String()))
	return n.inner.MarkReady(p)
}
func (n namespacedRepo) MarkFailed(p domain.Paste) error {
	p.Slug = n.slug(p.Slug)
	p.Identity = domain.Identity(n.owner(p.Identity.String()))
	return n.inner.MarkFailed(p)
}

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

func (n namespacedRepo) AppendVersionWithQuotaCheck(ctx context.Context, s domain.Slug, generation string,
	kind domain.ContentKind, sha string, size int, cap int64, at time.Time,
) (domain.AppendResult, error) {
	return n.inner.AppendVersionWithQuotaCheck(ctx, n.slug(s), generation, kind, sha, size, cap, at)
}

func (n namespacedRepo) DeleteVersion(s domain.Slug, generation string, ver int) error {
	return n.inner.DeleteVersion(n.slug(s), generation, ver)
}

func (n namespacedRepo) Delete(s domain.Slug, want domain.Identity, at time.Time) error {
	return n.inner.Delete(n.slug(s), domain.Identity(n.owner(want.String())), at)
}

func (n namespacedRepo) SetName(s domain.Slug, name string, want domain.Identity, at time.Time) error {
	return n.inner.SetName(n.slug(s), name, domain.Identity(n.owner(want.String())), at)
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

func (n namespacedRepo) SetPinnedVersion(s domain.Slug, generation string, v domain.Version) error {
	v.Slug = n.slug(s)
	return n.inner.SetPinnedVersion(n.slug(s), generation, v)
}

func (n namespacedRepo) Unpin(s domain.Slug, generation string) error {
	return n.inner.Unpin(n.slug(s), generation)
}

func (n namespacedRepo) OwnerSummary(o string, at time.Time) (domain.OwnerSummary, error) {
	return n.inner.OwnerSummary(n.owner(o), at)
}

// namespacedRooms keeps each run's app slugs distinct, for the reason the paste
// wrapper does: cells are durable and the suite's app slugs are fixed.
type namespacedRooms struct {
	inner  *celld.RoomRepo
	prefix string
}

func (n namespacedRooms) app(s domain.Slug) domain.Slug { return domain.Slug(n.prefix + string(s)) }

func (n namespacedRooms) CreateRoom(room domain.Room, subnet string, appCap int64, now time.Time) error {
	room.AppSlug = n.app(room.AppSlug)
	return n.inner.CreateRoom(room, n.prefix+subnet, appCap, now)
}

func (n namespacedRooms) GetRoom(app domain.Slug, id domain.RoomID) (domain.Room, error) {
	got, err := n.inner.GetRoom(n.app(app), id)
	if err != nil {
		return domain.Room{}, err
	}
	got.AppSlug = app
	return got, nil
}

func (n namespacedRooms) GetValue(app domain.Slug, id domain.RoomID, key string) ([]byte, error) {
	return n.inner.GetValue(n.app(app), id, key)
}

func (n namespacedRooms) ScanRoom(app domain.Slug, id domain.RoomID) (domain.RoomKV, error) {
	return n.inner.ScanRoom(n.app(app), id)
}

func (n namespacedRooms) PutValue(app domain.Slug, id domain.RoomID, key string, val []byte,
	appCap int64, now time.Time,
) (uint64, error) {
	return n.inner.PutValue(n.app(app), id, key, val, appCap, now)
}

func (n namespacedRooms) DeleteValue(app domain.Slug, id domain.RoomID, key string, now time.Time) (uint64, error) {
	return n.inner.DeleteValue(n.app(app), id, key, now)
}

func (n namespacedRooms) CountRoomCreates(app domain.Slug, subnet string, now time.Time,
	window time.Duration,
) (int, int, error) {
	return n.inner.CountRoomCreates(n.app(app), n.prefix+subnet, now, window)
}

func (n namespacedRooms) PushKey(app domain.Slug) (string, error) {
	return n.inner.PushKey(n.app(app))
}

func (n namespacedRooms) PutPushSubscription(app domain.Slug, id domain.RoomID, sub domain.PushSubscription,
	now time.Time,
) error {
	return n.inner.PutPushSubscription(n.app(app), id, sub, now)
}

func (n namespacedRooms) DeletePushSubscription(app domain.Slug, id domain.RoomID, endpoint string) error {
	return n.inner.DeletePushSubscription(n.app(app), id, endpoint)
}

func (n namespacedRooms) ListPushSubscriptions(app domain.Slug, id domain.RoomID) ([]domain.PushSubscriptionSummary, error) {
	return n.inner.ListPushSubscriptions(n.app(app), id)
}

func (n namespacedRooms) PutPushSchedule(app domain.Slug, id domain.RoomID, sched domain.PushSchedule, now time.Time) error {
	return n.inner.PutPushSchedule(n.app(app), id, sched, now)
}

func (n namespacedRooms) GetPushSchedule(app domain.Slug, id domain.RoomID) (domain.PushSchedule, error) {
	return n.inner.GetPushSchedule(n.app(app), id)
}

func (n namespacedRooms) TestPush(app domain.Slug, id domain.RoomID, now time.Time) (domain.PushTestResult, error) {
	return n.inner.TestPush(n.app(app), id, now)
}

// The FULL contract suite against celld, not a subset.
//
// It ran as three partial entries first - lifecycle, owner index, sites - and
// the paste ADMIN surface fell in the gap between them: versions, pins and
// tombstones went unverified, and a staging smoke run found two real defects
// there that no test would have. A backend either passes the whole contract or
// its gaps are found by someone else.
func TestConformance_Celld(t *testing.T) {
	base := os.Getenv("CELLD_TEST_ENDPOINT")
	if base == "" {
		t.Skip("CELLD_TEST_ENDPOINT not set; skipping the celld conformance")
	}
	newRepo := func(t *testing.T) conformanceRepo { return newNamespacedCelld(base) }
	newSites := func(t *testing.T) (conformanceRepo, conformanceSiteRepo) {
		r := newNamespacedCelld(base)
		return r, storage.NewSites(r)
	}
	newRooms := func(t *testing.T) roomConformanceStores {
		r := newNamespacedCelld(base)
		return roomConformanceStores{
			Rooms: namespacedRooms{inner: celld.NewRoomRepo(base, nil), prefix: r.prefix},
			Paste: r,
			Site:  storage.NewSites(r),
		}
	}
	runConformanceWithSites(t, "celld", newRepo, newSites, newRooms)
	runKeygateConformance(t, "celld", func(t *testing.T) keygateRepo { return newNamespacedCelld(base).kg })
}
