package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
	"github.com/Zamua/hostthis/internal/storage"
)

func newStandaloneUnit(t *testing.T) *StandaloneBlobUnit {
	t.Helper()
	return NewStandaloneBlobUnit(realBlobs(t))
}

// stage streams raw through the upload pipeline into object 0 of a fresh
// upload and returns that object's entry.
func stage(t *testing.T, u *StandaloneBlobUnit, raw []byte) domain.ManifestEntry {
	t.Helper()
	staged, err := streamUpload(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("streamUpload: %v", err)
	}
	defer staged.discard()
	entry := domain.ManifestEntry{Key: domain.UploadObjectKey(domain.NewUploadID(), 0)}
	if err := u.StagePrecompressed(context.Background(), entry.Key, staged.File, staged.encodedSize()); err != nil {
		t.Fatalf("StagePrecompressed: %v", err)
	}
	return entry
}

// streamIdentityStore records what PutPrecompressed received. The embedded nil
// store makes any other call panic.
type streamIdentityStore struct {
	testBlobStore
	want     io.Reader
	gotSame  bool
	gotKey   string
	gotSize  int64
	gotBytes []byte
}

func (s *streamIdentityStore) PutPrecompressed(key string, r io.Reader, size int64) error {
	s.gotSame = r == s.want
	s.gotKey = key
	s.gotSize = size
	var err error
	s.gotBytes, err = io.ReadAll(r)
	return err
}

// StagePrecompressed forwards the spill stream instead of materializing it.
func TestStandalone_StagePrecompressedStreamsOriginalReader(t *testing.T) {
	body := bytes.NewReader([]byte("encoded body"))
	store := &streamIdentityStore{want: body}
	u := NewStandaloneBlobUnit(store)

	if err := u.StagePrecompressed(context.Background(), "uploads/u/0", body, 12); err != nil {
		t.Fatalf("StagePrecompressed: %v", err)
	}
	if !store.gotSame {
		t.Fatal("store received a replacement reader; the byte plane buffered the spill")
	}
	if store.gotKey != "uploads/u/0" || store.gotSize != 12 || string(store.gotBytes) != "encoded body" {
		t.Fatalf("forwarded key/size/body = %q/%d/%q", store.gotKey, store.gotSize, store.gotBytes)
	}
}

func TestStandalone_StagePrecompressedRead_RoundTrip(t *testing.T) {
	u := newStandaloneUnit(t)
	raw := []byte("<!doctype html><h1>round trip</h1>")
	entry := stage(t, u, raw)

	got, err := readStream(t, u, entry)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("Read bytes: got %q, want %q", got, raw)
	}
}

func TestStandalone_StageEncodingRead_RoundTrip(t *testing.T) {
	u := newStandaloneUnit(t)
	raw := []byte("body{margin:0}\n/* a stylesheet a site file would carry */")
	entry := domain.ManifestEntry{Key: domain.UploadObjectKey(domain.NewUploadID(), 3)}

	stored, err := u.StageEncoding(context.Background(), entry.Key, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("StageEncoding: %v", err)
	}
	if stored <= 0 {
		t.Fatalf("stored size = %d, want positive", stored)
	}
	got, err := readStream(t, u, entry)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("StageEncoding round-trip: got %q, want %q", got, raw)
	}
}

func TestStandalone_Read_NotFound(t *testing.T) {
	u := newStandaloneUnit(t)
	for _, entry := range []domain.ManifestEntry{
		{Key: domain.UploadObjectKey(domain.NewUploadID(), 0)},
		{SHA: "deadbeef"},
		{},
	} {
		if _, _, err := u.Read(context.Background(), entry); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("Read %+v: got %v, want storage.ErrNotFound", entry, err)
		}
	}
}

// A legacy entry reads its content-addressed object, and an entry carrying a
// key reads the key even when a sha is also present.
func TestStandalone_Read_DualRead(t *testing.T) {
	root := filepath.Join(t.TempDir(), "blobs")
	disk, err := storage.NewBlobStore(root)
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	u := NewStandaloneBlobUnit(storage.NewCompressedBlobStore(disk))
	const sha = "0123abcd"
	legacy := []byte("<h1>written before upload prefixes</h1>")
	if err := disk.Put(sha[:2]+"/"+sha, bytes.NewReader(legacy), int64(len(legacy))); err != nil {
		t.Fatalf("seed legacy object: %v", err)
	}

	got, err := readStream(t, u, domain.ManifestEntry{SHA: sha})
	if err != nil || !bytes.Equal(got, legacy) {
		t.Fatalf("legacy read = (%q, %v), want %q", got, err, legacy)
	}

	keyed := stage(t, u, []byte("<h1>re-homed</h1>"))
	keyed.SHA = sha
	if got, err := readStream(t, u, keyed); err != nil || string(got) != "<h1>re-homed</h1>" {
		t.Fatalf("entry with key and sha = (%q, %v), want the keyed object", got, err)
	}
}

func TestStandalone_DeleteUpload(t *testing.T) {
	u := newStandaloneUnit(t)
	id, other := domain.NewUploadID(), domain.NewUploadID()
	for _, key := range []string{domain.UploadObjectKey(id, 0), domain.UploadObjectKey(id, 1), domain.UploadObjectKey(other, 0)} {
		if _, err := u.StageEncoding(context.Background(), key, bytes.NewReader([]byte(key))); err != nil {
			t.Fatalf("stage %s: %v", key, err)
		}
	}

	if err := u.DeleteUpload(context.Background(), id); err != nil {
		t.Fatalf("DeleteUpload: %v", err)
	}
	for _, n := range []int{0, 1} {
		if _, err := readStream(t, u, domain.ManifestEntry{Key: domain.UploadObjectKey(id, n)}); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("object %d after delete = %v, want ErrNotFound", n, err)
		}
	}
	if _, err := readStream(t, u, domain.ManifestEntry{Key: domain.UploadObjectKey(other, 0)}); err != nil {
		t.Fatalf("another upload's object after delete: %v", err)
	}
	if err := u.DeleteUpload(context.Background(), id); err != nil {
		t.Fatalf("repeat DeleteUpload = %v, want nil", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b"} {
		if err := u.DeleteUpload(context.Background(), bad); err == nil {
			t.Fatalf("DeleteUpload(%q) = nil, want a refusal", bad)
		}
	}
}

func readStream(t *testing.T, u *StandaloneBlobUnit, entry domain.ManifestEntry) ([]byte, error) {
	t.Helper()
	rc, _, err := u.Read(context.Background(), entry)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}
