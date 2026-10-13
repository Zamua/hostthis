package parity

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// comparison is one request's two answers while allowances apply to it.
type comparison struct {
	w    *world
	side *server
	req  request
	h    response // hostthisd's
	got  response // the Worker side's
	// explained is set by an allowance that accounts for every difference.
	explained bool
	// errs are requests an allowance could not make.
	errs []string
}

// allowance is one deliberate difference between hostthisd and the Worker.
// apply factors it out of c, by editing the pair or setting explained, and
// reports whether it did. It applies only where its condition holds AND the
// Worker gave the answer the SPEC names, so a Worker that differs some other
// way is still reported.
type allowance struct {
	name string
	// spec is the docs/SPEC.md section that makes the difference deliberate.
	spec  string
	apply func(c *comparison) bool
}

// allowances run in this order, each on what the ones before it left.
var allowances = []allowance{
	{
		// hostthisd's routing compares the Host as sent; the Worker normalizes
		// it first, so it answers what hostthisd answers for the normalized
		// host.
		name: "hosts-compare-normalized",
		spec: `"Public and internal hosts" ("Hosts compare after normalization: lowercase, no port, no trailing dot"); "The public surface", Deliberate differences ("Hosts compare lowercased")`,
		apply: func(c *comparison) bool {
			norm := normalizeHost(c.req.host)
			if norm == c.req.host {
				return false
			}
			r := c.req
			r.host = norm
			return c.refetch(r)
		},
	},
	{
		// The Worker's URL parser resolves "." and ".." segments, %2e spellings
		// included, before it routes, so it answers what hostthisd answers at
		// the resolved path. hostthisd redirects a literal dot segment there
		// and takes an escaped one as part of the path.
		name: "dot-segments-resolved",
		spec: `"The public surface" ("served at the resolved path, where hostthisd redirects a literal dot segment to that path and looks an escaped one up as part of the path"); Deliberate differences`,
		apply: func(c *comparison) bool {
			resolved, ok := resolveDots(c.req.target)
			if !ok {
				return false
			}
			r := c.req
			r.target = resolved
			return c.refetch(r)
		},
	},
	{
		// hostthisd's read and asset handlers answer any method as a GET.
		name: "reads-answer-get-and-head-only",
		spec: `"The public surface", Deliberate differences ("Reads and assets answer only GET and HEAD (405 otherwise)")`,
		apply: func(c *comparison) bool {
			m := c.req.method
			if m == "GET" || m == "HEAD" || c.got.status != 405 || c.got.header.Get("Allow") != "GET, HEAD" {
				return false
			}
			if !c.isRead() && !isAssetPath(c.path()) {
				return false
			}
			c.explained = true
			return true
		},
	},
	{
		// On Cloudflare, and under Miniflare, the static assets layer answers a
		// known /_hostthis/<name> before the Worker runs, whatever the host: it
		// adds its own ETag, and answers a method other than GET or HEAD with a
		// 405 of its own.
		name: "static-assets-layer",
		spec: `"The public surface", Renderer assets ("The static assets layer answers a known name on every host ... with a validator of its own ... and answers any other method with a 405 of its own"); Deliberate differences`,
		apply: func(c *comparison) bool {
			if !isStaticAsset(c.req.target) {
				return false
			}
			if m := c.req.method; m != "GET" && m != "HEAD" {
				if c.got.status != 405 {
					return false
				}
				c.explained = true
				return true
			}
			if c.h.header.Get("ETag") != "" || c.got.header.Get("ETag") == "" {
				return false
			}
			c.got.header = c.got.header.Clone()
			c.got.header.Del("ETag")
			return true
		},
	},
	{
		// A HEAD never opens the object on the Worker, so a damaged one is a 200
		// there; hostthisd's HEAD runs the GET, which fails.
		name: "head-never-opens-the-object",
		spec: `"The public surface", Deliberate differences ("A HEAD never opens the object")`,
		apply: func(c *comparison) bool {
			if c.req.method != "HEAD" || c.h.status != 500 || c.got.status != 200 {
				return false
			}
			c.explained = true
			return true
		},
	},
	{
		// The Worker compares If-None-Match weakly, as an edge that compresses a
		// response makes its ETag weak; hostthisd compares strings exactly.
		name: "weak-etag-comparison",
		spec: `"The public surface", Deliberate differences ("If-None-Match compares weakly"); "Cache-Control posture"`,
		apply: func(c *comparison) bool {
			inm := c.req.header.Get("If-None-Match")
			etag := c.h.header.Get("ETag")
			if !strings.HasPrefix(inm, "W/") || etag == "" || strings.TrimPrefix(inm, "W/") != etag ||
				c.h.status != 200 || c.got.status != 304 {
				return false
			}
			c.explained = true
			return true
		},
	},
	{
		// hostthisd serves its landing page at / on every host that is not a
		// slug's; the Worker's public router serves no landing page.
		name: "landing-stays-hostthisd",
		spec: `"Public and internal hosts" ("The apex / is one: the landing page stays hostthisd's")`,
		apply: func(c *comparison) bool {
			if c.slugHost() || c.path() != "/" || !c.isNotFound() || c.h.status != 200 {
				return false
			}
			c.explained = true
			return true
		},
	},
	{
		// hostthisd serves /p/<slug> on every host that is not a slug's, in
		// every URL mode; the Worker only on the apex host in path mode, which
		// the suite's Worker does not run.
		name: "path-mode-on-the-apex-only",
		spec: `"Public and internal hosts" (/p/<slug>/ only with HOSTTHIS_URL_MODE=path on the apex host); "The public surface", Deliberate differences`,
		apply: func(c *comparison) bool {
			if c.slugHost() || !strings.HasPrefix(c.path(), "/p/") || !c.isNotFound() {
				return false
			}
			c.explained = true
			return true
		},
	},
	{
		// Each server mints its own room id.
		name: "room-ids-are-minted",
		spec: `"The HTTP API" ("POST /api/rooms generates a fresh UUIDv4")`,
		apply: func(c *comparison) bool {
			if c.req.method != "POST" || c.h.status != 201 || c.got.status != 201 ||
				!mintedRoom(c.h.body) || !mintedRoom(c.got.body) {
				return false
			}
			c.got.body = c.h.body
			return true
		},
	},
	{
		// A whole-room GET carries each value's stored text on both servers,
		// but each spells the keys its own way (hostthisd escapes <, >, &,
		// U+2028 and U+2029), so the objects compare as decoded values.
		name: "room-key-spelling",
		spec: `"The HTTP API" ("A key's own spelling may still differ between the servers' GETs")`,
		apply: func(c *comparison) bool {
			if c.req.method != "GET" || !roomScanPath.MatchString(c.path()) ||
				c.h.status != 200 || c.got.status != 200 || !sameJSON(c.h.body, c.got.body) {
				return false
			}
			c.got.body = c.h.body
			return true
		},
	},
	{
		// workerd hands the Worker a request carrying Upgrade: websocket as a GET
		// whatever its method, and fails a non-GET one once the Worker accepts
		// it; hostthisd's library refuses the method with 405.
		name: "runtime-takes-upgrades-as-get",
		spec: `"Real-time room relay" ("workerd hands the Worker any request carrying Upgrade: websocket as a GET")`,
		apply: func(c *comparison) bool {
			if c.req.method == "GET" || !strings.EqualFold(c.req.header.Get("Upgrade"), "websocket") ||
				c.h.status != 405 || c.got.status != 500 {
				return false
			}
			c.explained = true
			return true
		},
	},
	{
		// workerd upgrades only a request whose Upgrade is websocket alone, so
		// the Worker refuses a list holding it, which hostthisd's library takes.
		name: "runtime-upgrades-websocket-alone",
		spec: `"Real-time room relay" ("workerd upgrades only a request whose Upgrade is websocket alone")`,
		apply: func(c *comparison) bool {
			up := c.req.header.Get("Upgrade")
			if strings.EqualFold(strings.TrimSpace(up), "websocket") || !strings.Contains(strings.ToLower(up), "websocket") ||
				c.h.status != 101 || c.got.status != 426 {
				return false
			}
			c.explained = true
			return true
		},
	},
	{
		// At a room's socket cap the Worker refuses the join with 429; hostthisd
		// has accepted its client before the Room cell refuses, so it closes
		// with 1011. Applied by the WebSocket test, which sees the close.
		name:  "full-room-closes-after-the-handshake",
		spec:  `"Real-time room relay", "Lifecycle and bounds" ("hostthisd accepts its client before it dials the cell, so there a refused join closes the client with 1011")`,
		apply: func(*comparison) bool { return false },
	},
}

// refetch replaces hostthisd's answer with its answer to r.
func (c *comparison) refetch(r request) bool {
	h, err := c.w.hostthisd.do(r)
	if err != nil {
		c.errs = append(c.errs, fmt.Sprintf("hostthisd at %s: %v", r, err))
		return false
	}
	c.h = h
	return true
}

// path is the request's decoded path within the app it addresses; "" for a
// malformed escape.
func (c *comparison) path() string {
	raw, _, _ := strings.Cut(c.req.target, "?")
	p, err := url.PathUnescape(raw)
	if err != nil {
		return ""
	}
	return p
}

func (c *comparison) slugHost() bool {
	label, ok := strings.CutSuffix(normalizeHost(c.req.host), "."+c.w.apex)
	return ok && slugPattern.MatchString(label)
}

// isRead reports whether the request is for an app's read path: on a slug
// host, outside the rooms and push routes and the health paths.
func (c *comparison) isRead() bool {
	p := c.path()
	return c.slugHost() && !strings.HasPrefix(p, "/api/rooms") && p != "/api/push/key" &&
		p != "/healthz" && p != "/readyz"
}

var (
	slugPattern  = regexp.MustCompile(`^[abcdefghijkmnpqrstuvwxyz23456789]{8}$`)
	roomScanPath = regexp.MustCompile(`^/api/rooms/[0-9A-Fa-f-]{36}/?$`)
	uuidV4       = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

func normalizeHost(host string) string {
	host = strings.ToLower(host)
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return strings.TrimSuffix(host, ".")
}

// resolveDots resolves target's dot segments as the WHATWG URL parser does,
// and reports whether it had any.
func resolveDots(target string) (string, bool) {
	raw, query, hasQuery := strings.Cut(target, "?")
	segs := strings.Split(raw, "/")[1:]
	var out []string
	found := false
	for i, seg := range segs {
		last := i == len(segs)-1
		switch strings.ToLower(seg) {
		case ".", "%2e":
			found = true
			if last {
				out = append(out, "")
			}
		case "..", ".%2e", "%2e.", "%2e%2e":
			found = true
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
			if last {
				out = append(out, "")
			}
		default:
			out = append(out, seg)
		}
	}
	resolved := "/" + strings.Join(out, "/")
	if hasQuery {
		resolved += "?" + query
	}
	return resolved, found
}

func isAssetPath(p string) bool { return strings.HasPrefix(p, "/_hostthis/") }

// isNotFound reports the Worker answering Go's http.NotFound; a HEAD's answer
// has no body.
func (c *comparison) isNotFound() bool {
	return c.got.status == 404 && (c.req.method == "HEAD" || string(c.got.body) == "404 page not found\n")
}

func mintedRoom(body []byte) bool {
	v, ok := decodeJSON(body)
	m, isMap := v.(map[string]any)
	id, isString := m["id"].(string)
	return ok && isMap && len(m) == 1 && isString && uuidV4.MatchString(id)
}

var (
	staticOnce  sync.Once
	staticNames map[string]bool
)

// isStaticAsset reports whether target is /_hostthis/<name> for a file the
// build put in the static assets directory, which the assets layer answers
// itself.
func isStaticAsset(target string) bool {
	staticOnce.Do(func() {
		staticNames = map[string]bool{}
		entries, _ := os.ReadDir(filepath.Join(repoRoot(), "worker", "dist", "assets", "_hostthis"))
		for _, e := range entries {
			staticNames[e.Name()] = true
		}
	})
	raw, _, _ := strings.Cut(target, "?")
	name, ok := strings.CutPrefix(raw, "/_hostthis/")
	return ok && staticNames[name]
}
