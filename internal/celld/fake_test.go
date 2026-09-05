package celld

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeCell stands in for the Worker: every request is recorded in order and
// answered by reply.
type fakeCell struct {
	reply func(cellRequest) (*http.Response, error)
	calls []cellRequest
}

type cellRequest struct {
	Method, Path, Query string
	Body                []byte
}

// fields decodes the request body as a JSON object.
func (c cellRequest) fields(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(c.Body, &body); err != nil {
		t.Fatalf("decode %s body %q: %v", c.Path, c.Body, err)
	}
	return body
}

func (f *fakeCell) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
	}
	c := cellRequest{Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Body: body}
	f.calls = append(f.calls, c)
	return f.reply(c)
}

func (f *fakeCell) last() cellRequest { return f.calls[len(f.calls)-1] }

func (f *fakeCell) client() *http.Client { return &http.Client{Transport: f} }

// fixedCell answers every request with one status and body.
func fixedCell(status int, body string) *fakeCell {
	return &fakeCell{reply: func(cellRequest) (*http.Response, error) {
		return cellResponse(status, body), nil
	}}
}

func cellResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
