package screen

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Zamua/hostthis/internal/domain"
)

func TestClient_SendsTheProtocolAndReadsTheVerdict(t *testing.T) {
	var got *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, body = r, must(io.ReadAll(r.Body))
		_, _ = w.Write([]byte(`{"allow":false,"rule":"yara:x","file":"a/b.html","message":"why"}`))
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL + "/", Token: "t"}
	v, err := c.Screen(context.Background(), domain.ScreenRequest{
		Shape: domain.ScreenDocument, Name: "n.html", Truncated: true, Body: bytes.NewBufferString("<h1>x</h1>"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v != (domain.ScreenVerdict{Allow: false, Rule: "yara:x", File: "a/b.html", Message: "why"}) {
		t.Fatalf("verdict %+v", v)
	}
	checks := map[string]string{
		"path": got.URL.Path, "shape": got.Header.Get("X-Hostthis-Shape"), "name": got.Header.Get("X-Hostthis-Name"),
		"truncated": got.Header.Get("X-Hostthis-Truncated"), "auth": got.Header.Get("Authorization"), "body": string(body),
	}
	want := map[string]string{
		"path": "/screen", "shape": "document", "name": "n.html", "truncated": "1", "auth": "Bearer t", "body": "<h1>x</h1>",
	}
	for k := range want {
		if checks[k] != want[k] {
			t.Errorf("%s = %q, want %q", k, checks[k], want[k])
		}
	}
	if got.ContentLength != int64(len("<h1>x</h1>")) {
		t.Errorf("Content-Length %d", got.ContentLength)
	}
}

func TestClient_AnythingButAVerdictIsAnError(t *testing.T) {
	for _, answer := range []struct {
		status int
		body   string
	}{{503, `{"error":"x"}`}, {200, `{}`}, {200, `not json`}, {401, ``}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(answer.status)
			_, _ = w.Write([]byte(answer.body))
		}))
		_, err := (&Client{Base: srv.URL}).Screen(context.Background(),
			domain.ScreenRequest{Shape: domain.ScreenArchive, Body: bytes.NewReader([]byte("x"))})
		srv.Close()
		if err == nil {
			t.Errorf("%d %q: no error", answer.status, answer.body)
		}
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
