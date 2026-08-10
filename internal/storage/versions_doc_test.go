package storage_test

// The versions document (docs/SPEC.md "The versions document (v2 version
// index)"): a pre-migration paste is healed by its first version-set WRITE
// from the legacy rows, reads fall back to those rows READ-ONLY while no
// document exists, a heal that skipped a row marks the document INCOMPLETE and
// the operations that need the whole set refuse rather than act, and the
// serving path is untouched throughout.
//
// The pre-migration shape is staged by writing through the normal API and then
// deleting the versions_doc key: the legacy rows a document release also
// maintains are exactly what a pre-document deployment left behind.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/shale/pkg/cluster"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// --- helpers ---------------------------------------------------------------

func versionsDocPresent(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) bool {
	t.Helper()
	raw, err := repo.GetRawForTest(storage.VersionsDocKeyForTest(slug))
	if err != nil {
		t.Fatalf("read versions doc: %v", err)
	}
	return len(raw) > 0
}

// deleteVersionsDoc regresses a paste to the pre-migration shape. Tolerant of
// an already-absent document, so it can run after any operation.
func deleteVersionsDoc(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) {
	t.Helper()
	if err := repo.DeleteRawForTest(storage.VersionsDocKeyForTest(slug)); err != nil {
		t.Fatalf("delete versions doc: %v", err)
	}
	if versionsDocPresent(t, repo, slug) {
		t.Fatal("versions doc still present after delete")
	}
}

// versionsDocFields reads the document's own bookkeeping.
func versionsDocFields(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) (nums []int, highWater int, complete bool) {
	t.Helper()
	raw, err := repo.GetRawForTest(storage.VersionsDocKeyForTest(slug))
	if err != nil || len(raw) == 0 {
		t.Fatalf("read versions doc: err=%v len=%d", err, len(raw))
	}
	var doc struct {
		Versions []struct {
			VerNum int `json:"ver_num"`
		} `json:"versions"`
		HighWater int  `json:"high_water"`
		Complete  bool `json:"complete"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode versions doc: %v", err)
	}
	for _, v := range doc.Versions {
		nums = append(nums, v.VerNum)
	}
	return nums, doc.HighWater, doc.Complete
}

// setHeadLatestVersion rewrites the head row's numbering mark in place, the
// shape a previous binary's append leaves. mark 0 removes the field, the shape
// of a row written before it existed.
func setHeadLatestVersion(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, mark int) {
	t.Helper()
	key := storage.LegacyPasteKeyForTest(slug)
	raw, err := repo.GetRawForTest(key)
	if err != nil || len(raw) == 0 {
		t.Fatalf("read head row: err=%v len=%d", err, len(raw))
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode head row: %v", err)
	}
	if mark == 0 {
		delete(row, "latest_version")
	} else {
		row["latest_version"] = mark
	}
	out, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode head row: %v", err)
	}
	if err := repo.PutRawForTest(key, out); err != nil {
		t.Fatalf("write head row: %v", err)
	}
}

// versionsDocGen reads the generation the document claims it was built from.
func versionsDocGen(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) int {
	t.Helper()
	raw, err := repo.GetRawForTest(storage.VersionsDocKeyForTest(slug))
	if err != nil || len(raw) == 0 {
		t.Fatalf("read versions doc: err=%v len=%d", err, len(raw))
	}
	var doc struct {
		Gen int `json:"gen"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode versions doc: %v", err)
	}
	return doc.Gen
}

// headFields reads the head row's version-set bookkeeping.
func headFields(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) (latest, gen int) {
	t.Helper()
	raw, err := repo.GetRawForTest(storage.LegacyPasteKeyForTest(slug))
	if err != nil || len(raw) == 0 {
		t.Fatalf("read head row: err=%v len=%d", err, len(raw))
	}
	var row struct {
		LatestVersion int `json:"latest_version"`
		VersionsGen   int `json:"versions_gen"`
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode head row: %v", err)
	}
	return row.LatestVersion, row.VersionsGen
}

// previousBinaryTombstoneVersion stages what a binary that predates the
// versions document leaves behind for a per-version delete: the legacy row's
// deleted flag flips in place, latest_version does not move (numbers are never
// reused), the document is not touched at all, and the head loses the
// versions_gen field because that binary's struct has no such field to
// re-marshal.
func previousBinaryTombstoneVersion(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, ver int) {
	t.Helper()
	verKey := storage.LegacyVersionKeyForTest(slug, ver)
	raw, err := repo.GetRawForTest(verKey)
	if err != nil || len(raw) == 0 {
		t.Fatalf("read v%d's row: err=%v len=%d", ver, err, len(raw))
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode v%d's row: %v", ver, err)
	}
	row["deleted"] = true
	out, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode v%d's row: %v", ver, err)
	}
	if err := repo.PutRawForTest(verKey, out); err != nil {
		t.Fatalf("write v%d's row: %v", ver, err)
	}
	stripHeadVersionsGen(t, repo, slug)
}

// stripHeadVersionsGen removes versions_gen from the head row, the shape any
// head rewrite by a binary without that field leaves.
func stripHeadVersionsGen(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug) {
	t.Helper()
	key := storage.LegacyPasteKeyForTest(slug)
	raw, err := repo.GetRawForTest(key)
	if err != nil || len(raw) == 0 {
		t.Fatalf("read head row: err=%v len=%d", err, len(raw))
	}
	var row map[string]any
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("decode head row: %v", err)
	}
	delete(row, "versions_gen")
	out, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode head row: %v", err)
	}
	if err := repo.PutRawForTest(key, out); err != nil {
		t.Fatalf("write head row: %v", err)
	}
}

func verNums(vs []domain.Version) []int {
	out := make([]int, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.VerNum)
	}
	return out
}

// captureRepoLog redirects the default logger (what a repo with no Logger of
// its own writes to) for the duration of the test.
func captureRepoLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// countLogLines counts captured log LINES carrying every substring, so a
// per-read report can be counted apart from a per-write one that shares a
// prefix.
func countLogLines(buf *bytes.Buffer, substrs ...string) int {
	n := 0
	for line := range strings.SplitSeq(buf.String(), "\n") {
		all := true
		for _, s := range substrs {
			if !strings.Contains(line, s) {
				all = false
				break
			}
		}
		if all && line != "" {
			n++
		}
	}
	return n
}

// deployManifest builds a directory manifest, one file named for this version
// so which manifest a head serves is decidable from the head alone.
func deployManifest(ver int) domain.Manifest {
	m := domain.NewManifest()
	m.Add("index.html", domain.ManifestEntry{
		SHA: fmt.Sprintf("sha-idx-v%d", ver), Size: 100, CompressedSize: 40,
		ContentType: "text/html", Kind: string(domain.KindHTML),
		BlobID: fmt.Sprintf("blob-idx-v%d", ver)})
	m.Add(fmt.Sprintf("v%d-only.css", ver), domain.ManifestEntry{
		SHA: fmt.Sprintf("sha-only-v%d", ver), Size: 20, CompressedSize: 9,
		ContentType: "text/css", BlobID: fmt.Sprintf("blob-only-v%d", ver)})
	return m
}

// deployDirectory inserts a directory paste and redeploys it up to versions.
func deployDirectory(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, owner string, versions int, now time.Time) {
	t.Helper()
	ctx := context.Background()
	v1 := deployManifest(1)
	root, _ := v1.Lookup("/")
	if err := repo.InsertWithQuotaCheck(ctx, domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Status: domain.PasteStatusReady,
		Kind: domain.KindSite, ContentSHA: root.SHA, Size: 1000,
		CreatedAt: now, UpdatedAt: now, Manifest: v1}, 0, now); err != nil {
		t.Fatalf("insert directory: %v", err)
	}
	repo.WaitPendingConfirms()
	for ver := 2; ver <= versions; ver++ {
		m := deployManifest(ver)
		r, _ := m.Lookup("/")
		if _, err := repo.AppendManifestVersion(ctx, slug, m, r, 1000, 0, now); err != nil {
			t.Fatalf("redeploy v%d: %v", ver, err)
		}
	}
}

// localCount counts the keys under prefix physically resident on the node. At
// ReplicationFactor=1 that is every key, which is what makes a survivor
// countable rather than merely unreachable through the read API.
func localCount(t *testing.T, repo *storage.ShaleRepo, prefix string) int {
	t.Helper()
	n, err := repo.LocalKeyCountForTest([]byte(prefix))
	if err != nil {
		t.Fatalf("count %q: %v", prefix, err)
	}
	return n
}

// stageBlob puts one blob's bytes on the slug's unit and returns the ref a
// write binds, plus the epoch that write must carry (an unclaimed bind is
// fenced as a recovery takeover).
func stageBlob(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, body string) (cluster.BlobRef, int64) {
	t.Helper()
	ctx := context.Background()
	routeKey := repo.RouteKeyForSlug(slug.String())
	ref, err := repo.StageBlobStream(ctx, routeKey, strings.NewReader(body), int64(len(body)), body)
	if err != nil {
		t.Fatalf("stage blob: %v", err)
	}
	epoch, err := repo.ClaimBlobOwnership(ctx, slug)
	if err != nil {
		t.Fatalf("claim blob ownership: %v", err)
	}
	return ref, epoch
}

func blobWriteCtx(ref cluster.BlobRef, epoch int64) context.Context {
	return storage.WithBlobOwnerEpoch(
		storage.WithPendingBinds(context.Background(), []cluster.BlobRef{ref}), epoch)
}

// insertWithBlob creates a paste whose v1 has a real bound blob.
func insertWithBlob(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, owner, sha string, now time.Time) {
	t.Helper()
	ref, epoch := stageBlob(t, repo, slug, sha)
	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Status: domain.PasteStatusReady,
		Kind: domain.KindHTML, ContentSHA: sha, Size: len(sha), CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(blobWriteCtx(ref, epoch), p, 0, now); err != nil {
		t.Fatalf("insert with blob: %v", err)
	}
	repo.WaitPendingConfirms()
}

// appendWithBlob adds a version with a real bound blob.
func appendWithBlob(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, sha string, now time.Time) {
	t.Helper()
	ref, epoch := stageBlob(t, repo, slug, sha)
	if _, err := repo.AppendVersionWithQuotaCheck(blobWriteCtx(ref, epoch), slug,
		domain.KindHTML, sha, len(sha), 0, now); err != nil {
		t.Fatalf("append with blob: %v", err)
	}
}

// --- a. characterization: identical on BOTH representations ----------------

// The version surface's observable behavior (ordering, numbering-never-reuses,
// tombstone rendering, pin and unpin rolls, the live-byte read-through) must be
// the same whether the document answers or the legacy rows do. The legacy pass
// deletes the document after every mutation, so every following operation meets
// a pre-migration paste.
func TestVersionsDoc_Characterization_ListNumberingTombstonesPins(t *testing.T) {
	for _, tc := range []struct {
		name    string
		regress func(*testing.T, *storage.ShaleRepo, domain.Slug)
	}{
		{name: "doc", regress: func(*testing.T, *storage.ShaleRepo, domain.Slug) {}},
		{name: "legacy-rows", regress: deleteVersionsDoc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			characterizeVersionSurface(t, tc.regress)
		})
	}
}

// regress runs after every operation that could write the document.
func characterizeVersionSurface(t *testing.T, regress func(*testing.T, *storage.ShaleRepo, domain.Slug)) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:vchar"
	slug := domain.Slug("vchar001")
	ctx := context.Background()
	mutated := func() { regress(t, repo, slug) }

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-vchar-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	mutated()
	for i, size := range []int{200, 100} {
		res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vchar-v%d", i+2), size, 0, now)
		if err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
		if res.NewVer != i+2 {
			t.Fatalf("append numbering: got v%d, want v%d", res.NewVer, i+2)
		}
		mutated()
	}

	// Tombstone v2: it stays listed, flagged deleted.
	if err := repo.DeleteVersion(slug, 2); err != nil {
		t.Fatalf("tombstone v2: %v", err)
	}
	mutated()
	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list versions: %v", err)
	}
	if got := verNums(vers); !reflect.DeepEqual(got, []int{3, 2, 1}) {
		t.Fatalf("list order: got %v, want [3 2 1] (newest first, tombstones stay listed)", got)
	}
	if !vers[1].Deleted || vers[0].Deleted || vers[2].Deleted {
		t.Fatalf("tombstone flags: got %+v, want only v2 deleted", vers)
	}
	if vers[2].ContentSHA != "sha-vchar-v1" || vers[2].Size != 300 || vers[2].Kind != domain.KindHTML {
		t.Fatalf("v1 fields: got %+v", vers[2])
	}

	// Numbering counts tombstones: the next append is 4, not 2.
	res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vchar-v4", 50, 0, now)
	if err != nil {
		t.Fatalf("append after tombstone: %v", err)
	}
	if res.NewVer != 4 {
		t.Fatalf("numbering after tombstone: got v%d, want v4 (numbers are never reused)", res.NewVer)
	}
	mutated()

	// A missing version is ErrNotFound; re-tombstoning is a silent no-op.
	if err := repo.DeleteVersion(slug, 99); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("tombstone of a missing version: got %v, want ErrNotFound", err)
	}
	if err := repo.DeleteVersion(slug, 2); err != nil {
		t.Fatalf("re-tombstone must be a no-op: %v", err)
	}
	mutated()

	// Pin v1: the URL serves v1's content; appends record but do not roll.
	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	mutated()
	got, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ContentSHA != "sha-vchar-v1" || got.PinnedVersion != 1 {
		t.Fatalf("pinned head: got sha=%q pin=%d, want sha-vchar-v1 pin=1", got.ContentSHA, got.PinnedVersion)
	}
	res, err = repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vchar-v5", 40, 0, now)
	if err != nil {
		t.Fatalf("append while pinned: %v", err)
	}
	if res.NewVer != 5 || !res.WasPinned {
		t.Fatalf("append while pinned: got %+v, want v5 with WasPinned", res)
	}
	mutated()
	if got, err = repo.Get(slug); err != nil || got.ContentSHA != "sha-vchar-v1" {
		t.Fatalf("pinned head must not roll on append: got sha=%q err=%v", got.ContentSHA, err)
	}

	// Tombstone the un-served max (5), then append: still max+1 = 6.
	if err := repo.DeleteVersion(slug, 5); err != nil {
		t.Fatalf("tombstone v5: %v", err)
	}
	mutated()
	res, err = repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vchar-v6", 30, 0, now)
	if err != nil {
		t.Fatalf("append after max tombstone: %v", err)
	}
	if res.NewVer != 6 {
		t.Fatalf("numbering after max tombstone: got v%d, want v6", res.NewVer)
	}
	mutated()

	// Unpin rolls the head to the latest LIVE version (v6).
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	mutated()
	if got, err = repo.Get(slug); err != nil ||
		got.ContentSHA != "sha-vchar-v6" || got.PinnedVersion != 0 {
		t.Fatalf("unpinned head: got sha=%q pin=%d err=%v, want sha-vchar-v6 pin=0", got.ContentSHA, got.PinnedVersion, err)
	}

	// The quota read-through sums live versions only: 300+100+50+30.
	// (v2=200 and v5=40 are tombstoned.)
	entryKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	if err := repo.PutEmptyBackendForTest(entryKey); err != nil {
		t.Fatalf("plant legacy empty entry: %v", err)
	}
	deleteOwnerDoc(t, repo, owner)
	if got := mustSum(t, repo, owner, now); got != 480 {
		t.Fatalf("live-byte read-through: got %d, want 480", got)
	}
}

// --- b. heal-on-write ------------------------------------------------------

func TestVersionsDoc_FirstMutationHealsFromLegacyRows(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	owner := "key:vheal"
	slug := domain.Slug("vheal001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: domain.Identity(owner), Kind: domain.KindHTML,
		ContentSHA: "sha-vheal-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vheal-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("tombstone v1: %v", err)
	}
	before, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	// Pre-migration shape: legacy rows only.
	deleteVersionsDoc(t, repo, slug)

	// One mutation migrates the paste, carrying the pre-existing set.
	res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vheal-v3", 100, 0, now)
	if err != nil {
		t.Fatalf("healing append: %v", err)
	}
	if res.NewVer != 3 {
		t.Fatalf("healed numbering: got v%d, want v3 (max over the healed set, tombstones included)", res.NewVer)
	}
	if !versionsDocPresent(t, repo, slug) {
		t.Fatal("a mutation on a pre-document paste must heal the document")
	}
	nums, hw, complete := versionsDocFields(t, repo, slug)
	if !reflect.DeepEqual(nums, []int{1, 2, 3}) || hw != 3 || !complete {
		t.Fatalf("healed document = versions %v high_water %d complete %v, want [1 2 3] 3 true", nums, hw, complete)
	}
	after, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list after: %v", err)
	}
	if len(after) != 3 || after[0].VerNum != 3 {
		t.Fatalf("list after heal: got %+v, want v3 on top of the pre-existing set", after)
	}
	if !reflect.DeepEqual(after[1:], before) {
		t.Fatalf("heal must preserve the pre-existing render:\n got %+v\n want %+v", after[1:], before)
	}
	if !after[2].Deleted {
		t.Fatalf("healed document lost the v1 tombstone: %+v", after)
	}
	// The live sum reads the healed document: v2+v3, v1 tombstoned.
	entryKey := storage.IdentityPasteKeyForTest(owner, slug.String())
	if err := repo.PutEmptyBackendForTest(entryKey); err != nil {
		t.Fatalf("plant legacy empty entry: %v", err)
	}
	deleteOwnerDoc(t, repo, owner)
	if got := mustSum(t, repo, owner, now); got != 300 {
		t.Fatalf("live sum after heal: got %d, want 300", got)
	}
}

// A read must never migrate the paste: the fallback is READ-ONLY, which is what
// keeps reads free of write races by construction.
func TestVersionsDoc_ReadsFallBackWithoutWritingTheDoc(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vro00001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vro", Kind: domain.KindHTML,
		ContentSHA: "sha-vro-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vro-v2", 200, 0, now); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("tombstone v1: %v", err)
	}
	fromDoc, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("document list: %v", err)
	}
	docOne, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("document get v1: %v", err)
	}
	docID, docResolveErr := repo.ResolveBlobID(slug, "sha-vro-v1")

	deleteVersionsDoc(t, repo, slug)

	fromLegacy, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("legacy fallback list: %v", err)
	}
	if !reflect.DeepEqual(fromDoc, fromLegacy) {
		t.Fatalf("fallback render diverged:\n doc:    %+v\n legacy: %+v", fromDoc, fromLegacy)
	}
	legacyOne, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("legacy get v1: %v", err)
	}
	if !reflect.DeepEqual(docOne, legacyOne) {
		t.Fatalf("GetVersion diverged:\n doc:    %+v\n legacy: %+v", docOne, legacyOne)
	}
	legacyID, legacyResolveErr := repo.ResolveBlobID(slug, "sha-vro-v1")
	if docID != legacyID || (docResolveErr == nil) != (legacyResolveErr == nil) {
		t.Fatalf("blob resolution diverged: doc (%q, %v) vs legacy (%q, %v)", docID, docResolveErr, legacyID, legacyResolveErr)
	}
	if versionsDocPresent(t, repo, slug) {
		t.Fatal("a read on a pre-document paste must not write the document")
	}
}

// --- c. the heal fails OPEN ------------------------------------------------

// One unreadable legacy row must not brick the paste's write surface: the heal
// walk skips it and the mutation succeeds.
func TestVersionsDoc_HealFailsOpenOnUnreadableLegacyRow(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vfo00001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vfo", Kind: domain.KindHTML,
		ContentSHA: "sha-vfo-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vfo-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	deleteVersionsDoc(t, repo, slug)
	if err := repo.PutRawForTest(storage.LegacyVersionKeyForTest(slug, 2), corruptJSON); err != nil {
		t.Fatalf("corrupt legacy v2: %v", err)
	}

	logs := captureRepoLog(t)
	res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vfo-v4", 50, 0, now)
	if err != nil {
		t.Fatalf("a mutation must heal AROUND an unreadable version row, not fail on it: %v", err)
	}
	if res.NewVer != 4 {
		t.Fatalf("healed numbering: got v%d, want v4", res.NewVer)
	}
	nums, _, _ := versionsDocFields(t, repo, slug)
	if !reflect.DeepEqual(nums, []int{1, 3, 4}) {
		t.Fatalf("healed document: got versions %v, want [1 3 4] (v2 skipped)", nums)
	}
	if !strings.Contains(logs.String(), "versions-doc heal for "+slug.String()) {
		t.Fatalf("the skipped row must be reported:\n%s", logs.String())
	}
	// The list renders what is present: an under-count, never a failure.
	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list after fail-open heal: %v", err)
	}
	if got := verNums(vers); !reflect.DeepEqual(got, []int{4, 3, 1}) {
		t.Fatalf("list after fail-open heal: got %v, want [4 3 1]", got)
	}
}

// --- d. the incomplete document refuses rather than acting -----------------

// The heal that skipped a row marks the document INCOMPLETE, and unpin then
// REFUSES instead of rolling the head to the truncated list's newest live
// version. Acting would supersede v3 permanently: the paste would serve v1
// forever while v3's content still exists and is still the newest.
func TestVersionsDoc_IncompleteDocRefusesUnpinInsteadOfRollingBackwards(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vinc0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vinc", Kind: domain.KindHTML,
		ContentSHA: "sha-vinc-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vinc-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	// Pin v1 so unpin has a roll to make, and keep v3's row for later repair.
	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	v3Key := storage.LegacyVersionKeyForTest(slug, 3)
	v3Raw, err := repo.GetRawForTest(v3Key)
	if err != nil || len(v3Raw) == 0 {
		t.Fatalf("read v3's row: err=%v len=%d", err, len(v3Raw))
	}

	// Pre-migration, with v3 - the newest live version - unreadable.
	deleteVersionsDoc(t, repo, slug)
	if err := repo.PutRawForTest(v3Key, corruptJSON); err != nil {
		t.Fatalf("corrupt legacy v3: %v", err)
	}
	// A tombstone of v2 heals the document around the damage.
	if err := repo.DeleteVersion(slug, 2); err != nil {
		t.Fatalf("tombstone v2 must fail open: %v", err)
	}
	nums, hw, complete := versionsDocFields(t, repo, slug)
	if complete {
		t.Fatalf("a heal that skipped a row must mark the document INCOMPLETE: versions %v high_water %d", nums, hw)
	}
	if !reflect.DeepEqual(nums, []int{1, 2}) {
		t.Fatalf("healed document: got versions %v, want [1 2] (v3 skipped)", nums)
	}

	// The refusal, and nothing applied.
	if err := repo.Unpin(slug); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("unpin over an incomplete document: got %v, want ErrVersionsIncomplete", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get after the refused unpin: %v", err)
	}
	if head.PinnedVersion != 1 || head.ContentSHA != "sha-vinc-v1" {
		t.Fatalf("a refused unpin must apply nothing: pin=%d sha=%q, want pin=1 sha-vinc-v1", head.PinnedVersion, head.ContentSHA)
	}

	// A number the truncated list lacks is unavailable, not not-found.
	if _, err := repo.GetVersion(slug, 3); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("GetVersion of a number an incomplete document lacks: got %v, want ErrVersionsIncomplete", err)
	}
	// Operations that need only what is present proceed.
	if vers, err := repo.ListVersions(slug); err != nil || len(vers) != 2 {
		t.Fatalf("versions must still render over an incomplete document: got %+v err=%v", vers, err)
	}

	// Repair the row; the next mutation's heal reads everything and clears the
	// flag WITHOUT the document being removed first, and the unpin it refused
	// now reaches v3 - which is the version the refusal preserved.
	if err := repo.PutRawForTest(v3Key, v3Raw); err != nil {
		t.Fatalf("repair v3's row: %v", err)
	}
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vinc-v4", 10, 0, now); err != nil {
		t.Fatalf("re-healing append: %v", err)
	}
	if _, _, complete := versionsDocFields(t, repo, slug); !complete {
		t.Fatal("a heal that reads everything must clear the incomplete flag")
	}
	if err := repo.DeleteVersion(slug, 4); err != nil {
		t.Fatalf("tombstone v4: %v", err)
	}
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin over a repaired document: %v", err)
	}
	head, err = repo.Get(slug)
	if err != nil {
		t.Fatalf("get after unpin: %v", err)
	}
	if head.PinnedVersion != 0 || head.ContentSHA != "sha-vinc-v3" {
		t.Fatalf("the repaired unpin must roll to v3, the newest live version: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
}

// --- e. a corrupt document is not a per-paste outage -----------------------

func TestVersionsDoc_CorruptDocReadsFallBackAndWriteRebuilds(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vcor0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vcor", Kind: domain.KindHTML,
		ContentSHA: "sha-vcor-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vcor-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	healthy, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list before: %v", err)
	}

	if err := repo.PutRawForTest(storage.VersionsDocKeyForTest(slug), corruptJSON); err != nil {
		t.Fatalf("corrupt the document: %v", err)
	}

	// Reads fail open to the legacy rows.
	fallback, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list over a corrupt document must fall back, not fail: %v", err)
	}
	if !reflect.DeepEqual(fallback, healthy) {
		t.Fatalf("fallback render diverged:\n got:  %+v\n want: %+v", fallback, healthy)
	}
	if got, err := repo.GetVersion(slug, 2); err != nil || got.ContentSHA != "sha-vcor-v2" {
		t.Fatalf("GetVersion over a corrupt document: got %+v err=%v", got, err)
	}
	// Unpin takes the legacy path rather than refusing forever.
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin over a corrupt document must fall back: %v", err)
	}

	// The next version write replaces the corrupt document with a good one.
	res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vcor-v3", 100, 0, now)
	if err != nil {
		t.Fatalf("append over a corrupt document must rebuild it, not fail: %v", err)
	}
	if res.NewVer != 3 {
		t.Fatalf("numbering over a rebuilt document: got v%d, want v3", res.NewVer)
	}
	raw, err := repo.GetRawForTest(storage.VersionsDocKeyForTest(slug))
	if err != nil {
		t.Fatalf("read document after rebuild: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("rebuilt document must decode; got %q", raw)
	}
	nums, hw, complete := versionsDocFields(t, repo, slug)
	if !reflect.DeepEqual(nums, []int{1, 2, 3}) || hw != 3 || !complete {
		t.Fatalf("rebuilt document = versions %v high_water %d complete %v, want [1 2 3] 3 true", nums, hw, complete)
	}
}

// --- f. a row that could not be READ is never overwritten ------------------

// The tombstone flips the legacy row in place, so a row that will not decode is
// left EXACTLY as it is: overwriting would destroy the bytes a repair needs and
// manufacture a row that reads back cleanly while describing content it does
// not have.
func TestVersionsDoc_DeleteVersionLeavesAnUnreadableLegacyRowUntouched(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vunt0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vunt", Kind: domain.KindHTML,
		ContentSHA: "sha-vunt-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vunt-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	// v1's legacy row is damaged AFTER the document already carries it, so the
	// document holds the version while the row does not read.
	v1Key := storage.LegacyVersionKeyForTest(slug, 1)
	if err := repo.PutRawForTest(v1Key, corruptJSON); err != nil {
		t.Fatalf("corrupt v1's row: %v", err)
	}

	logs := captureRepoLog(t)
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("tombstoning a version whose legacy row is unreadable must succeed: %v", err)
	}
	if raw, err := repo.GetRawForTest(v1Key); err != nil {
		t.Fatalf("read v1's row: %v", err)
	} else if !bytes.Equal(raw, corruptJSON) {
		t.Fatalf("the unreadable row must be left byte-for-byte untouched; got %q", raw)
	}
	if !strings.Contains(logs.String(), "unreadable while tombstoning") {
		t.Fatalf("the untouched row must be reported:\n%s", logs.String())
	}
	// The tombstone landed in the representation that is authoritative for it.
	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1 after the tombstone: %v", err)
	}
	if !v1.Deleted {
		t.Fatal("the document record must carry the tombstone")
	}
	// And its bytes left the paste's live total.
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get head: %v", err)
	}
	if head.ContentSHA != "sha-vunt-v2" {
		t.Fatalf("head must still serve v2: %q", head.ContentSHA)
	}
}

// --- g. the serving path is unchanged --------------------------------------

// A served request is answered from pastes/<slug> alone. The document must not
// change that in any of its three states: present, absent, corrupt.
func TestVersionsDoc_ServingPathUnchanged(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vsrv0001")
	deployDirectory(t, repo, slug, "key:vsrv", 2, now)

	// The served snapshot: the raw head row plus everything a request derives
	// from it - the rendered paste, its whole manifest, and the blob id each
	// served file resolves to.
	snapshot := func(t *testing.T, stage string) (string, domain.Paste, map[string]string) {
		t.Helper()
		raw, err := repo.GetRawForTest(storage.LegacyPasteKeyForTest(slug))
		if err != nil || len(raw) == 0 {
			t.Fatalf("%s: read head row: err=%v len=%d", stage, err, len(raw))
		}
		got, err := repo.Get(slug)
		if err != nil {
			t.Fatalf("%s: get: %v", stage, err)
		}
		if len(got.Manifest.Files) == 0 {
			t.Fatalf("%s: the head serves an EMPTY manifest: every path 404s", stage)
		}
		ids := map[string]string{}
		for path, e := range got.Manifest.Files {
			id, rerr := repo.ResolveBlobID(slug, e.SHA)
			if rerr != nil {
				t.Fatalf("%s: resolve %s: %v", stage, path, rerr)
			}
			ids[path] = id
		}
		return string(raw), got, ids
	}

	wantRaw, wantPaste, wantIDs := snapshot(t, "document present")
	if _, ok := wantPaste.Manifest.Files["v2-only.css"]; !ok {
		t.Fatalf("fixture: the head must serve v2's files: %+v", wantPaste.Manifest.Files)
	}

	for _, stage := range []struct {
		name   string
		damage func(*testing.T)
	}{
		{name: "document absent", damage: func(t *testing.T) { deleteVersionsDoc(t, repo, slug) }},
		{name: "document corrupt", damage: func(t *testing.T) {
			if err := repo.PutRawForTest(storage.VersionsDocKeyForTest(slug), corruptJSON); err != nil {
				t.Fatalf("corrupt the document: %v", err)
			}
		}},
	} {
		stage.damage(t)
		gotRaw, gotPaste, gotIDs := snapshot(t, stage.name)
		if gotRaw != wantRaw {
			t.Fatalf("%s: the head row changed:\n got:  %s\n want: %s", stage.name, gotRaw, wantRaw)
		}
		if !reflect.DeepEqual(gotPaste, wantPaste) {
			t.Fatalf("%s: the served paste changed:\n got:  %+v\n want: %+v", stage.name, gotPaste, wantPaste)
		}
		if !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Fatalf("%s: the served files resolved differently:\n got:  %v\n want: %v", stage.name, gotIDs, wantIDs)
		}
	}
}

// A binary that predates the document appends by writing a version row and
// bumping the head's mark, leaving the document behind. Rolling forward onto
// that state must not re-issue the number the old binary already used: the
// document's own next number is stale, and only the head's mark says so.
func TestVersionsDoc_StaleDocAfterRollbackDoesNotReissueANumber(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vrbk0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vrbk", Kind: domain.KindHTML,
		ContentSHA: "sha-vrbk-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	// What the previous binary's append leaves: the row plus the head's mark,
	// with the document untouched and therefore stale.
	v2, err := storage.LegacyVersionValueForTest(2, domain.KindHTML, "sha-vrbk-v2", 200, now, false)
	if err != nil {
		t.Fatalf("encode v2 row: %v", err)
	}
	if err := repo.PutRawForTest(storage.LegacyVersionKeyForTest(slug, 2), v2); err != nil {
		t.Fatalf("write v2 row: %v", err)
	}
	setHeadLatestVersion(t, repo, slug, 2)

	res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vrbk-v3", 100, 0, now)
	if err != nil {
		t.Fatalf("append onto a stale document: %v", err)
	}
	if res.NewVer != 3 {
		t.Fatalf("append onto a stale document: got v%d, want v3 (v2 is already issued)", res.NewVer)
	}
	if raw, err := repo.GetRawForTest(storage.LegacyVersionKeyForTest(slug, 2)); err != nil {
		t.Fatalf("read v2's row: %v", err)
	} else if !bytes.Equal(raw, v2) {
		t.Fatalf("the previous binary's row must survive untouched; got %q", raw)
	}
}

// --- h. numbering never reuses a number the heal could not read ------------

// The mark is seeded from the KEY, which is readable even when the value is
// not, so the skipped version's number stays reserved and its row is never
// written over.
func TestVersionsDoc_NumberingNeverReusesAnUnreadableNumber(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vnum0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vnum", Kind: domain.KindHTML,
		ContentSHA: "sha-vnum-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vnum-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	// v3 - the HIGHEST number - is the one the heal cannot read. The head's
	// numbering mark is zeroed too (the shape of a row written before that
	// field existed), so its KEY is the only thing left that reserves 3.
	deleteVersionsDoc(t, repo, slug)
	v3Key := storage.LegacyVersionKeyForTest(slug, 3)
	if err := repo.PutRawForTest(v3Key, corruptJSON); err != nil {
		t.Fatalf("corrupt legacy v3: %v", err)
	}
	setHeadLatestVersion(t, repo, slug, 0)

	res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vnum-v4", 50, 0, now)
	if err != nil {
		t.Fatalf("healing append: %v", err)
	}
	if res.NewVer != 4 {
		t.Fatalf("the damaged version's number must stay reserved: got v%d, want v4", res.NewVer)
	}
	if raw, err := repo.GetRawForTest(v3Key); err != nil {
		t.Fatalf("read v3's row: %v", err)
	} else if !bytes.Equal(raw, corruptJSON) {
		t.Fatalf("a re-minted number would have written over the unreadable row; got %q", raw)
	}
	_, hw, complete := versionsDocFields(t, repo, slug)
	if hw != 4 || complete {
		t.Fatalf("document mark after the fail-open heal: high_water %d complete %v, want 4 false", hw, complete)
	}
	// The mark also survives a set that holds no such number at all: a second
	// heal from the same damaged rows must not lower it.
	deleteVersionsDoc(t, repo, slug)
	res, err = repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vnum-v5", 40, 0, now)
	if err != nil {
		t.Fatalf("second healing append: %v", err)
	}
	if res.NewVer != 5 {
		t.Fatalf("numbering after a second heal: got v%d, want v5", res.NewVer)
	}
}

// --- i. a document BEHIND the rows is a truncated document -----------------

// The head's numbering mark is the staleness oracle: it names a version that
// EXISTS, so a document whose highest record is below it is missing a real,
// live version while claiming to be complete. Every rolling deploy and every
// rollback produces exactly that state, because the previous binary appends by
// writing a row and bumping the mark without touching the document.
//
// Unpin must REFUSE there rather than roll the head to the truncated list's
// newest live version - which would supersede the newer version permanently -
// and the paste's next write must repair the document so the unpin it refused
// then reaches the version the refusal preserved.
func TestVersionsDoc_StaleDocRefusesUnpinThenConvergesOnTheNextWrite(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vstl0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vstl", Kind: domain.KindHTML,
		ContentSHA: "sha-vstl-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vstl-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}

	// The previous binary's append: a version row plus the head's mark, with
	// the document left exactly as it was.
	v4, err := storage.LegacyVersionValueForTest(4, domain.KindHTML, "sha-vstl-v4", 50, now, false)
	if err != nil {
		t.Fatalf("encode v4 row: %v", err)
	}
	if err := repo.PutRawForTest(storage.LegacyVersionKeyForTest(slug, 4), v4); err != nil {
		t.Fatalf("write v4 row: %v", err)
	}
	setHeadLatestVersion(t, repo, slug, 4)
	if nums, _, complete := versionsDocFields(t, repo, slug); !complete || len(nums) != 3 {
		t.Fatalf("fixture: the document must still claim complete over versions %v", nums)
	}

	// The refusal, and nothing applied. Rolling here would pick v3 - the
	// truncated list's newest live version - and supersede v4 permanently.
	if err := repo.Unpin(slug); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("unpin over a document behind the head's mark: got %v, want ErrVersionsIncomplete", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get after the refused unpin: %v", err)
	}
	if head.PinnedVersion != 1 || head.ContentSHA != "sha-vstl-v1" {
		t.Fatalf("a refused unpin must apply nothing: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
	// A number the stale document lacks is unavailable, not not-found: the
	// version exists.
	if _, err := repo.GetVersion(slug, 4); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("GetVersion of a number a stale document lacks: got %v, want ErrVersionsIncomplete", err)
	}

	// Convergence: a mutation re-walks the rows and the document stops being
	// behind, WITHOUT the document being removed first.
	if err := repo.DeleteVersion(slug, 2); err != nil {
		t.Fatalf("tombstone v2 over a stale document: %v", err)
	}
	nums, hw, complete := versionsDocFields(t, repo, slug)
	if !reflect.DeepEqual(nums, []int{1, 2, 3, 4}) || hw < 4 || !complete {
		t.Fatalf("the re-heal must adopt v4 and clear the flag: versions %v high_water %d complete %v", nums, hw, complete)
	}

	// The unpin it refused now rolls FORWARD, to the version the refusal
	// preserved.
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin over the repaired document: %v", err)
	}
	head, err = repo.Get(slug)
	if err != nil {
		t.Fatalf("get after unpin: %v", err)
	}
	if head.PinnedVersion != 0 || head.ContentSHA != "sha-vstl-v4" {
		t.Fatalf("unpin must roll to v4, the true newest live version: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
}

// The document is repairable: an incomplete flag clears on the next mutation
// once the damaged row reads again, with no operator step beyond the repair the
// log line names. A flag that only cleared when the document was REMOVED would
// hide every version the heal skipped for the life of the paste.
func TestVersionsDoc_IncompleteFlagClearsOnTheNextWriteAfterARepair(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vcnv0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vcnv", Kind: domain.KindHTML,
		ContentSHA: "sha-vcnv-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vcnv-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	v2Key := storage.LegacyVersionKeyForTest(slug, 2)
	v2Raw, err := repo.GetRawForTest(v2Key)
	if err != nil || len(v2Raw) == 0 {
		t.Fatalf("read v2's row: err=%v len=%d", err, len(v2Raw))
	}

	// Heal around a damaged row: the document loses v2 and records that.
	deleteVersionsDoc(t, repo, slug)
	if err := repo.PutRawForTest(v2Key, corruptJSON); err != nil {
		t.Fatalf("corrupt legacy v2: %v", err)
	}
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vcnv-v4", 10, 0, now); err != nil {
		t.Fatalf("healing append: %v", err)
	}
	nums, _, complete := versionsDocFields(t, repo, slug)
	if complete || !reflect.DeepEqual(nums, []int{1, 3, 4}) {
		t.Fatalf("fixture: expected an INCOMPLETE document over versions [1 3 4], got %v complete %v", nums, complete)
	}
	if _, err := repo.GetVersion(slug, 2); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("the skipped version must read as unavailable: got %v", err)
	}

	// The repair the log line asks for, and one ordinary mutation. The document
	// is NOT removed: if only a missing document triggered a heal, the flag
	// would latch here and v2 would stay hidden forever.
	if err := repo.PutRawForTest(v2Key, v2Raw); err != nil {
		t.Fatalf("repair v2's row: %v", err)
	}
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vcnv-v5", 10, 0, now); err != nil {
		t.Fatalf("converging append: %v", err)
	}
	nums, _, complete = versionsDocFields(t, repo, slug)
	if !complete || !reflect.DeepEqual(nums, []int{1, 2, 3, 4, 5}) {
		t.Fatalf("a walk that reads everything must clear the flag: versions %v complete %v", nums, complete)
	}
	if got, err := repo.GetVersion(slug, 2); err != nil || got.ContentSHA != "sha-vcnv-v2" {
		t.Fatalf("the repaired version must be readable again: %+v err=%v", got, err)
	}
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin must stop refusing once the document is whole: %v", err)
	}
}

// --- j. the served path never needs the document ---------------------------

// The head carries the served version's descriptor AND its whole manifest, so
// every sha a served request can name is answerable from it. A version index
// that cannot be read must therefore not fail a served request - otherwise one
// damaged value is a 500 for every asset of a static site.
func TestVersionsDoc_ServedRequestSurvivesAnUnreadableVersionIndex(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vres0001")
	deployDirectory(t, repo, slug, "key:vres", 2, now)

	// BOTH representations damaged: the document will not decode, and the
	// legacy rows it falls back to will not either. Any read of the version set
	// now fails outright.
	if err := repo.PutRawForTest(storage.VersionsDocKeyForTest(slug), corruptJSON); err != nil {
		t.Fatalf("corrupt the document: %v", err)
	}
	for ver := 1; ver <= 2; ver++ {
		if err := repo.PutRawForTest(storage.LegacyVersionKeyForTest(slug, ver), corruptJSON); err != nil {
			t.Fatalf("corrupt legacy v%d: %v", ver, err)
		}
	}

	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for path, e := range head.Manifest.Files {
		id, rerr := repo.ResolveBlobID(slug, e.SHA)
		if rerr != nil {
			t.Fatalf("a served file must resolve from the head alone: %s: %v", path, rerr)
		}
		if id == "" {
			t.Fatalf("a served file resolved to no blob id: %s", path)
		}
	}

	// The claim is about SERVED requests only, and the test would prove nothing
	// if the damaged set were unreachable: a sha only a NON-served version
	// names still has to go through it, and still fails.
	if _, rerr := repo.ResolveBlobID(slug, "sha-only-v1"); rerr == nil {
		t.Fatal("a sha no served record names must still reach the version set, so this test is not vacuous")
	}
}

// --- k. the whole-paste delete cannot leave a version behind ---------------

// The cascade comes from the version PREFIX, so a document that is BEHIND the
// rows cannot make the delete miss one. A missed version leaves its row AND its
// bound blob alive while the slug returns to the mint, and the next paste to
// take that slug inherits both.
func TestVersionsDoc_WholePasteDeleteCascadesFromThePrefixNotTheDocument(t *testing.T) {
	repo := newBlobShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vcas0001")
	owner := "key:vcas"

	insertWithBlob(t, repo, slug, owner, "sha-vcas-v1", now)
	appendWithBlob(t, repo, slug, "sha-vcas-v2", now)
	// Snapshot the document, then append v3 and put the snapshot back: the row
	// and its bound blob exist and the document has never heard of them, which
	// is exactly what an append by the previous binary leaves.
	docKey := storage.VersionsDocKeyForTest(slug)
	behind, err := repo.GetRawForTest(docKey)
	if err != nil || len(behind) == 0 {
		t.Fatalf("read the document: err=%v len=%d", err, len(behind))
	}
	appendWithBlob(t, repo, slug, "sha-vcas-v3", now)
	if err := repo.PutRawForTest(docKey, behind); err != nil {
		t.Fatalf("restore the behind document: %v", err)
	}
	if nums, _, complete := versionsDocFields(t, repo, slug); len(nums) != 2 || !complete {
		t.Fatalf("fixture: the document must claim complete over versions %v", nums)
	}
	if rows := localCount(t, repo, "versions/"+slug.String()+"/"); rows != 3 {
		t.Fatalf("fixture: want 3 version rows, got %d", rows)
	}
	if brefs := localCount(t, repo, "bref/{"+slug.String()+"}/"); brefs != 3 {
		t.Fatalf("fixture: want 3 bound blobs, got %d", brefs)
	}

	if err := repo.Delete(slug); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows := localCount(t, repo, "versions/"+slug.String()+"/"); rows != 0 {
		t.Fatalf("%d version row(s) survived the whole-paste delete", rows)
	}
	if brefs := localCount(t, repo, "bref/{"+slug.String()+"}/"); brefs != 0 {
		t.Fatalf("%d blob(s) stayed bound after the whole-paste delete", brefs)
	}
	if raw, err := repo.GetRawForTest(docKey); err != nil || len(raw) != 0 {
		t.Fatalf("the document survived the delete: err=%v len=%d", err, len(raw))
	}

	// What the survivors would have cost: the slug returns to the mint, and a
	// new paste on it must not inherit anything.
	insertWithBlob(t, repo, slug, "key:vcas-next", "sha-vcas-next", now)
	vers, err := repo.ListVersions(slug)
	if err != nil {
		t.Fatalf("list versions of the re-minted slug: %v", err)
	}
	if got := verNums(vers); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("a re-minted slug inherited versions %v from the deleted paste", got)
	}
}

// --- l. heal-walk bookkeeping ---------------------------------------------

// A record that disagrees with its own key cannot be placed: filing it under
// either number would answer confidently about a version that is not there.
// Both numbers stay reserved, and the document says it is not whole.
func TestVersionsDoc_HealSkipsARecordThatDisagreesWithItsKey(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vkey0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vkey", Kind: domain.KindHTML,
		ContentSHA: "sha-vkey-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	// A row filed at 2 whose record calls itself 9.
	mismatched, err := storage.LegacyVersionValueForTest(9, domain.KindHTML, "sha-vkey-mismatch", 10, now, false)
	if err != nil {
		t.Fatalf("encode the mismatched row: %v", err)
	}
	if err := repo.PutRawForTest(storage.LegacyVersionKeyForTest(slug, 2), mismatched); err != nil {
		t.Fatalf("write the mismatched row: %v", err)
	}
	deleteVersionsDoc(t, repo, slug)
	setHeadLatestVersion(t, repo, slug, 0)

	res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vkey-next", 10, 0, now)
	if err != nil {
		t.Fatalf("append over a mismatched row: %v", err)
	}
	if res.NewVer != 10 {
		t.Fatalf("both numbers must stay reserved: got v%d, want v10", res.NewVer)
	}
	nums, _, complete := versionsDocFields(t, repo, slug)
	if complete {
		t.Fatalf("a record that cannot be placed must mark the document incomplete: versions %v", nums)
	}
	if !reflect.DeepEqual(nums, []int{1, 10}) {
		t.Fatalf("the unplaceable record must not be filed: versions %v, want [1 10]", nums)
	}
}

// A document that will not decode is met on every read, so the report is
// per-pass, not per-read: one damaged value must not become an unbounded log
// stream.
func TestVersionsDoc_UndecodableDocumentReportsOncePerSlug(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vlog0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vlog", Kind: domain.KindHTML,
		ContentSHA: "sha-vlog-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if err := repo.PutRawForTest(storage.VersionsDocKeyForTest(slug), corruptJSON); err != nil {
		t.Fatalf("corrupt the document: %v", err)
	}

	logs := captureRepoLog(t)
	for range 10 {
		if _, err := repo.ListVersions(slug); err != nil {
			t.Fatalf("list over a corrupt document: %v", err)
		}
	}
	if n := strings.Count(logs.String(), "versions doc for "+slug.String()+" undecodable"); n != 1 {
		t.Fatalf("10 reads of one damaged document logged %d lines, want 1:\n%s", n, logs.String())
	}
}

// The plan's document decoded and the transaction's does not, so the write has
// no seed to rebuild from. Starting from an empty document would drop every
// version, so nothing is applied and the caller re-plans - which walks the rows
// and succeeds.
func TestVersionsDoc_UndecodableInsideTheTxAppliesNothing(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vseed001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vseed", Kind: domain.KindHTML,
		ContentSHA: "sha-vseed-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vseed-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}

	// Damage the document in the window between the plan's read and the
	// transaction's, exactly once.
	var fired bool
	damageOnce := func(s domain.Slug) {
		if fired || s != slug {
			return
		}
		fired = true
		if err := repo.PutRawForTest(storage.VersionsDocKeyForTest(slug), corruptJSON); err != nil {
			t.Fatalf("corrupt the document mid-write: %v", err)
		}
	}

	repo.SetVersionsDocPlannedHookForTest(damageOnce)
	err := repo.DeleteVersion(slug, 1)
	repo.SetVersionsDocPlannedHookForTest(nil)
	if !errors.Is(err, storage.ErrConcurrentChange) {
		t.Fatalf("tombstone over a document that decayed mid-write: got %v, want ErrConcurrentChange", err)
	}
	if got, err := repo.GetVersion(slug, 1); err != nil || got.Deleted {
		t.Fatalf("a refused tombstone must apply nothing: %+v err=%v", got, err)
	}

	// The re-plan meets the damage, walks the rows, and rebuilds.
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("the retry must succeed: %v", err)
	}
	nums, _, complete := versionsDocFields(t, repo, slug)
	if !complete || !reflect.DeepEqual(nums, []int{1, 2}) {
		t.Fatalf("the rebuilt document: versions %v complete %v, want [1 2] true", nums, complete)
	}

	// An append rides its own retry loop over the same window rather than
	// surfacing it.
	fired = false
	repo.SetVersionsDocPlannedHookForTest(damageOnce)
	res, aerr := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vseed-v3", 100, 0, now)
	repo.SetVersionsDocPlannedHookForTest(nil)
	if aerr != nil {
		t.Fatalf("append across the same window: %v", aerr)
	}
	if res.NewVer != 3 {
		t.Fatalf("append across the same window: got v%d, want v3", res.NewVer)
	}
}

// --- m. the generation oracle ----------------------------------------------

// The head's numbering mark is a COUNTER, so it detects a version ADDED behind
// the document's back and nothing else. The previous binary's per-version
// delete adds no version: it flips a legacy row's deleted flag, leaves
// latest_version where it was, and never touches the document. The set keeps
// its size while the document's newest record becomes a lie - a version it
// swears is live whose blob is already unbound - and the count-based oracle
// cannot see it at all.
//
// The generation can: every version mutation this binary makes bumps it on the
// head and stamps it on the document, and the previous binary re-marshals the
// head through a struct that has no such field, so the field DISAPPEARS. Unpin
// must refuse rather than roll the head onto the unbound version, and the
// paste's next write must re-heal from the rows that carry the truth.
func TestVersionsDoc_PreviousBinaryTombstoneIsCaughtByTheGeneration(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vgen0001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vgen", Kind: domain.KindHTML,
		ContentSHA: "sha-vgen-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vgen-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append v%d: %v", i+2, err)
		}
	}
	v2, err := repo.GetVersion(slug, 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v2); err != nil {
		t.Fatalf("pin v2: %v", err)
	}
	if gen := versionsDocGen(t, repo, slug); gen == 0 {
		t.Fatal("fixture: the document must carry a generation")
	}

	previousBinaryTombstoneVersion(t, repo, slug, 3)

	// What the count-based oracle sees: nothing. The document holds every
	// number that exists, claims to be complete, and is not behind the mark -
	// yet its record for v3 says live and the row says deleted.
	nums, _, complete := versionsDocFields(t, repo, slug)
	latest, headGen := headFields(t, repo, slug)
	if !reflect.DeepEqual(nums, []int{1, 2, 3}) || !complete || latest != 3 {
		t.Fatalf("fixture: want a complete document over [1 2 3] under mark 3; got %v complete=%v mark=%d", nums, complete, latest)
	}
	if headGen != 0 {
		t.Fatalf("fixture: the previous binary's rewrite must drop versions_gen, got %d", headGen)
	}

	// The refusal. Rolling here would point the head at v3, whose blob the
	// previous binary already unbound.
	if err := repo.Unpin(slug); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("unpin over a document the head cannot account for: got %v, want ErrVersionsIncomplete", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get after the refused unpin: %v", err)
	}
	if head.PinnedVersion != 2 || head.ContentSHA != "sha-vgen-v2" {
		t.Fatalf("a refused unpin must apply nothing: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
	// The gated read stops believing the document too, and answers from the
	// rows the previous binary maintained.
	if got, gerr := repo.GetVersion(slug, 3); gerr != nil || !got.Deleted {
		t.Fatalf("GetVersion must report the row's truth: %+v err=%v", got, gerr)
	}

	// Re-heal: one ordinary mutation re-walks the rows, and the rows WIN over
	// the document's stale record. Filling instead would leave v3 claiming to
	// be live and then stamp a matching generation on it, cementing the lie.
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("tombstone v1 over an unaccountable document: %v", err)
	}
	docGen := versionsDocGen(t, repo, slug)
	_, headGen = headFields(t, repo, slug)
	if docGen == 0 || docGen != headGen {
		t.Fatalf("the re-heal must re-establish the generation: doc %d head %d", docGen, headGen)
	}
	// The rebuilt document carries the ROW's truth, not the record it held.
	if got, gerr := repo.GetVersion(slug, 3); gerr != nil || !got.Deleted {
		t.Fatalf("the re-heal must adopt the row's tombstone: %+v err=%v", got, gerr)
	}

	// The unpin it refused now rolls to v2 - the true newest live version -
	// and NOT to v3.
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin over the repaired document: %v", err)
	}
	head, err = repo.Get(slug)
	if err != nil {
		t.Fatalf("get after unpin: %v", err)
	}
	if head.PinnedVersion != 0 || head.ContentSHA != "sha-vgen-v2" {
		t.Fatalf("unpin must roll to v2, not the tombstoned v3: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
}

// A pin must not roll the head onto a record an unaccountable document holds
// either: pin reads the descriptor from INSIDE its own transaction, so the same
// check has to guard that read. Here the ROW carries a tombstone the document
// does not, and the refusal is the proof of which one the pin consulted.
func TestVersionsDoc_PinOverAnUnaccountableDocumentUsesTheRow(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vgen0002")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vgen2", Kind: domain.KindHTML,
		ContentSHA: "sha-vgen2-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vgen2-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	previousBinaryTombstoneVersion(t, repo, slug, 2)

	// The document still describes v2 as live content. The pin must resolve
	// from the row instead, which is what the previous binary maintained - and
	// the row says v2 is a tombstone, so the roll is refused.
	target, err := repo.GetVersion(slug, 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if !target.Deleted {
		t.Fatalf("fixture: v2 must read as tombstoned, got %+v", target)
	}
	if err := repo.SetPinnedVersion(slug, target); !errors.Is(err, storage.ErrVersionDeleted) {
		t.Fatalf("a pin resolved from the row must refuse its tombstone, got %v", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get after the refused pin: %v", err)
	}
	if head.PinnedVersion != 0 {
		t.Fatalf("a refused pin must apply nothing, got pin=%d", head.PinnedVersion)
	}
}

// The descriptor a pin rolls comes from the ROW when the document is
// unaccountable, which only a divergence between the two can show: the document
// here names a blob that was never staged, and the head must not end up
// carrying it.
func TestVersionsDoc_PinOverAnUnaccountableDocumentTakesTheRowsDescriptor(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vgen0007")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vgen7", Kind: domain.KindHTML,
		ContentSHA: "sha-vgen7-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vgen7-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	setDocVersionSHA(t, repo, slug, 1, "sha-vgen7-FROM-THE-DOCUMENT")
	stripHeadVersionsGen(t, repo, slug)

	target, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, target); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get after pin: %v", err)
	}
	if head.PinnedVersion != 1 || head.ContentSHA != "sha-vgen7-v1" {
		t.Fatalf("pin must roll the ROW's descriptor: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
}

// setDocVersionSHA rewrites one record's content sha inside the stored
// document, so the document and the legacy row disagree about the same number.
func setDocVersionSHA(t *testing.T, repo *storage.ShaleRepo, slug domain.Slug, ver int, sha string) {
	t.Helper()
	key := storage.VersionsDocKeyForTest(slug)
	raw, err := repo.GetRawForTest(key)
	if err != nil || len(raw) == 0 {
		t.Fatalf("read versions doc: err=%v len=%d", err, len(raw))
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode versions doc: %v", err)
	}
	held, _ := doc["versions"].([]any)
	patched := false
	for _, entry := range held {
		rec, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("unexpected version record shape %T", entry)
		}
		if n, ok := rec["ver_num"].(float64); ok && int(n) == ver {
			rec["content_sha"] = sha
			patched = true
		}
	}
	if !patched {
		t.Fatalf("fixture: v%d was not in the document", ver)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode versions doc: %v", err)
	}
	if err := repo.PutRawForTest(key, out); err != nil {
		t.Fatalf("write versions doc: %v", err)
	}
}

// The generation is stamped on the head and the document by the SAME
// transaction, so an ordinary write leaves the two agreeing. A tombstone that
// turns out to be a no-op still writes the document (it applied a heal, it
// stamped a generation), so it must write the head too - otherwise the paste
// would be left with a document nothing can account for and every gated
// operation would refuse until some unrelated write happened to fix it.
func TestVersionsDoc_GenerationMovesTogetherIncludingARepeatedTombstone(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vgen0003")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vgen3", Kind: domain.KindHTML,
		ContentSHA: "sha-vgen3-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vgen3-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	agree := func(stage string) {
		t.Helper()
		_, headGen := headFields(t, repo, slug)
		docGen := versionsDocGen(t, repo, slug)
		if headGen == 0 || headGen != docGen {
			t.Fatalf("%s: head generation %d, document generation %d", stage, headGen, docGen)
		}
	}
	agree("after the append")
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("tombstone v1: %v", err)
	}
	agree("after the tombstone")
	if err := repo.DeleteVersion(slug, 1); err != nil {
		t.Fatalf("repeat the tombstone: %v", err)
	}
	agree("after the repeated tombstone")
	// The paste stays fully operable, which is what "accountable" buys.
	if err := repo.Unpin(slug); err != nil {
		t.Fatalf("unpin after the repeated tombstone: %v", err)
	}
}

// --- n. the whole-paste delete's guard covers its own key set --------------

// The cascade enumerates keys with a scan and reads the document for the blob
// ids of rows that will not decode, and its ExpectAbsent probe is derived from
// BOTH. The probe's one job is to equal the number the first append committing
// after the enumeration will claim, so an append landing between the two reads
// aborts the delete or joins its key set - never neither.
//
// Reading the document AFTER the scan breaks exactly that: the racing append's
// number reaches the probe through the document while its ROW stays out of the
// scan's key set, so the probe steps past the very row it was meant to catch
// and the delete commits leaving a live version under a deleted paste, whose
// slug has already returned to the mint.
func TestVersionsDoc_CascadeGuardCatchesAnAppendBetweenItsTwoReads(t *testing.T) {
	repo := newBlobShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vrace001")
	owner := "key:vrace"

	insertWithBlob(t, repo, slug, owner, "sha-vrace-v1", now)
	appendWithBlob(t, repo, slug, "sha-vrace-v2", now)

	// Commit a version in the window between the cascade's two reads, once.
	var fired bool
	repo.SetVersionsCascadeMidReadHookForTest(func(s domain.Slug) {
		if fired || s != slug {
			return
		}
		fired = true
		appendWithBlob(t, repo, slug, "sha-vrace-v3", now)
	})
	err := repo.Delete(slug)
	repo.SetVersionsCascadeMidReadHookForTest(nil)
	if !fired {
		t.Fatal("the hook never fired, so this test proves nothing")
	}
	if err != nil && !errors.Is(err, storage.ErrConcurrentChange) {
		t.Fatalf("delete racing an append: got %v, want nil or ErrConcurrentChange", err)
	}
	if err == nil {
		if rows := localCount(t, repo, "versions/"+slug.String()+"/"); rows != 0 {
			t.Fatalf("%d version row(s) survived a delete that reported success", rows)
		}
		if brefs := localCount(t, repo, "bref/{"+slug.String()+"}/"); brefs != 0 {
			t.Fatalf("%d blob(s) stayed bound under a deleted paste", brefs)
		}
	}

	// What a survivor would have cost: the slug returns to the mint, and the
	// next paste on it must inherit nothing.
	if err == nil {
		insertWithBlob(t, repo, slug, "key:vrace-next", "sha-vrace-next", now)
		vers, lerr := repo.ListVersions(slug)
		if lerr != nil {
			t.Fatalf("list versions of the re-minted slug: %v", lerr)
		}
		if got := verNums(vers); !reflect.DeepEqual(got, []int{1}) {
			t.Fatalf("a re-minted slug inherited versions %v from the deleted paste", got)
		}
	}
}

// --- o. envelope damage is damage, not a read failure ----------------------

// truncatedEnvelope is what a partially written value looks like at replication
// factor >1: the LWW magic byte followed by a header that runs off the end. It
// fails one layer BELOW invalid JSON - at the envelope strip, before anything
// is parsed - which is why every fail-open path has to treat the two alike.
var truncatedEnvelope = []byte{0xE0, 0x01, 0x02}

// A document damaged at the envelope layer must degrade exactly like one
// damaged at the JSON layer: reads fall back to the legacy rows, the
// whole-paste delete still runs (it is the one operation that could clear the
// damage, so it must never be behind it), and the next write rebuilds.
func TestVersionsDoc_TruncatedEnvelopeDegradesLikeUndecodableJSON(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	stage := func(t *testing.T, slug domain.Slug) *storage.ShaleRepo {
		t.Helper()
		p := domain.Paste{
			Slug: slug, Identity: "key:venv", Kind: domain.KindHTML,
			ContentSHA: "sha-venv-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
		if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
			t.Fatalf("insert: %v", err)
		}
		repo.WaitPendingConfirms()
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-venv-v2", 200, 0, now); err != nil {
			t.Fatalf("append v2: %v", err)
		}
		if err := repo.PutRawForTest(storage.VersionsDocKeyForTest(slug), truncatedEnvelope); err != nil {
			t.Fatalf("truncate the document's envelope: %v", err)
		}
		return repo
	}

	t.Run("reads fall back and a write rebuilds", func(t *testing.T) {
		slug := domain.Slug("venv0001")
		stage(t, slug)

		vers, err := repo.ListVersions(slug)
		if err != nil {
			t.Fatalf("list over a truncated envelope must fall back, not fail: %v", err)
		}
		if got := verNums(vers); !reflect.DeepEqual(got, []int{2, 1}) {
			t.Fatalf("fallback list: %v, want [2 1]", got)
		}
		if got, gerr := repo.GetVersion(slug, 2); gerr != nil || got.ContentSHA != "sha-venv-v2" {
			t.Fatalf("GetVersion over a truncated envelope: %+v err=%v", got, gerr)
		}
		if err := repo.Unpin(slug); err != nil {
			t.Fatalf("unpin over a truncated envelope must fall back: %v", err)
		}
		if _, err := repo.SumActiveBytesByOwner("key:venv", now); err != nil {
			t.Fatalf("the quota read must not fail on a truncated envelope: %v", err)
		}
		res, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-venv-v3", 100, 0, now)
		if err != nil {
			t.Fatalf("append over a truncated envelope must rebuild it, not fail: %v", err)
		}
		if res.NewVer != 3 {
			t.Fatalf("numbering over a rebuilt document: got v%d, want v3", res.NewVer)
		}
		nums, _, complete := versionsDocFields(t, repo, slug)
		if !reflect.DeepEqual(nums, []int{1, 2, 3}) || !complete {
			t.Fatalf("rebuilt document: versions %v complete %v", nums, complete)
		}
	})

	t.Run("the whole-paste delete is not behind the damage", func(t *testing.T) {
		slug := domain.Slug("venv0002")
		stage(t, slug)

		if err := repo.Delete(slug); err != nil {
			t.Fatalf("a delete must never be blocked by the value it would remove: %v", err)
		}
		if _, err := repo.Get(slug); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("the paste survived the delete: %v", err)
		}
		if rows := localCount(t, repo, "versions/"+slug.String()+"/"); rows != 0 {
			t.Fatalf("%d version row(s) survived the delete", rows)
		}
	})
}

// The read-side report latches per slug so one damaged value cannot become an
// unbounded log stream, and the latch CLEARS when a write repairs the value -
// otherwise the first corruption silences every later one for the life of the
// process.
func TestVersionsDoc_UndecodableReportRearmsAfterARepair(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vlog0002")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vlog2", Kind: domain.KindHTML,
		ContentSHA: "sha-vlog2-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()

	logs := captureRepoLog(t)
	damageReadRepair := func(stage string) {
		t.Helper()
		if err := repo.PutRawForTest(storage.VersionsDocKeyForTest(slug), corruptJSON); err != nil {
			t.Fatalf("%s: corrupt the document: %v", stage, err)
		}
		for range 3 {
			if _, err := repo.ListVersions(slug); err != nil {
				t.Fatalf("%s: list over a corrupt document: %v", stage, err)
			}
		}
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vlog2-"+stage, 10, 0, now); err != nil {
			t.Fatalf("%s: repairing append: %v", stage, err)
		}
	}
	// The read-side report only, not the write's "rebuilding it" line.
	reports := func() int {
		return countLogLines(logs, "versions doc for "+slug.String()+" undecodable", "reads fall back")
	}
	damageReadRepair("first")
	if n := reports(); n != 1 {
		t.Fatalf("the first damage logged %d lines, want 1:\n%s", n, logs.String())
	}
	damageReadRepair("second")
	if n := reports(); n != 2 {
		t.Fatalf("a corruption after a repair must be reported again: %d lines, want 2:\n%s", n, logs.String())
	}
}

// --- p. a plan-time seed applied in a later transaction cannot confer trust --

// D-STALE. A heal seed is walked OUTSIDE the transaction that applies it. In the
// plan-to-tx window a concurrent (new-binary) tombstone unbinds a version's blob
// and rewrites the document; a following (old-binary) head rewrite drops
// versions_gen, so the tombstone's own transaction meets an UNACCOUNTABLE
// document and rebuilds from the STALE seed - which still shows the tombstoned
// version live. The rebuilt document must NOT be stamped complete/accountable,
// or a later unpin rolls the head onto the unbound blob.
func TestVersionsDoc_StaleSeedRebuildStaysUntrustedNotRolledOnto(t *testing.T) {
	repo := newBlobShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vsld001")
	owner := "key:vsld"

	insertWithBlob(t, repo, slug, owner, "sha-vsld-v1", now)
	appendWithBlob(t, repo, slug, "sha-vsld-v2", now)
	appendWithBlob(t, repo, slug, "sha-vsld-v3", now)
	// Pin v1 so the head does not serve v2, and unpin still has a roll to make.
	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	// An unaccountable document at plan time forces the rebuild path, and the
	// seed captures v2 LIVE.
	stripHeadVersionsGen(t, repo, slug)

	// In the plan-to-tx window: a real tombstone of v2 (unbinds its blob and
	// rewrites the document), then an old-binary head rewrite that drops the
	// generation so the outer transaction meets an unaccountable document.
	var fired bool
	repo.SetVersionsDocPlannedHookForTest(func(s domain.Slug) {
		if fired || s != slug {
			return
		}
		fired = true
		if derr := repo.DeleteVersion(slug, 2); derr != nil {
			t.Fatalf("concurrent tombstone of v2: %v", derr)
		}
		stripHeadVersionsGen(t, repo, slug)
	})
	err = repo.DeleteVersion(slug, 3)
	repo.SetVersionsDocPlannedHookForTest(nil)
	if !fired {
		t.Fatal("the window hook never fired, so this test proves nothing")
	}
	if err != nil {
		t.Fatalf("tombstone v3 across the window: %v", err)
	}

	// The rebuild reinstated v2 from the stale seed, but nothing may trust it:
	// not complete, not accountable.
	if _, _, complete := versionsDocFields(t, repo, slug); complete {
		t.Fatal("a document rebuilt from a plan-time seed the window changed must not be complete")
	}
	if gen := versionsDocGen(t, repo, slug); gen != 0 {
		t.Fatalf("an untrusted rebuild must not stamp a matching generation, got gen %d", gen)
	}

	// The gate holds: unpin refuses rather than rolling the head onto v2, whose
	// blob the concurrent tombstone already unbound.
	if err := repo.Unpin(slug); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("unpin over an untrusted rebuild: got %v, want ErrVersionsIncomplete", err)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get after the refused unpin: %v", err)
	}
	if head.PinnedVersion != 1 || head.ContentSHA != "sha-vsld-v1" {
		t.Fatalf("a refused unpin must apply nothing: pin=%d sha=%q", head.PinnedVersion, head.ContentSHA)
	}
	// A pin of v2 falls back to the row the concurrent tombstone flipped, and
	// refuses it rather than publishing the unbound blob.
	v2, err := repo.GetVersion(slug, 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if !v2.Deleted {
		t.Fatalf("fixture: v2 must read as tombstoned, got %+v", v2)
	}
	if err := repo.SetPinnedVersion(slug, v2); !errors.Is(err, storage.ErrVersionDeleted) {
		t.Fatalf("pin of the concurrently-tombstoned v2: got %v, want ErrVersionDeleted", err)
	}

	// Convergence: one quiet mutation re-heals from the rows (v2 tombstoned) and
	// the document becomes trusted again.
	appendWithBlob(t, repo, slug, "sha-vsld-v4", now)
	if _, _, complete := versionsDocFields(t, repo, slug); !complete {
		t.Fatal("a quiet mutation must re-heal the document to complete")
	}
	docGen := versionsDocGen(t, repo, slug)
	_, headGen := headFields(t, repo, slug)
	if docGen == 0 || docGen != headGen {
		t.Fatalf("the re-heal must re-establish the generation: doc %d head %d", docGen, headGen)
	}
	if got, gerr := repo.GetVersion(slug, 2); gerr != nil || !got.Deleted {
		t.Fatalf("the re-heal must keep v2 tombstoned: %+v err=%v", got, gerr)
	}
}

// --- q. a seed is proven to pertain to the paste it is applied to -----------

// D-PASTE. A heal seed walked for paste P1 must not be folded into a DIFFERENT
// paste P2 that re-minted the slug in the plan-to-tx window. The seed carries
// P1's identity; the transaction reads P2's head, so it discards the foreign
// seed rather than installing P1's versions into P2's document.
func TestVersionsDoc_ForeignSeedIsNotFoldedIntoAReMintedPaste(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vpst001")
	ctx := context.Background()

	// P1: three versions, owned by O1.
	p1 := domain.Paste{
		Slug: slug, Identity: "key:vpst-1", Kind: domain.KindHTML,
		ContentSHA: "sha-vpst1-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p1, 0, now); err != nil {
		t.Fatalf("insert P1: %v", err)
	}
	repo.WaitPendingConfirms()
	for i, size := range []int{200, 100} {
		if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML,
			fmt.Sprintf("sha-vpst1-v%d", i+2), size, 0, now); err != nil {
			t.Fatalf("append P1 v%d: %v", i+2, err)
		}
	}
	// Force DeleteVersion(v3) to walk a seed at plan time.
	stripHeadVersionsGen(t, repo, slug)

	// In the plan-to-tx window: delete P1 and re-mint the slug as P2, a
	// DIFFERENT owner holding only its own v1.
	var fired bool
	repo.SetVersionsDocPlannedHookForTest(func(s domain.Slug) {
		if fired || s != slug {
			return
		}
		fired = true
		if derr := repo.Delete(slug); derr != nil {
			t.Fatalf("delete P1: %v", derr)
		}
		p2 := domain.Paste{
			Slug: slug, Identity: "key:vpst-2", Kind: domain.KindHTML,
			ContentSHA: "sha-vpst2-v1", Size: 50, CreatedAt: now, UpdatedAt: now}
		if derr := repo.InsertWithQuotaCheck(ctx, p2, 0, now); derr != nil {
			t.Fatalf("re-mint P2: %v", derr)
		}
		repo.WaitPendingConfirms()
	})
	err := repo.DeleteVersion(slug, 3)
	repo.SetVersionsDocPlannedHookForTest(nil)
	if !fired {
		t.Fatal("the re-mint hook never fired, so this test proves nothing")
	}
	if !errors.Is(err, storage.ErrConcurrentChange) {
		t.Fatalf("tombstone across a re-mint: got %v, want ErrConcurrentChange", err)
	}

	// P2's document holds ONLY its own v1: P1's v2 and v3 were not folded in.
	nums, _, complete := versionsDocFields(t, repo, slug)
	if !reflect.DeepEqual(nums, []int{1}) || !complete {
		t.Fatalf("P2's document must hold only its own v1: versions %v complete %v", nums, complete)
	}
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get P2: %v", err)
	}
	if head.ContentSHA != "sha-vpst2-v1" {
		t.Fatalf("P2 must serve its own v1, got sha %q", head.ContentSHA)
	}
	if vers, verr := repo.ListVersions(slug); verr != nil || !reflect.DeepEqual(verNums(vers), []int{1}) {
		t.Fatalf("P2's version list must be [1]: %v err=%v", verNums(vers), verr)
	}
	// The re-plan runs against P2: deleting a version P2 lacks is not-found, not
	// a fold of P1's records.
	if err := repo.DeleteVersion(slug, 3); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("re-planned tombstone of a version P2 lacks: got %v, want ErrNotFound", err)
	}
}

// --- r. the pre-migration unpin refuses incomplete on an unreadable row -----

// The legacy fallback returns the versions-incomplete sentinel when a version
// row cannot be read, rather than surfacing a raw envelope-strip/decode error:
// the set cannot be read in full, so the roll must not pick a "newest live" from
// a truncated scan.
func TestVersionsDoc_LegacyUnpinRefusesIncompleteOnAnUnreadableRow(t *testing.T) {
	repo := newShaleRepoForTest(t)
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	slug := domain.Slug("vlgu001")
	ctx := context.Background()

	p := domain.Paste{
		Slug: slug, Identity: "key:vlgu", Kind: domain.KindHTML,
		ContentSHA: "sha-vlgu-v1", Size: 300, CreatedAt: now, UpdatedAt: now}
	if err := repo.InsertWithQuotaCheck(ctx, p, 0, now); err != nil {
		t.Fatalf("insert: %v", err)
	}
	repo.WaitPendingConfirms()
	if _, err := repo.AppendVersionWithQuotaCheck(ctx, slug, domain.KindHTML, "sha-vlgu-v2", 200, 0, now); err != nil {
		t.Fatalf("append v2: %v", err)
	}
	v1, err := repo.GetVersion(slug, 1)
	if err != nil {
		t.Fatalf("get v1: %v", err)
	}
	if err := repo.SetPinnedVersion(slug, v1); err != nil {
		t.Fatalf("pin v1: %v", err)
	}
	// Pre-migration shape (no document) with an unreadable version row.
	deleteVersionsDoc(t, repo, slug)
	if err := repo.PutRawForTest(storage.LegacyVersionKeyForTest(slug, 2), corruptJSON); err != nil {
		t.Fatalf("corrupt legacy v2: %v", err)
	}

	if err := repo.Unpin(slug); !errors.Is(err, storage.ErrVersionsIncomplete) {
		t.Fatalf("legacy unpin over an unreadable row: got %v, want ErrVersionsIncomplete", err)
	}
	// Nothing applied: the pin stands.
	head, err := repo.Get(slug)
	if err != nil {
		t.Fatalf("get after the refused unpin: %v", err)
	}
	if head.PinnedVersion != 1 {
		t.Fatalf("a refused unpin must apply nothing: pin=%d", head.PinnedVersion)
	}
}
