package celld

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// cell is one celld Worker endpoint. The repos share it: every op is one
// routed HTTP request against it.
type cell struct {
	base   string
	client *http.Client
}

func newCell(base string, c *http.Client) *cell {
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	return &cell{base: base, client: c}
}

// isCellAnswer reports whether a status is part of the adapter's vocabulary
// rather than a failure. Kept as a list so adding a new one is a deliberate
// edit, not a widened comparison.
func isCellAnswer(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusConflict,
		http.StatusRequestEntityTooLarge, http.StatusInsufficientStorage:
		return true
	}
	return false
}

// do sends one cell request and returns the raw response; call is the usual
// wrapper. Callers needing the body of an answer status (a 429 carrying its
// wait) use do directly.
func (c *cell) do(ctx context.Context, method, path, key, val string, body any) (*http.Response, error) {
	// The routing param is MERGED rather than appended: a path may already carry
	// query params of its own, and a second bare "?" makes the whole string one
	// unparseable query, which presents as the cell rejecting a param that is
	// plainly in the URL.
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	u := fmt.Sprintf("%s%s%s%s=%s", c.base, path, sep, key, url.QueryEscape(val))
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("celld: encode %s: %w", path, err)
		}
		payload = b
	}
	rdr := bytes.NewReader(payload)
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, fmt.Errorf("celld: build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("celld: %s: %w", path, err)
	}
	return resp, nil
}

func (c *cell) call(ctx context.Context, method, path, key, val string, body, out any) (int, error) {
	resp, err := c.do(ctx, method, path, key, val, body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	// Answer statuses are returned, never wrapped: wrapping one as an error
	// would hide the sentinel behind a transport failure. A status off the
	// list is a fault: the cell did something the adapter does not model.
	if resp.StatusCode >= 400 && !isCellAnswer(resp.StatusCode) {
		// Carry the cell's own explanation up. The identity cell refuses an
		// impossible charge total and says WHICH value it refused; discarding
		// that would trade a legible failure for a bare status code, and the
		// number is the whole diagnostic.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if len(detail) > 0 {
			return resp.StatusCode, fmt.Errorf("celld: %s: status %d: %s", path, resp.StatusCode, detail)
		}
		return resp.StatusCode, nil
	}
	// Answer statuses >= 300 carry sentinel meaning, not a decodable body: a
	// cell 404 says "not found\n", which is no caller's JSON.
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return resp.StatusCode, fmt.Errorf("celld: decode %s: %w", path, err)
		}
	}
	return resp.StatusCode, nil
}

var notFound = map[int]error{http.StatusNotFound: domain.ErrNotFound}

// answer maps the outcome of one call: a listed status returns its sentinel,
// any other status past 2xx is a fault naming op.
func answer(op string, status int, err error, answers map[int]error) error {
	if err != nil {
		return err
	}
	if mapped, ok := answers[status]; ok {
		return mapped
	}
	if status >= 300 {
		return fmt.Errorf("celld: %s: unexpected status %d", op, status)
	}
	return nil
}

// ask is call with its status mapped by answer.
func (c *cell) ask(ctx context.Context, op, method, path, key, val string, body, out any,
	answers map[int]error,
) error {
	status, err := c.call(ctx, method, path, key, val, body, out)
	return answer(op, status, err, answers)
}
