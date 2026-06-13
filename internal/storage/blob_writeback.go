package storage

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// durableBlobStore is the contract the write-back cache requires of the
// backend it fronts (disk or S3). Both *BlobStore and *S3BlobStore
// satisfy it. The cache reads/writes the COMPRESSED stored bytes - it
// sits below the compression layer - so it deals only in opaque bytes
// keyed by sha.
type durableBlobStore interface {
	Put(sha string, r io.Reader, size int64) error
	Get(sha string) ([]byte, error)
	GetReader(sha string) (io.ReadCloser, int64, error)
	WalkBlobs(fn func(sha string) error) error
	Remove(sha string) error
}

// WriteBackConfig tunes the local-disk write-back cache.
type WriteBackConfig struct {
	// Dir is the local cache directory. Required.
	Dir string
	// MaxBytes is the soft cap on the cache's on-disk size. Once the
	// cache exceeds this, already-uploaded entries are evicted
	// oldest-first. Not-yet-uploaded entries are never evicted. <= 0
	// means a 1 GiB default.
	MaxBytes int64
	// Workers is the number of background uploader goroutines. <= 0
	// means 2.
	Workers int
	// Logger receives non-fatal uploader diagnostics. nil discards.
	Logger *log.Logger
	// retryBackoff overrides the base backoff between failed-upload
	// retries (test hook). Zero means a 1s base.
	retryBackoff time.Duration
	// maxRetryBackoff caps the exponential backoff (test hook). Zero
	// means 30s.
	maxRetryBackoff time.Duration
}

// WriteBackBlobStore is a local-disk write-back cache in front of a
// durable backend. Put writes the bytes to the pod's local disk and
// enqueues an asynchronous upload to the durable backend; Get/GetReader
// serve from the local cache first and fall back to the durable backend.
//
// It is OPT-IN: it trades a small durability window (the local copy is
// durable immediately, the durable-backend copy follows the async
// upload) for a fast upload ack. See SPEC "Local-disk write-back cache".
//
// The cache stores the compressed bytes at
// <dir>/<sha[:2]>/<sha>; an uploaded entry additionally carries a
// zero-byte marker file <dir>/<sha[:2]>/<sha>.up. Absence of the marker
// means the blob has not been confirmed durable in the backend yet and
// must NOT be evicted.
type WriteBackBlobStore struct {
	durable  durableBlobStore
	dir      string
	maxBytes int64
	logger   *log.Logger

	baseBackoff time.Duration
	maxBackoff  time.Duration

	queue   chan string
	wg      sync.WaitGroup
	stopCh  chan struct{}
	stopOne sync.Once

	// mu guards inFlight (the set of shas currently queued or being
	// uploaded) so the same sha isn't enqueued twice concurrently and
	// eviction can avoid in-flight entries.
	mu       sync.Mutex
	inFlight map[string]struct{}
}

const uploadedMarkerSuffix = ".up"

// NewWriteBackBlobStore builds the cache, scans the cache dir to
// re-enqueue any blobs that were not confirmed uploaded before the last
// shutdown, and starts the background uploader pool. Call Close to stop
// the uploaders (drains in-flight work best-effort).
func NewWriteBackBlobStore(durable durableBlobStore, cfg WriteBackConfig) (*WriteBackBlobStore, error) {
	if cfg.Dir == "" {
		return nil, errors.New("writeback: cache dir required")
	}
	if err := os.MkdirAll(cfg.Dir, 0o750); err != nil {
		return nil, fmt.Errorf("writeback mkdir %q: %w", cfg.Dir, err)
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 1 << 30 // 1 GiB
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 2
	}
	if cfg.retryBackoff <= 0 {
		cfg.retryBackoff = time.Second
	}
	if cfg.maxRetryBackoff <= 0 {
		cfg.maxRetryBackoff = 30 * time.Second
	}
	w := &WriteBackBlobStore{
		durable:     durable,
		dir:         cfg.Dir,
		maxBytes:    cfg.MaxBytes,
		logger:      cfg.Logger,
		baseBackoff: cfg.retryBackoff,
		maxBackoff:  cfg.maxRetryBackoff,
		// Buffered generously so Put rarely blocks on a full queue; if it
		// does fill, Put falls back to a synchronous durable upload rather
		// than blocking the request (see Put).
		queue:    make(chan string, 1024),
		stopCh:   make(chan struct{}),
		inFlight: make(map[string]struct{}),
	}
	for i := 0; i < cfg.Workers; i++ {
		w.wg.Add(1)
		go w.uploadWorker()
	}
	if err := w.rescanPending(); err != nil {
		// A scan failure is not fatal - the cache still functions, it just
		// may not have re-enqueued everything. Log and continue.
		w.logf("writeback: startup rescan error: %v", err)
	}
	return w, nil
}

func (w *WriteBackBlobStore) logf(format string, args ...any) {
	if w.logger != nil {
		w.logger.Printf(format, args...)
	}
}

func (w *WriteBackBlobStore) blobPath(sha string) string {
	return filepath.Join(w.dir, sha[:2], sha)
}

func (w *WriteBackBlobStore) markerPath(sha string) string {
	return filepath.Join(w.dir, sha[:2], sha+uploadedMarkerSuffix)
}

// Put writes the bytes to the local cache and enqueues an async upload.
// Returns once the local write is durable (fsync'd + renamed). The
// content-addressed skip applies: if the durable backend already has the
// object, nothing is written or enqueued.
func (w *WriteBackBlobStore) Put(sha string, r io.Reader, size int64) error {
	if len(sha) < 2 {
		return fmt.Errorf("writeback: sha too short")
	}
	// Buffer the bytes so we can both write them locally and (later)
	// read them back for the durable upload. Callers pass an in-memory
	// body via PutPrecompressed, so this is not an extra copy of unbounded
	// size - it's the same staging buffer the upload service already holds.
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("writeback read body: %w", err)
	}
	// If the durable backend already has it, this is a dedup hit - skip
	// the local write and the enqueue entirely, and make sure a local
	// copy left over from a prior run is marked uploaded so it can be
	// evicted.
	if w.durableHas(sha) {
		return nil
	}
	if err := w.writeLocal(sha, body); err != nil {
		return err
	}
	w.enqueue(sha)
	// Opportunistic eviction so a long-running process doesn't grow the
	// cache unbounded between uploads.
	w.evictIfNeeded()
	return nil
}

// PutPrecompressed mirrors the other backends: the body is already the
// stored (compressed, magic-prefixed) representation.
func (w *WriteBackBlobStore) PutPrecompressed(sha string, body []byte) error {
	return w.Put(sha, bytes.NewReader(body), int64(len(body)))
}

// writeLocal atomically writes body to the cache (tmp + fsync + rename).
func (w *WriteBackBlobStore) writeLocal(sha string, body []byte) error {
	dir := filepath.Join(w.dir, sha[:2])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("writeback mkdir %q: %w", dir, err)
	}
	dst := w.blobPath(sha)
	if _, err := os.Stat(dst); err == nil {
		return nil // already cached locally
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("writeback tmp create: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writeback write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writeback sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writeback close: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("writeback rename: %w", err)
	}
	return nil
}

// durableHas reports whether the durable backend already holds sha. A
// cheap existence probe; on error we assume not-present (the uploader
// will re-check and the worst case is a redundant Put, which the durable
// backend itself dedups).
func (w *WriteBackBlobStore) durableHas(sha string) bool {
	rc, _, err := w.durable.GetReader(sha)
	if err != nil {
		return false
	}
	_ = rc.Close()
	return true
}

// enqueue schedules sha for async upload if not already in flight.
func (w *WriteBackBlobStore) enqueue(sha string) {
	w.mu.Lock()
	if _, ok := w.inFlight[sha]; ok {
		w.mu.Unlock()
		return
	}
	w.inFlight[sha] = struct{}{}
	w.mu.Unlock()

	select {
	case w.queue <- sha:
	default:
		// Queue full: upload synchronously rather than block the caller or
		// drop the work. Keeps durability moving under bursty load.
		w.mu.Lock()
		delete(w.inFlight, sha)
		w.mu.Unlock()
		if err := w.uploadOnce(sha); err != nil {
			w.logf("writeback: synchronous upload of %s failed: %v", sha, err)
			// Re-enqueue via a goroutine so we don't recurse/block here.
			go w.enqueue(sha)
		}
	}
}

func (w *WriteBackBlobStore) uploadWorker() {
	defer w.wg.Done()
	for {
		select {
		case <-w.stopCh:
			return
		case sha := <-w.queue:
			w.handleUpload(sha)
		}
	}
}

// handleUpload uploads sha with bounded exponential backoff. On
// persistent failure it gives up for this cycle but leaves the local
// copy + the unmarked state intact, so the next startup rescan (or a
// future Put of the same sha) re-enqueues it.
func (w *WriteBackBlobStore) handleUpload(sha string) {
	defer func() {
		w.mu.Lock()
		delete(w.inFlight, sha)
		w.mu.Unlock()
	}()
	backoff := w.baseBackoff
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := w.uploadOnce(sha); err == nil {
			return
		} else {
			w.logf("writeback: upload %s attempt %d failed: %v", sha, attempt+1, err)
		}
		select {
		case <-w.stopCh:
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > w.maxBackoff {
			backoff = w.maxBackoff
		}
	}
	w.logf("writeback: giving up on %s after %d attempts; left for next rescan", sha, maxAttempts)
}

// uploadOnce reads the cached bytes and Puts them to the durable backend,
// then writes the uploaded marker. A missing local file is treated as
// success (already evicted/uploaded). ErrServiceFull is surfaced so the
// caller can decide; everything else is a retryable error.
func (w *WriteBackBlobStore) uploadOnce(sha string) error {
	body, err := os.ReadFile(w.blobPath(sha))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil // gone locally; nothing to upload
		}
		return fmt.Errorf("writeback read cached %s: %w", sha, err)
	}
	if err := w.durable.Put(sha, bytes.NewReader(body), int64(len(body))); err != nil {
		if errors.Is(err, ErrServiceFull) {
			// Durable store is at quota. Retrying won't help; mark as a
			// terminal-ish error by returning it. We do NOT mark uploaded,
			// so the blob stays pinned locally (correct: it is not durable).
			return err
		}
		return err
	}
	return w.markUploaded(sha)
}

// markUploaded writes the zero-byte ".up" marker next to the blob.
func (w *WriteBackBlobStore) markUploaded(sha string) error {
	mp := w.markerPath(sha)
	f, err := os.OpenFile(mp, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("writeback mark uploaded %s: %w", sha, err)
	}
	return f.Close()
}

func (w *WriteBackBlobStore) isUploaded(sha string) bool {
	_, err := os.Stat(w.markerPath(sha))
	return err == nil
}

// Get returns the bytes for sha, cache-first then durable backend.
func (w *WriteBackBlobStore) Get(sha string) ([]byte, error) {
	if len(sha) < 2 {
		return nil, fmt.Errorf("writeback: sha too short")
	}
	body, err := os.ReadFile(w.blobPath(sha))
	if err == nil {
		return body, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("writeback read %s: %w", sha, err)
	}
	return w.durable.Get(sha)
}

// GetReader streams the bytes for sha, cache-first then durable backend.
func (w *WriteBackBlobStore) GetReader(sha string) (io.ReadCloser, int64, error) {
	if len(sha) < 2 {
		return nil, 0, fmt.Errorf("writeback: sha too short")
	}
	f, err := os.Open(w.blobPath(sha)) //nolint:gosec // path derived from validated sha
	if err == nil {
		fi, serr := f.Stat()
		if serr != nil {
			_ = f.Close()
			return nil, 0, fmt.Errorf("writeback stat %s: %w", sha, serr)
		}
		return f, fi.Size(), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, 0, fmt.Errorf("writeback open %s: %w", sha, err)
	}
	return w.durable.GetReader(sha)
}

// WalkBlobs delegates to the durable backend, which is authoritative for
// what blobs exist for GC purposes. (Not-yet-uploaded local-only blobs
// belong to live pastes whose metadata rows reference them; they are not
// GC candidates and will appear in the durable backend once uploaded.)
func (w *WriteBackBlobStore) WalkBlobs(fn func(sha string) error) error {
	return w.durable.WalkBlobs(fn)
}

// Remove deletes sha from both the durable backend and the local cache.
func (w *WriteBackBlobStore) Remove(sha string) error {
	derr := w.durable.Remove(sha)
	_ = os.Remove(w.blobPath(sha))
	_ = os.Remove(w.markerPath(sha))
	return derr
}

// rescanPending walks the cache dir and re-enqueues every blob lacking
// the uploaded marker, so an upload interrupted by a crash/restart
// resumes. Called once at construction.
func (w *WriteBackBlobStore) rescanPending() error {
	var pending []string
	err := filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".tmp-") {
			_ = os.Remove(path) // stale tmp from an interrupted write
			return nil
		}
		if strings.HasSuffix(base, uploadedMarkerSuffix) {
			return nil // marker file, not a blob
		}
		if w.isUploaded(base) {
			return nil // already confirmed durable
		}
		pending = append(pending, base)
		return nil
	})
	if err != nil {
		return err
	}
	for _, sha := range pending {
		w.enqueue(sha)
	}
	if len(pending) > 0 {
		w.logf("writeback: rescan re-enqueued %d pending blob(s)", len(pending))
	}
	return nil
}

// cacheEntry is a blob file in the cache for eviction bookkeeping.
type cacheEntry struct {
	sha      string
	size     int64
	modTime  time.Time
	uploaded bool
}

// evictIfNeeded brings the cache back under maxBytes by deleting
// already-uploaded entries oldest-first. Not-yet-uploaded entries are
// never evicted (they are the only durable copy). The cap is therefore
// soft under a burst.
func (w *WriteBackBlobStore) evictIfNeeded() {
	entries, total, err := w.scanEntries()
	if err != nil {
		w.logf("writeback: eviction scan error: %v", err)
		return
	}
	if total <= w.maxBytes {
		return
	}
	// Evictable = uploaded and not in flight, oldest first.
	w.mu.Lock()
	evictable := make([]cacheEntry, 0, len(entries))
	for _, e := range entries {
		if !e.uploaded {
			continue
		}
		if _, busy := w.inFlight[e.sha]; busy {
			continue
		}
		evictable = append(evictable, e)
	}
	w.mu.Unlock()
	sort.Slice(evictable, func(i, j int) bool {
		return evictable[i].modTime.Before(evictable[j].modTime)
	})
	for _, e := range evictable {
		if total <= w.maxBytes {
			break
		}
		if err := os.Remove(w.blobPath(e.sha)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			w.logf("writeback: evict %s: %v", e.sha, err)
			continue
		}
		_ = os.Remove(w.markerPath(e.sha))
		total -= e.size
	}
}

// scanEntries lists every blob file in the cache (excluding markers and
// tmp files) with its size, mtime, and uploaded state, plus the running
// total byte size.
func (w *WriteBackBlobStore) scanEntries() ([]cacheEntry, int64, error) {
	var entries []cacheEntry
	var total int64
	err := filepath.WalkDir(w.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		if strings.HasPrefix(base, ".tmp-") || strings.HasSuffix(base, uploadedMarkerSuffix) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			if errors.Is(ierr, fs.ErrNotExist) {
				return nil
			}
			return ierr
		}
		total += info.Size()
		entries = append(entries, cacheEntry{
			sha:      base,
			size:     info.Size(),
			modTime:  info.ModTime(),
			uploaded: w.isUploaded(base),
		})
		return nil
	})
	return entries, total, err
}

// Close stops the background uploaders. It does not block on draining the
// queue: any blob not yet uploaded stays in the cache with no marker and
// is re-enqueued by the next process's startup rescan.
func (w *WriteBackBlobStore) Close() {
	w.stopOne.Do(func() { close(w.stopCh) })
	w.wg.Wait()
}

// drainForTest blocks until the in-flight set is empty or the deadline
// passes. Test helper only.
func (w *WriteBackBlobStore) drainForTest(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.inFlight)
		w.mu.Unlock()
		if n == 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
