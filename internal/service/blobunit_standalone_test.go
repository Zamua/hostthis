package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Zamua/hostthis/internal/storage"
)

func newStandaloneUnit(t *testing.T) *StandaloneBlobUnit {
	t.Helper()
	return NewStandaloneBlobUnit(realBlobs(t))
}

func stage(t *testing.T, u *StandaloneBlobUnit, raw []byte) string {
	t.Helper()
	staged, err := streamUpload(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("streamUpload: %v", err)
	}
	defer staged.discard()
	if err := u.StagePrecompressed(
		context.Background(), staged.SHA, staged.File, staged.encodedSize(),
	); err != nil {
		t.Fatalf("StagePrecompressed: %v", err)
	}
	return staged.SHA
}

// streamIdentityStore records what PutPrecompressed received. The embedded nil
// store makes any other call panic.
type streamIdentityStore struct {
	testBlobStore
	want     io.Reader
	gotSame  bool
	gotSize  int64
	gotBytes []byte
}

func (s *streamIdentityStore) PutPrecompressed(_ string, r io.Reader, size int64) error {
	s.gotSame = r == s.want
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

	if err := u.StagePrecompressed(context.Background(), "sha", body, 12); err != nil {
		t.Fatalf("StagePrecompressed: %v", err)
	}
	if !store.gotSame {
		t.Fatal("store received a replacement reader; the byte plane buffered the spill")
	}
	if store.gotSize != 12 || string(store.gotBytes) != "encoded body" {
		t.Fatalf("forwarded size/body = %d/%q", store.gotSize, store.gotBytes)
	}
}

func TestStandalone_StagePrecompressedRead_RoundTrip(t *testing.T) {
	u := newStandaloneUnit(t)
	raw := []byte("<!doctype html><h1>round trip</h1>")
	sha := stage(t, u, raw)

	got, err := readStream(t, u, sha)
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

	sha, stored, err := u.StageEncoding(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("StageEncoding: %v", err)
	}
	if sha != sha256Hex(raw) {
		t.Fatalf("sha = %q, want %q", sha, sha256Hex(raw))
	}
	if stored <= 0 {
		t.Fatalf("stored size = %d, want positive", stored)
	}
	got, err := readStream(t, u, sha)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("StageEncoding round-trip: got %q, want %q", got, raw)
	}
}

func TestStandalone_Read_NotFound(t *testing.T) {
	u := newStandaloneUnit(t)
	if _, _, err := u.Read(context.Background(), "deadbeef"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Read missing: got %v, want storage.ErrNotFound", err)
	}
}

func readStream(t *testing.T, u *StandaloneBlobUnit, sha string) ([]byte, error) {
	t.Helper()
	rc, _, err := u.Read(context.Background(), sha)
	if err != nil {
		return nil, err
	}
	defer rc.Close() //nolint:errcheck
	return io.ReadAll(rc)
}
