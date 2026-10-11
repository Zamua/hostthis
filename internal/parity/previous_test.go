package parity

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// previousRelease applies with PARITY_REFERENCE=previous, which compares a
// new Worker against the previous hostthisd release behind its own front
// door, as a release that deploys the Worker first does. It factors out what
// that release changed on purpose, each by the SPEC section that now says
// otherwise, and what a front door does to a request before its server sees
// it. A status or body difference outside these still fails.
//
// The release also declares Content-Length on stored bytes. That is framing,
// which no mode compares, so it needs no rule.
var previousRelease = []allowance{
	{
		// A front door that refuses a request answers an HTML 400 of its own,
		// which neither server writes. A refused side is asked again at the
		// path its front door's normalization would resolve dot segments to; a
		// malformed request both sides refuse agrees on its status alone.
		name: "front-door-refusal",
		spec: `"Public and internal hosts" (the public router's 400 for a malformed escape); "The public surface" (dot segments resolved)`,
		apply: func(c *comparison) bool {
			hRefused, gotRefused := frontDoorRefused(c.h), frontDoorRefused(c.got)
			if !hRefused && !gotRefused {
				return false
			}
			if (hRefused || c.h.status == 400) && (gotRefused || c.got.status == 400) {
				c.explained = true
				return true
			}
			resolved, ok := resolveDots(c.req.target)
			if !ok {
				return false
			}
			r := c.req
			r.target = resolved
			if hRefused {
				return c.refetch(r)
			}
			return c.refetchSide(r)
		},
	},
	{
		// A front door may clean a path (a doubled slash) before its server
		// sees it, where the other server redirects to the cleaned path: the
		// redirect, followed, must give the other server's answer.
		name: "front-door-cleans-paths",
		spec: `"Public and internal hosts" ("a path that cleaning changes ... is redirected 307 to its cleaned form")`,
		apply: func(c *comparison) bool {
			if loc, ok := cleanedRedirect(c.got, c.req.target); ok && c.h.status/100 != 3 {
				r := c.req
				r.target = loc
				return c.refetchSide(r)
			}
			if loc, ok := cleanedRedirect(c.h, c.req.target); ok && c.got.status/100 != 3 {
				r := c.req
				r.target = loc
				return c.refetch(r)
			}
			return false
		},
	},
	{
		// The previous release honored If-Modified-Since beside an
		// If-None-Match that did not match; If-None-Match now decides alone, so
		// the previous release is asked without the date.
		name: "if-none-match-decides-alone",
		spec: `"Cache-Control posture" ("A present If-None-Match decides alone")`,
		apply: func(c *comparison) bool {
			if c.req.header.Get("If-None-Match") == "" || c.req.header.Get("If-Modified-Since") == "" ||
				c.h.status != 304 || c.got.status != 200 {
				return false
			}
			r := c.req
			r.header = r.header.Clone()
			r.header.Del("If-Modified-Since")
			return c.refetch(r)
		},
	},
	{
		// The previous release's read-path 5xx kept the content headers set
		// before it failed; a 5xx is now bare. Its status, type and body are
		// still compared.
		name: "bare-5xx",
		spec: `"The public surface" ("Every 500 is bare"); "5xx observability on the read surface"`,
		apply: func(c *comparison) bool {
			if c.h.status/100 != 5 || c.got.status/100 != 5 || c.got.header.Get("Cache-Control") != "no-store" {
				return false
			}
			for _, name := range contentHeaders[1:] {
				if c.got.header.Get(name) != "" {
					return false
				}
			}
			return c.takeWorkerHeaders(contentHeaders...)
		},
	},
	{
		// The previous release let a browser reuse a paste for an hour; a
		// content response now revalidates on every load.
		name: "browser-revalidates",
		spec: `"Cache-Control posture" ("Cache-Control: public, no-cache")`,
		apply: func(c *comparison) bool {
			if c.h.header.Get("Cache-Control") != "public, max-age=3600" || c.got.header.Get("Cache-Control") != "public, no-cache" {
				return false
			}
			return c.takeWorkerHeaders("Cache-Control")
		},
	},
}

// contentHeaders are what a content response carries and a bare 5xx does not;
// a bare 5xx's Cache-Control is no-store.
var contentHeaders = []string{
	"Cache-Control", "ETag", "Last-Modified", "X-Frame-Options", "Referrer-Policy", "Permissions-Policy",
	"Content-Security-Policy",
}

// rules are the allowances a world applies, in order.
func (w *world) rules() []allowance {
	if !w.previous {
		return allowances
	}
	// Again after the allowances: one that asks hostthisd again gets an
	// answer in the previous release's shape.
	return slices.Concat(previousRelease, allowances, previousRelease)
}

// frontDoorRefused reports an answer neither server writes: a 400 page in
// HTML, which a front door gives a request it will not pass on.
func frontDoorRefused(res response) bool {
	return res.status == 400 && strings.HasPrefix(res.header.Get("Content-Type"), "text/html")
}

// cleanedRedirect is res's Location when res redirects target to another path
// on the same host.
func cleanedRedirect(res response, target string) (string, bool) {
	loc := res.header.Get("Location")
	if res.status != 307 || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") ||
		strings.HasPrefix(loc, `/\`) || loc == target {
		return "", false
	}
	return loc, true
}

// refetchSide replaces the Worker side's answer with its answer to r.
func (c *comparison) refetchSide(r request) bool {
	got, err := c.side.do(r)
	if err != nil {
		c.errs = append(c.errs, fmt.Sprintf("%s at %s: %v", c.side.name, r, err))
		return false
	}
	c.got = got
	return true
}

// takeWorkerHeaders gives hostthisd's answer the Worker's values of names,
// and reports whether any changed.
func (c *comparison) takeWorkerHeaders(names ...string) bool {
	h := c.h.header.Clone()
	if h == nil {
		h = http.Header{}
	}
	changed := false
	for _, name := range names {
		if slices.Equal(h.Values(name), c.got.header.Values(name)) {
			continue
		}
		h.Del(name)
		for _, v := range c.got.header.Values(name) {
			h.Add(name, v)
		}
		changed = true
	}
	c.h.header = h
	return changed
}
