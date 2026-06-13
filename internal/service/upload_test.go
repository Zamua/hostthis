package service

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

// newRealStack builds the upload service backed by real sqlite and
// real blob store under t.TempDir(). DDD payoff: this is the same
// stack that production runs; no mocks.
func newRealStack(t *testing.T) *Upload {
	t.Helper()
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	return NewUpload(storage.NewPasteRepo(db), blobs)
}

func TestUpload_Create_HTML(t *testing.T) {
	u := newRealStack(t)
	body := []byte("<!doctype html><p>hi</p>")
	res, err := u.Create(bytes.NewReader(body), "owner-key-hash", "demo", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Paste.Kind != domain.KindHTML {
		t.Fatalf("kind: got %q, want html", res.Paste.Kind)
	}
	if string(res.Paste.Identity) != "owner-key-hash" {
		t.Fatalf("identity: got %q, want %q", res.Paste.Identity, "owner-key-hash")
	}
	if res.Paste.Name != "demo" {
		t.Fatalf("name: got %q, want %q", res.Paste.Name, "demo")
	}
	// After compression, Size is the compressed (stored) byte count.
	// For short input zstd's header overhead can make it larger than
	// the original; only assert "positive + plausible".
	if res.Paste.Size <= 0 || res.Paste.Size > len(body)*2+64 {
		t.Fatalf("size: got %d, want positive ~within 2x of %d", res.Paste.Size, len(body))
	}
	if res.Paste.ContentSHA != domain.HashContent(body) {
		t.Fatalf("sha mismatch")
	}
	// Slug should be valid per the alphabet rules.
	if _, err := domain.ParseSlug(string(res.Paste.Slug)); err != nil {
		t.Fatalf("returned slug is invalid: %v", err)
	}
	// Expiry should be RetentionWindow from now.
	if res.Paste.ExpiresAt.Sub(res.Paste.CreatedAt) != domain.RetentionWindow {
		t.Fatalf("expiry: got %v, want %v", res.Paste.ExpiresAt.Sub(res.Paste.CreatedAt), domain.RetentionWindow)
	}
}

func TestUpload_Create_Markdown(t *testing.T) {
	u := newRealStack(t)
	res, err := u.Create(bytes.NewReader([]byte("# Title\n\nbody")), "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Paste.Kind != domain.KindMarkdown {
		t.Fatalf("kind: got %q, want markdown", res.Paste.Kind)
	}
	if res.Paste.Identity != "" {
		t.Fatalf("anonymous should have empty Identity, got %q", res.Paste.Identity)
	}
}

func TestUpload_Create_RejectsUnsupportedKind(t *testing.T) {
	u := newRealStack(t)
	_, err := u.Create(bytes.NewReader([]byte("\x89PNG\r\n\x1a\n...binary bytes...")), "", "", "")
	if !errors.Is(err, domain.ErrUnsupportedKind) {
		t.Fatalf("err: got %v, want ErrUnsupportedKind", err)
	}
}

func TestUpload_Create_RejectsEmpty(t *testing.T) {
	u := newRealStack(t)
	_, err := u.Create(bytes.NewReader([]byte{}), "", "", "")
	if err == nil {
		t.Fatalf("empty upload should error")
	}
}

func TestUpload_Create_RejectsOversize(t *testing.T) {
	u := newRealStack(t)
	body := make([]byte, domain.MaxPasteBytes+1)
	body[0] = '<' // doesn't really matter, we should reject before sniffing
	_, err := u.Create(bytes.NewReader(body), "", "", "")
	if err == nil {
		t.Fatalf("oversize upload should error")
	}
}

func TestUpload_Create_HonorsHint(t *testing.T) {
	u := newRealStack(t)
	// "anything" doesn't look like html or markdown, but the hint
	// should force html acceptance.
	res, err := u.Create(bytes.NewReader([]byte("anything goes")), "", "", "html")
	if err != nil {
		t.Fatalf("create with html hint: %v", err)
	}
	if res.Paste.Kind != domain.KindHTML {
		t.Fatalf("kind: got %q, want html", res.Paste.Kind)
	}
}

func TestUpload_Create_DedupsBlobOnSameBytes(t *testing.T) {
	u := newRealStack(t)
	body := []byte("<!doctype html><p>same</p>")
	r1, err := u.Create(bytes.NewReader(body), "", "", "")
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	r2, err := u.Create(bytes.NewReader(body), "", "", "")
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if r1.Paste.Slug == r2.Paste.Slug {
		t.Fatalf("expected distinct slugs, got %q twice", r1.Paste.Slug)
	}
	if r1.Paste.ContentSHA != r2.Paste.ContentSHA {
		t.Fatalf("same bytes should produce same content sha")
	}
}

func TestUpload_Create_TimestampStable(t *testing.T) {
	u := newRealStack(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	u.Now = func() time.Time { return now }
	res, err := u.Create(bytes.NewReader([]byte("<p>x")), "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !res.Paste.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt: got %v, want %v", res.Paste.CreatedAt, now)
	}
	if !res.Paste.ExpiresAt.Equal(now.Add(domain.RetentionWindow)) {
		t.Fatalf("ExpiresAt: got %v, want %v",
			res.Paste.ExpiresAt, now.Add(domain.RetentionWindow))
	}
}

// errBlobStore fails every Put with a caller-supplied error. Reads
// delegate to a real disk store so a rollback-then-read still works.
type errBlobStore struct {
	real BlobStore
	err  error
}

func (e errBlobStore) Put(sha string, r io.Reader, size int64) error {
	_, _ = io.Copy(io.Discard, r)
	return e.err
}
func (e errBlobStore) PutPrecompressed(sha string, body []byte) error { return e.err }
func (e errBlobStore) Get(sha string) ([]byte, error)                 { return e.real.Get(sha) }

// TestUpload_Create_Parallel_Success pins that the concurrent blob+metadata
// write path still yields a correct, readable, quota-correct paste: the
// blob exists (Get returns the original bytes) and the owner's active-byte
// sum reflects exactly the stored compressed size.
func TestUpload_Create_Parallel_Success(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := storage.NewPasteRepo(db)
	rawBlobs, err := storage.NewBlobStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("blobs: %v", err)
	}
	blobs := storage.NewCompressedBlobStore(rawBlobs)
	u := NewUpload(repo, blobs)

	body := []byte("<!doctype html><p>hello world</p>")
	res, err := u.Create(bytes.NewReader(body), "key:owner", "demo", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Readable via the repo at the returned slug.
	got, err := repo.Get(res.Paste.Slug)
	if err != nil {
		t.Fatalf("Get after create: %v", err)
	}
	if got.ContentSHA != res.Paste.ContentSHA {
		t.Fatalf("sha mismatch: repo %q vs result %q", got.ContentSHA, res.Paste.ContentSHA)
	}
	// The blob exists and round-trips to the original bytes.
	blobBytes, err := blobs.Get(res.Paste.ContentSHA)
	if err != nil {
		t.Fatalf("blob Get: %v", err)
	}
	if !bytes.Equal(blobBytes, body) {
		t.Fatalf("blob bytes mismatch")
	}
	// Quota reflects exactly this paste's compressed size.
	sum, err := repo.SumActiveBytesByOwner("key:owner", time.Now())
	if err != nil {
		t.Fatalf("sum bytes: %v", err)
	}
	if sum != res.Paste.Size {
		t.Fatalf("active bytes = %d, want %d", sum, res.Paste.Size)
	}
}

// TestUpload_Create_Parallel_BlobFailRollsBack pins the failure semantics
// of the concurrent write path: when the blob Put fails AFTER (or while)
// the metadata insert commits, the metadata is rolled back so no usable
// paste survives a missing blob, and the owner's quota is freed.
func TestUpload_Create_Parallel_BlobFailRollsBack(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := storage.NewPasteRepo(db)

	boom := errors.New("object store unreachable")
	u := NewUpload(repo, errBlobStore{real: realBlobs(t), err: boom})

	_, err = u.Create(bytes.NewReader([]byte("<!doctype html><p>x</p>")), "key:owner", "demo", "")
	if err == nil {
		t.Fatalf("expected error when blob Put fails")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped %v", err, boom)
	}
	// No usable paste: the owner lists nothing.
	n, err := repo.CountByOwner("key:owner")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountByOwner = %d after rolled-back insert, want 0", n)
	}
	// Quota fully freed.
	sum, err := repo.SumActiveBytesByOwner("key:owner", time.Now())
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if sum != 0 {
		t.Fatalf("active bytes = %d after rollback, want 0", sum)
	}
}

// stubRepo is an in-memory PasteRepo stub that lets a test force the
// metadata insert to fail deterministically, independent of the real
// quota arithmetic. Delete records whether a rollback was attempted.
type stubRepo struct {
	insertErr error
	inserted  map[domain.Slug]domain.Paste
	deleted   []domain.Slug
}

func newStubRepo() *stubRepo { return &stubRepo{inserted: map[domain.Slug]domain.Paste{}} }

func (s *stubRepo) InsertWithQuotaCheck(p domain.Paste, _ int64, _ time.Time) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted[p.Slug] = p
	return nil
}
func (s *stubRepo) Get(slug domain.Slug) (domain.Paste, error) {
	p, ok := s.inserted[slug]
	if !ok {
		return domain.Paste{}, storage.ErrNotFound
	}
	return p, nil
}
func (s *stubRepo) Delete(slug domain.Slug) error {
	s.deleted = append(s.deleted, slug)
	delete(s.inserted, slug)
	return nil
}

// TestUpload_Create_Parallel_MetadataFailLeavesNoPaste pins the other
// failure direction: when the metadata insert fails, Create returns the
// translated sentinel, inserts nothing, and does NOT issue a rollback
// Delete (there is nothing to roll back). Any blob written is a SHA-keyed
// orphan the sweep's blob-GC reclaims.
func TestUpload_Create_Parallel_MetadataFailLeavesNoPaste(t *testing.T) {
	repo := newStubRepo()
	repo.insertErr = storage.ErrOverUserQuota
	u := NewUpload(repo, realBlobs(t))

	_, err := u.Create(bytes.NewReader([]byte("<!doctype html><p>x</p>")), "key:owner", "demo", "")
	if !errors.Is(err, ErrOverQuota) {
		t.Fatalf("metadata-fail create = %v, want ErrOverQuota", err)
	}
	if len(repo.inserted) != 0 {
		t.Fatalf("expected no inserted pastes, got %d", len(repo.inserted))
	}
	if len(repo.deleted) != 0 {
		t.Fatalf("expected no rollback Delete on a failed insert, got %v", repo.deleted)
	}
}

// TestUpload_Create_Parallel_BlobFailRollsBackViaDelete pins that when the
// metadata insert SUCCEEDS but the blob Put fails, exactly the inserted
// slug is rolled back via Delete (so a missing blob never leaves a usable
// paste), using a stub repo so the assertion is exact.
func TestUpload_Create_Parallel_BlobFailRollsBackViaDelete(t *testing.T) {
	repo := newStubRepo()
	boom := errors.New("blob down")
	u := NewUpload(repo, errBlobStore{real: realBlobs(t), err: boom})

	_, err := u.Create(bytes.NewReader([]byte("<!doctype html><p>x</p>")), "key:owner", "demo", "")
	if !errors.Is(err, boom) {
		t.Fatalf("blob-fail create = %v, want wrapped %v", err, boom)
	}
	if len(repo.deleted) != 1 {
		t.Fatalf("expected exactly one rollback Delete, got %v", repo.deleted)
	}
	if len(repo.inserted) != 0 {
		t.Fatalf("rolled-back paste still present: %v", repo.inserted)
	}
}
