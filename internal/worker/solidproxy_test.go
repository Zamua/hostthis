package worker

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A /_solid/ request reaches the Worker as a forwarded request for the apex,
// without the reader's credentials.
func TestSolidProxyForwardsAsTheApex(t *testing.T) {
	var got *http.Request
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	proxy, err := NewSolidProxy(upstream.URL, "paste.test", http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://paste.test/_solid/jwks", nil)
	r.Header.Set("Authorization", "Bearer reader")
	r.Header.Set("Cookie", "a=b")
	r.RemoteAddr = "203.0.113.7:5555"
	w := httptest.NewRecorder()
	proxy.ServeHTTP(w, r)
	if w.Code != 200 || got == nil {
		t.Fatalf("status %d", w.Code)
	}
	if got.URL.Path != "/_solid/jwks" || got.Header.Get("X-Hostthis-Forwarded-Host") != "paste.test" ||
		got.Header.Get("X-Hostthis-Client-IP") != "203.0.113.7" || got.Header.Get("Authorization") != "" || got.Header.Get("Cookie") != "" {
		t.Fatalf("forwarded %s with %v", got.URL.Path, got.Header)
	}
}
