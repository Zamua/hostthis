package http

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerErrorLog_ScrubsPeerAddresses(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http: panic serving 203.0.113.5:40000: boom", "http: panic serving <peer>: boom"},
		{"http: TLS handshake error from [2001:db8::1]:5555: EOF", "http: TLS handshake error from <peer>: EOF"},
		{"http: panic serving 203.0.113.5: boom", "http: panic serving <peer>: boom"},
		{"http: Accept error: too many open files; retrying in 5ms", "http: Accept error: too many open files; retrying in 5ms"},
	} {
		var got []string
		l := ServerErrorLog(func(f string, a ...any) { got = append(got, strings.TrimSpace(fmt.Sprintf(f, a...))) })
		l.Print(tc.in)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A panicking handler is logged by net/http with the peer address; the line
// that reaches the log has none.
func TestServerErrorLog_ARealPanicPrintsNoPeer(t *testing.T) {
	var lines []string
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	ts.Config.ErrorLog = ServerErrorLog(func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) })
	ts.Start()
	defer ts.Close()

	if resp, err := http.Get(ts.URL); err == nil {
		_ = resp.Body.Close()
	}
	ts.Close()
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "panic serving") {
		t.Fatalf("no panic line logged: %q", joined)
	}
	if strings.Contains(joined, "127.0.0.1") || strings.Contains(joined, "::1") {
		t.Fatalf("log carries the peer address: %q", joined)
	}
}
