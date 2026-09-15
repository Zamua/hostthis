package storage_test

// The raw object store contract, run against every backend: disk always, S3
// when MINIO_TEST_ENDPOINT names an object store. Anything one backend passes
// and another fails is a divergence, not a feature.

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

type rawBlobStore interface {
	Put(key string, r io.Reader, size int64) error
	GetReader(key string) (io.ReadCloser, int64, error)
	DeletePrefix(prefix string) error
}

func runBlobContract(t *testing.T, newStore func(t *testing.T) rawBlobStore) {
	t.Run("RoundTrip", func(t *testing.T) { blobContractRoundTrip(t, newStore(t)) })
	t.Run("MissingIsNotFound", func(t *testing.T) { blobContractMissing(t, newStore(t)) })
	t.Run("DeletePrefixRemovesOnlyThatUpload", func(t *testing.T) { blobContractDeletePrefix(t, newStore(t)) })
	t.Run("DeleteAbsentPrefixSucceeds", func(t *testing.T) { blobContractDeleteAbsent(t, newStore(t)) })
	t.Run("RejectsUnsafeNames", func(t *testing.T) { blobContractRejectsUnsafe(t, newStore(t)) })
}

func put(t *testing.T, s rawBlobStore, key string, body []byte) {
	t.Helper()
	if err := s.Put(key, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func read(t *testing.T, s rawBlobStore, key string) ([]byte, error) {
	t.Helper()
	rc, _, err := s.GetReader(key)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}

// deleteAfter removes an upload's prefix at test end, so a durable bucket does
// not accumulate test objects.
func deleteAfter(t *testing.T, s rawBlobStore, id string) {
	t.Cleanup(func() { _ = s.DeletePrefix(domain.UploadPrefix(id)) })
}

func blobContractRoundTrip(t *testing.T, s rawBlobStore) {
	id := domain.NewUploadID()
	deleteAfter(t, s, id)
	key := domain.UploadObjectKey(id, 0)
	body := []byte("the bytes a paste is made of\n")
	put(t, s, key, body)

	rc, n, err := s.GetReader(key)
	if err != nil {
		t.Fatalf("get reader: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) || n != int64(len(body)) {
		t.Fatalf("GetReader = %q (length %d), want %q (length %d)", got, n, body, len(body))
	}
}

// A missing object is ErrNotFound, not an opaque transport error, so the read
// path can tell "no such object" from "the store is broken".
func blobContractMissing(t *testing.T, s rawBlobStore) {
	if _, err := read(t, s, domain.UploadObjectKey(domain.NewUploadID(), 0)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GetReader of an absent key = %v, want ErrNotFound", err)
	}
}

// A prefix delete ends on a segment boundary: an upload whose id merely starts
// with the deleted one keeps its objects.
func blobContractDeletePrefix(t *testing.T, s rawBlobStore) {
	doomed := domain.NewUploadID()
	lookalike := doomed + "x"
	other := domain.NewUploadID()
	for _, id := range []string{doomed, lookalike, other} {
		deleteAfter(t, s, id)
	}
	put(t, s, domain.UploadObjectKey(doomed, 0), []byte("a"))
	put(t, s, domain.UploadObjectKey(doomed, 1), []byte("b"))
	put(t, s, domain.UploadObjectKey(lookalike, 0), []byte("c"))
	put(t, s, domain.UploadObjectKey(other, 0), []byte("d"))

	if err := s.DeletePrefix(domain.UploadPrefix(doomed)); err != nil {
		t.Fatalf("DeletePrefix: %v", err)
	}
	for _, n := range []int{0, 1} {
		if _, err := read(t, s, domain.UploadObjectKey(doomed, n)); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("object %d of the deleted upload = %v, want ErrNotFound", n, err)
		}
	}
	for id, want := range map[string]string{lookalike: "c", other: "d"} {
		got, err := read(t, s, domain.UploadObjectKey(id, 0))
		if err != nil || string(got) != want {
			t.Fatalf("surviving upload %s = (%q, %v), want %q", id, got, err, want)
		}
	}
}

func blobContractDeleteAbsent(t *testing.T, s rawBlobStore) {
	if err := s.DeletePrefix(domain.UploadPrefix(domain.NewUploadID())); err != nil {
		t.Fatalf("DeletePrefix of an absent upload = %v, want nil", err)
	}
}

// No name read back from metadata may reach outside its own object, and no
// prefix delete may widen past one upload.
func blobContractRejectsUnsafe(t *testing.T, s rawBlobStore) {
	for _, key := range []string{"", "/uploads/x/0", "uploads/../x", "uploads/x/", "uploads//0", `uploads\x`} {
		if err := s.Put(key, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Errorf("Put(%q) = nil, want a refusal", key)
		}
		if _, _, err := s.GetReader(key); err == nil || errors.Is(err, storage.ErrNotFound) {
			t.Errorf("GetReader(%q) = %v, want a refusal", key, err)
		}
	}
	for _, prefix := range []string{"", "/", "uploads", "uploads/x", "../", "uploads/../"} {
		if err := s.DeletePrefix(prefix); err == nil {
			t.Errorf("DeletePrefix(%q) = nil, want a refusal", prefix)
		}
	}
}

func TestDiskBlobContract(t *testing.T) {
	runBlobContract(t, func(t *testing.T) rawBlobStore {
		bs, err := storage.NewBlobStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewBlobStore: %v", err)
		}
		return bs
	})
}
