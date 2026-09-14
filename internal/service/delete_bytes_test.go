package service

// Deletes remove bytes after the metadata commits, naming them from the
// delete's own answer.

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
	"github.com/Zamua/hostthis/internal/storagetest"
)

const bytesOwner = "key:delete-bytes"

func bytesStack(t *testing.T) (*Upload, *Manage, *storage.MemRepo, *storage.CompressedBlobStore, string) {
	t.Helper()
	repo := storagetest.NewRepo(t)
	blobs, root := realBlobsAt(t)
	unit := NewStandaloneBlobUnit(blobs)
	up := NewUpload(repo, unit)
	t.Cleanup(up.WaitFinalize)
	return up, NewManage(repo, unit), repo, blobs, root
}

func createReady(t *testing.T, up *Upload, body string) domain.Paste {
	t.Helper()
	res, err := up.Create(strings.NewReader(body), bytesOwner, "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	up.WaitFinalize()
	return res.Paste
}

func TestDelete_RemovesEveryVersionsBytes(t *testing.T) {
	up, m, _, blobs, root := bytesStack(t)
	keep := createReady(t, up, "<!doctype html><p>keep</p>")
	doomed := createReady(t, up, "<!doctype html><p>v1</p>")
	for _, body := range []string{"<!doctype html><p>v2</p>", "<!doctype html><p>v3</p>"} {
		if _, err := m.Update(doomed.Slug, bytesOwner, strings.NewReader(body), ""); err != nil {
			t.Fatalf("update: %v", err)
		}
	}

	if _, err := m.DeleteVersion(doomed.Slug, bytesOwner, 1); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	if n := objectsUnder(t, root); n != 3 {
		t.Fatalf("objects after deleting v1 = %d, want 3 (the kept paste, v2, v3)", n)
	}
	if err := m.Delete(doomed.Slug, bytesOwner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := objectsUnder(t, root); n != 1 {
		t.Fatalf("objects after deleting the paste = %d, want 1 (the kept paste)", n)
	}
	if body, err := readObject(t, blobs, keep.RootEntry().Key); err != nil || string(body) != "<!doctype html><p>keep</p>" {
		t.Fatalf("another paste after the delete = (%q, %v)", body, err)
	}
}

func TestDeleteVersion_RemovesOnlyThatVersionsBytes(t *testing.T) {
	up, m, repo, blobs, _ := bytesStack(t)
	p := createReady(t, up, "<!doctype html><p>v1</p>")
	if _, err := m.Update(p.Slug, bytesOwner, strings.NewReader("<!doctype html><p>v2</p>"), ""); err != nil {
		t.Fatalf("update: %v", err)
	}
	v2, err := repo.GetVersion(p.Slug, 2)
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}

	if _, err := m.DeleteVersion(p.Slug, bytesOwner, 1); err != nil {
		t.Fatalf("delete v1: %v", err)
	}
	if _, err := readObject(t, blobs, p.RootEntry().Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("v1 object after its delete = %v, want ErrNotFound", err)
	}
	if body, err := readObject(t, blobs, v2.Manifest.Files[domain.Root].Key); err != nil || string(body) != "<!doctype html><p>v2</p>" {
		t.Fatalf("v2 object after deleting v1 = (%q, %v)", body, err)
	}
}

// A legacy version's object is content-addressed and may back other pastes,
// so deleting its paste removes metadata only.
func TestDelete_LegacyVersionKeepsItsObject(t *testing.T) {
	_, m, repo, _, root := bytesStack(t)
	disk, err := storage.NewBlobStore(root)
	if err != nil {
		t.Fatalf("disk store: %v", err)
	}
	const sha = "0123456789abcdef"
	legacyBody := []byte("<!doctype html><p>legacy</p>")
	if err := disk.Put(sha[:2]+"/"+sha, bytes.NewReader(legacyBody), int64(len(legacyBody))); err != nil {
		t.Fatalf("seed legacy object: %v", err)
	}
	legacy := domain.Paste{
		Slug: "legacy23", Generation: "generation-legacy", Identity: bytesOwner,
		Status: domain.PasteStatusReady, Kind: domain.KindHTML, ContentSHA: sha,
		Size: len(legacyBody), CreatedAt: fixedNow, UpdatedAt: fixedNow,
	}
	if err := repo.InsertWithQuotaCheck(context.Background(), legacy, 0, fixedNow); err != nil {
		t.Fatalf("insert legacy paste: %v", err)
	}
	if _, err := m.Update(legacy.Slug, bytesOwner, strings.NewReader("<!doctype html><p>v2</p>"), ""); err != nil {
		t.Fatalf("update: %v", err)
	}

	if err := m.Delete(legacy.Slug, bytesOwner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := objectsUnder(t, root); n != 0 {
		t.Fatalf("upload objects after delete = %d, want 0", n)
	}
	if rc, _, err := disk.GetLegacyReader(sha); err != nil {
		t.Fatalf("legacy object after delete: %v", err)
	} else {
		_ = rc.Close()
	}
}

// failingDeleteUnit is a working byte plane whose prefix deletes fail.
type failingDeleteUnit struct{ BlobUnit }

func (failingDeleteUnit) DeleteUpload(context.Context, string) error {
	return errors.New("object store unavailable")
}

// A byte delete that fails after the metadata delete committed is logged, not
// reported: the paste is gone either way.
func TestDelete_ByteFailureDoesNotFailTheDelete(t *testing.T) {
	up, _, repo, blobs, _ := bytesStack(t)
	p := createReady(t, up, "<!doctype html><p>doomed</p>")
	var logs bytes.Buffer
	m := NewManage(repo, failingDeleteUnit{BlobUnit: NewStandaloneBlobUnit(blobs)})
	m.Logger = log.New(&logs, "", 0)

	if err := m.Delete(p.Slug, bytesOwner); err != nil {
		t.Fatalf("delete = %v, want nil", err)
	}
	if _, err := repo.Get(p.Slug); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("paste after delete: %v, want ErrNotFound", err)
	}
	if !strings.Contains(logs.String(), p.UploadID) {
		t.Fatalf("log %q does not name upload %s", logs.String(), p.UploadID)
	}
}

// remintOnDeleteRepo re-creates the slug the instant a delete commits, the way
// a concurrent upload can.
type remintOnDeleteRepo struct {
	*storage.MemRepo
	remint domain.Paste
}

func (r *remintOnDeleteRepo) Delete(slug domain.Slug, identity domain.Identity, createdAt time.Time) ([]string, error) {
	ids, err := r.MemRepo.Delete(slug, identity, createdAt)
	if err != nil {
		return nil, err
	}
	if err := r.InsertWithQuotaCheck(context.Background(), r.remint, 0, r.remint.CreatedAt); err != nil {
		return nil, err
	}
	return ids, nil
}

// The bytes deleted are the ones the delete answered with: a slug re-minted
// right after the removal keeps its objects.
func TestDelete_BytesComeFromTheDeleteAnswer(t *testing.T) {
	up, _, repo, blobs, _ := bytesStack(t)
	old := createReady(t, up, "<!doctype html><p>old</p>")
	unit := NewStandaloneBlobUnit(blobs)
	newID := domain.NewUploadID()
	newKey := domain.UploadObjectKey(newID, 0)
	if _, err := unit.StageEncoding(context.Background(), newKey, strings.NewReader("<p>new owner</p>")); err != nil {
		t.Fatalf("stage the re-mint's object: %v", err)
	}
	m := NewManage(&remintOnDeleteRepo{MemRepo: repo, remint: domain.Paste{
		Slug: old.Slug, Generation: "generation-remint", Identity: "key:new-owner",
		Status: domain.PasteStatusReady, Kind: domain.KindHTML, UploadID: newID, Size: 1,
		Manifest:  domain.DocumentManifest(domain.ManifestEntry{Key: newKey}),
		CreatedAt: fixedNow.Add(time.Hour), UpdatedAt: fixedNow.Add(time.Hour),
	}}, unit)

	if err := m.Delete(old.Slug, bytesOwner); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := readObject(t, blobs, old.RootEntry().Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted paste's object = %v, want ErrNotFound", err)
	}
	if body, err := readObject(t, blobs, newKey); err != nil || string(body) != "<p>new owner</p>" {
		t.Fatalf("re-minted paste's object = (%q, %v), want it kept", body, err)
	}
}
