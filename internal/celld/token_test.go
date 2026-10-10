package celld_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"

	"github.com/Zamua/hostthis/internal/celld"
)

const testToken = "tok-3f9a.b_~+/="

// recorder is a TLS server that records the Authorization header of each
// request, so the tests cover the https endpoint a public Worker sits behind.
func recorder(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() []string) {
	t.Helper()
	var (
		mu   sync.Mutex
		seen []string
	)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if handler != nil {
			handler(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

// trusting is a transport that trusts every httptest TLS server.
func trusting() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test servers only
	return tr
}

func authorized(t *testing.T, endpoint, token string) *http.Client {
	t.Helper()
	rt, err := celld.Authorize(trusting(), endpoint, token)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return &http.Client{Transport: rt}
}

func get(t *testing.T, c *http.Client, url string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	resp.Body.Close() //nolint:errcheck
}

func TestAuthorizeSendsTheBearerTokenToTheWorker(t *testing.T) {
	srv, seen := recorder(t, nil)
	get(t, authorized(t, srv.URL+"/ns/x", testToken), srv.URL+"/ns/x/paste/get?slug=abc12345")
	if got := seen(); len(got) != 1 || got[0] != "Bearer "+testToken {
		t.Fatalf("Authorization = %q, want the bearer token", got)
	}
}

func TestAuthorizeWithoutATokenSendsNoHeader(t *testing.T) {
	srv, seen := recorder(t, nil)
	get(t, authorized(t, srv.URL, ""), srv.URL+"/paste/get")
	if got := seen(); len(got) != 1 || got[0] != "" {
		t.Fatalf("Authorization = %q, want none", got)
	}
}

// A redirect off the Worker's host must not carry the token with it.
func TestAuthorizeKeepsTheTokenFromOtherHosts(t *testing.T) {
	other, otherSeen := recorder(t, nil)
	worker, workerSeen := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusFound)
	})
	get(t, authorized(t, worker.URL, testToken), worker.URL+"/blob/k")
	if got := workerSeen(); len(got) != 1 || got[0] != "Bearer "+testToken {
		t.Fatalf("worker saw Authorization %q", got)
	}
	if got := otherSeen(); len(got) != 1 || got[0] != "" {
		t.Fatalf("redirect target saw Authorization %q, want none", got)
	}
}

func TestAuthorizeLeavesTheCallersRequestUnchanged(t *testing.T) {
	srv, _ := recorder(t, nil)
	c := authorized(t, srv.URL, testToken)
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/paste/get", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("caller's request gained Authorization %q", got)
	}
}

// A token a header cannot carry, or an endpoint without a host, stops startup,
// and the error never repeats the token.
func TestAuthorizeRefusesUnusableConfig(t *testing.T) {
	for _, tc := range []struct{ endpoint, token string }{
		{"https://worker.example", "tok\n"},
		{"https://worker.example", "two words"},
		{"https://worker.example", "tök"},
		{"", testToken},
		{"not a url", testToken},
	} {
		_, err := celld.Authorize(nil, tc.endpoint, tc.token)
		if err == nil {
			t.Fatalf("endpoint %q token %q accepted", tc.endpoint, tc.token)
		}
		if strings.Contains(err.Error(), tc.token) {
			t.Fatalf("error %q repeats the token", err)
		}
	}
}

// The room relay dials an https Worker as wss, carrying the token.
func TestRoomProxyDialsAnHTTPSWorkerWithTheToken(t *testing.T) {
	const snapshot = `{"type":"snapshot","seq":0,"state":{}}`
	cell, seen := recorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, []byte(snapshot))
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	})
	_, client, ctx := proxiedBy(t, celld.NewRoomProxy(cell.URL, authorized(t, cell.URL, testToken)))
	_, data, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read snapshot: %v (cell saw Authorization %q)", err, seen())
	}
	if string(data) != snapshot {
		t.Fatalf("snapshot = %q", data)
	}
}
