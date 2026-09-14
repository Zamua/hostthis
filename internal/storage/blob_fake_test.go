package storage

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
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

func (f *fakeDurable) read(key string) (io.ReadCloser, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objs[key]
	if !ok {
		return nil, 0, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (f *fakeDurable) GetReader(key string) (io.ReadCloser, int64, error) { return f.read(key) }

func (f *fakeDurable) GetLegacyReader(sha string) (io.ReadCloser, int64, error) {
	return f.read("legacy/" + sha)
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

func (f *fakeDurable) rawSize(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.objs[key])
}

// putRaw stores bytes as-is, bypassing the compression layer.
func (f *fakeDurable) putRaw(key string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objs[key] = append([]byte(nil), body...)
}

// putEncoded writes body through the production encoder. The buffer is test-only.
func putEncoded(t testing.TB, c *CompressedBlobStore, key string, body []byte) {
	t.Helper()
	var buf bytes.Buffer
	if _, _, err := c.EncodeTo(&buf, bytes.NewReader(body)); err != nil {
		t.Fatalf("EncodeTo: %v", err)
	}
	if err := c.PutPrecompressed(key, &buf, int64(buf.Len())); err != nil {
		t.Fatalf("PutPrecompressed: %v", err)
	}
}

// readKey drains the object at key, failing the test on any error.
func readKey(t testing.TB, s interface {
	GetReader(string) (io.ReadCloser, int64, error)
}, key string) []byte {
	t.Helper()
	rc, _, err := s.GetReader(key)
	return drain(t, rc, err)
}

// readLegacy drains the legacy object at sha, failing the test on any error.
func readLegacy(t testing.TB, s interface {
	GetLegacyReader(string) (io.ReadCloser, int64, error)
}, sha string) []byte {
	t.Helper()
	rc, _, err := s.GetLegacyReader(sha)
	return drain(t, rc, err)
}

func drain(t testing.TB, rc io.ReadCloser, err error) []byte {
	t.Helper()
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
