package parity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// comparedHeaders is the header set docs/SPEC.md "The public surface" holds
// equal. Transport headers (Date, Server, Content-Length against chunked, an
// edge's Content-Encoding, cf-*, alt-svc, nel, report-to) are not in it.
var comparedHeaders = []string{
	"Content-Type",
	"Cache-Control",
	"ETag",
	"Last-Modified",
	"X-Frame-Options",
	"Referrer-Policy",
	"Permissions-Policy",
	"Content-Security-Policy",
	"Retry-After",
	"Allow",
	"Location",
	"X-Content-Type-Options",
}

// world is the two servers under comparison and what they serve.
type world struct {
	local     bool
	apex      string
	hostthisd *server
	workers   []*server
	// cell is the Worker's internal endpoint, for states no upload reaches.
	// Nil outside a local run.
	cell *cellAPI
	ssh  *sshClient
	// slugs are the pastes the reads compare.
	slugs []slugCase
	// legacy are reads of entries without a decoded size, each also held to
	// its file's bytes. Empty outside a local run.
	legacy []legacyCase
	// writable allows room and socket writes, confined to writeApp outside a
	// local run.
	writable bool
	writeApp string

	compared atomic.Int64
	mu       sync.Mutex
	used     map[string]int
}

// slugCase is one paste and the request-targets beyond the generic set that
// reach into it.
type slugCase struct {
	slug    string
	label   string
	targets []string
}

func (w *world) host(slug string) string { return slug + "." + w.apex }

func (w *world) noteAllowance(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.used == nil {
		w.used = map[string]int{}
	}
	w.used[name]++
}

// checkAllowancesUsed fails on an allowance no request needed: either the
// difference is gone, or the suite stopped making the request that shows it.
func (w *world) checkAllowancesUsed(t *testing.T) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range allowances {
		if w.used[a.name] == 0 {
			t.Errorf("allowance %q explained no difference; drop it or restore the request that needs it", a.name)
		}
	}
}

// expect is check for a request whose answer the scenario fixes, so a setup
// that failed to reach the state cannot pass as two servers agreeing.
func (w *world) expect(t *testing.T, r request, status int) {
	t.Helper()
	if h := w.check(t, r); h.status != status {
		t.Errorf("%s: hostthisd answered %d, the scenario needs %d", r, h.status, status)
	}
}

// check sends r to hostthisd and to every Worker side and reports each
// difference no allowance explains. It returns hostthisd's answer.
func (w *world) check(t *testing.T, r request) response {
	t.Helper()
	w.compared.Add(1)
	h, err := w.hostthisd.do(r)
	if err != nil {
		t.Errorf("%s: hostthisd: %v", r, err)
		return response{}
	}
	for _, s := range w.workers {
		got, err := s.do(r)
		if err != nil {
			t.Errorf("%s: %s: %v", r, s.name, err)
			continue
		}
		if diffs := w.compare(r, s, h, got); len(diffs) > 0 {
			t.Errorf("%s: %s differs from hostthisd:\n  %s", r, s.name, strings.Join(diffs, "\n  "))
		}
	}
	return h
}

// compare returns the differences between hostthisd's answer h and the
// Worker side s's answer got that no allowance explains.
func (w *world) compare(r request, s *server, h, got response) []string {
	c := &comparison{w: w, side: s, req: r, h: h, got: got}
	for _, a := range allowances {
		if len(differences(c.h, c.got)) == 0 {
			break
		}
		if a.apply(c) {
			w.noteAllowance(a.name)
			if c.explained {
				return c.errs
			}
		}
	}
	return append(c.errs, differences(c.h, c.got)...)
}

func differences(h, got response) []string {
	var out []string
	if h.status != got.status {
		out = append(out, fmt.Sprintf("status: hostthisd %d, worker %d", h.status, got.status))
	}
	for _, name := range comparedHeaders {
		hv, wv := h.header.Values(name), got.header.Values(name)
		if !slices.Equal(hv, wv) {
			out = append(out, fmt.Sprintf("%s: hostthisd %q, worker %q", name, hv, wv))
		}
	}
	if (h.bodyErr == nil) != (got.bodyErr == nil) {
		out = append(out, fmt.Sprintf("body error: hostthisd %v, worker %v", h.bodyErr, got.bodyErr))
	}
	if !bytes.Equal(h.body, got.body) {
		out = append(out, bodyDiff(h.body, got.body))
	}
	return out
}

func decodeJSON(b []byte) (any, bool) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, false
	}
	return v, !d.More()
}

// sameJSON compares two documents as values; numbers keep their spelling.
func sameJSON(a, b []byte) bool {
	av, aok := decodeJSON(a)
	bv, bok := decodeJSON(b)
	return aok && bok && reflect.DeepEqual(av, bv)
}

func bodyDiff(h, got []byte) string {
	i := 0
	for i < len(h) && i < len(got) && h[i] == got[i] {
		i++
	}
	lo := max(0, i-40)
	window := func(b []byte) []byte { return b[min(lo, len(b)):min(i+80, len(b))] }
	return fmt.Sprintf("body: hostthisd %d bytes, worker %d bytes, first difference at %d: hostthisd %q, worker %q",
		len(h), len(got), i, window(h), window(got))
}
