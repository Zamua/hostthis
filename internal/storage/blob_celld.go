package storage

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// blobSizeHeader carries a GET's stored length: the Worker streams bodies
// chunked, without Content-Length.
const blobSizeHeader = "X-Blob-Size"

// CelldBlobStore is the object store behind the celld Worker's /blob routes,
// which keep each object in the Worker's R2 binding. The Worker stores opaque
// bytes; the at-rest encoding belongs to CompressedBlobStore, as for the other
// raw stores.
type CelldBlobStore struct {
	base   string
	client *http.Client
}

// NewCelldBlobStore talks to the Worker at base. A nil client gets one with no
// overall deadline, since a body of any size streams through it.
func NewCelldBlobStore(base string, client *http.Client) (*CelldBlobStore, error) {
	if base == "" {
		return nil, errors.New("blob celld: endpoint required")
	}
	if client == nil {
		client = defaultCelldBlobClient()
	}
	return &CelldBlobStore{base: strings.TrimRight(base, "/"), client: client}, nil
}

// defaultCelldBlobClient bounds the wait for a response, which starts only
// after the request body is fully sent, rather than the whole exchange.
func defaultCelldBlobClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = time.Minute
	return &http.Client{Transport: tr}
}

func (s *CelldBlobStore) objectURL(key string) string {
	segs := strings.Split(key, "/")
	for i, seg := range segs {
		segs[i] = url.PathEscape(seg)
	}
	return s.base + "/blob/" + strings.Join(segs, "/")
}

// Put streams r to key with size as its Content-Length. A negative size sends
// the body chunked.
func (s *CelldBlobStore) Put(key string, r io.Reader, size int64) error {
	if err := checkKey(key); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, s.objectURL(key), r)
	if err != nil {
		return fmt.Errorf("blob celld: put %s: %w", key, err)
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("blob celld: put %s: %w", key, err)
	}
	return expectNoContent(resp, "put "+key)
}

// GetReader streams the stored bytes and their length. The caller closes it.
func (s *CelldBlobStore) GetReader(key string) (io.ReadCloser, int64, error) {
	if err := checkKey(key); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequest(http.MethodGet, s.objectURL(key), nil)
	if err != nil {
		return nil, 0, fmt.Errorf("blob celld: get %s: %w", key, err)
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("blob celld: get %s: %w", key, err)
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
		return nil, 0, fmt.Errorf("blob celld: get %s: bad %s %q", key, blobSizeHeader, resp.Header.Get(blobSizeHeader))
	}
	return resp.Body, size, nil
}

// DeletePrefix asks the Worker to delete every object under prefix.
func (s *CelldBlobStore) DeletePrefix(prefix string) error {
	if err := checkPrefix(prefix); err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodDelete, s.base+"/blob?prefix="+url.QueryEscape(prefix), nil)
	if err != nil {
		return fmt.Errorf("blob celld: delete %s: %w", prefix, err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("blob celld: delete %s: %w", prefix, err)
	}
	return expectNoContent(resp, "delete "+prefix)
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
	return fmt.Errorf("blob celld: %s: %s: %s", op, resp.Status, strings.TrimSpace(string(msg)))
}

// discardBody drains a bounded tail so the connection can be reused, then
// closes it.
func discardBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}
