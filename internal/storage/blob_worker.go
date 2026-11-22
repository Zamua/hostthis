package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// blobSizeHeader carries a GET's stored length: the Worker streams bodies
// chunked, without Content-Length.
const blobSizeHeader = "X-Blob-Size"

// WorkerBlobStore is the object store behind the Worker's /blob routes,
// which keep each object in the Worker's R2 binding. The Worker stores opaque
// bytes; the at-rest encoding belongs to CompressedBlobStore, as for the other
// raw stores.
type WorkerBlobStore struct {
	base   string
	client *http.Client
	// StallTimeout ends a Put whose body the Worker stops reading for this
	// long, so a stuck upstream cannot hold the write (and whatever admitted
	// it) forever. Zero disables it.
	StallTimeout time.Duration
	// WriteFloor and WriteMinRate bound a whole Put of known size, response
	// included, to WriteFloor plus the time to send the body at WriteMinRate
	// bytes per second. That ends a write the stall timeout cannot, such as
	// one the Worker reads a trickle at a time or never answers in full. A
	// zero WriteFloor disables the bound.
	WriteFloor   time.Duration
	WriteMinRate int64
}

const (
	// DefaultBlobStallTimeout is how long a Put's body may sit unread.
	DefaultBlobStallTimeout = time.Minute
	// DefaultBlobWriteFloor is the least time any Put gets; it matches the
	// transport's wait for a response, so a small write keeps that allowance.
	DefaultBlobWriteFloor = time.Minute
	// DefaultBlobWriteMinRate is the slowest sustained transfer a Put may
	// make, in bytes per second.
	DefaultBlobWriteMinRate = 256 << 10
)

var (
	// errBlobStalled is the cause of a Put ended by StallTimeout.
	errBlobStalled = errors.New("the store stopped reading the body")
	// errBlobDeadline is the cause of a Put ended by its write deadline.
	errBlobDeadline = errors.New("the write ran past its deadline")
)

// NewWorkerBlobStore talks to the Worker at base. A nil client gets one over
// WorkerBlobTransport with no overall deadline, since a body of any size streams
// through it; a write is bounded by progress (StallTimeout) and by a deadline
// that grows with its size (WriteFloor, WriteMinRate) instead.
func NewWorkerBlobStore(base string, client *http.Client) (*WorkerBlobStore, error) {
	if base == "" {
		return nil, errors.New("blob worker: endpoint required")
	}
	if client == nil {
		client = &http.Client{Transport: WorkerBlobTransport()}
	}
	return &WorkerBlobStore{
		base: strings.TrimRight(base, "/"), client: client, StallTimeout: DefaultBlobStallTimeout,
		WriteFloor: DefaultBlobWriteFloor, WriteMinRate: DefaultBlobWriteMinRate,
	}, nil
}

// WorkerBlobTransport bounds the wait for a response, which starts only after
// the request body is fully sent, rather than the whole exchange.
func WorkerBlobTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = time.Minute
	return tr
}

func (s *WorkerBlobStore) objectURL(key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return s.base + "/blob/" + strings.Join(segs, "/")
}

// Put streams r to key with size as its Content-Length. A negative size sends
// the body chunked. A seekable body (a spill file) is resent once when the
// exchange fails before any response: the edge in front of the Worker resets
// an occasional stream, and a PUT of the same bytes to the same key is
// idempotent.
func (s *WorkerBlobStore) Put(key string, r io.Reader, size int64) error {
	return s.PutContext(context.Background(), key, r, size)
}

// PutContext is Put ended early when ctx is done.
func (s *WorkerBlobStore) PutContext(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := checkKey(key); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if limit := s.writeDeadline(size); limit > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeoutCause(ctx, limit, fmt.Errorf("%w of %s", errBlobDeadline, limit))
		defer stop()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.objectURL(key), r)
	if err != nil {
		return fmt.Errorf("blob worker: put %s: %w", key, err)
	}
	req.ContentLength = size
	switch rs, ok := r.(io.ReadSeeker); {
	case size == 0:
		req.Body = http.NoBody
	case ok:
		// The transport closes a request body; the caller owns the file.
		req.Body = io.NopCloser(rs)
		start, err := rs.Seek(0, io.SeekCurrent)
		if err != nil {
			return fmt.Errorf("blob worker: put %s: %w", key, err)
		}
		req.GetBody = func() (io.ReadCloser, error) {
			if _, err := rs.Seek(start, io.SeekStart); err != nil {
				return nil, err
			}
			return io.NopCloser(rs), nil
		}
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	watch := watchBodyProgress(req, s.StallTimeout, func() {
		cancel(fmt.Errorf("%w for %s", errBlobStalled, s.StallTimeout))
	})
	defer watch.stop()
	resp, err := s.client.Do(req)
	if err != nil && req.GetBody != nil && req.Context().Err() == nil {
		retry := req.Clone(req.Context())
		if retry.Body, err = req.GetBody(); err == nil {
			resp, err = s.client.Do(retry)
		}
	}
	if err != nil {
		if cause := context.Cause(ctx); errors.Is(cause, errBlobStalled) || errors.Is(cause, errBlobDeadline) {
			return fmt.Errorf("blob worker: put %s: %w", key, cause)
		}
		return fmt.Errorf("blob worker: put %s: %w", key, err)
	}
	return expectNoContent(resp, "put "+key)
}

// writeDeadline is how long a Put of size bytes may take in all, or zero for
// no bound. A body of unknown length has only the stall timeout.
func (s *WorkerBlobStore) writeDeadline(size int64) time.Duration {
	if s.WriteFloor <= 0 || size < 0 {
		return 0
	}
	limit := s.WriteFloor
	if s.WriteMinRate > 0 {
		limit += time.Duration(float64(size) / float64(s.WriteMinRate) * float64(time.Second))
	}
	return limit
}

// bodyWatch ends a request whose body goes unread for too long. Only the
// sending of the body is watched: once it is fully read, the transport's own
// response timeout takes over.
type bodyWatch struct {
	mu       sync.Mutex
	last     time.Time
	size     int64 // the body's length, negative when unknown
	sent     int64
	done     bool
	timer    *time.Timer
	limit    time.Duration
	onStall  func()
	stopOnce sync.Once
}

// watchBodyProgress wraps req's body (and the copy a resend makes) so every
// read counts as progress, and calls onStall after limit without one.
func watchBodyProgress(req *http.Request, limit time.Duration, onStall func()) *bodyWatch {
	w := &bodyWatch{last: time.Now(), size: req.ContentLength, limit: limit, onStall: onStall}
	if limit <= 0 || req.Body == nil || req.Body == http.NoBody {
		w.done = true
		return w
	}
	req.Body = &watchedBody{ReadCloser: req.Body, watch: w}
	if getBody := req.GetBody; getBody != nil {
		req.GetBody = func() (io.ReadCloser, error) {
			body, err := getBody()
			if err != nil {
				return nil, err
			}
			w.restart()
			return &watchedBody{ReadCloser: body, watch: w}, nil
		}
	}
	w.timer = time.AfterFunc(limit, w.check)
	return w
}

// progress records n more bytes read. The body is done at its end, at a read
// error, or once its whole length has been read: the transport stops reading
// a body of known length there, without asking for the end.
func (w *bodyWatch) progress(n int, ended bool) {
	w.mu.Lock()
	w.last = time.Now()
	w.sent += int64(n)
	w.done = w.done || ended || (w.size >= 0 && w.sent >= w.size)
	w.mu.Unlock()
}

// restart begins the count again for a resent body.
func (w *bodyWatch) restart() {
	w.mu.Lock()
	w.last, w.sent, w.done = time.Now(), 0, false
	w.mu.Unlock()
}

func (w *bodyWatch) check() {
	w.mu.Lock()
	idle, done := time.Since(w.last), w.done
	w.mu.Unlock()
	switch {
	case done:
	case idle >= w.limit:
		w.onStall()
	default:
		w.timer.Reset(w.limit - idle)
	}
}

func (w *bodyWatch) stop() {
	w.stopOnce.Do(func() {
		w.progress(0, true)
		if w.timer != nil {
			w.timer.Stop()
		}
	})
}

type watchedBody struct {
	io.ReadCloser
	watch *bodyWatch
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.watch.progress(n, err != nil)
	return n, err
}

// GetReader streams the stored bytes and their length. The caller closes it.
func (s *WorkerBlobStore) GetReader(key string) (io.ReadCloser, int64, error) {
	if err := checkKey(key); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("blob worker: get %s: %w", key, err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.doRead(req)
	if err != nil {
		return nil, 0, fmt.Errorf("blob worker: get %s: %w", key, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		discardBody(resp)
		return nil, 0, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, 0, statusError(resp, "get "+key)
	}
	size, err := strconv.ParseInt(resp.Header.Get(blobSizeHeader), 10, 64)
	if err != nil || size < 0 {
		discardBody(resp)
		return nil, 0, fmt.Errorf("blob worker: get %s: bad %s %q", key, blobSizeHeader, resp.Header.Get(blobSizeHeader))
	}
	return resp.Body, size, nil
}

// GetRange streams length bytes of key from offset. The Worker answers 206
// with the slice's length in X-Blob-Size.
func (s *WorkerBlobStore) GetRange(key string, offset, length int64) (io.ReadCloser, error) {
	if err := checkKey(key); err != nil {
		return nil, err
	}
	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("blob worker: get %s: bad range %d+%d", key, offset, length)
	}
	req, err := http.NewRequest(http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, fmt.Errorf("blob worker: get %s: %w", key, err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	resp, err := s.doRead(req)
	if err != nil {
		return nil, fmt.Errorf("blob worker: get %s: %w", key, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		discardBody(resp)
		return nil, ErrNotFound
	case resp.StatusCode != http.StatusPartialContent:
		return nil, statusError(resp, "get range "+key)
	}
	if got := resp.Header.Get(blobSizeHeader); got != strconv.FormatInt(length, 10) {
		discardBody(resp)
		return nil, fmt.Errorf("blob worker: get %s: range answered %s %q, want %d", key, blobSizeHeader, got, length)
	}
	return resp.Body, nil
}

// DeletePrefix asks the Worker to delete every object under prefix.
func (s *WorkerBlobStore) DeletePrefix(prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, s.base+"/blob?prefix="+url.QueryEscape(prefix), nil)
	if err != nil {
		return fmt.Errorf("blob worker: delete %s: %w", prefix, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("blob worker: delete %s: %w", prefix, err)
	}
	return expectNoContent(resp, "delete "+prefix)
}

// Delete asks the Worker to delete one object.
func (s *WorkerBlobStore) Delete(key string) error {
	req, err := http.NewRequest(http.MethodDelete, s.objectURL(key), nil)
	if err != nil {
		return fmt.Errorf("blob worker: delete %s: %w", key, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("blob worker: delete %s: %w", key, err)
	}
	return expectNoContent(resp, "delete "+key)
}

// doRead sends a bodiless read, retrying once when the exchange fails before
// any response: a dropped connection mid-deploy otherwise refuses the whole
// deploy over one read.
func (s *WorkerBlobStore) doRead(req *http.Request) (*http.Response, error) {
	resp, err := s.client.Do(req)
	if err == nil || req.Context().Err() != nil {
		return resp, err
	}
	return s.client.Do(req.Clone(req.Context()))
}

func expectNoContent(resp *http.Response, op string) error {
	if resp.StatusCode != http.StatusNoContent {
		return statusError(resp, op)
	}
	discardBody(resp)
	return nil
}

// statusError closes resp and names its status with a bounded excerpt of the
// Worker's message.
func statusError(resp *http.Response, op string) error {
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	discardBody(resp)
	var pod struct {
		Error  string `json:"error"`
		Status int    `json:"status"`
	}
	if resp.StatusCode == http.StatusBadGateway && json.Unmarshal(msg, &pod) == nil && pod.Error == "pod" {
		return fmt.Errorf("blob worker: %s: %w", op, &domain.PodRefusedError{Status: pod.Status})
	}
	return fmt.Errorf("blob worker: %s: %s: %s", op, resp.Status, strings.TrimSpace(string(msg)))
}

// discardBody drains a bounded tail so the connection can be reused, then
// closes it.
func discardBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
