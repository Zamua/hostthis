package storage

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

// fakeDurable is an in-memory raw object store keyed like the real ones.
type fakeDurable struct {
	mu   sync.Mutex
	objs map[string][]byte
}

func newFakeDurable() *fakeDurable { return &fakeDurable{objs: map[string][]byte{}} }

func (f *fakeDurable) Put(key string, r io.Reader, _ int64) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = body
	return nil
}

func (f *fakeDurable) GetReader(key string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs[key]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *fakeDurable) DeletePrefix(prefix string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			delete(f.objs, k)
		}
	}
	return nil
}

// raw returns the stored bytes at key, bypassing the encoding.
func (f *fakeDurable) raw(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objs[key]
}

// putRaw stores bytes as-is, bypassing the encoding.
func (f *fakeDurable) putRaw(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = append([]byte(nil), body...)
}

// putEncoded writes body through the production encoder.
func putEncoded(t testing.TB, c *CompressedBlobStore, key string, body []byte) {
	t.Helper()
	if _, err := c.StageEncoding(context.Background(), key, bytes.NewReader(body)); err != nil {
		t.Fatalf("StageEncoding: %v", err)
	}
}

// readKey drains the decoded object at key, failing the test on any error.
func readKey(t testing.TB, c *CompressedBlobStore, key string) []byte {
	t.Helper()
	rc, _, err := c.Read(context.Background(), domain.ManifestEntry{Key: key})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return b
}
