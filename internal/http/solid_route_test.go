package http

import (
	"net/http"
	"testing"
)

// The apex /_solid/ paths reach the Solid handler: the Worker serves them, but
// no route reaches the apex, which SSH keeps off Cloudflare.
func TestSolidPathsReachTheProxyOnTheApexOnly(t *testing.T) {
	srv := &Server{ApexDomain: "paste.test", Solid: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("solid\n"))
	})}
	if w := serve(srv, "paste.test", "/_solid/jwks"); w.Code != 200 || w.Body.String() != "solid\n" {
		t.Fatalf("apex: status %d body %q", w.Code, w.Body.String())
	}
	if w := serve(srv, "abc23456.paste.test", "/_solid/jwks"); w.Code != 404 {
		t.Fatalf("slug host: status %d, want 404", w.Code)
	}
	if w := serve(&Server{ApexDomain: "paste.test"}, "paste.test", "/_solid/jwks"); w.Code != 404 {
		t.Fatalf("no handler: status %d, want 404", w.Code)
	}
}
