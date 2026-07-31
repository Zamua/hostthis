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
	"sync/atomic"
	"time"
)

// durableBlobStore is the contract the write-back cache requires of the backend
// it fronts. The cache sits BELOW the compression layer, so it moves only
// opaque already-compressed bytes keyed by sha.
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
	// MaxBytes is the soft cap on the cache's on-disk size. Past it,
	// already-uploaded entries are evicted oldest-first; not-yet-uploaded
	// entries are never evicted. <= 0 means 1 GiB.
	MaxBytes int64
	// Workers is the number of background uploader goroutines. <= 0 means 2.
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

// WriteBackBlobStore is a local-disk write-back cache in front of a durable
// backend: Put writes to the pod's local disk and enqueues an asynchronous
// upload, Get/GetReader serve from the cache first and fall back to the backend.
// It is OPT-IN, trading a durability window (the local copy lands immediately,
// the backend copy follows the async upload) for a fast upload ack. See SPEC
// "Local-disk write-back cache".
//
// Compressed bytes live at <dir>/<sha[:2]>/<sha>; an uploaded entry additionally
// carries a zero-byte <dir>/<sha[:2]>/<sha>.up marker. No marker means the blob
// is not yet confirmed durable in the backend, so the local copy is the ONLY one
// and must not be evicted.
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

	// diskBytes is a running count of the cached blob bytes so the common
	// under-cap Put skips the full-directory walk. Any walk resyncs it, and
	// the accounting only ever errs high (one extra walk), never low (a
	// skipped eviction).
	diskBytes atomic.Int64

	// mu guards inFlight (shas queued or uploading) and closed, so the same
	// sha is never enqueued twice concurrently, eviction can skip in-flight
	// entries, and no retry goroutine starts after Close.
	mu       sync.Mutex
	inFlight map[string]struct{}
	closed   bool
}

const uploadedMarkerSuffix = ".up"

// NewWriteBackBlobStore builds the cache, re-enqueues any blob the cache dir
// shows was not confirmed uploaded before the last shutdown, and starts the
// uploader pool. Call Close to stop the uploaders.
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
		// Buffered generously so Put rarely meets a full queue; when it
		// does, enqueue uploads synchronously rather than block the request.
		queue:    make(chan string, 1024),
		stopCh:   make(chan struct{}),
		inFlight: make(map[string]struct{}),
	}
	for i := 0; i < cfg.Workers; i++ {
		w.wg.Add(1)
		go w.uploadWorker()
	}
	if err := w.rescanPending(); err != nil {
		// Not fatal: the cache still functions, it may just not have
		// re-enqueued everything.
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

// Put writes the bytes to the local cache and enqueues an async upload,
// returning once the local write is durable (fsync'd + renamed). If the durable
// backend already holds the object, nothing is written or enqueued.
func (w *WriteBackBlobStore) Put(sha string, r io.Reader, size int64) error {
	if len(sha) < 2 {
		return fmt.Errorf("writeback: sha too short")
	}
	// Buffered so the bytes can be written locally and read back for the
	// upload. Callers pass an in-memory body via PutPrecompressed, so this is
	// the staging buffer the upload service already holds, not an extra copy
	// of unbounded size.
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("writeback read body: %w", err)
	}
	// Dedup hit: skip the local write and the enqueue entirely.
	if w.durableHas(sha) {
		return nil
	}
	if err := w.writeLocal(sha, body); err != nil {
		return err
	}
	w.enqueue(sha)
	// Opportunistic, so a long-running process does not grow the cache
	// unbounded between uploads.
	w.evictIfNeeded()
	return nil
}

// PutPrecompressed takes the body already in its stored (compressed,
// magic-prefixed) representation, mirroring the other backends.
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
	defer os.Remove(tmpName) //nolint:errcheck
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
	w.diskBytes.Add(int64(len(body)))
	return nil
}

// durableHas reports whether the durable backend already holds sha. An error
// reads as not-present: the worst case is a redundant Put, which the durable
// backend itself dedups.
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
		return
	default:
	}
	// Queue full: attempt the upload here rather than block the caller on the
	// queue or drop the work. sha stays in flight for the attempt so eviction
	// leaves it alone. A failure hands off to ONE tracked retry goroutine with
	// the workers' bounded backoff; re-entering enqueue instead would spawn an
	// unbounded chain that hammers the failing backend and outlives Close.
	err := w.uploadOnce(sha)
	if err == nil {
		w.releaseInFlight(sha)
		return
	}
	w.logf("writeback: synchronous upload of %s failed: %v", sha, err)
	if !w.goTracked(func() { w.retryUpload(sha) }) {
		// Shutting down: the local copy stays unmarked for the next rescan.
		w.releaseInFlight(sha)
	}
}

func (w *WriteBackBlobStore) releaseInFlight(sha string) {
	w.mu.Lock()
	delete(w.inFlight, sha)
	w.mu.Unlock()
}

// goTracked runs fn in a goroutine the uploader WaitGroup covers, so Close
// waits for it. Reports false, starting nothing, once Close has begun.
func (w *WriteBackBlobStore) goTracked(fn func()) bool {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return false
	}
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		fn()
	}()
	return true
}

// retryUpload waits out one backoff, then runs the same bounded retry the
// workers use. Entered only from the queue-full path, whose first attempt has
// already failed.
func (w *WriteBackBlobStore) retryUpload(sha string) {
	select {
	case <-w.stopCh:
		w.releaseInFlight(sha)
	case <-time.After(w.baseBackoff):
		w.handleUpload(sha)
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

// handleUpload uploads sha with bounded exponential backoff. Persistent failure
// gives up for this cycle but leaves the local copy unmarked, so the next
// startup rescan (or a future Put of the same sha) re-enqueues it.
func (w *WriteBackBlobStore) handleUpload(sha string) {
	defer w.releaseInFlight(sha)
	backoff := w.baseBackoff
	const maxAttempts = 8
	for attempt := range maxAttempts {
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

// uploadOnce reads the cached bytes, Puts them to the durable backend, and
// writes the uploaded marker. A missing local file counts as success (already
// evicted or uploaded).
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
			// Durable store at quota. No marker is written, so the blob stays
			// pinned locally, which is correct: it is not durable yet.
			return err
		}
		return err
	}
	return w.markUploaded(sha)
}

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

// WalkBlobs delegates to the durable backend, which is authoritative for what
// exists for GC purposes. A not-yet-uploaded local-only blob belongs to a live
// paste whose metadata references it, so it is not a GC candidate and shows up
// in the backend once uploaded.
func (w *WriteBackBlobStore) WalkBlobs(fn func(sha string) error) error {
	return w.durable.WalkBlobs(fn)
}

// Remove deletes sha from both the durable backend and the local cache.
func (w *WriteBackBlobStore) Remove(sha string) error {
	derr := w.durable.Remove(sha)
	// Too short to shard, so it cannot name a cache path; the durable backend
	// above decides what such a sha means.
	if len(sha) < 2 {
		return derr
	}
	fi, statErr := os.Stat(w.blobPath(sha))
	if err := os.Remove(w.blobPath(sha)); err == nil && statErr == nil {
		w.diskBytes.Add(-fi.Size())
	}
	_ = os.Remove(w.markerPath(sha))
	return derr
}

// rescanPending re-enqueues every cached blob lacking the uploaded marker, so an
// upload interrupted by a crash or restart resumes. Called once at construction.
func (w *WriteBackBlobStore) rescanPending() error {
	var pending []string
	var total int64
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
		if len(base) < 2 {
			return nil // too short to shard, so not a blob this cache wrote
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		if w.isUploaded(base) {
			return nil // already confirmed durable
		}
		pending = append(pending, base)
		return nil
	})
	// Seed the running byte count even from a partial walk: a walk that errors
	// blocks eviction anyway, so a low seed costs nothing it did not already.
	w.diskBytes.Store(total)
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
// already-uploaded entries oldest-first. A not-yet-uploaded entry is the only
// durable copy of its bytes and is never evicted, so the cap is soft under a
// burst of uploads the backend has not absorbed yet.
func (w *WriteBackBlobStore) evictIfNeeded() {
	// Put calls this every time, so answer from the running count when the
	// cache is nowhere near the cap: the walk below costs a stat per cached
	// file, which a multi-file site deploy would otherwise pay per Put.
	before := w.diskBytes.Load()
	if before <= w.maxBytes {
		return
	}
	entries, total, err := w.scanEntries()
	if err != nil {
		w.logf("writeback: eviction scan error: %v", err)
		return
	}
	// Resync as a delta, not a store, so a concurrent Put's increment survives.
	w.diskBytes.Add(total - before)
	if total <= w.maxBytes {
		return
	}
	// Evictable: uploaded and not in flight, oldest first.
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
		w.diskBytes.Add(-e.size)
	}
}

// scanEntries lists every blob file in the cache, excluding markers and tmp
// files, plus the total byte size.
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
		if len(base) < 2 {
			return nil // too short to shard, so not a blob this cache wrote
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

// Close stops the background uploaders without draining the queue: a blob not
// yet uploaded stays in the cache unmarked and is re-enqueued by the next
// process's startup rescan.
func (w *WriteBackBlobStore) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.stopOne.Do(func() { close(w.stopCh) })
	w.wg.Wait()
}

// drainForTest blocks until the in-flight set is empty or the deadline passes.
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
