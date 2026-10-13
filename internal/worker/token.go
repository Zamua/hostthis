package worker

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// Authorize returns rt with "Authorization: Bearer <token>" on every request
// to endpoint's host. Requests to any other host, a redirect target included,
// go out without it. An empty token returns rt unchanged, for a Worker that
// requires none. A nil rt means http.DefaultTransport.
func Authorize(rt http.RoundTripper, endpoint, token string) (http.RoundTripper, error) {
	if rt == nil {
		rt = http.DefaultTransport
	}
	if token == "" {
		return rt, nil
	}
	// Errors name the problem, never the value: the token must not reach a log.
	for i := 0; i < len(token); i++ {
		if token[i] <= ' ' || token[i] > '~' {
			return nil, errors.New("worker: token must be visible ASCII without spaces")
		}
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, errors.New("worker: a token needs an endpoint with a host")
	}
	return &bearer{rt: rt, host: u.Host, header: "Bearer " + token}, nil
}

type bearer struct {
	rt     http.RoundTripper
	host   string
	header string
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.EqualFold(req.URL.Host, b.host) {
		return b.rt.RoundTrip(req)
	}
	// A RoundTripper must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", b.header)
	return b.rt.RoundTrip(req)
}
