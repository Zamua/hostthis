package worker

import (
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// NewSolidProxy forwards apex /_solid/ requests to the Worker's public router
// as forwarded requests (docs/SPEC.md "Pod storage"), over rt, which carries
// the Worker token. apex is the public host the Worker serves them as.
func NewSolidProxy(base, apex string, rt http.RoundTripper) (http.Handler, error) {
	target, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	host := apex
	if h, _, err := net.SplitHostPort(apex); err == nil {
		host = h
	}
	return &httputil.ReverseProxy{
		Transport: rt,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = target.Host
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("Cookie")
			r.Out.Header.Set("X-Hostthis-Forwarded-Host", host)
			if ip, _, err := net.SplitHostPort(r.In.RemoteAddr); err == nil {
				r.Out.Header.Set("X-Hostthis-Client-IP", ip)
			}
		},
	}, nil
}
