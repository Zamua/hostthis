package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Zamua/hostthis/internal/domain"
)

// cell is one Worker endpoint. The repos share it: every op is one
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
	case http.StatusNotFound, http.StatusConflict, http.StatusLocked,
		http.StatusRequestEntityTooLarge, http.StatusInsufficientStorage:
		return true
	}
	return false
}

// do sends one cell request and returns the raw response; call is the usual
// wrapper. Callers needing the body of an answer status (a 429 carrying its
// wait) use do directly.
func (c *cell) do(ctx context.Context, rt route, val string, body any) (*http.Response, error) {
	path := rt.path
	u := c.base + rt.target(val)
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("worker: encode %s: %w", path, err)
		}
		payload = b
	}
	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, rt.method, u, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return c.client.Do(req)
	}
	resp, err := send()
	// A read is safe to send twice, and a pooled connection the far side has
	// already closed fails one exchange with nothing answered.
	if err != nil && rt.method == http.MethodGet && ctx.Err() == nil {
		resp, err = send()
	}
	if err != nil {
		return nil, fmt.Errorf("worker: %s: %w", path, err)
	}
	return resp, nil
}

func (c *cell) call(ctx context.Context, rt route, val string, body, out any) (int, error) {
	path := rt.path
	resp, err := c.do(ctx, rt, val, body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close() //nolint:errcheck
	// Answer statuses are returned, never wrapped, or the sentinel would hide
	// behind a transport failure. A status off the list is a fault.
	if resp.StatusCode >= 400 && !isCellAnswer(resp.StatusCode) {
		// Carry the cell's own explanation up: the identity cell says WHICH
		// value it refused, and the number is the whole diagnostic.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if len(detail) > 0 {
			return resp.StatusCode, fmt.Errorf("worker: %s: status %d: %s", path, resp.StatusCode, detail)
		}
		return resp.StatusCode, fmt.Errorf("worker: %s: status %d", path, resp.StatusCode)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 300 {
		// An answer's JSON body names which refusal it is. A plain-text body (a
		// cell 404's "not found\n") decodes to nothing and leaves out untouched.
		_ = json.NewDecoder(resp.Body).Decode(out)
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return resp.StatusCode, fmt.Errorf("worker: decode %s: %w", path, err)
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
		return fmt.Errorf("worker: %s: unexpected status %d", op, status)
	}
	return nil
}

// ask is call with its status mapped by answer.
func (c *cell) ask(ctx context.Context, op string, rt route, val string, body, out any,
	answers map[int]error,
) error {
	status, err := c.call(ctx, rt, val, body, out)
	return answer(op, status, err, answers)
}
