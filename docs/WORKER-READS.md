# Serving reads from the Worker

Status: approved plan (decisions in section 8). Nothing here is built. `docs/SPEC.md` describes
current behavior and stays the source of truth; the spec edits in section 9
land with the code, spec first.

## 0. The decision

`hostthisd` keeps SSH ingest and the apex (landing page, `/healthz`, `/readyz`),
because SSH needs the apex to stay DNS-only. Every request to `<slug>.<apex>`
moves to the Worker: paste shells, `?raw` bytes, HTML pastes, site files with
SPA fallback, knowledge bases, the `/_hostthis/` renderer assets, the rooms
HTTP API, room push, and the room WebSocket. The Worker answers from its own
cells and its `PAYLOADS` binding, caching whole responses at the edge until a
write purges the slug (2.7).

The public surface is off unless configured. A deployment that sets nothing,
including a self-hosted celld, behaves exactly as today.

## 1. Current state

### 1.1 Routing in hostthisd

`internal/http/server.go`, `Server.Handler`:

| order | match | handler |
| --- | --- | --- |
| 1 | `/_hostthis/<name>`, any Host | `serveAsset` (`shells.go`): whitelisted flat namespace, shadows that path in every site |
| 2 | `/healthz`, `/readyz`, any Host | `serveHealthz`, `serveReadyz` (`readyz.go`) |
| 3 | Host `<slug>.<apex>` (one label that parses as a slug) | `route` |
| 4 | `/p/<slug>[/<path>]` on any other Host | `route`, in every URL mode |
| 5 | `/` | `serveLanding`, `Cache-Control: public, max-age=300` |
| 6 | anything else | 404 |

`route` carves `/api/rooms[/...]` (`rooms.go`) and `/api/push/key`
(`rooms_push.go`) out before `serveSlug`, so no site file can shadow them.

Row 4 means a production deploy serves `https://<apex>/p/<slug>` today: user
content on the apex origin, which the SPEC says never happens (see 8, Q6).

### 1.2 Paste and site reads

One head read decides a request: `PasteRepo.Get` calls `GET /paste/get?slug=`,
which returns the whole row: served manifest, owner identity, name, generation
(`internal/worker/pasterepo.go`). On not-found the router calls `SiteRepo.Get`,
the same call again. Bytes come from `GET /blob/<key>` (the Worker's stateless
R2 route) through `CompressedBlobStore.Read`, which checks the `HZ\0\x01` magic
and zstd-decodes (`internal/storage/blob_compressed.go`).

Outcomes (`serveSlug`, `servePaste`, `serveFromManifest`,
`serveKnowledgeBase`, `streamDocument`, `serveFileList`):

| case | status | Cache-Control | ETag | other |
| --- | --- | --- | --- | --- |
| unknown slug | 404 | | | `404 page not found` |
| metadata or blob error | 500 | | | `internal error`, warn log naming the slug |
| single-file paste, path not `/` | 404 | | | |
| `pending` | 200 | `no-store` | | `Retry-After: 1`, XFO, Referrer; meta-refresh page |
| `failed` | 410 | `no-store` | | XFO, Referrer; error page |
| root entry without a key | 404 | | | |
| `html` | 200 | `public, max-age=3600` | `"<object key>"` | sandbox; stored bytes |
| client-rendered kind, bare URL | 200 | `public, max-age=3600` | `"<shell version>"` | sandbox, CSP; shell with `__VER__`, `__KIND__` substituted |
| client-rendered kind, `?raw` (any value) | 200 | `public, max-age=3600` | `"<object key>"` | sandbox; `rawContentType[kind]` |
| `site` path | 200 or 404 | `public, no-cache` | `"<object key>"` | sandbox; `entry.ContentType` or octet-stream; `Manifest.LookupWithSPAFallback` |
| knowledge base shell (root, `.md` path, route-shaped miss) | 200 | `public, max-age=3600` | `"<kb shell version>"` | sandbox, CSP |
| knowledge base `.md` with `?raw` | 200 | root: `max-age=3600`, else `no-cache` | `"<object key>"` | sandbox; `text/markdown` |
| knowledge base `?files` | 200 | same rule | `"files-<upload id>"` | sorted JSON path list |
| knowledge base other file | as `site` | | | |
| knowledge base asset-shaped miss | 404 | | | |

Sandbox headers: `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
`Permissions-Policy: camera=(), microphone=(), geolocation=(), usb=(), payment=()`.
Every 200 with an ETag also sets `Last-Modified` from the paste's `updatedAt`;
`notModified` answers `If-None-Match` (exact string match) or
`If-Modified-Since` with 304 before any blob read. Site and knowledge base
kinds serve from the manifest without consulting status. Handlers accept any
method; there is no Range support and no Content-Length on streamed bodies.

No HTTP route addresses a non-served version: `versions`, `pin` and `unpin`
are SSH verbs that change what the head serves. Nothing version-specific moves.

### 1.3 Shells and renderer assets

`internal/http/shells.go`, `mdshell.go`, `diffshell.go`, `kbshell.go`,
`newshells.go`; files under `internal/http/assets/<shell>/`, `go:embed`ded.

- One `shell.html` per shell: md, diff, mermaid, pdf, data (csv and json),
  flame, text, log, kb; plus `common/` (deeplink, linegutter).
- Assets share one flat namespace, `/_hostthis/<name>?v=<shell version>`,
  whitelisted per shell, served `public, max-age=31536000, immutable` with an
  explicit Content-Type. The shell version constant (`mdshell-v7`, ...) is
  both the cache buster and the shell ETag.
- CSP: `shellCSP`; pdf and data use `wasmWorkerCSP` (`'wasm-unsafe-eval'`,
  `worker-src 'self' blob:`).
- 41 MB in total. `datashell/duckdb-eh.wasm` alone is 35.7 MB (lazy-loaded by
  the SQL console); `mermaid.min.js` is 3.6 MB.

### 1.4 Caching and the CDN

- The wildcard `*.<apex>` is proxied by Cloudflare; the apex is DNS-only so SSH
  reaches the origin (SPEC "Apex must stay DNS-only").
- `service.CacheInvalidating` (`internal/service/manage_cache.go`, wired in
  `cmd/hostthisd/main.go`) purges after a successful update, pin and unpin,
  and after any delete that passed the ownership check.
  `internal/cache/cloudflare.go` posts the variants from
  `internal/cache/urls.go`: `/`, `/?raw=1`, `/?files=1`. A deploy purges the
  edge for shell changes.

### 1.5 Rooms HTTP and push

`internal/http/rooms.go`, `rooms_push.go`, `internal/service/rooms.go`,
`roompush.go`, `internal/worker/roomrepo.go`, `roompush.go`. Policy held in Go:

- `Cache-Control: no-store` on every response under `/api/rooms`.
- Room id is a UUIDv4 (400 if malformed); keys 1 to 256 bytes, `ws`, `push`
  and `push/...` reserved (400); PUT body read up to 256 KiB + 1; push bodies
  up to 64 KiB.
- `appExists` (a paste row exists, any status) gates create, push, push key
  and WS: 404 otherwise.
- Creation limits: 60 per subnet and 300 per app per hour, counted by
  `/paste/roomcounts` before the create (a soft bound). Subnet (/24, /48) from
  `RemoteAddr`, or the right-most `X-Forwarded-For` with
  `HOSTTHIS_HTTP_TRUST_XFF=true`. 429 with `Retry-After: 3600`.
- UUID minting; `domain.RoomWireValue` (raw JSON when `json.Valid`, else a JSON
  string) computed in Go and sent with each PUT so the cell echoes it into
  frames; scan re-encodes from bytes; single-key GET labels JSON
  `application/json`, else `application/octet-stream`.
- Status mapping: 404 room or key, 413 room cap, 507 app cap, 429, 405 with
  `Allow`. Push `sub` claim is `https://<apex>`.

Worker calls per request today:

| request | cell ops |
| --- | --- |
| `POST /api/rooms` | paste `get` (1 or 2), paste `roomcounts`, room `create` (which calls paste `roompreflight`, `roomcreated`) |
| `GET /api/rooms/<id>` | room `meta`, room `scan` |
| `GET`/`PUT`/`DELETE .../<key>` | room `meta` + `get`; room `put` or `del` (which call paste `roomdecide`) |
| push routes | paste `get`; room `pushsub*`, `pushschedule*`, `pushtest` (which calls paste `pushsign`) |
| `GET /api/push/key` | paste `get`, paste `pushkey` |
| WS | paste `get`, room `meta`, room `scan`, then a WebSocket dial to `/room/join` |

### 1.6 Room WebSocket

`internal/http/ws.go`, `internal/worker/roomproxy.go`,
`internal/roomwire/roomwire.go`:

- Before the 101: app exists (404), room exists (404), per-process admission
  of 64 sockets per room and 1024 per app (429), then `websocket.Accept` with
  `OriginPatterns: ["*.<apex>", "<apex>"]`. Same-host and a missing `Origin`
  are allowed; any other origin is 403.
- hostthisd dials `/room/join?room=<slug>|<uuid>`; the Room cell accepts a
  hibernatable socket and sends the snapshot in the same turn.
- The proxy limits client frames to 32 KiB (close 1009) and upstream frames to
  2 MiB, copies frames opaquely, pings the client every 20 s and reaps it after
  10 s without a pong, never pings upstream, and closes 1012 on shutdown and
  1011 when the dial fails.

### 1.7 What lives in Go and must exist in the Worker

Read rules (lookup, exact file, SPA fallback, asset extensions, markdown paths,
kind to shell and raw type, knowledge base cache rule, status gating, error
shapes); shells and assets; zstd decoding of the at-rest format; the rooms and
push policy above; WebSocket admission, origin policy and frame limit.

## 2. Target

### 2.1 Who serves what

| surface | today | target |
| --- | --- | --- |
| `ssh <apex>` | hostthisd | hostthisd |
| `<apex>/`, `/healthz`, `/readyz` | hostthisd | hostthisd |
| `<apex>/p/<slug>` | hostthisd | hostthisd until phase 5, then dev-only |
| `<slug>.<apex>/*` reads | hostthisd behind the CDN | Worker |
| `<slug>.<apex>/_hostthis/*` | hostthisd behind the CDN | Workers static assets, one file from R2 |
| `/api/rooms/*`, `/api/push/key` | hostthisd, then Worker | Worker |
| `/api/rooms/<uuid>/ws` | hostthisd proxy to the Room cell | Worker to the Room cell |
| uploads, manage verbs, blob writes, compression, `get` | hostthisd | hostthisd |
| internal cell and `/blob` routes | Worker, token | unchanged |

### 2.2 Dispatch in the Worker

The default `fetch` classifies before anything else:

```
host = lowercase url.hostname, no port
if APEX set and (host == APEX or host ends with "." + APEX):
    return publicRouter(request)            # never reaches internal routes
if authorized(request) and request has X-Hostthis-Forwarded-Host:
    return publicRouter(request, forwarded)  # section 3
return internalRouter(request)              # today's router, unchanged
```

- Every host under the apex is public, including non-slugs (`www.<apex>`,
  `a.b.<apex>`), which get a 404 from the public router. No public hostname
  can reach a cell op, `/blob` or `/__cell`, whatever the path.
- With `HOSTTHIS_APEX_DOMAIN` set and `HOSTTHIS_WORKER_TOKEN` unset, the
  internal router answers 503 to everything: a public Worker never runs an
  open internal router.

Worker configuration (new names reuse hostthisd's where the meaning is the
same; the URL-mode default differs on purpose):

| var / binding | default | meaning |
| --- | --- | --- |
| `HOSTTHIS_APEX_DOMAIN` | unset | public surface off when unset |
| `HOSTTHIS_URL_MODE` | `subdomain` | `path` also serves `/p/<slug>` on the apex host; dev and tests only |
| `HOSTTHIS_PUBLIC_SCHEME` | `https` | VAPID `sub` |
| `HOSTTHIS_CLIENT_IP_SOURCE` | unset | `cf-connecting-ip` or `xff-rightmost`; unset puts every client in one bucket |
| `HOSTTHIS_EDGE_CACHE_TTL_S` | `0` | response cache TTL at the edge; `0` is off (2.7) |
| `HOSTTHIS_BLOB_CACHE_MAX_BYTES` | `0` | blob cache size ceiling; `0` is off (2.7) |
| `ASSETS` binding | absent | static assets; absent means `/_hostthis/` comes from `PAYLOADS` under `static/` |

### 2.3 Public routes

Mirrors 1.1 for public hosts:

1. `/_hostthis/<name>`: static assets answer before the Worker runs (2.5); an
   unknown name reaches the Worker and is a 404.
2. `/healthz`, `/readyz`: same bodies and headers as hostthisd.
3. Slug host: `/api/rooms[/...]`, `/api/push/key`, else the read path.
4. Path mode only: `/p/<slug>[/<path>]` on the apex host. The apex `/` is 404
   here; the landing page stays hostthisd's.
5. Anything else: 404.

- The Worker never redirects between schemes: that is the front door's job
  (Always Use HTTPS on Cloudflare, the TLS proxy on celld). celld reports every
  request as `http` unless it trusts forwarded headers, so a Worker-side
  redirect would loop there.
- The path is percent-decoded once, as Go's `URL.Path` is; a malformed escape
  is 400. ServeMux's 301 for non-canonical paths (`//`, `/./`, `/../`) is
  reproduced so a URL resolves the same way on both servers.
- Read paths answer GET and HEAD; other methods get 405 with `Allow`. HEAD
  never opens a blob.

### 2.4 Read path

A new read-only Paste cell op, `resolve`, takes the slug (from the Host only),
the decoded path and the `raw` and `files` flags, runs the read rules against
the row the cell holds, and returns a small answer, never the row:

```
{ found: false }
{ status: "pending" | "failed" }
{ action, kind, updatedAt, uploadId,
  entry?: { key, contentType, size },     // bytes to stream
  shell?: "<kind>",                        // template to render
  files?: [ ...sorted paths ] }            // ?files on a knowledge base
action: html | shell | raw | file | kbshell | kbraw | files | notfound
```

- The rules are pure functions in `worker/src/read.js` (no I/O), ported from
  `domain.Manifest.Lookup`, `File`, `LookupWithSPAFallback`, `LooksLikeAsset`,
  `IsMarkdownPath` and the dispatch in `serveSlug`. Only the extension table's
  asset flag is needed: content types come from the stored entry.
- Running them in the cell matters: a 5000-file site manifest is over a
  megabyte of JSON that today crosses to hostthisd and is parsed on every file
  request. `resolve` returns a few hundred bytes. One cell call per request
  (today one, two on not-found).
- An undecodable manifest reads as none, so the answer is `notfound` (CLAUDE.md
  principle 3).
- Room routes use a separate `exists` read op (row present), so they never load
  a manifest.

The stateless layer then sets headers per the 1.2 table, answers conditional
requests from the ETag inputs and `updatedAt` without touching R2, and streams:
`PAYLOADS.get(key)`, check the magic, `pipeThrough` a zstd decoder, return. A
missing object or bad magic is a 500 `internal error` plus one `console.warn`
naming the slug and cause (Workers Logs enabled on the deployment).

`If-None-Match` uses weak comparison (RFC 9110): an edge that compresses a
response turns a strong ETag into `W/"..."`, which hostthisd's exact match
never matches, so revalidations behind a compressing edge become full
responses today. Deliberate difference.

The zstd decoder is vendored: the Workers runtime's `DecompressionStream` does
gzip, deflate and deflate-raw only. It is libzstd's decompression API compiled
to WASM (decoder only, about 72 KB), built reproducibly from a pinned zstd
release by a script in the repo, with the `.wasm` committed beside it and its
hash checked in CI. Chosen over pure-JS fzstd on a golden corpus written by
`internal/zstdenc`: about 2.5x less CPU per decoded MiB on text, and it
verifies the frame checksum the encoder writes (fzstd served corrupted bytes
silently). The stream wrapper is our own and pull-based: each call produces at
most 128 KiB, so backpressure is exact and memory stays flat (0.1 MiB live
through a 100 MiB decode); every packaged wrapper buffered whole outputs or
accepted truncated streams. Input is batched to 64 KiB per call. Frames whose
window exceeds 8 MiB are refused before allocation. A small pool of instances
per isolate is reused. The WASM is imported as a module (runtimes refuse to
compile code from bytes), so celld needs v0.6.1 or later for direct or
forwarded serving.

The checksum sits at the end of a frame, so corruption is detected after most
bytes are sent: a decode error after the headers errors the body stream; a 500
is only possible before the first byte. Principle 1 is asserted by a test
decoding a 100 MiB output under a ceiling of a few MiB.

Deferred: passing the stored stream through as `Content-Encoding: zstd` to
clients that accept it (strip the magic, `encodeBody: "manual"`). Safari only
gained zstd in 26.3, Cloudflare's handling of a Worker-supplied zstd body is
unverified, and it complicates HEAD and edge compression. Revisit if decode CPU
is material.

### 2.5 Shells and assets

- One source: move `internal/http/assets/**` and the shell table (kind to shell
  dir, version, asset content types, CSP variant, raw content type) to
  `web/shells/` with a `shells.json`. Go embeds it through a small `web`
  package; the Worker build generates a module holding the templates and the
  table, and copies the assets to `_hostthis/<name>` in the assets directory.
  While both servers exist they read the same versions, so a shell bump cannot
  make them disagree. Templates are never served at any path.
- Cloudflare: Workers static assets with only `_hostthis/` in the directory and
  `run_worker_first` left at its default, so a matching asset is served without
  running the Worker (static asset requests are free) while any other path,
  including an unknown `/_hostthis/` name, reaches the Worker. An exclusion
  rule (`"!/_hostthis/*"`) would make the asset layer answer unknown names
  itself, with an empty 404 carrying the year-long `Cache-Control`. A
  `_headers` file gives `public, max-age=31536000, immutable` and a
  Content-Type rule for every asset type: a file whose extension has no rule
  is served with no Content-Type at all.
- `duckdb-eh.wasm` (35.7 MB) is over the 25 MiB per-asset limit. It goes in
  `PAYLOADS` under a reserved `static/<datashell version>/duckdb-eh.wasm` key,
  outside `uploads/` so no delete route can reach it, uploaded by the deploy
  and streamed by the Worker with the same headers. A runtime without `ASSETS`
  serves every `/_hostthis/` file this way.

### 2.6 Response parity

Equal on: status, `Content-Type`, `Cache-Control`, `ETag`, `Last-Modified`,
`X-Frame-Options`, `Referrer-Policy`, `Permissions-Policy`,
`Content-Security-Policy`, `Retry-After`, `Allow`, `Location`,
`X-Content-Type-Options`. Ignored: transport headers (`Date`, `Server`,
`cf-*`, `Content-Length` versus chunked, edge-added `Content-Encoding`,
`alt-svc`, `nel`, `report-to`). Bodies: byte-identical. In rooms JSON every
value is the text the Room cell stored with it, on both servers and in the
frames, in keys ordered by their UTF-8 bytes; only a key's own spelling may
differ (Go escapes `<`, `>`, `&`, U+2028 and U+2029 in it; JS does not), so a
key is compared as its decoded string.

Deliberate differences, each spec'd: 405 on non-GET/HEAD reads; HEAD does not
open the blob (so a damaged object is 200 on HEAD, 500 on GET); weak ETag
comparison; no server heartbeat (2.9). `/api/push/key` carries `no-store` on
every response, its 404 and 405 included, on both servers.

### 2.7 Caching

Goal: a reader always sees the latest served version, and every read that can
come from a cache does.

- **Browser**: paste and site responses carry `Cache-Control: no-cache` with
  their ETag, so every load revalidates and an unchanged paste costs a 304.
  Versioned `/_hostthis/` assets stay `max-age=31536000, immutable`.
  (Today pastes are `max-age=3600`, so a reload after an update can show the
  old version for up to an hour from the browser's own cache.)
- **Edge**: the Worker caches whole responses in the data center's cache
  (Cache API), keyed by the request URL, with an edge TTL of one day and a
  `Cache-Tag` naming the slug. A hit answers GET, HEAD and conditional
  requests without a cell call or an R2 read.
- **Invalidation**: every write that changes what a slug serves (update, pin,
  unpin, delete, site redeploy) purges that slug globally, by hostname
  (`<slug>.<apex>`) so every file of a site goes at once. hostthisd's
  existing `CacheInvalidating` decorator and `CachePurger` port do this; the
  Cloudflare adapter moves from URL variants to hostname purge. A failed purge
  is logged and bounded by the one-day edge TTL.
- **Blob cache**: below the response cache, the decoded bytes of an object
  key are cached per data center. A key never names different bytes, so it
  needs no invalidation; it serves the miss right after a purge when the bytes
  did not change (a pin back to an older version). Only objects whose decoded
  size is under `HOSTTHIS_BLOB_CACHE_MAX_BYTES`, because teeing a stream into
  `cache.put` buffers the slower branch.
- Cloudflare documents that Cache API entries are purged globally by the
  standard purge options (purge everything, tags from a `Cache-Tag` header,
  hostname, prefix); URL purge does not apply to custom cache keys. Each cached
  response carries `Cache-Tag: <slug>`, so purge by tag and by hostname both
  reach it. The Cache API is not tiered: each data center fills once.
  `cache.match` answers `If-None-Match` and `If-Modified-Since` with a 304 by
  itself. Phase 3 verifies purge on the canary routes, with purge-to-miss
  timing, before the flip.
- All caching is a decorator around the read path, wired at the entrypoint
  and off by default in the bundle; a deployment without a purger (celld
  self-host) leaves it off.

### 2.8 Rooms in the Worker

The public router ports the 1.5 policy and calls the same cell ops through the
bindings:

- Constants from `internal/domain/room.go` (caps, key rules, creation limits).
- `RoomWireValue` and `RoomValueIsJSON` with Go's semantics: invalid UTF-8
  replaced per byte as Go does (the WHATWG decoder replaces per maximal
  subpart), BOM kept (`TextDecoder` with `ignoreBOM: true`), and `json.Valid`'s
  nesting limit of 10000. Values already stored keep their stored wire form:
  every read surface carries the text the cell stored, and `scan` answers it.
- Client subnet from `HOSTTHIS_CLIENT_IP_SOURCE`: `cf-connecting-ip` on
  Cloudflare (set by the edge, not forgeable through it), `xff-rightmost`
  behind a trusted proxy. Forwarded requests use `X-Hostthis-Client-IP`.
- Room ids from `crypto.randomUUID()`; push `sub` from the scheme and apex.

### 2.9 WebSocket in the Worker

```
GET /api/rooms/<uuid>/ws on <slug>.<apex>
  400  malformed UUID
  404  no paste row for the slug (exists), no room (room meta)
  403  Origin not allowed (same rule as 1.6)
  400/426 not a WebSocket upgrade
  429  room at its socket cap (decided in the Room cell)
  101  from new Request("https://cell/room/join?room=<slug>|<uuid>",
                        { headers: { Upgrade: "websocket" } }) sent to the Room stub
```

- The client's URL, query and body never reach the cell. `Room.fetch`
  dispatches by the last path segment or an `op` query, so forwarding
  `.../ws?op=put` would run a mutation past every public check.
- The Room cell's own rules below (the ping answer, the frame limit, the cap
  and eviction) apply only with `HOSTTHIS_APEX_DOMAIN` set on the Worker. In
  off mode (section 3) the cell relays every frame and admits every join, as
  hostthisd's proxy, which owns those bounds there, expects.
- The 32 KiB client frame limit moves into `Room.webSocketMessage` (close 1009).
  Server frames need no limit; the cell writes them.
- The per-process caps go away with the process. The Room cell enforces one
  global per-room cap of 64 at accept, counting the open sockets it holds. The
  per-app cap is dropped; creation limits bound the room count.
- Neither runtime pings or times out an idle socket: on Cloudflare a frozen
  client stayed counted for the whole 11-minute measurement, while a client
  whose process exits is closed within milliseconds. The Room cell sets
  `setWebSocketAutoResponse` for a reserved `{"type":"ping"}` frame (answered
  `{"type":"pong"}` without waking the cell, on both runtimes); the room
  client sends it every 20 s. Only a join that finds the room at its cap
  evicts: the cell closes, with 4001 `stale`, the sockets whose last
  auto-response is older than 120 s, then counts again. A join under the cap
  evicts nothing, so a client whose pings are only late (a throttled
  background tab) keeps its socket. A socket that has never pinged is never
  evicted this way; it leaves on close or a failed write. A spec change: a
  new reserved frame type and close code.
- The snapshot and `put` frames splice each value's stored text in, never a
  parse and re-serialization, so they carry the whole-room GET's bytes in
  every mode.
- A Worker deploy restarts the cells and drops sockets. That is already true
  because the cells run on Cloudflare; clients reconnect and resnapshot.
- During the flip, sockets through hostthisd and direct sockets share the Room
  cell as the single broadcast point, so mixed populations interoperate.

### 2.10 Routing on Cloudflare

- Production: a Workers route `*.<apex>/*` bound to the Worker. The proxied
  wildcard record stays (a route needs a proxied record) and keeps pointing at
  the origin, which is the rollback. The apex stays DNS-only and `*.` does not
  match it. Universal SSL already covers `*.<apex>`.
- Any other proxied hostname in the zone that must keep reaching the origin
  gets a more specific route with no Worker.
- A staging deploy whose apex is itself a subdomain (`staging.<apex>`) has a
  two-level wildcard. Universal SSL does not cover it, and a route needs a
  proxied record with an edge certificate. Options:
  - **a.** Advanced Certificate Manager on the zone (about $10/month) for
    `*.staging.<apex>`, proxy that record, route `*.staging.<apex>/*` to the
    staging Worker. The more specific route wins over `*.<apex>/*`.
  - **b.** A separate staging zone, whose wildcard is one level.
  - **c.** Keep staging DNS-only and run staging hostthisd in forwarded mode
    (section 3). This tests all Worker code but not the edge route; prod
    canary routes (phase 3) cover the edge.
- Misrouting is safe: a public router rejects `x.staging.<apex>` as not a slug
  (404), so staging traffic can never read a production slug of the same name.
- The repo keeps `worker/wrangler.jsonc` for celld and adds a Cloudflare
  template (vars, assets, routes) with placeholders; real values stay with the
  operator.

### 2.11 hostthisd after cutover

Stops: subdomain reads, `/_hostthis/`, rooms HTTP and push, the WebSocket
proxy (admission, heartbeat), room rate-limit accounting. Keeps:
SSH, uploads and every write, `get` over SSH (the Go blob read path stays),
apex landing, health, metrics.

## 3. Compatibility with self-hosted celld

One bundle, three modes:

| mode | Worker config | serves `<slug>.<apex>` | for |
| --- | --- | --- | --- |
| off (default) | `HOSTTHIS_APEX_DOMAIN` unset | hostthisd, Go read path as today | every existing deployment until it opts in |
| direct | apex set | the Worker, by Host | Cloudflare; celld when its listener receives `*.<apex>` behind a TLS proxy |
| forwarded | token set; hostthisd sends `X-Hostthis-Forwarded-Host` | the Worker, through hostthisd as a reverse proxy | runtimes not exposed publicly; the routeless parity check |

- **Off is today, byte for byte.** The classifier is skipped, the internal
  router is unchanged, the Room cell's ping answer, frame limit, cap and
  eviction stay off (2.9), and `resolve` and `exists` are additive ops no old
  caller uses, which keeps adjacent Worker generations compatible during
  celld's residency window. The friend's deployment upgrades with no change
  but the rooms bug fixes, which apply in every mode (the frames carry each
  value's stored text, 2.9).
- **Forwarded** is honored only on the internal router, so only after the
  token check, or on a network where the internal router is already trusted.
  Anyone who can reach that router can already write any cell, so trusting the
  forwarded host and client IP there adds no capability. The forwarded host
  must be under the apex (else 400). Phase 5 turns hostthisd's Go read path
  into this reverse proxy (Host and client IP in the two headers, `Upgrade`
  passed through), so the read rules exist once.
- **Direct on celld** (checked against celld v0.6.1 and v0.6.2, which is the
  minimum version for this work):
  - Static assets: the same `assets` block (`directory`, `binding`,
    `run_worker_first` with negation, `_headers`) works, and `env.ASSETS.fetch`
    takes a Request, URL or string. celld sends a strong sha256 ETag and no
    compression; the 25 MiB per-file limit matches Cloudflare.
  - Modules: celld loads one JS module (plus sibling `.wasm`) under
    `no_bundle`. Source stays split into modules; the build emits one bundled
    `dist/index.js` (and the decoder's `.wasm`, if any) that both the celld
    and Cloudflare configs point `main` at.
  - WASM imports work from v0.6.1.
  - The Cache API exists but always misses, so the caches stay off on celld.
  - Client address: none is provided; headers arrive as the client sent
    them. Use `xff-rightmost` behind an ingress that appends
    `X-Forwarded-For`; never `cf-connecting-ip`.
  - `request.url` is `http://` unless the node runs with
    `--trust-forwarded-headers`; the Worker therefore never redirects schemes.
  - `request.cf` is `{}`: nothing reads it.
  - Hibernation APIs, including `setWebSocketAutoResponse`, behave as on
    Cloudflare. There is no server ping and no read timeout, so a client that
    vanishes without a FIN stays counted until a write fails (2.9 reaper).
  - The local dev store returns R2 objects as one chunk; memory tests run
    against a real bucket.

## 4. Security

- **Host split first, fail closed** (2.2). Public hostnames never reach
  internal routes; a public deployment never runs a tokenless internal router.
- **The public router never forwards a client request to a cell.** It builds
  every cell request from parsed values: slug from Host, UUID from the path,
  decoded key, validated body. A test sends `?op=<every op>`, `/__cell/...`
  and `/blob/...` through every public path and asserts no cell or bucket
  change.
- **No row fields leave.** `resolve` returns the served entry only: no
  identity, name, generation, or other versions' upload ids.
- **Blob keys come only from `resolve`**, never from the request. `/blob`
  exists only on the internal router; `static/` keys are fixed by the build.
- **Room isolation is unchanged.** The cell address is `<slug>|<uuid>` from
  the Host-derived slug and a parsed UUIDv4, so cross-app and cross-room
  isolation stay structural, and the 404 shapes (per-key versus scan) match
  1.5. The app is never taken from the path or body.
- **Origin policy is unchanged** and checked before the upgrade. Tightening it
  to same-host only is a separate decision (8, Q3).
- **Rate limits keep a source address the client cannot forge** (2.8).
  Request-rate abuse stays a front-door concern; on Cloudflare a WAF
  rate-limiting rule or the Workers rate-limiting binding can take it.
- **Path mode is dev only**; the Cloudflare template never sets it.
- **Zone features that rewrite responses** (Email Address Obfuscation, Rocket
  Loader, Automatic HTTPS Rewrites, Transform Rules) would alter user HTML and
  shells and break the byte-identical contract. They apply to proxied
  responses today too; verify they are off for the wildcard before phase 3.
- Secrets do not move: the cell token stays a Worker secret, VAPID private keys
  stay in Paste cells.

## 5. Test plan

1. **Read rules.** `node --test` over `read.js`, driven by a shared vector file
   `testdata/read-vectors.json` ((manifest, kind, status, path, flags) to
   action and entry). The Go tests run the same vectors against
   `internal/http`, so the two implementations cannot drift while both exist.
   A second vector file covers the room wire encoding (invalid UTF-8, BOM, deep
   nesting, numbers, empty values).
2. **Decoder.** The golden corpus from `zstdenc` decoded by the Worker and
   compared by sha256; missing magic, truncation and an empty object give 500.
3. **Parity suite.** One Miniflare runtime, one hostthisd, a corpus uploaded
   over SSH; every request goes to hostthisd's HTTP and to the Worker's public
   surface and is diffed per 2.6, against an explicit list of allowed
   differences with reasons.
   - Corpus: every kind; pending and failed (forced through internal ops);
     updated, pinned, unpinned and deleted pastes; the `testdata/sitefixtures/`
     sites (React, Vue, Svelte, plain); a knowledge base with nested `.md`,
     HTML, images and a `files.json`; a damaged manifest; names with spaces and
     non-ASCII.
   - Requests: bare, `?raw`, `?raw=0`, `?files=1`, deep paths, SPA routes,
     `.html` routes, missing assets, `dir` and `dir/`, percent-encoding, `//`,
     `/../`, every `/_hostthis/` asset plus an unknown name, `/healthz`,
     `/readyz`, non-slug and multi-label hosts; GET and HEAD; matching and
     non-matching `If-None-Match`, `If-Modified-Since`.
   - Rooms: create, scan, get, put, delete; 400, 404, 405, 413, 507; reserved
     keys; rate limits with lowered constants in the test runtime; push routes
     and push key.
   - WebSocket: 400, 404, 403, 429; snapshot; mirrored put and delete with a
     dense seq; ephemeral frames to peers only; reserved frame close 1008;
     oversized frame close 1009; cross-room and cross-app isolation; one client
     through hostthisd and one direct in the same room.
   - The suite reads two base URLs and a token from the environment, so an
     operator can run it read-only against a deployed environment in forwarded
     mode, with writes confined to a dedicated test paste.
   - Built: `internal/parity` (`make parity`, part of `make test`). Rate
     limits are reached by filling the creation ledger through the internal
     `roomcreated` op, and the app cap by real writes, so no constant is
     lowered. A deployed run reads `PARITY_HOSTTHISD_URL`,
     `PARITY_WORKER_URL`, `PARITY_TOKEN_FILE` and `PARITY_SLUGS`, and writes
     only with `PARITY_ALLOW_WRITES=1` and `PARITY_WRITE_SLUG`;
     `PARITY_REFERENCE=previous` compares against the previous hostthisd.
4. **e2e.** The Playwright suite gets `E2E_READS=hostthisd|worker`. In worker
   mode hostthisd emits URLs on the Worker's public host (path mode, with a
   hostname distinct from the internal endpoint's, such as `localhost` versus
   `127.0.0.1`). CI runs both until phase 5, then worker only. Built:
   `e2e/fixtures.ts`, with the runtime's public surface from
   `HOSTTHIS_LOCALRT_*` (`worker/README.md`).
5. **Live rooms.** `TestLiveRoom` pointed at the Worker's public WebSocket.
6. **Edge checks** on real Cloudflare (phase 3): headers after the edge, no
   injected markup, WebSocket through the edge, time to reap a vanished client,
   p50 and p95 time to first byte from at least three regions against today's
   CDN path, CPU per request from Workers analytics.

## 6. Migration and cutover

**Phase 0, spec and spikes.** SPEC edits (section 9). Spikes: decoder choice
with numbers; the celld checks in section 3; `_headers` and
`run_worker_first` on Cloudflare; client reaping time on Cloudflare; today's
peak sockets per room from the Room cell's `count` op. Exit: the owner
approves the numbers and section 8.

**Phase 1, build behind the flag.** Public router, `resolve` and `exists`,
`read.js`, `web/shells/`, the assets build, the rooms and push port, direct
WebSocket, forwarded mode, the caches (off by default). Tests 5.1 to 5.5 green
in CI. Release.

**Phase 2, routeless deploy and parity.** Deploy the Worker to staging and
production with `HOSTTHIS_APEX_DOMAIN` set and no route: no public request
changes. hostthisd stays on the previous release, which is the reference
(`PARITY_REFERENCE=previous`, which factors out the release's own changes and
each front door's path handling). An entry's `Size` can be wrong on rows
written before `DecodedSize` existed, so neither server declares a length from
it, and the parity run over EVERY live slug (not a sample) holds each body to
the previous release's whole bytes. Run the suite in forwarded mode against
both environments (production read-only, plus writes on a test paste). Only
then release the new hostthisd. Exit: zero unexplained differences.

**Phase 3, edge canary.** Staging gets the option chosen in 2.10. Production
gets exact-host routes (`<slug>.<apex>/*`) for a few test slugs owned by an
operator key: the real edge and real data, with nothing else affected. Run e2e
and parity through the edge, plus 5.6, plus a purge check per canary slug:
update it over SSH and time the first fresh read from two regions. Exit:
checks pass, an update is visible within a second, and latency is within the
agreed bound.

**Phase 4, flip.** Add `*.<apex>/*` in production (staging first under 2.10a
or b). hostthisd's HTTP code, the origin record and the purger stay exactly as
they are, so rollback is deleting the route and takes effect in seconds. Keeping
the purger on keeps the CDN consistent for that rollback. There is no data to
reconcile: every write still goes through hostthisd into the same cells, and
the Room cell broadcasts to sockets on either path. Watch Worker exceptions and
5xx, CPU per request, the request rate on hot Paste cells, room socket counts,
and smoke. Bake at least a week.

**Phase 5, slim hostthisd.** Spec first. Delete
the Go read handlers, shells and assets (`web/shells/` is then read only by the
Worker build), rooms HTTP, push and WebSocket handlers,
`internal/worker/roomproxy.go`, `internal/roomwire`, the rooms services and
repos only HTTP used, and `/p/<slug>` serving on the apex in subdomain mode.
Keep a small reverse proxy implementing forwarded mode for deployments that do
not expose the Worker. The `CachePurger` port stays: it invalidates the
Worker's edge cache.

## 7. Cost (Workers Paid)

Published prices at the time of writing, to recheck before purchase:

| item | included | over |
| --- | --- | --- |
| Workers plan | $5/month | |
| Worker requests | 10M/month | $0.30 per million |
| Worker CPU | 30M CPU-ms/month | $0.02 per million CPU-ms |
| static asset requests | free, unlimited | |
| Durable Object requests (WebSocket messages in at 20:1) | 1M/month | $0.15 per million |
| Durable Object duration | 400k GB-s/month | $12.50 per million GB-s; idle cells that can hibernate are not billed |
| R2 Class B (reads) | 10M/month | $0.36 per million |
| R2 storage, egress | unchanged; egress free | |

Per read that misses the edge cache: one Worker request, one Durable Object
request, one R2 read (none for a 304, HEAD, shell or asset), and CPU for
routing plus decoding. Decode CPU is unmeasured; the spike sets it.

Illustration at 5 CPU-ms per read on average (an assumption, not a
measurement):

| reads/month | Workers | DO | R2 | CPU | total |
| --- | --- | --- | --- | --- | --- |
| 1M | included | included | included | included | about $5 |
| 10M | included | $1.35 | included | $0.40 | about $7 |
| 50M | $12.00 | $7.35 | $14.40 | $4.40 | about $43 |

An edge hit costs one Worker request and no Durable Object, R2 or decode cost, so hot content stays near the Workers column alone.

Limits that shape serving large files by streaming:

- CPU: 30 s per request by default, configurable to 5 min. Only executing time
  counts, so a slow client does not spend CPU while the stream waits on it.
- No wall-clock limit while the client stays connected.
- Memory: 128 MB per isolate, shared by its concurrent requests. Streaming
  holds a decoder window and a few chunks per request; nothing may read a body
  whole (`arrayBuffer()`, `text()`, or a tee into the cache above the ceiling).
- The largest single stream is a paste decoding to at most 100 MiB (the
  raw-input fast-fail), or one site file within the site cap.
- Static assets: 25 MiB per file (hence the R2 copy of duckdb), 100,000 files
  per version.
- Subrequests: a read uses two or three of the 10,000 allowed.
- Durable Objects: a soft limit of 1,000 requests per second per object. Every
  read of one slug goes to its one Paste cell.

What goes away: origin egress for subdomain traffic, hostthisd CPU and memory
for reads and sockets.

## 8. Risks and open questions

Risks:

1. **Latency for distant readers.** A CDN hit is served at the edge today.
   After, every read crosses to the Paste cell's region and to R2. Requests
   that miss the CDN today (every site file revalidation, every uncached
   paste) already make that trip through the origin, so they do not get worse.
   Edge hits are local; misses are measured in phase 3.
2. **One hot slug** can approach the per-object soft limit (a viral paste, a
   site page loading many files). The edge cache absorbs repeat reads.
3. **Decode CPU on large files** is unmeasured until the spike.
4. **Two implementations of the read rules** until phase 5. Shared vectors and
   the parity suite hold them together; phase 5 ends it.
5. **Room wire edge cases** (invalid UTF-8, BOM, nesting depth) differ between
   Go and JS unless ported deliberately. Vectors; stored values keep their
   stored wire form.
6. **No server heartbeat.** Vanished clients may linger and count against the
   per-room cap. Measured in phase 0; keepalive option in 2.9.
7. **Worker releases now gate public availability.** Today the CDN keeps
   serving cached reads through a broken Worker release. Use gradual
   deployments (versioned percentage rollout) and run parity as a post-deploy
   gate.
8. **`compatibility_date`** is pinned to 2025-01-01. A feature that needs a
   newer date changes runtime behavior for the cell code too; run the cell
   suites at the new date on both runtimes.
9. **Zone settings** that change Worker responses (section 4) or exempt
   hostnames under the wildcard. Audit before phase 3.

Decisions:

1. Staging edge: 2.10c, forwarded mode on staging plus production canary
   routes.
2. One global cap of 64 sockets per room, enforced by the Room cell; the
   per-app cap is dropped.
3. WebSocket origin stays `*.<apex>`; tightening it is a separate change.
4. Reads answer GET and HEAD; other methods get 405.
5. Caching per 2.7: browsers revalidate every load, the edge serves whole
   responses until a write purges the slug, and a one-day edge TTL bounds a
   missed purge.
6. Phase 5 stops serving `/p/<slug>` on the apex in subdomain mode.
7. Bake one week before phase 5.
8. Self-hosting documents forwarded mode until direct mode is verified on
   celld.
9. The Cloudflare purge adapter stays (hostname purge) and fires on every
   write, as today.

## 9. SPEC.md sections that change

- "URL shape" and "Dev-only path mode": who serves subdomains; apex `/p/`.
- "Supported formats" (streaming wording), "Serving a directory", "SPA
  fallback", "Knowledge bases" (the purge sentence): served by the Worker,
  rules unchanged.
- "Rooms": "The HTTP API", "Storage" (existence check placement), "Quota and
  abuse" (trusted source address).
- "Real-time room relay": "Cell-owned fan-out", "Lifecycle and bounds".
- "Limits": "Reads are constant-memory".
- "Blob storage backends": "On-disk format" (the Worker decodes for reads).
- "Worker-backed metadata storage": "Blob and operational boundaries"
  (hostthisd is no longer the only public surface).
- "Edge caching": the Worker response cache, hostname purge, `no-cache` for browsers, weak ETag
  comparison.
- "HTML sandboxing": "Markdown and diff rendering".
- "Health endpoints", "Self-hosting" (Worker variables and modes).
