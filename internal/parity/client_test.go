package parity

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The Worker's forwarded mode headers (docs/SPEC.md "Public and internal
// hosts").
const (
	forwardedHost     = "X-Hostthis-Forwarded-Host"
	forwardedClientIP = "X-Hostthis-Client-IP"
)

// request is one request as both servers receive it: the same method,
// request-target, headers and body, addressed to the same public host.
type request struct {
	method string
	host   string // the public host it addresses
	target string // the request-target, sent as written
	header http.Header
	body   []byte
	// client is the client's address for the room creation limit, "" for
	// none.
	client string
}

func get(host, target string) request {
	return request{method: "GET", host: host, target: target}
}

func (r request) with(name, value string) request {
	r.header = r.header.Clone()
	if r.header == nil {
		r.header = http.Header{}
	}
	r.header.Set(name, value)
	return r
}

func (r request) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s http://%s%s", r.method, r.host, r.target)
	for _, name := range []string{"If-None-Match", "If-Modified-Since", "Origin", "Upgrade"} {
		if v := r.header.Get(name); v != "" {
			fmt.Fprintf(&b, " [%s: %s]", name, v)
		}
	}
	if len(r.body) > 0 {
		fmt.Fprintf(&b, " (%d byte body)", len(r.body))
	}
	return b.String()
}

type response struct {
	status int
	header http.Header
	body   []byte
	// bodyErr is a body that ended in an error, such as a stream cut short.
	bodyErr error
}

// kind names how a server is reached.
type kind int

const (
	// hostthisd's HTTP surface.
	viaHostthisd kind = iota
	// The Worker's public surface, by Host.
	viaDirect
	// The Worker's internal endpoint, with the token and the forwarded host.
	viaForwarded
)

// server is one side of the comparison.
type server struct {
	name string
	via  kind
	// addr is the address every request dials; empty dials the request's own
	// public host at port.
	addr   string
	port   string
	tls    bool
	token  string
	trusts bool // takes X-Forwarded-For as the client address

	mu   sync.Mutex
	idle map[string][]*pooled
}

// pooled is a kept-alive connection: a run makes tens of thousands of
// requests, more than the ephemeral ports one connection each would take.
type pooled struct {
	net.Conn
	r *bufio.Reader
}

func (s *server) target(host string) string {
	if s.addr != "" {
		return s.addr
	}
	return net.JoinHostPort(strings.TrimSuffix(host, "."), s.port)
}

// conn is an idle connection to addr, or a new one; reused says which.
func (s *server) conn(addr string) (c *pooled, reused bool, err error) {
	s.mu.Lock()
	if n := len(s.idle[addr]); n > 0 {
		c = s.idle[addr][n-1]
		s.idle[addr] = s.idle[addr][:n-1]
		s.mu.Unlock()
		return c, true, nil
	}
	s.mu.Unlock()
	d := &net.Dialer{Timeout: 15 * time.Second}
	var nc net.Conn
	if s.tls {
		name, _, _ := net.SplitHostPort(addr)
		nc, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: name, NextProtos: []string{"http/1.1"}})
	} else {
		nc, err = d.Dial("tcp", addr)
	}
	if err != nil {
		return nil, false, err
	}
	return &pooled{Conn: nc, r: bufio.NewReader(nc)}, false, nil
}

func (s *server) release(addr string, c *pooled) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idle == nil {
		s.idle = map[string][]*pooled{}
	}
	s.idle[addr] = append(s.idle[addr], c)
}

// headers are r's headers as s receives them.
func (s *server) headers(r request, addr string) http.Header {
	h := r.header.Clone()
	if h == nil {
		h = http.Header{}
	}
	switch s.via {
	case viaForwarded:
		h.Set("Host", addr)
		h.Set("Authorization", "Bearer "+s.token)
		h.Set(forwardedHost, r.host)
		if r.client != "" {
			h.Set(forwardedClientIP, r.client)
		}
	default:
		h.Set("Host", r.host)
		if r.client != "" && s.trusts {
			h.Set("X-Forwarded-For", r.client)
		}
	}
	return h
}

// do sends r byte for byte as written and reads the whole answer.
func (s *server) do(r request) (response, error) {
	addr := s.target(r.host)
	for {
		c, reused, err := s.conn(addr)
		if err != nil {
			return response{}, err
		}
		out, keep, err := s.exchange(c, r, addr)
		if err != nil {
			_ = c.Close()
			// The server closed an idle connection before reading the request.
			if reused && errors.Is(err, errStale) {
				continue
			}
			return response{}, err
		}
		if keep {
			s.release(addr, c)
		} else {
			_ = c.Close()
		}
		return out, nil
	}
}

var errStale = errors.New("connection closed before a response")

func (s *server) exchange(c *pooled, r request, addr string) (response, bool, error) {
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	h := s.headers(r, addr)
	if r.body != nil || r.method == "POST" || r.method == "PUT" {
		h.Set("Content-Length", fmt.Sprint(len(r.body)))
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", r.method, r.target)
	if err := h.Write(&b); err != nil {
		return response{}, false, err
	}
	b.WriteString("\r\n")
	b.Write(r.body)
	if _, err := c.Write(b.Bytes()); err != nil {
		return response{}, false, errStale
	}
	if _, err := c.r.Peek(1); err != nil {
		return response{}, false, errStale
	}
	res, err := http.ReadResponse(c.r, &http.Request{Method: r.method})
	if err != nil {
		return response{}, false, err
	}
	out := response{status: res.StatusCode, header: res.Header}
	if res.StatusCode == http.StatusSwitchingProtocols {
		return out, false, nil
	}
	out.body, out.bodyErr = io.ReadAll(res.Body)
	_ = res.Body.Close()
	return out, out.bodyErr == nil && !res.Close, nil
}

// dial opens a WebSocket for r on s.
func (s *server) dial(ctx context.Context, r request) (*websocket.Conn, *http.Response, error) {
	addr := s.target(r.host)
	scheme := "ws"
	if s.tls {
		scheme = "wss"
	}
	h := s.headers(r, addr)
	host := h.Get("Host")
	h.Del("Host")
	return websocket.Dial(ctx, scheme+"://"+addr+r.target, &websocket.DialOptions{HTTPHeader: h, Host: host})
}
