# hostthis - spec

A self-hostable, dev-first paste service for content that *needs rendering
to be shareable* - HTML, Markdown, unified diffs, and a small future set
of rendered formats. Pipe a file to ssh, get back a URL. No signup, no UI,
no CLI to install. Your existing ssh key is the account.

```
$ cat index.html | ssh hostthis.dev
https://abc12345.hostthis.dev
```

This document defines the v1 surface. Anything not in it is intentionally
out of scope for v1 - see "Non-goals" at the bottom.

---

## What it is, what it isn't

- It IS: a hosting target for **content that needs rendering to share
  well** - HTML pages, Markdown docs, unified diffs - addressable by URL,
  with ssh-pipe as the primary upload mechanism.
- It IS: dev-first. The mental model is `git push` for documents - your ssh
  key is your identity, every operation is one line in a terminal.
- It IS NOT: a general file host. ZIPs, binaries, photos, videos belong
  elsewhere.
- It IS NOT: a comment/collaboration platform.
- It IS NOT: a transient blob host for opaque bytes.

## Supported formats

HTML, Markdown, diff, Mermaid, PDF, CSV/TSV, JSON/JSONL, folded stacks
(flame graph), structured logs (NDJSON), and plain text.

Detection: by content sniffed from the first **8 KiB** of the upload, plus an
optional explicit `--type` flag. The MIME classification inside that gate
still reads only the first 512 bytes, which is all the sniffing algorithm
defines, but the per-format heuristics get the larger window: they need to
see structure, and a single line of a real profile or a wide CSV row can
exceed 512 bytes on its own, leaving the smaller window with no complete
line to judge. A Markdown paste is served as a
fixed, content-independent HTML shell that loads a bundled client-side
renderer (marked + DOMPurify); the shell fetches the raw Markdown bytes
(via `?raw`) and renders them in the browser.
The server streams the raw bytes with `io.Copy`, so its memory stays
constant regardless of paste size, mirroring the HTML serve path. The
shell follows the same sandboxing rules as user-supplied HTML, and
DOMPurify sanitizes the rendered output in the browser the same way the
old server-side bluemonday pass did.

A **diff** paste is a unified diff (`git diff` / `diff -u` output). It is
served exactly like Markdown - a fixed, content-independent HTML shell
that loads a bundled client-side renderer, here
[diff2html](https://diff2html.xyz) + [highlight.js](https://highlightjs.org)
- and the shell fetches the raw diff bytes (via `?raw`) and renders them
in the browser. No server-side diffing: the
server only ever streams the raw bytes with `io.Copy`, so its memory stays
constant regardless of paste size, mirroring the HTML and Markdown paths.
The rendered view defaults to **line-by-line** (reads well on mobile) with
a control to switch to **side-by-side**; the choice is persisted in
`localStorage` so it sticks across pastes. Code inside the diff is
syntax-highlighted via highlight.js, and the view is dark-mode aware via
`prefers-color-scheme`. Detection is conservative (see "File handling ->
Format gate"): a paste must carry at least one real unified-diff hunk
header to auto-detect as a diff, so an ordinary text paste that merely
contains `+`/`-` lines is not mis-rendered; `--type diff` forces it.

**Fenced blocks in a markdown paste.** A ` ```mermaid ` block becomes a
diagram, drawn by the same renderer the standalone mermaid kind uses and
fetched only when such a fence is present, so a prose paste never loads it.

A ` ```diff ` block stays a **code block**, tinted by leading character
(`+` green, `-` red, `@@` and file headers emphasised). The fence is a
LANGUAGE TAG in the same sense as ` ```java `: it asks for highlighting
inside a code block, not for the standalone diff viewer's chrome. Tinting
by leading character is the whole grammar, so it needs no highlighter
library and no lazy load at all.

A **mermaid** paste is a [Mermaid](https://mermaid.js.org) diagram source
(`graph`, `sequenceDiagram`, `flowchart`, ...). Same shape again: a fixed
shell fetches the raw source via `?raw` and renders it to SVG in the
browser. Mermaid also renders **inside markdown pastes**: a fenced
` ```mermaid ` block in any markdown paste becomes a diagram. The renderer
is ~3.5 MB, so the markdown shell loads it **only when the fetched source
actually contains a mermaid fence** - a prose paste never pays for it.

A **pdf** paste renders through [pdf.js](https://mozilla.github.io/pdf.js/)
with **scripting disabled**, giving page navigation, text selection, and
per-page deep links. PDF is the first accepted kind whose bytes are not
text; see "File handling -> Format gate" for why that is a smaller step
than it looks.

A **csv** paste (also TSV) renders as a sortable table with inferred column
types and per-column statistics (row count, null count, distinct count,
min/max for numeric columns). A **json** paste (also JSONL) renders as a
collapsible tree with a filter box. Both are parsed in the browser from
the same `?raw` bytes; the server does not parse either format.

A **flamegraph** paste is a profile in **folded stack** format: one line
per unique stack, frames separated by `;`, then a space and an integer
sample count.

```
main;handleRequest;parseHeader 118
main;handleRequest;readBody 402
main;flush 27
```

That format was chosen because it is the one every profiler already
converts to. `perf script` via stackcollapse, Go's pprof, py-spy, async
profiler, and `dtrace` all emit or export it, so hostthis needs no
per-profiler parsing and no binary decoding. Accepting pprof's protobuf
directly was rejected for v1: it is gzipped protobuf, which cannot be
told apart from other gzipped bytes by sniffing without decoding it, and
the conversion is a single command on the producer's side.

The rendered view is an interactive flame graph: width is proportional to
samples, clicking a frame zooms to it, and a filter box highlights
matching frames and reports their combined share. Rendering is entirely
client-side from the `?raw` bytes, like every other kind. The renderer is
written for this shell rather than pulled in as a library, because the
whole algorithm is a prefix-tree aggregation plus a rectangle layout.

**A flame graph is not a timeline.** Frames are aggregated by stack, so
the x-axis is share of samples, not time; a wide frame means "much CPU",
never "ran for a long time". Profiles of work that is mostly waiting
(network, locks, disk) will look almost empty, which is information, not
a bug.

A **text** paste is anything textual that matches no richer format. It
renders with a line-number gutter, and every line is addressable: clicking
or tapping a number selects it, and a second click (shift) or tap extends
to a range, exactly as the diff viewer does. That addressability is the
whole value, and it is what the raw bytes cannot give.

Text is the **fallback**, never a competitor: it is reached only after
every other gate declines, so a document that looks like Markdown still
renders as Markdown. Before it existed, prose carrying no Markdown cue -
no heading, list, fence, blockquote or link - was rejected outright, which
meant a config file, a stack trace, or a transcript bounced.

Accepting it does not turn hostthis into a general file host. The bar is
unchanged: a rendered view has to beat the raw bytes, and a citable line
range does. The gate still requires the bytes to sniff as text, so nothing
binary reaches it.

A **log** paste is structured logs as NDJSON: one JSON object per line.
That is the shape Loki and OpenSearch both work in and what every JSON
logger emits, so it is the one format that covers the ecosystem rather
than any single tool. Three container shapes are unwrapped on the way in,
because they are what the tools actually export:

- Loki's stream objects, `{"stream":{...},"values":[[ts, line], ...]}`
- OpenSearch bulk NDJSON, whose *action* lines (`{"create":{}}`) alternate
  with documents and would otherwise render as empty records
- an OpenSearch search response, with records under `hits.hits[]._source`

Detection requires most lines to carry a recognisable timestamp **and** a
level or message field, under any of the usual names (`@timestamp`, `ts`,
`time`; `level`, `severity`; `message`, `msg`). It runs **before** the JSON
gate, which would otherwise claim NDJSON and render a log as a collapsible
tree - correct, but useless for reading logs.

The view adds what a log file cannot. A **query bar** takes field matchers
rather than a plain substring:

```
level=error status>=500 path=~^/p/ "connection reset"
```

Equality, negation (`!=`), numeric comparison (`>`, `>=`, `<`, `<=`),
regex (`=~`, `!~`), and quoted phrases matched against the whole record.
Terms combine with AND, which is what a reader narrowing an investigation
means; OR is available per-field as `level=error|warn`. Level chips write
into the same query rather than filtering separately, so there is one
mechanism and the URL describes the whole view.

A **volume histogram** buckets records over time. Dragging across it
selects a time window, which is the fastest way to get from "something
broke" to the minute it broke in.

Everything runs against the whole file in the browser, so filtering costs
no round trip. That is the one structural advantage a paste has over a
log service, and it is why the query bar can re-evaluate on every
keystroke.

A **field sidebar** lists every field the records actually carry, ordered
by coverage, with its distinct-value count and its top values as shares of
the current result set. Clicking a value filters to it; clicking a field
promotes it to its own column so values line up down the page. Both are
recomputed against the filtered set, so the sidebar describes what is on
screen rather than the whole file.

Fields already rendered as columns (time, level, message) are omitted:
listing them would offer the reader a way to print a field twice. Distinct
values are tracked up to a cap and reported as `200+` beyond it, because a
request-id field has one value per record and an exact count of those is
neither cheap nor useful.

**Clicking a record expands it** to every field it carries, each one
clickable to filter by that value, plus its raw JSON. Reading a value and
then retyping it into the query bar is the tedious half of an
investigation, so the value itself is the control.

An expanded record also offers **the records around it**, which are shown
even though the query excludes them. "What happened just before this
error" is the question a filtered log cannot otherwise answer without
abandoning the filter that found the error. Context records are dimmed and
marked so they can never be mistaken for matches.

**The view lives in the URL.** Query, time window and selection are all
in the fragment, so a filtered, time-boxed view is a link. A paste
service has no place to save a search, and it does not need one: the link
carries the data and the view together.

Uploads of unsupported types are **rejected** with a clear error pointing
at what we accept:

```
$ cat photo.jpg | ssh hostthis.dev
error: hostthis only accepts content it can render
       (html, markdown, diff, mermaid, pdf, csv, json, flamegraph, log, text)
```

This is deliberate scope. The inclusion test is not "can we store it" -
storage is format-blind and always has been - but:

> **Does the rendered view beat downloading the file?**

A CSV you can sort and summarise beats opening a spreadsheet; a PDF you
can link to page 7 of beats an attachment; a diagram beats a screenshot of
a diagram. An archive, an executable, or a video does not clear that bar,
so hostthis is not a general file host. Every accepted format expands the
surface for abuse and sandboxing edge cases, so each one has to earn it.

### Deep links (addressing a location inside a paste)

Every rendered kind accepts a URL **fragment** naming a place inside the
content, so a paste can be *cited* and not merely sent:

| Fragment | Kinds | Meaning |
| --- | --- | --- |
| `#<heading-slug>` | markdown | scroll to that heading |
| `#page=<n>` | pdf | that page |
| `#row=<n>` | csv | that row (1-based, excluding the header) |
| `#F<f>L<n>` / `#F<f>L<a>-L<b>` | diff | that line of file `f`, or that range |
| `#focus=<a;b;c>` | flamegraph | zoom to that stack |
| `#q=<text>` | flamegraph | highlight frames matching that text |
| `#focus=<a;b;c>&q=<text>` | flamegraph | both at once |
| `#L<n>` / `#L<a>-L<b>` | text, log | that line, or that range |

**A flame graph anchor names the stack, not a coordinate.** Frame indices
and pixel positions both change whenever the profile is re-recorded, so a
link written against one run would silently point somewhere else in the
next. The stack path is the only identifier stable across recordings, and
it stays readable in the URL. A `#focus=` naming a stack absent from the
profile resolves to the nearest ancestor that does exist rather than
failing, so a link taken from one run still lands usefully in the next.

Zoom and highlight combine because they are independent axes: a reader who
has zoomed and then searches would otherwise produce a URL describing only
the search, silently discarding the zoom that gives it meaning. The
fragment always states the viewer's whole state. `&` separates them, and
both values are percent-encoded, so a frame name containing `&` or `;`
survives the round trip.

**A diff line anchor carries the number the reader can SEE**, not a position:
the new-file line for context and added lines, the old-file line for deletions,
which is the only number those rows display. Anything else produces a link that
contradicts the page - an ordinal scheme numbering content rows made `#L75`
highlight the row whose gutter reads `180`.

`F<f>` scopes it to the f-th file, because the same line number occurs in every
file of a multi-file diff. A bare `#L<n>` is accepted and means file 1.

Those two numbering schemes can collide inside one file: a line deleted at old
line 17 and another added at new line 17 both display `17`. The bare number
resolves to the **new-file** row, since that is the line a reader shares.

**A range covers the rows between its endpoints, not the numbers between them.**
Gutter numbers are not consecutive across a deletion, so a range is resolved by
spanning rows and then clamped to the ones that exist - a hand-written range
running past the end of a hunk lands on the rows it does cover rather than
failing, and a range drawn over a deletion has no holes in it.

**Selecting a range needs no keyboard.** Shift-click extends from the last
selected line. A touch device has no shift key, so there a second tap extends
instead, and tapping any line inside the selection clears it. Escape also
clears. The two behaviours are chosen per input event, not per device, so a
touchscreen laptop follows whichever one the reader actually used.

The numbers are those of the **line-by-line** rendering. Side-by-side splits one
logical line across two rows, so a link resolved there would land differently: a
line link therefore switches the view when it is RESOLVED. That override happens
only at resolve time (load, or a hashchange), never on a re-render, so a reader
who presses side-by-side while a line link sits in the URL is not yanked back.

Fragments are chosen over query parameters deliberately: a fragment is
never sent to the server, so deep links add no routing, cost no extra
request, and leave every URL a single cacheable representation. The
viewers also *produce* them - clicking a line number, row, or heading
anchor rewrites `location.hash` in place so the address bar always holds a
copyable deep link.

**The load order rule.** Every kind here renders client-side after an async
`?raw` fetch, so the browser's native fragment scroll fires against an
empty document and lands at the top. A shell MUST therefore resolve the
fragment *itself* after render, and again on `hashchange`. This is a
property of the client-render architecture, not of any one viewer: any new
shell has the same obligation.

---

## URL shape

Each paste lives at its own subdomain on the apex:

- `https://abc12345.hostthis.dev` - random 8-char slug
  (alphabet: `abcdefghijkmnpqrstuvwxyz23456789` - lowercase, no ambiguous chars)

There's no concept of "public vs private" because the slug *is* the
secret: 32^8 ≈ 1.1 × 10^12 possibilities, computationally infeasible to
guess. Anyone with the URL can view; anyone without it can't find it.

Apex `https://hostthis.dev` is the homepage / docs. Never serves user
content.

**Subdomain-per-paste, not path-per-paste**, because:
- Each paste gets its own origin - cookies, JS, CSP can't reach apex or
  other pastes. Standard sandbox pattern for multi-tenant content hosts.
- Reads cleaner in chats ("check this out: `acme-demo.hostthis.dev`").
- Shorter total URL than `hostthis.dev/p/abc12345`.

Wildcard cert covers `*.hostthis.dev` via Let's Encrypt DNS-01.

### Reserved subdomains

There is no explicit reserved-names list. Conflicts can't occur:
slugs are exactly 8 characters from the 32-char alphabet
`abcdefghijkmnpqrstuvwxyz23456789` (no `l`, `o`, `0`, `1` to avoid
visual ambiguity). Anything shorter - `www`, `api`, `admin`, `mcp` -
isn't a parseable slug, so `slugFromHost` rejects it and the request
falls through to the apex handler. Anything 8 chars long that the
operator might want reserved (e.g. `dashboard`) contains a letter
outside the alphabet (`o`, here) and can't be generated by
`NewRandomSlug` in the first place.

The operator is free to claim e.g. `status.hostthis.dev` for a
status page - it'll never collide with a generated slug.

### Dev-only path mode

Production runs subdomain mode (`<slug>.<apex>`). For local development
where wildcard DNS + certs are friction, the binary also supports a
`--mode path` flag (or `HOSTTHIS_URL_MODE=path` env):

- Pastes live at `<apex>/p/<slug>` instead of `<slug>.<apex>`.
- The SSH server emits the path-shape URL after upload.
- The HTTP router accepts BOTH forms at runtime - same handler - so
  changing modes is just changing what URL gets emitted.

**Path mode is dev-only and breaks the origin-isolation property** -
all pastes share the apex origin, so user-uploaded JS could read apex
cookies or talk to other pastes' state. The binary's startup logs a
loud warning when running in path mode, and any production deploy
must use `--mode subdomain`.

---

## Identity

**SSH key fingerprint IS the account.** No signup form, no email, no
password. First time a key fingerprint connects, the server creates an
account row keyed on the SHA256 fingerprint. Every subsequent connection
from the same key is "the same user".

A key is required on every session. Without one the server prints
"ssh key required" on stderr, points at `ssh-keygen`, and exits 3.
There is no anonymous mode.

There is no "new key cooldown" or trust ramp - the per-identity
quota (see Limits) already bounds abuse via key rotation, so we
don't add a second mechanism for the same problem.

### Security: snooping a public key gives nothing

SSH auth requires proving you hold the matching **private** key - server
sends a random challenge, client signs with the private key, server
verifies the signature against the public key. So a leaked `id_*.pub` is
harmless. Same model as `git push` or ssh-ing into a Linux box.

Implementation must use `golang.org/x/crypto/ssh`'s `PublicKeyCallback`,
which is invoked AFTER the lib has already cryptographically verified the
signature. Never trust a self-asserted username or fingerprint.

### SSH session hardening

A hostthis session is a single short verb exchange: client connects,
runs one command, server replies, connection closes. None of the other
SSH protocol surfaces are needed, and every one we leave open is a
potential pivot if an identity is ever compromised. The server refuses
them all at the wish/charmbracelet layer, as defense in depth on top
of the per-identity quota and the Sybil per-subnet gate.

Disabled:

- **Local port forwarding** (`ssh -L`, the `direct-tcpip` channel).
  `LocalPortForwardingCallback` returns false. The default wish channel
  handler map doesn't register `direct-tcpip` either, so the channel-
  open is refused before the callback is even consulted. Belt and
  suspenders.
- **Reverse port forwarding** (`ssh -R`, the `tcpip-forward` global
  request). `ReversePortForwardingCallback` returns false. The client
  receives a denial on the forward request and never gets a listener.
- **Subsystems** (sftp, scp-via-subsystem, anything else). The
  `SessionRequestCallback` returns false for `requestType ==
  "subsystem"`. The library also has an empty `SubsystemHandlers` map,
  so any subsystem name would be refused regardless; the callback
  guarantees the refusal even if a future upstream change adds a
  default subsystem.
- **X11 forwarding** (`ssh -X`, the `x11-req` session request). The
  library has no x11-req branch in its request switch, so it falls to
  the default case which replies `false`. No additional gate needed,
  but the contract is documented so a future change can't quietly
  enable it.
- **Agent forwarding** (`ssh -A`, the `auth-agent-req@openssh.com`
  session request). The library acknowledges the request but hostthis
  never sets up a forwarding socket, so the request is functionally a
  no-op for any client that tried to use it. No agent is ever exposed
  back to the connecting client.

Kept enabled:

- **PTY allocation.** The verb-help formatter switches LF to CRLF when
  a PTY is present, and the test suite exercises both shapes. A PTY by
  itself is not a tunnel; it's just stdin/stdout wrapped in line
  discipline.
- **shell and exec session requests.** These are the canonical paths
  for running a verb. The `SessionRequestCallback` returns true for
  both.

The mechanism lives in `internal/ssh/hardening.go` (the `withHardening`
ssh.Option) and is wired into `wish.NewServer` alongside the other
With* options. Refusal behavior is pinned by
`internal/ssh/hardening_test.go`.

---

## File handling

- **Per-paste hard cap**: 10 MiB of COMPRESSED bytes (post-zstd).
  Equal to the per-identity quota (see Limits). Compression-aware:
  heavily redundant content (typical HTML/Markdown) compresses 5–10×,
  so a user can upload ~50–100 MiB of text and still fit; binary or
  already-compressed payloads compress poorly and hit the cap fast.
  The user-visible cap is the same number in both directions because
  it's the number that actually constrains the service.
- **Hard raw-byte fast-fail**: 100 MiB. To prevent an attacker from
  streaming arbitrary bytes forever just to discover whether they
  compress under the cap, the server stops reading after 100 MiB of
  INPUT regardless of how well it compresses. Anything that requires
  reading more than that is "too big to evaluate" and rejected with
  `upload too large to consider`. Generous enough that no legitimate
  text payload ever hits this; tight enough to bound the read.
- **Format gate**: accept only supported content types. Server sniffs the
  first 512 bytes for content type via `http.DetectContentType` and
  cross-checks any explicit `--type` flag. Unsupported content is rejected
  with a clear error pointing at what we accept - no silent fallback to
  `attachment` rendering.

  Detection order among the text kinds is precision-first, because the
  cheap checks are the imprecise ones: **diff** (a real unified-diff hunk
  header `@@ -<n>[,<n>] +<n>[,<n>] @@`), then **mermaid** (an opening
  diagram keyword on the first non-blank line), then **json** (the prefix
  parses as a JSON value), then **csv** (a consistent delimiter count
  across the first several lines), then **markdown** (any structural cue),
  which is the loosest and so must run last. Each gate is specific enough
  that ordinary prose never trips it; `--type` forces any kind.

  A hunk header appearing AFTER a markdown code fence is **quoted**, not
  the document's own format, so that document is markdown. This is what
  lets a design doc show a diff without its prose being served as diff
  noise, and nothing is lost by it: the markdown viewer draws a fenced
  diff through the same renderer the diff kind uses. The ordering test is
  what keeps a real diff OF a markdown file working - there the hunk
  header comes first and the fence is part of the diffed content.

- **Binary kinds pass the same gate, not a hole in it.** PDF is accepted
  by its `%PDF-` magic exactly as a site archive is accepted by its gzip
  magic - an explicit format signature, checked in the same function,
  never a fallback for "bytes we could not classify." The textual branches
  still reject binary under a text hint, so `--type html` cannot smuggle a
  binary through and have it served as `text/html`.

  What makes this a small step rather than a new posture: hostthis already
  stores and serves arbitrary bytes with correct content types inside site
  archives, and every paste already gets its own origin. A PDF kind adds a
  viewer and a single-file upload path, not a new storage capability and
  not a new sandbox boundary. The PDF is served with `Content-Type:
  application/pdf` and rendered by pdf.js with scripting disabled, so an
  embedded-JS PDF cannot execute.
- **Streaming I/O**: server reads stdin as a stream (no full-buffer
  allocation), tees through three sinks in parallel: a sha256 hasher
  (over uncompressed bytes - content addressability is by ORIGINAL
  content), a zstd writer (compressed output to staging), and a
  raw-byte counter (the 100 MiB fast-fail). After EOF, the compressed
  staging buffer's size is compared against the 10 MiB cap. If it
  fits, the staged bytes are flushed to the configured `BlobStore`
  under the original-content sha256 key. If not, the upload is
  rejected with `upload exceeds 10 MiB compressed cap; your bytes
  compress to <actual> - try removing binary data` and the staging
  buffer is discarded.
- **Storage backend**: pluggable. See "Blob storage backends" below.
  Default is the on-disk store (`data/blobs/<sha256[:2]>/<sha256>`).
  Markdown and diff are never rendered on the read path: such a read
  either streams the raw bytes (when the client asks for them via `?raw`)
  or serves the fixed client-render shell, so server memory is constant
  regardless of paste size.
- **Storage compression**: all blob bytes are persisted zstd-encoded
  by the storage layer. Compression is invisible above the BlobStore
  interface - `Put` compresses on the way in (level 3, balance of
  speed and ratio), `Get` decompresses on the way out. The blob key
  remains the sha256 of the ORIGINAL (uncompressed) bytes, so dedup
  works on logical content. Both the disk store and the S3 backend
  share the same encoding. See "Blob storage backends → On-disk
  format" below for the compression-version header and the fallback
  for older uncompressed blobs (rolling-migration support; no flag
  day).

## Paste lifecycle status (async blob write)

A paste has a **status**: `pending`, `ready`, or `failed`. The status
exists to hide the slow part of `Create` (the blob write to object
storage, ~250 ms of a ~400 ms paste) behind a fast metadata-only
acknowledgement, so the uploader gets a URL back as soon as the paste is
durably reserved + named, not after the bytes finish landing in the
object store.

This whole section describes the DETACHED-store path (the default - local,
and shale-without-a-blob-bucket), where the blob write happens after
the metadata commit. On the shale-collocated blob path (`HOSTTHIS_SHALE_BLOB_BUCKET`
set, see "Shale-collocated blobs") this model COLLAPSES: the bytes are staged
durably before the metadata commits and the pointer co-commits with the row, so
there is no window between row and bytes - a shale-collocated paste commits
`ready` directly and the pending machinery below does not run for it.

### Why it exists

The original `Create` ran strictly synchronously: stream + hash +
compress (in memory, ~3 ms), then `PutPrecompressed` the blob to the
object store (~250 ms, the bottleneck), then the metadata insert
(~20 ms). The uploader waited for all three before seeing the URL.

The blob bytes are content-addressed and immutable, and the metadata
insert already enforces quota (the committed paste + version rows count
against the owner's quota the moment they land). So we can return the URL
right after the metadata is committed and finish the blob write in the
background. The cost is a window where the URL exists but the bytes do not
yet, which the status models explicitly.

### The three states

- **`pending`**: the metadata is committed (slug claimed, quota checked,
  paste row written) but the content blob has not finished landing in
  the object store. A GET on a pending paste serves a lightweight
  **loading page** that auto-refreshes (a `<meta http-equiv="refresh">`
  ~1 s poll) until the paste resolves. The pending paste's authoritative
  rows already count toward the uploader's quota (the quota scan / sum
  includes any non-`failed` paste), so a pending paste counts against
  quota exactly like a ready one.
- **`ready`**: the blob write succeeded and the status was flipped
  `pending -> ready` by the background finalizer. A GET serves the
  content exactly as before this feature existed. This is the terminal
  success state.
- **`failed`**: the background blob write failed (object store error,
  or the finalizer explicitly failed the write). A GET serves a small
  **error page**. A `failed` paste is excluded from the quota scan / sum
  as part of the transition, so it no longer charges quota.

A paste that predates this feature (a row with no persisted status) is
read as **`ready`**: the absence of a status means "written before the
lifecycle existed, therefore complete." This keeps the change a pure
additive migration with no flag day.

### Create: the synchronous half

`Create` now does, synchronously, before returning the URL:

1. stream + hash + compress (hold the compressed body in the handling
   pod's memory),
2. detect the content kind,
3. **check quota + write the authoritative paste row with
   `status=pending`** (the fast metadata path, ~20 ms),
4. return the URL.

Quota is enforced HERE, synchronously, by the same quota check used
before (a scan of the owner's live rows on shale; a serialized in-transaction
sum on the single-transaction backends). An over-quota upload is rejected before any URL is
handed out: the async split does not weaken the quota gate.

### Finalize: the asynchronous half

After `Create` returns the URL, a background goroutine (owned by the
upload service) runs the finalizer:

1. `PutPrecompressed` the held bytes to the object store,
2. on success: flip the paste `pending -> ready` (a small metadata CAS),
3. on failure: flip the paste `pending -> failed` (a failed paste is
   excluded from the quota scan / sum, returning the charged bytes).

The transitions are guarded: the finalizer only advances a paste that is
still `pending`, so a late-arriving finalize against a row something else
already moved off `pending` is a no-op rather than a resurrection.

### Durability trade (read this)

The compressed bytes live ONLY in the handling pod's memory between the
synchronous metadata commit and the background blob write. **If the pod
crashes in that window, those bytes are lost**: the metadata says
`pending` but no blob will ever arrive. This is the one durability
regression the feature introduces, and it is bounded:

- The window is the blob-write latency (~250 ms), not the whole request.
- A paste stuck `pending` STAYS pending: nothing ages it out (see
  "Phantom entries are accepted, not repaired"). It keeps its charged
  bytes and shows a loading screen until its owner deletes it. This is the
  detached-store path only - the shale-collocated path prod runs commits
  READY with the bytes already durable, so it has no pending window at all.
- Nothing that was previously durable becomes less durable: a `ready`
  paste is exactly as durable as before (blob in object store, metadata
  committed). Only the brief pending window is at-risk, and only for
  bytes the uploader has not yet been told are permanent.

This is an explicit, documented trade: a ~250 ms at-risk window on the
freshest uploads, in exchange for hiding the ~250 ms blob-write latency
from every uploader. It applies only to the detached-store path; the
deployed shape does not take it.

## Static site archives

A single renderable file is the common case, but the same upload pipe
also accepts a **gzip-tar archive of a static site** (HTML / CSS / JS).
Pipe the tarball exactly the way you pipe any file:

```
$ tar czf - mysite/ | ssh hostthis.dev
https://abc12345.hostthis.dev
```

There is no new verb and no flag. hostthis DETECTS the archive the same
way it detects Markdown vs HTML (by sniffing the upload), safe-untars
it, stores each file as a content-addressed blob plus a manifest, and
serves the directory at `<slug>.hostthis.dev/<path>`. Everything else -
identity, quota, versioning, the security model - is
identical to a single-file paste. A static site is just "a paste that
happens to be a directory."

A single shared leading directory is flattened: when every entry is
under one top-level dir (the natural `tar czf - mysite/`), that dir is
stripped so `index.html` serves at the site root rather than under
`/mysite/`. OS sidecar files - macOS AppleDouble `._*`, `.DS_Store`, and
the `__MACOSX/` container - are skipped, never published. A site is
deleted the same way as a paste: `ssh hostthis.dev delete <slug>` (the
delete verb tries the paste first, then falls through to an
owner-checked site delete; a non-site / foreign slug collapses to
not-found, no existence leak).

This is the now-real form of the "Static directory hosting" bullet
under "Future directions"; the persistence-API bullet there stays a
proposal.

### Detection: gzip-tar as a format

The format gate (`DetectKind`, see "File handling → Format gate") gains
one branch. After the existing HTML / Markdown sniffing, the detector
checks the captured upload prefix for the **gzip magic** (`0x1f 0x8b`),
and if present, peeks one tar header out of the decompressed stream to
confirm a tar inside (a gzip-tar = `.tar.gz` / `.tgz`). The detection
is by content, never by filename - the SSH pipe carries no filename, so
this matches how every other format is recognized.

A gzip-tar that survives the safe-untar (below) AND contains web
content (an `index.html`, or at least one `.html` / `.css` / `.js`
file) routes to the **site** path. A gzip-tar with no web content is
**rejected** as unsupported, the same outcome as any unsupported
upload today (see the "Supported formats" rejection). This keeps the
scope narrow on purpose: hostthis hosts renderable web content, not
arbitrary file trees.

Scope for this version is **gzip-tar only**. Plain (uncompressed) tar
and zip are natural follow-ons but out of scope here; an upload that
sniffs as zip or bare tar is rejected like any other unsupported type.

### Detection: unified diff as a format

The format gate also recognizes a **unified diff** (`git diff` /
`diff -u` output). Within `DetectKind`, after the gzip-tar and HTML
branches but BEFORE the Markdown fallback, the detector scans the upload
prefix for a real unified-diff hunk header matching
`@@ -<n>[,<n>] +<n>[,<n>] @@`. The hunk header is the gate: `diff --git`,
`--- ` / `+++ ` file headers, or `Index:` lines may strengthen the
signal but are not sufficient on their own, and a paste that merely
contains `+`/`-` lines (prose, source code, a markdown list) does NOT
match. This conservatism is deliberate - a false positive renders normal
text through diff2html, which looks broken, whereas a false negative just
falls through to the Markdown/HTML path. Detection is by content, never
by filename, matching every other format. `--type diff` (hint `"diff"`)
forces the kind; like the other text hints it still requires the bytes to
sniff as text, so a binary relabelled `diff` is rejected.

### Safe-untar (security-critical)

Untarring attacker-controlled bytes is the load-bearing risk, so the
extractor enforces three guards while it STREAMS the archive (never
"decompress fully, then validate"):

- **Path safety (zip-slip / tar traversal guard).** Every tar entry
  must be a regular file or a directory; symlinks, hardlinks, devices,
  FIFOs, and every other type are rejected outright (no following a
  symlink out of the site root, no hardlink games). Each entry's path
  is cleaned and rejected if it is absolute, contains a `..` segment,
  or otherwise escapes the site root. The manifest only ever holds
  safe, site-root-relative paths.
- **Decompression-bomb guard.** Total UNCOMPRESSED bytes are tracked as
  the tar is streamed, and extraction ABORTS the instant the running
  total would exceed the identity's available quota (and a max-site-size
  cap). A tiny archive can expand to gigabytes, so the check is on the
  bytes as they are read out, never on the post-decompression result.
  The aborted upload writes nothing durable.
- **File-count and manifest-size caps.** The number of regular-file
  entries is capped (5000) and the total manifest path text is bounded
  (1 MiB), with a per-path length cap (1 KiB), so a "million tiny files"
  archive cannot exhaust file descriptors, inodes, or metadata-store
  space even though each file is small.

Any guard tripping aborts the whole deploy: a half-extracted site is
never persisted and never served (deploys are atomic - see Versioning
below).

### One artifact, not two aggregates

There is ONE stored thing. A paste and a site are the same aggregate at
different cardinalities, and modelling them separately was a mistake that cost
a duplicate implementation of every cross-cutting concern.

An **Artifact** is a slug, an owner, and a versioned **Manifest**:

- `Slug` - 8 chars from one alphabet and ONE namespace. A slug names exactly
  one artifact; the cross-family collision read that used to keep pastes and
  sites from colliding is gone, because there is no second family to collide
  with.
- `Identity` - the owner's SSH key fingerprint. Quota and capability gate.
- `Manifest` - the value object mapping each safe relative path to its blob
  (sha256, size, content-type-by-extension).
- `PinnedVersion` / `LatestVersion`, `CreatedAt` / `UpdatedAt` - as before.

**A single document is a one-entry manifest at `/`.** That is the whole
difference between what used to be called a paste and what used to be called a
site: how many entries the manifest holds. Nothing in storage, quota,
enumeration, listing, deletion, or crash recovery distinguishes them.

The `Manifest` stays a pure value object: building it, looking a path up in it,
and deriving content-type from an extension are I/O-free domain operations.

### A version is a whole-manifest snapshot

Versioning applies to the artifact, uniformly. `versions/<slug>/<N>` holds a
MANIFEST, not a single blob reference:

- updating a single-file artifact writes a new one-entry manifest,
- redeploying a multi-file artifact writes a new N-entry manifest,
- `pin` selects a version, so it means the same thing for both.

Per-file versioning was rejected: it gives no coherent answer to "what did this
look like at version 3" and no sensible pin target.

**Stored shape.** The version row carries the encoded manifest alongside the
flat root descriptor (kind, sha, size) it already had. The flat fields are
RETAINED rather than replaced: a row written before versions carried a manifest
has no other description of its content, and resolves through them via
`Version.RootKind` / `RootSHA` / `RootSize`. So the manifest is additive - no
migration, and an old row is readable unchanged. A manifest that fails to
decode falls back the same way rather than failing the read, since the content
it describes is still perfectly readable.

Every version WRITTEN from here on carries one, including a single document,
whose manifest is simply of length one at `/`. That is what lets a reader stop
asking which shape it holds.

The manifest lives INSIDE the served-content descriptor, not beside it, so the
head's existing "the whole served descriptor rolls, never one field" invariant
carries it. Insert, append, pin and unpin all roll the head from the version it
serves, which is why the head can answer a path lookup on its own - one read,
not a head read followed by a version read.

Two consequences worth stating:

**Multi-file artifacts gain version history, pin, and rollback.** Under the old
split, a redeploy destroyed the previous state and a bad deploy was
unrecoverable. Uniform versioning removes that cliff.

**A redeploy is an update.** It appends the new manifest as a version and the
previous one stays live, so a directory pins, rolls back and rolls forward like
any other artifact. An update has never thrown away what it replaced, and a
directory is not an exception.

It therefore charges like an update: every live version counts against quota,
and an owner reclaims bytes by deleting versions they no longer want. Blob
dedup keeps the growth proportional to what actually changed - a redeploy
touching one file of two hundred stores and charges for one blob.

**A redeploy is the migration.** A directory deployed before the collapse has
no artifact, so redeploying it writes one and drops the legacy row. That is not
only a convenience: without it a legacy directory becomes permanently
un-redeployable the moment the artifact path is wired. The artifact is written
FIRST, so a failure between the two steps leaves the old row readable rather
than losing it.

The legacy row cannot be kept as a rollback escape hatch. A directory present
in both families is enumerated by both, so its owner would see it twice and be
charged for it twice.

**One artifact appears in one listing.** A directory is an artifact, so it is
enumerated by the artifact index. Anything that also reports it as a site would
show it twice and charge it twice - the same trap in two places. During the
migration the site surface therefore reports ONLY rows the artifact families do
not yet cover, and that set empties as the migration runs.

**Unchanged files cost nothing across versions.** Blobs are content-addressed
and were already deduped within a manifest; the same dedup now spans versions,
so a redeploy that changes one file of two hundred stores one blob.

### Storage

Files live in the content-addressed `BlobStore`: each is `Put` under its
sha256, so identical files across versions, across artifacts, and across owners
are stored once.

On the transactional blob path, where blobs are addressed by ID rather than by
hash, each manifest entry carries its own blob id. A manifest is therefore
self-sufficient: resolving a file needs the manifest alone, with no side-table
to keep in step with it. A redeploy stages only the files that CHANGED, so an
entry with no newly staged blob keeps the id a previous deploy bound.

The flat descriptor's blob id is the ROOT entry's, on every write path that
sets one. Pairing the root's content hash with an arbitrary one of a
directory's staged blobs resolves the root to some other file's bytes - a
silent content mixup rather than an error.

### Draining the legacy site family

A directory deployed before the collapse migrates when redeployed, but one
nobody touches would sit on the legacy read path forever - and the old family
cannot be deleted while any row remains. A sweep converts the rest: a
node-local scan of the site family on the units THIS node mounted, no fan-out.
It runs late for the same reason the intent sweep does - converting a row writes
to families on other shards, which may not be mounted anywhere during a cold
start - so a first pass runs once the node is serving.

It then repeats on the sweep interval until it converges. Which units a node
owns is still settling while a rollout is in flight, so a pass that runs then
can legitimately see nothing, and a once-only pass would never run again. Each
pass reports what it moved even when that is nothing, because a drain that is
silent when idle cannot be told from one that was never wired.

Each conversion is ONE transaction. The two families co-shard on the slug, so
the artifact is written and the legacy row deleted together: the directory is
never in both places (listed twice, charged twice) and never in neither (lost).
The charged size carries over verbatim - a migration must not re-price what the
owner is already paying.

**Only a directory's own identity may supersede it**, checked inside that
transaction against the legacy row's recorded owner rather than trusted from the
caller. Every caller does check first, so this refuses nothing they would ask
for; it is stated here because the shape a caller that forgot would take is slug
takeover - one identity's artifact standing where another's directory was.

A directory is written through the SAME insert a document uses: the caller
supplies a manifest, which is carried into the stored descriptor verbatim. A
caller that supplies none leaves it empty and the insert synthesizes the
one-entry form from the flat fields, so a document needs to know nothing about
manifests. There is no site-specific write path.

One **ArtifactRepo** persists, gets and deletes artifacts on every metadata
backend. There is no second repository, no second enumeration index, no second
quota scan, and no second entry in the durable-intent vocabulary. Quota is the
stored-byte total across an artifact's non-deleted versions - the same rule
for one file or two hundred.

### Serving a directory

One head read answers every request. The head carries the served version's
whole descriptor including its manifest, so resolving a request path is a
manifest lookup on a value already in hand - not a head read followed by a
version read, and not a site lookup followed by a paste lookup.

**The shape is DECLARED, never inferred.** An artifact whose kind is `site` is
a directory; anything else is a document. Counting manifest entries would get a
one-file directory wrong, serving it rendered instead of handing back its
bytes.

A document answers only at its own URL. A deeper path under it is a 404: paths
inside an artifact are a directory's affair.

Lookup resolves either shape. A document keys its single entry at `/`, having
no filename to be known by; a directory keys files by path and answers `/` with
its index. The root lookup checks `/` first and then `index.html`, so one
function serves both.

A slug with no artifact falls back to a legacy site row, the separate family
that predates the unified model. That fallback is what the migration removes.

### SPA fallback (route vs. asset)

A built single-page app (React Router, Vue Router, SvelteKit in SPA
mode) uses client-side routes like `/about` or `/users/123` that are NOT
real files on disk. Landing at `/` works - the server serves the root
`index.html`, the bundle boots, and the router takes over. But a DIRECT
link to `/about`, or a REFRESH while on `/about`, hits the server for a
path with no manifest entry. Without a fallback the server 404s and the
app never loads.

The fix: when a request misses the manifest (it is neither a file nor a
directory index), the server serves the site's **root `index.html`** so
the SPA's JS loads and its router can render the route client-side -
*unless* the path looks like a real, missing static asset, in which case
it stays a **404**. Distinguishing the two is a pure, I/O-free decision
on the request path's last segment:

- **Looks like a ROUTE -> serve root `index.html` (HTTP 200).** The last
  path segment has **no extension** (`/about`, `/users/123`) or an
  **`.html` extension** (`/about.html` for a pre-rendered route that the
  build did not emit as a file). These are how client-side routers spell
  locations.
- **Looks like a missing ASSET -> 404.** The last path segment has a
  known **static-asset extension** (`.js`, `.mjs`, `.css`, `.json`,
  `.map`, `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.avif`, `.svg`,
  `.ico`, `.woff`, `.woff2`, `.ttf`, `.otf`, `.eot`, `.wasm`, `.xml`,
  `.txt`, `.pdf`, `.webmanifest`, media such as `.mp4` / `.webm` /
  `.mp3`, pre-compressed `.gz` / `.br`, ...). A bundle that requests
  `/assets/app-deadbeef.js` and gets back `index.html` with a `200` and
  a `text/html` content-type would be a silent, confusing failure (the
  browser tries to execute HTML as a script); a clean `404` is the
  correct, debuggable answer for a genuinely-absent asset.

The heuristic is **extension-based on the last segment only**, never on
intermediate path components, so `/users/123/edit` (no trailing
extension) is a route and `/img/logo.png` (asset extension) is an asset.
An unknown extension (one not in the asset set and not `.html`) is
treated as a route and gets the `index.html` fallback - the asset set is
the deny-list; everything else falls back. The root `index.html` served
by the fallback carries the **same sandbox headers, cache posture, and
`200` status** as serving `index.html` directly; only the request path
differs.

The fallback is **default-on for every site**, not an opt-in flag. Two
reasons. First, the upload pipe is flagless by design
(`tar czf - site/ | ssh hostthis.dev` carries no filename and no
options), so there is no clean place for a user to signal opt-in at
upload time; a per-site flag would need a new column, new
deploy-time plumbing, and a new way to set it, for no UX win. Second,
the heuristic is **safe for plain static sites too**: a hand-written
multi-page site never requests a no-extension/`.html` path that isn't a
real file during normal navigation (its links point at real `.html`
files or real directories, which the manifest lookup already resolves),
and any asset it does request still 404s correctly when absent. So
default-on costs a plain static site nothing and saves every SPA the
broken-on-refresh experience. If a future need for opt-OUT appears
(e.g. a site that wants hard 404s on unknown routes), it can be added as
a flag then; until a concrete second case shows up, the simpler
default-on shape wins.

Content-type is derived purely from the path's extension (an I/O-free
domain decision). An unknown extension is served as
`application/octet-stream` - never mislabeled as `text/html`, so an
unexpected file can't be coerced into running as script on the origin.
A `.md` / `.txt` file in a site is served raw as `text/plain` (NOT
rendered - Markdown rendering is the single-file paste path, where the
browser renders it client-side, not the site path).

Site reads carry the **same sandbox headers** as HTML paste reads
(`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
`Permissions-Policy: ...`) and the same cache posture. Files are served
**raw**: the site's own HTML/CSS/JS runs exactly as uploaded, secured
by per-subdomain origin isolation, not by sanitizing the bytes.

### Same security model as HTML pastes (not a new posture)

A static site introduces **no new security posture**. An HTML paste is
already served RAW (its JS runs) and is secured by **origin isolation**:
each paste gets its own subdomain, its own browser origin, and the same
response headers (see "HTML sandboxing"). Only the Markdown path is
sanitized, because Markdown is rendered server-side; raw HTML never is.

A static site is the SAME model: raw files, its own subdomain, the same
headers, the same origin isolation between sites and against the apex.
So this feature is not a new trust boundary - it is the existing
"raw HTML on an isolated origin" boundary applied to a directory of
files instead of one file. As with any hostthis URL, treat a site as
untrusted user content, the way you would a CodePen or a `github.io`
page. Path mode (`--mode path`, dev-only) collapses every site onto
the shared apex origin and breaks this isolation exactly as it does for
pastes; production runs subdomain mode.

### Reuse: identity, quota, versioning

Nothing about the product opinions changes for sites:

- **Identity** is the SSH key fingerprint, the same account a paste
  upload uses, gated by the same Sybil per-subnet admission.
- **Quota** counts the manifest's DEDUPED total blob size against the
  SAME per-identity cap (see "Limits → Per-identity quota"), using the
  **stored (post-zstd) COMPRESSED** size per distinct blob -
  `Manifest.CompressedDedupedSize()` - so a site is charged its real
  on-disk footprint, exactly as a paste is charged its compressed size.
  (Each entry keeps its uncompressed `Size` for display; the compressed
  size is the quota basis.) The decompression-bomb guard still aborts the
  untar on the UNCOMPRESSED running total (a memory/bomb bound, so the
  guard is at worst more conservative than the charge), so a site can
  never be persisted over-quota.
- **Versioning** reuses the paste-versioning shape where it is low-cost:
  a deploy to an OWNED site slug re-deploys the site in place (same slug,
  same URL, new immutable manifest), so rollback / history ride the
  existing machinery; otherwise a deploy lands as a fresh slug, matching
  whatever pastes do. Either way a deploy is ATOMIC - the new manifest
  only becomes the served one once every blob is written and the
  manifest is persisted; a half-uploaded site never serves.

#### Deploy to an existing site slug (re-deploy in place)

Piping a gzip-tar archive to a slug positional arg
(`tar czf - -C site . | ssh hostthis.dev <slug>`) re-deploys the SITE
at that slug, in place, when the slug names a site the connecting key
owns. The slug and its URL are unchanged: the same `<slug>` serves the
new content. This is the static-site analogue of "Upload (update an
existing slug)" for pastes - the format gate (gzip magic) decides
site-vs-paste, the slug decides new-vs-update.

- **Ownership-gated, no existence leak.** A slug that names a site
  owned by a DIFFERENT key, or that does not exist as a site at all,
  returns *not found* (exit 4) - byte-for-byte the SAME shape as any
  other not-found, so a non-owner cannot probe for which slugs exist
  or who owns them. This matches the paste-update ownership posture
  exactly (see "Upload (update an existing slug)").
- **Atomic replace.** The new manifest's blobs are all written first;
  then a single transaction swaps the `sites/<slug>` row (new manifest,
  new `DedupedSize`, refreshed `UpdatedAt`). The URL keeps serving the OLD
  manifest until that
  swap lands, and serves the new one immediately after; a half-finished
  re-deploy never serves a partial site.
- **Quota is the replace DELTA.** The owner is charged the new site's
  deduped bytes and credited the old site's deduped bytes in the SAME
  atomic check, so re-deploying a same-size site does not double-count,
  and a smaller re-deploy frees the difference. The per-identity cap is
  evaluated against `existing_owned - old_deduped + new_deduped`. The
  mid-untar decompression-bomb guard still bounds extraction against the
  remaining budget so an over-quota archive is rejected before any blob
  lands.

A re-deploy to an existing slug NEVER lands as a fresh slug: the slug is
the explicit target. The fresh-random-slug path is only the no-slug
create case. The `EnsureUnique` slug collision dance (the
random-slug retry loop) does NOT apply to a targeted re-deploy.

### Byte-identical validation harness

The static-site contract is "what you upload is what is served": every
file round-trips **byte-for-byte**, with the content-type its extension
implies; `/` and `/<dir>/` serve that directory's `index.html`; an
unmatched route serves the root `index.html` via the SPA fallback; and a
genuinely-missing asset 404s. That contract is pinned by a validation
harness that deploys **real, framework-built sites** through the SAME
archive pipeline an `ssh` tar upload hits.

The harness ships four committed site fixtures under
`testdata/sitefixtures/`:

- three **vite SPA** builds - React (`react-router-dom`), Vue
  (`vue-router`), and Svelte (`svelte-routing`) - each with a home route
  plus an `/about` (and `/users/:id`) client-side route that is NOT a real
  file, so the SPA fallback is exercised against a genuine framework
  bundle, and
- a **plain-static** demo (hand-written `index.html` + `about.html` +
  `css/app.css` + `js/app.js`, no framework, no build step) so the
  round-trip is also proven for a multi-page site whose second page is a
  real file served directly, never via the fallback.

For each fixture the harness tars the build output, deploys it through the
real `DeploySite` use case over a real repo + content-addressed
blob store, then fetches every built file back over the real HTTP serving
surface and asserts the served bytes are byte-identical to the fixture
file, the content-type matches the extension, `/` serves the root
`index.html`, the deep route serves the root `index.html` via the SPA
fallback (200, index bytes), and a missing asset (`/assets/nope.js`) 404s.

The committed build output is the **known-good snapshot**: CI byte-compares
against it and never runs `npm`. The demo SOURCE and a `SHA256SUMS`
manifest are committed alongside each `dist/`; `make rebuild-site-fixtures`
regenerates both from source (`npm ci` + `vite build`, deterministic
because vite content-hashes asset names), and a snapshot test fails loudly
if any committed fixture file drifts from its pinned hash. `node_modules`
is gitignored - the fixtures need no toolchain to validate, only to
regenerate.

## Rooms (app persistence)

Static sites can SHIP, but a static site has no backend: it can render,
not remember. **Rooms** add the missing piece - a small persistence tier
so a deployed static-site app can store and load state without an account
system and without any server-side app code. This is the first real cut
of the "A persistence API" bullet under "Future directions"; that bullet
called for a per-app KV store fronted by a thin HTTP layer over shale,
and this section makes the no-auth, capability-based form of it real.

The deliberately-scoped commitment for this tier: **a key-value store
keyed by an unguessable room UUID, with strict per-room isolation, served
under the deployed app's own subdomain.** No accounts, no JWT, no
WebSockets - those are later tiers (see "Scope fence" below). What ships
is enough to build a collaborative app with no signup: a when2meet, a
shared list, a poll, a retro board.

### The model: an app, a room, a namespace

Three nouns, in a strict containment hierarchy:

- An **app** is a deployed static site (the "Static site archives"
  feature above) or a paste. It is identified by its **slug** - the same
  8-char slug the site/paste is served at - so `<slug>.hostthis.dev` is
  both where the app's files live and where its rooms API is served. An
  app's identity is that slug; there is no second registration step. There
  IS, however, an existence requirement: the slug must name a live (non
  site or paste. `POST /api/rooms` against a slug that names no
  live app is a **404**, so rooms can only ever be created under a slug an
  operator-facing upload actually provisioned. This ties the per-app
  caps (creation rate limit + aggregate byte cap) to a finite, provisioned
  set of apps: an attacker cannot rotate through the ~10^12 well-formed
  slug space to mint a fresh per-app budget under each one, because almost
  all of those slugs name nothing. The read/write verbs do NOT repeat this
  check - a room only exists under a slug that passed it at creation time,
  and the per-room UUID is the access capability from there on.
- A **room** is a `(app, UUIDv4, KV namespace)` triple created under an
  app. The UUIDv4 is minted server-side on creation and is the room's
  **capability**: holding it grants full read / write / delete to that
  room's data, and nothing else grants it. UUIDv4 is 122 bits of
  randomness, computationally infeasible to guess, so a room is private to
  whoever has the link - exactly the same "the identifier IS the secret"
  property the 8-char paste slug already relies on, scaled up to a UUID
  because room URLs are shared more widely and held longer than a paste
  link.
- A **namespace** is the room's flat key-value space. A value is a small
  opaque blob (JSON or bytes the app chose); a key is an app-chosen
  string. hostthis never parses the value - it is app STATE, stored and
  returned verbatim.

There is no login, no password, no per-user account anywhere in this
tier. Possession of the room UUID is the whole access model.

**Collaborative on refresh.** Because a room is one shared namespace keyed
by `(app, room-uuid)` and every participant addresses that same namespace,
two participants who hold the same room link see each other's writes on
their next read: A writes a value, B scans the room (its "join") and
observes it, B writes back, and A sees that on its next scan. This is the
consistency model the KV verbs ship - request/response KV, so the
propagation is "on the next read," not pushed by the KV path itself. An
app that wants near-real-time either re-scans on an interval OR opens the
room's WebSocket relay (see "Real-time room relay (WebSocket)" below),
which pushes one participant's message to the others live. The names
participants attach to their writes are cosmetic
attribution, not access control (see "In-room identity" below): any holder
of the UUID can write under any key.

### Strict room isolation (the security property)

Every value is namespaced by the triple `(app-slug, room-uuid, key)`. The
isolation guarantees that fall out of that key shape:

- **Cross-room**: one room's UUID can never read or write another room's
  data, even within the same app. The UUID is part of the key, so a
  request carrying room A's UUID can only ever address keys under room A.
- **Cross-app**: one app's rooms are separate from another app's. The app
  slug is the outermost key segment, so even an identical room-UUID-shaped
  string under a different app addresses a different keyspace. (Room UUIDs
  are unique in practice, but the app segment makes the isolation
  structural, not probabilistic.)
- **Existence is not leaked on the per-key path**: a *per-key* request
  (`GET`/`PUT`/`DELETE /api/rooms/<uuid>/<key>`) to a
  well-formed-but-nonexistent room UUID returns **404**, the same shape as
  a request for a missing key in a real room, so a per-key probe cannot
  distinguish "no such room" from "no such key in this room." This mirrors
  the paste / site rule where a cross-owner read surfaces as not-found
  rather than forbidden. The *whole-room scan* (`GET /api/rooms/<uuid>`)
  does draw a 200-vs-404 line: an existing-but-empty room scans to `200 {}`
  while a nonexistent room scans to `404`. That distinction is an
  intentional existence signal scoped to a party who *already holds* the
  122-bit UUID - it is not a probing oracle, since guessing a live UUID is
  computationally infeasible and the per-key path (the only surface a
  brute-force scan would hammer) never leaks the distinction. A holder of a
  real UUID learning "this room exists but is empty" reveals nothing they
  could not already write into existence.

This is the same posture as the rest of hostthis: an unguessable
identifier is the capability, and the storage layer enforces the namespace
boundary so a forged or guessed identifier addresses nothing.

### In-room identity is the APP's concern, not hostthis's

hostthis does **not** authenticate participants in a room. There is no
notion of "user A" versus "user B" at the storage layer - only "whoever
holds the room UUID." If an app wants participant names (a when2meet needs
to label availability by person; a retro board needs to attribute cards),
the app stores those names AS room data: a `participants` key, or
per-participant keys like `participant/<browser-id>`. The browser
generates or types the name, keeps it in `localStorage`, and writes it
into the room like any other value.

This is **cosmetic attribution, not access control**. Anyone with the room
UUID can write under any participant key, so the names are a display
convenience, not an identity boundary. That is the correct trade for this
tier: the room UUID is the access boundary, and finer-grained per-user
access control (one participant cannot overwrite another's record) is the
job of the LATER auth tier (see "Scope fence"), which adds verifiable
end-user identity on top. Until then, a room is a shared space where the
link is the key, exactly like a shared Google Doc link.

### The HTTP API

Rooms are served by the same HTTP surface that serves pastes and sites,
under the app's own subdomain (`<app-slug>.hostthis.dev`), at the
reserved `/api/rooms` path prefix. Because the app and its API share an
origin, the app's own JavaScript can call the API with same-origin fetch
and no CORS dance. The `/api/` prefix is carved out of the site's path
space: a site's manifest lookup never serves a file at `/api/rooms/...`,
so the API path and the static-file path do not collide. (In dev path
mode the same routes live under `<apex>/p/<app-slug>/api/rooms/...`.)

```
POST   /api/rooms                  mint a UUIDv4, create the room, return { "id": "<uuid>" }
GET    /api/rooms/<uuid>           list/scan every key+value pair in the room (load full state on join)
GET    /api/rooms/<uuid>/<key>     read one value (404 if absent)
PUT    /api/rooms/<uuid>/<key>     write one value (request body is the value)
DELETE /api/rooms/<uuid>/<key>     delete one value
```

Behavior, endpoint by endpoint:

- **POST /api/rooms** generates a fresh UUIDv4, creates an empty room
  under the requesting app slug, and returns `{"id": "<uuid>"}` (HTTP
  201). The app stores that id (in the URL hash, in `localStorage`, in a
  share link) and uses it for every subsequent call. Creation is subject
  to the room-creation rate limit (see "Quota and abuse").
- **GET /api/rooms/<uuid>** scans and returns every key+value pair in the
  room as a single JSON object, so an app loads the full room state in one
  request on join. Each value is embedded as raw JSON when the stored bytes
  parse as JSON - so a value the app PUT as a JSON object comes back as a
  NESTED object, not a JSON-string of escaped text - and as a JSON string of
  the verbatim bytes otherwise. (The WebSocket `snapshot` and `put` frames
  encode values identically; the single-key GET below instead returns the raw
  bytes.) A well-formed UUID that names no room returns **404**.
- **GET /api/rooms/<uuid>/<key>** returns the stored value verbatim with a
  conservative content type (`application/octet-stream` unless the app
  stored a recognizable JSON value, which is served `application/json`). A
  missing key returns **404**; a missing room returns the same **404**.
- **PUT /api/rooms/<uuid>/<key>** writes the request body as the value for
  `<key>`, creating or overwriting. Subject to the per-room data cap (see
  "Quota and abuse"); a write that would push the room over the cap is
  rejected (HTTP 413) and the prior value is left intact. A successful
  write moves the room's `UpdatedAt`.
- **DELETE /api/rooms/<uuid>/<key>** removes the value. Idempotent:
  deleting an absent key is a success (the post-condition - "the key is
  gone" - holds either way). A delete also moves `UpdatedAt`,
  since it is a write to the room.

A malformed UUID (not a parseable UUIDv4) is a **400**, distinct from the
**404** a well-formed-but-nonexistent room gets: a 400 says "this is not a
room id at all," a 404 says "no room here," and neither confirms the
existence of any specific room.

### Storage: a room-namespaced KV over the metadata backend

Room data is small JSON/bytes blobs - app STATE, not files - so it lives
in the **metadata backend** hostthis already runs (the configurable
metadata store: the local engine for single-host, shale for the
object-store-backed and horizontally-scaled deploys), NOT in the
content-addressed BlobStore. The BlobStore is for the larger,
dedupe-worthy file bytes of pastes and sites; room values are small,
mutable, and per-room, so they belong with the metadata. Large blobs are
explicitly out of scope for rooms - an app that needs to host files uses
the archive/site feature, not a room value.

The implementation follows the existing repo-behind-a-service-interface
pattern exactly the way the paste repo and the shale `ShaleRepo` do:

- A new domain aggregate, **`Room`** (slug-of-the-owning-app + room id +
  the key-value namespace as a value object), lives in `internal/domain`
  alongside `Paste` and `Site` and imports nothing from infrastructure.
  The namespace is a pure value object: putting, getting, deleting, and
  scanning keys, plus computing the room's total byte size and key count
  for the cap check, are all I/O-free domain operations.
- A new **`RoomKVRepo`** in `internal/storage` persists rooms with
  namespaced keys and is queried by `(app-slug, room-uuid)`, behind a
  small service-layer interface (a `RoomRepo` declared in
  `internal/service`, the same way `PasteRepo` / `PasteAdmin` /
  `SweepRepo` / `KeyGateRepo` are; the sweep-side view is `SweepRooms`).
  Both metadata backends implement it - **local** (single-host) and
  **shale** (horizontally-scaled cluster) - so the `/api/rooms` surface runs
  on every backend hostthis can be deployed on, including the shale backend
  prod runs. The
  domain, HTTP, and service layers stay unaware of which backend is wired.
- Every backend models a room as a set of key families co-located in the
  one metadata keyspace, so a room read or write is a single transaction
  or prefix scan. Every read and write is scoped by the
  `(app_slug, room_id)` pair, so the namespace boundary is enforced by the
  KEY, not by a filter a caller could forget. The full layout,
  the cap + isolation + rate-limit + TTL mapping onto KV ops, and the
  fixed-width TTL timestamp are specified under **"Room storage on the
  shale backend"** below (near the metadata-backend section,
  alongside the parallel static-site layout). The single-writer backends
  store the same logical rows; the observable contract is identical across
  backends, the way it is for the paste and site families, and the
  backend-agnostic conformance suite pins them identical.

The slug-must-name-a-live-app **existence requirement** is enforced at the
HTTP layer (it reads the site + paste readers the router already holds),
NOT inside the room repo, so it holds identically across backends without
the room repo needing a separate existence reader: room creation 404s a
slug that names no live site or paste on every backend.

### Quota and abuse

A writable, no-auth, public API needs the abuse surface bounded as
deliberately as the paste upload path is. Four controls, each with a
concrete default flagged as a starting point (tunable as real usage
informs them):

- **Room-creation rate limit.** `POST /api/rooms` is gated per source IP
  AND per app: **default 60 rooms per IP per hour** and **default 300
  rooms per app per hour**, so a script cannot spam rooms into existence.
  The per-IP gate reuses the subnet-derivation the SSH Sybil gate already
  computes (`/24` for IPv4, `/48` for IPv6); the per-app gate bounds a
  single popular app's blast radius. Over the limit returns **429** with a
  `Retry-After`. (The HTTP read/write verbs - GET / PUT / DELETE on an
  existing room - are NOT room-creation and ride the per-room data cap
  below rather than this gate; a reverse-proxy per-IP request limit is the
  right layer for raw request-rate abuse, the same division of labor the
  paste threat model already documents.)

  **Trusted source-IP derivation.** The per-IP bucket is derived from the
  TCP `RemoteAddr` by default, NOT from any client-supplied header. A
  client-controlled `X-Forwarded-For` is ignored unless the operator
  explicitly opts in by setting `HOSTTHIS_HTTP_TRUST_XFF=true` - the same
  discipline the SSH side uses for `HOSTTHIS_SSH_PROXY_PROTOCOL`. Trusting
  a client header by default is a rate-limit bypass: an attacker would set
  a fresh `X-Forwarded-For` per `POST` and land in a new per-IP bucket each
  time. When `HOSTTHIS_HTTP_TRUST_XFF=true` is set (because hostthis sits
  behind a reverse proxy that appends the real client IP), the gate reads
  the **right-most** value of the `X-Forwarded-For` list - the hop the
  trusted proxy itself recorded, which the client cannot forge past the
  proxy - not the left-most value, which is fully attacker-controlled. An
  operator who terminates TLS at a proxy MUST set this flag (otherwise
  every request's `RemoteAddr` is the proxy's own IP and the per-IP gate
  collapses to a single global bucket); an operator with hostthis directly
  on the public internet MUST leave it unset.
- **Per-room data cap.** A single room cannot store unbounded data:
  **default 256 KiB total value bytes** and **default 256 keys** per room.
  A `PUT` that would push the room past either cap is rejected (**413**)
  and the prior state is unchanged. The cap is sized for app STATE
  (a poll's votes, a retro board's cards, a when2meet's availability grid)
  - generous for those, tight enough that a room cannot be turned into a
  free file host.
- **Per-app aggregate.** A popular app's rooms in aggregate are bounded by
  **default 64 MiB of room data per app** (and the room-creation rate
  limit caps the growth rate). Past the per-app aggregate, new room
  creation and new writes for that app return **507** until the app deletes
  rooms or values. This is the room-tier analogue of the per-identity paste
  quota: it stops one app from consuming the whole service. It is flagged
  as a starting default - an operator running many apps may want it lower,
  a single-app operator higher. It
  differs from the per-IDENTITY paste/site quota, which the local + shaledb
  backends free at READ time; the per-app room aggregate is uniformly
  sweep-time so the cap behaves identically across local and
  shale.
- **Durable total-bytes ceiling.** Room data does NOT carry its own
  service-wide byte scan. Rooms hold no blobs (a room value lives entirely
  in the metadata backend, not the content-addressed `BlobStore`), so a
  room write touches no object-store quota directly and is bounded by the
  per-room and per-app caps above. The service's durable total-bytes
  ceiling is enforced at the object store for the blob-holding kinds
  (pastes + sites); see "Limits → Durable total-bytes ceiling: an
  object-store quota". The per-app aggregate is therefore the primary
  structural bound on a room's growth, and a reverse-proxy per-IP rate
  limit remains the appropriate layer for raw request-rate abuse, exactly
  as for the paste path.

### Scope fence

This tier is **KV persistence plus a real-time relay**. The KV verbs
above are the durable surface; the WebSocket relay below adds live push
on top of the SAME room. Two things are still explicitly NOT in it, each
a later tier with its own design:

- **NO per-user auth / JWT / "Sign in with hostthis."** The room UUID IS
  the access capability; there are no accounts, no roles, no token
  verification in this tier. The "A persistence API" future-directions
  bullet describes a richer end-user identity spectrum (capability token,
  browser keypair, JWT-verifying resource server with a turnkey-or-BYO
  issuer) that lets an app enforce `request.user == resource.owner`
  rules - that is a deliberately-separate LATER tier layered on top of
  this one, not a prerequisite for it. Rooms ship the no-auth, capability
  form first because it unlocks real apps with zero account machinery.
- **NO server-side app functions.** hostthis stores what the room writes;
  it never runs app logic. This is not a FaaS, and the data-integrity
  problem ("is this score real?") stays unsolvable client-side, exactly as
  the future-directions bullet notes - an app either accepts that
  (fine for a casual leaderboard or a shared list) or is not a fit.

Rooms are **additive**: they introduce no change to the paste or the
site/archive read-and-write behavior. A slug that owns a site keeps
serving its files; the `/api/rooms` prefix is the only new surface on that
subdomain. A live paste's slug is also a valid app to host rooms under (a
paste is an app per the "an app" definition above); a slug that names no
live site or paste names no app, so room creation under it is a 404.

## Real-time room relay (WebSocket)

The KV verbs above make a room **collaborative on refresh**: a participant
sees another's writes on the next read. That is enough for a poll or a
shared list, but not for the headline app this tier exists to unlock - a
**when2meet** where everyone paints a calendar and watches each other's
availability fill in LIVE. Polling `GET /api/rooms/<uuid>` on an interval
is the workaround and it is bad: too slow to feel live, too chatty to
scale, and it never catches the moment between two polls.

The real-time relay closes that gap with **one generic per-room WebSocket
endpoint** layered on the SAME room. hostthis runs **no app-specific
server logic**: it is a dumb live channel. A message from one client in a
room is fanned out, verbatim, to every OTHER client in that same room.
The clients hold all the app logic (the when2meet's grid, the retro
board's cards); hostthis is the live wire plus the durable backing store.
This is the deliberately-client-authoritative model the "A persistence
API" future-directions bullet describes ("no reactive subscriptions" was
the line drawn against a FaaS - a generic broadcast relay is NOT app code,
so it sits on the right side of that line), made real for the no-auth
capability tier.

### The endpoint and the room-UUID capability

The relay is served by the same HTTP surface that serves the KV verbs,
under the app's own subdomain, at a reserved `/api/rooms/<uuid>/ws` path:

```
GET (Upgrade: websocket)   /api/rooms/<uuid>/ws
```

- Production (subdomain mode): `wss://<app-slug>.hostthis.dev/api/rooms/<uuid>/ws`.
- Dev (path mode): `ws://<apex>/p/<app-slug>/api/rooms/<uuid>/ws`.

The path lives under the existing `/api/rooms` carve-out, so it is never
shadowed by a manifest file, and `/ws` is a reserved trailing segment a
room KEY can never name (a key path is `/api/rooms/<uuid>/<key>`; the
relay claims `<key> == "ws"` for the upgrade, the one key the KV verbs do
not serve as data). Because the relay shares the app's origin, the app's
own JavaScript opens it same-origin with no CORS dance.

**The room UUID is the entire access model, exactly as it is for the KV
verbs.** Holding the UUID lets you join that room's relay; nothing else
grants it. On upgrade the server validates two things and rejects
otherwise, BEFORE completing the WebSocket handshake:

- **The app slug names a LIVE app.** The same existence requirement room
  creation rides: the slug must name a site or paste (checked
  via the site + paste readers the router already holds). An upgrade under
  an unprovisioned slug is refused, so the relay cannot be opened under one
  of the ~10^12 well-formed-but-empty slugs. This ties the relay's per-app
  connection caps to the same finite, provisioned set of apps the KV caps
  are tied to.
- **The UUID is a canonical UUIDv4 that names an existing room.** A
  malformed id is refused at the boundary (the same `ParseRoomID` the KV
  path uses); a well-formed-but-nonexistent room is refused too (a relay to
  a room that was never created has nothing to back its late-join snapshot).
  A holder of a real UUID is the only party who can open the channel.

A rejected upgrade is refused with a normal HTTP status (not a 101), so a
client's WebSocket open fails cleanly: a malformed UUID is a **400**, an
unknown app slug or nonexistent room is a **404** (the
existence-not-leaked shape, same as the KV path), an over-limit room or
app is a **429**, and a non-Upgrade request to the `/ws` path is a **426
Upgrade Required**. No relay is ever stood up for a request that fails
validation, so a forged or guessed id reaches no hub.

### Strict isolation: a connection joins exactly one room

A WebSocket connection is bound at upgrade time to the **one** room whose
`(app-slug, room-uuid)` it connected with, and it can never affect any
other. The isolation is the same structural property the KV key shape
gives the durable tier, lifted to the live tier:

- **A message never crosses to another room.** The connection is
  registered in exactly one per-room hub (keyed by `(app-slug, room-uuid)`);
  a broadcast is fanned out only to the other members of THAT hub. There is
  no cross-hub path - a client cannot address, subscribe to, or leak into
  another room even within the same app.
- **A message never crosses to another app.** The hub key's outermost
  segment is the app slug, so an identical room-UUID-shaped string under a
  different app resolves to a different hub. One app's live traffic is
  disjoint from another's, structurally, not by a filter a handler could
  forget.
- **The relay carries no cross-room addressing in its payload.** hostthis
  does not interpret the message, so there is no "target room" field a
  client could set; the connection's bound room is the only destination,
  fixed at upgrade and immutable for the connection's life.

hostthis does not parse the relayed payload at all: it is opaque bytes /
JSON the app chose, fanned out verbatim. The only server-side
interpretation is the connection-lifecycle control frames (ping/pong, the
late-join snapshot framing, and the optional durable-write convention
below), never the app's message contents.

### Persistence and late-join: the KV is the durable state, the relay is the live delta

This is the crux of "no gap, no dup." The relay integrates with the room
KV (the durable tier specified above) so a client that JOINS - including a
client that reloaded the page mid-session - is caught up to the current
state and then sees every subsequent change exactly once.

**The model: snapshot-then-stream, ordered by the room's durable
sequence.** Every durable mutation (a committed PUT or DELETE) is
assigned a **dense per-room sequence number at commit** - `seq` - by the
storage backend, inside the same transaction that commits the write (the
assignment mechanics are specified in "Multi-pod relay" below). The
snapshot and every live mirror frame carry it, and it - not any lock -
is what makes late-join correct. On a successful upgrade the server:

1. **Registers the connection in the hub FIRST**, so every mirror frame
   broadcast from that instant on is queued for the joiner. The queue
   holds a reserved first-frame slot the snapshot will fill; the writer
   sends nothing until the snapshot is in it, so the snapshot is still
   the first frame ON THE WIRE even though live frames may already be
   buffered behind it.
2. **Then reads the room KV snapshot** (`RoomRepo.ScanRoom`) and sends it
   as that first frame, tagged as the snapshot control envelope and
   stamped with the exact sequence number `S` its state reflects. The
   envelope's `state` object is the same key -> value object
   `GET /api/rooms/<uuid>` returns, so the same client code that loads
   state on a cold HTTP start consumes it.
3. **The live stream follows.** The client applies a mirror frame with
   seq > S and discards a frame with seq <= S (its effect is already in
   the snapshot).

Register-then-snapshot plus the sequence is what makes late-join
correct, and unlike the earlier hub-lock formulation it holds across
pods (the full cross-pod derivation, and the reasons the design moved
off the lock, are in "Multi-pod relay" below):

- **No gap.** A mirror frame for seq N is only ever broadcast AFTER
  mutation N durably committed, and the snapshot read observes every
  commit that precedes its start. So a frame the joiner MISSED (one
  broadcast before it registered) came from a commit that landed before
  the snapshot read began, is therefore IN the snapshot, and has
  seq <= S; a frame from any later commit finds the connection already
  registered and is delivered live. Every change is in the snapshot or
  in the stream.
- **No dup.** A change CAN arrive in both (a frame broadcast inside the
  join window is also caught by the snapshot read that follows the
  register). The sequence de-duplicates it: the client discards every
  frame with seq <= S, so the change is APPLIED exactly once. hostthis
  is payload-opaque and makes no idempotency assumption about the app's
  bytes (an app that treats a live mirror as a delta / increment /
  append corrupts on a double-apply), which is why the discard rule is
  keyed on the exact snapshot sequence, never on a heuristic.
- **Gaps are detectable.** The sequence is DENSE - exactly +1 per
  committed mutation, a counter, not a timestamp - so a subscriber that
  holds seq N and receives seq N+2 KNOWS a frame is missing and resyncs
  (the splice contract in "Multi-pod relay"). Live delivery is
  best-effort; DETECTION is guaranteed by the data.

Because correctness rides the sequence, the durable commit does NOT run
under the room's hub lock: the hub lock guards the connection set only,
and a room's live broadcasts never stall behind a durable write's
object-storage round trip. (An earlier single-pod formulation held the
hub lock across the commit + mirror as one critical section to get the
no-dup guarantee; the sequence carries that guarantee now - and carries
it cross-pod, which no pod-local lock can - so the lock shrinks back to
a pure membership mutex.) The durable path remains the LOW-frequency
one (a finished availability cell, a placed card, a final vote); the
high-frequency live texture (cursors, strokes-in-progress at 60 Hz)
rides ephemeral raw relay frames, which carry no seq and never touch
the durable tier. An app with high-frequency DURABLE writes should
batch its commits or move motion to ephemeral frames.

**What persists vs what is ephemeral.** The relay separates two message
flavors, and this is the abuse + correctness lever:

- **Ephemeral (broadcast only, NOT persisted).** High-frequency live
  signals: a cursor position, a stroke-in-progress, a "user is dragging
  the selection." These are fanned out to peers and never written to the
  KV. They are the live texture of the session; a client that joins later
  does not need them (they are stale the instant they are sent), so they
  cost zero KV writes. This is what keeps the relay from forcing a durable
  write per frame at 60 Hz - the thing that would make the KV the
  bottleneck and blow the per-room cap in seconds.
- **Durable (broadcast AND persisted to the KV).** The committed state: a
  finished availability cell, a placed retro card, a final vote. The
  durable set is what a late joiner must see, so it must survive a full
  disconnect + reload, so it lands in the room KV via the SAME
  `RoomRepo.PutValue` / `DeleteValue` the HTTP verbs use - which means it
  rides the SAME per-room and per-app caps and resets the
  SAME clock. A durable mutation is therefore consistent whether
  it arrives over the relay or over `PUT /api/rooms/<uuid>/<key>`: both
  funnel through the one room repo, so the snapshot a future joiner reads
  reflects it identically.

**How a client signals which flavor a message is.** hostthis stays generic
by NOT inventing an app protocol, but it must know which messages to
persist. The chosen convention: the durable path is the EXISTING HTTP KV
verb, and the relay is broadcast-only by default. An app that wants a
change to be both durable AND pushed live does the durable write with `PUT
/api/rooms/<uuid>/<key>` (which the server commits, then mirrors to the
room's connected clients on EVERY pod as a live message tagged with the
key and the mutation's room sequence - the sequence, not a lock, is what
keeps a join racing the PUT from double-applying or missing it; see
above), and uses raw relay frames only for ephemeral signals. This keeps the relay payload-opaque (no reserved fields in the
app's bytes), makes the durable write go through the one audited cap-
checked path, and gives the live fan-out of a committed change for free.

  Why route durable writes through the HTTP verb rather than a
  message-type tag inside the relayed bytes: a tag inside the payload would
  force hostthis to parse the app's message (breaking the payload-opaque
  property and the isolation argument that rests on it), and it would
  duplicate the cap-check logic on a second code path. A
  `PUT` that the server mirrors to the hub reuses the entire durable path
  unchanged and adds only the fan-out. The relay's own frames stay pure
  ephemeral broadcast - the server never persists a raw relay frame, so a
  flood of relay frames can never grow the durable store (see "Limits").
  An app whose every change is durable simply does every change as a `PUT`
  and uses the relay only to LOWER its latency (the live mirror), or not at
  all; an app with a lot of ephemeral motion uses raw relay frames for the
  motion and `PUT`s only the committed deltas.

**Reconnect is just join again.** A reconnecting client - the canonical
case is a page reload or a backgrounded PWA resuming - opens a fresh
WebSocket, gets a fresh snapshot-then-stream, and is caught up with no gap
and no dup by the exact same mechanism as a first-time joiner. The server
holds no per-client durable session state across a disconnect: a
connection is not assumed unique or permanent, and a client may have zero,
one, or several live connections to the same room at once (two tabs).
Re-syncing from the KV snapshot on every (re)connect is what makes the
relay reconnect-friendly - there is deliberately no incremental "replay
me everything since sequence N" protocol to get wrong. The room sequence
orders, de-duplicates, and detects loss; it never drives a replay (the
server retains no per-room frame history). The durable KV is always the
authoritative full state and a fresh snapshot is always correct.

### Connection lifecycle (the finicky core)

This is what separates a relay that feels solid from one that drops
messages and leaks goroutines. The server side and the client side each
own four pieces; they interlock.

**Server side.**

- **Per-room hub.** A hub is the in-memory registry for one room's live
  connections, keyed by `(app-slug, room-uuid)`. It owns the set of
  connected clients, the register / unregister path, and the broadcast
  fan-out. A hub is created lazily on the first connection to a room and
  torn down when its last connection leaves (no idle empty hubs linger).
  The hub registry (the map of room-key -> hub) and each hub's client set
  are the two in-memory structures, and BOTH are bounded (see "Limits").
  These are two separate locks - the global registry lock (the hub map plus
  the per-app + total-rooms counters) and each hub's own lock (its client
  set) - and the per-room isolation is a LATENCY property as well as a
  correctness one: an upgrade's admission does the global-lock work (the
  per-app / total-rooms cap check, the lazy hub-create) and then RELEASES
  the global lock BEFORE it takes the target hub's lock for the per-room cap
  check + register. So a join to one room never holds the global lock while
  waiting on another room's hub lock. Neither lock is ever held across
  I/O: the durable commit runs outside the hub lock (the room sequence,
  not the lock, carries the no-gap / no-dup guarantee - see "Persistence
  and late-join"), and the join's snapshot read runs after the register,
  also outside it. A hub lock is held only for map mutation and the
  wait-free buffer enqueues of a fan-out, so one room's contention stays
  local to that room and no room's broadcasts stall behind storage.

  Decoupling admission from the global lock opens a window: between an
  admission reserving its per-app slot (and releasing the global lock) and
  registering its reservation into the hub, the hub is momentarily empty
  from the perspective of any other goroutine. A concurrent leave that
  empties the hub fires its empty-hub teardown in that window and would
  remove the very hub the admission is about to register into, orphaning
  the registration (it lands in a hub no longer in the map, so it misses
  live frames) and leaking its per-app slot (a later release finds no
  hub and skips the decrement). A PENDING-ADMIT guard closes this: the
  registry tracks a per-room count of in-flight admissions, incremented
  under the global lock when the slot is reserved (before the lock is
  released) and decremented under it once the register has run. Every
  hub-removal path - the empty-hub teardown the last leave fires, the
  laggard-drop path that empties a hub, and an admission's own
  per-room-cap rollback - removes a hub only when it is empty AND has zero
  in-flight admissions, so a hub an admission is about to register into is
  never torn out. The guard keeps the admission decoupled (it still holds no
  global lock while taking the hub lock to register), so the per-room
  isolation above is preserved; it only narrows "the hub is idle" to also
  mean "no admission is mid-flight into it." (A durable write, for its
  part, never creates a hub: the mirror fan-out looks the hub up and a
  missing hub just means no local subscribers - the frame is dropped
  locally, exactly as a peer-received frame for a subscriber-less room
  is.)
- **Server heartbeat (ping/pong) to reap dead connections.** The server
  sends a WebSocket ping to each connection on a fixed interval and expects
  a pong back within a deadline; a connection that misses the pong deadline
  is considered dead and is closed and unregistered. This is what detects a
  client that vanished without a clean close (a killed PWA, a dropped
  mobile link, a yanked cable) - TCP alone can take minutes to notice, and
  an idle proxy will cut the connection silently. The server ping interval
  is chosen UNDER the proxy idle timeout (the relay runs behind traefik /
  nginx, whose idle defaults are 60-120 s), so the heartbeat also keeps a
  legitimately-quiet connection alive through the proxy. The server both
  SENDS its own pings AND tolerates client-initiated pings (responds with a
  pong, treated as a liveness no-op) - the client lifecycle below pings on
  its own ~25 s cadence and the server must not punish it for that.
- **Backpressure: a slow client must never block the room.** Each
  connection has a **bounded per-client send buffer**. The broadcast path
  writes to each connection's buffer and returns immediately; a dedicated
  per-connection writer goroutine drains the buffer to the socket. If a
  client is slow or stuck and its buffer is FULL when a broadcast tries to
  enqueue, the server does NOT block the broadcast waiting for that one
  client (head-of-line blocking the whole room on the slowest member) - it
  **drops that client**: closes the connection and unregisters it. A
  laggard is ejected, never tolerated at the cost of everyone else's
  latency. The bound is small (a handful of frames): a client that cannot
  keep up with a handful of buffered frames is not a viable live
  participant and is better off reconnecting (which re-syncs it from the
  KV snapshot cleanly). The broadcast is therefore wait-free with respect
  to any individual client. Dropping a laggard reclaims its connection
  accounting (the per-room hub slot AND the per-app aggregate counter) the
  same way a clean leave does - the drop path is a real disconnect, not a
  shortcut that forgets the counters, so a room that drops laggards under
  load does not slowly leak its per-app connection budget.
- **Clean disconnect handling.** Every connection close - clean client
  close, heartbeat-timeout reap, slow-client drop, server shutdown -
  unregisters the connection from its hub, decrements the per-app
  connection counter exactly once, and stops its reader and writer
  goroutines, with no leaked goroutine, no dangling map entry, and no
  leaked connection-count slot. Each disconnect decrements the per-app
  counter exactly once regardless of which path tore the connection down
  (a clean unregister and a backpressure drop must not BOTH decrement, and
  neither must SKIP it). The last connection leaving a room tears the hub
  down. Server shutdown closes all connections with a normal-closure status
  so clients reconnect on the client backoff schedule rather than hammering
  instantly.
- **Read bound.** The reader applies a max message size per inbound frame
  (see "Limits"); a frame over the cap closes the connection. Liveness is
  the heartbeat's job, not the reader's: the server's ping/pong loop is the
  sole reaper, and it cannot be starved (it runs in its own goroutine on a
  fixed ticker, independent of whether the reader is blocked on a quiet
  socket). A connection that goes silent is therefore reaped by the missed
  pong, so the reader needs no separate per-read deadline as a backstop -
  one reaper, sufficient, with no second timeout to keep consistent with the
  heartbeat window.

**Client side (the relay must SUPPORT this; the POC implements it).** The
server is built so the canonical 4-piece client lifecycle works against
it. This is the same lifecycle every production WebSocket client needs;
the relay's job is to not fight it:

1. **Heartbeat ping every ~25 s** (under the proxy idle default). The
   server treats a client ping as a liveness no-op and pongs it. A quiet
   connection stays alive across an idle network and a suspended PWA.
2. **Auto-reconnect with exponential backoff + jitter** on close. Start
   ~500 ms, double each attempt, cap ~30 s, jitter to avoid a thundering
   herd if many clients reconnect at once (a server restart drops every
   connection in a room simultaneously). The server is reconnect-friendly:
   a reconnecting client re-syncs from the KV snapshot, so backoff costs a
   little latency, never correctness.
3. **`visibilitychange` recovery.** When a tab / PWA returns to the
   foreground, force-reconnect immediately rather than waiting out the
   backoff - iOS aggressively suspends backgrounded WebSockets, and the
   snapshot-then-stream rejoin catches the client up on whatever it missed.
4. **Send buffer (client-side).** Queue actions submitted while
   disconnected (capped), flush them on the next reconnect after the
   snapshot handshake. Durable actions a client took offline are `PUT`s
   that retry on reconnect; ephemeral signals taken offline are simply
   dropped (they are stale).

The server makes no assumption that a connection is unique or permanent,
so all four client pieces are safe: a reconnect is a new join, a duplicate
connection from the same client is just another hub member, and a missed
heartbeat is reaped without corrupting room state (the durable state lives
in the KV, untouched by a connection dying).

### Limits and abuse posture

The relay is a new always-open, push-capable surface, so every in-memory
structure is bounded and the abuse posture is the room-UUID capability
plus these caps - **no new auth**, consistent with the rest of the tier.
Each limit has a concrete default flagged as a starting point (tunable as
real usage informs it):

- **Max concurrent connections per room** (default **64**). A room is a
  small collaborative session (a team's retro, a friend group's
  when2meet); past this, new upgrades to that room are refused **429**.
  This bounds one hub's client-set size and one room's fan-out cost (a
  broadcast is O(connections)).
- **Max concurrent connections per app** (default **1024**). Bounds the
  total live connections any one app's rooms hold open in aggregate, the
  live-tier analogue of the per-app aggregate byte cap. Past it, new
  upgrades under that app are refused **429**.
- **Cap on total active relay rooms** (a service-wide bound on the number
  of live hubs, default sized to the node's memory budget). Bounds the hub
  registry itself so the count of distinct live rooms cannot grow
  unbounded. Past it, an upgrade that would create a NEW hub is refused
  **503**; joins to already-live rooms still succeed.
- **Max message size per inbound frame** (default **32 KiB**). An app's
  live message is small (a cursor, a cell, a card); a frame over the cap
  closes the connection. This bounds per-frame memory and stops a single
  giant frame from being a memory-amplification vector. (It is independent
  of the per-room DURABLE byte cap, which the `PUT` path enforces; a relay
  frame is never persisted, so it is bounded for memory, not for storage.)
- **Per-connection send rate limit** (default a small frames-per-second
  ceiling, e.g. **120 msg/s**). A client that exceeds its inbound rate is
  throttled or dropped, so one hostile connection cannot saturate a room's
  fan-out (every inbound frame is multiplied by the room's connection
  count on the way out). This is the relay's analogue of the room-creation
  rate limit on the KV side.

The bounded per-client send buffer (the backpressure mechanism above) is
itself a per-connection memory bound. Together these cap connections,
rooms, per-frame bytes, in-flight buffered bytes, and message rate - every
axis a hostile client could push on. A reverse-proxy per-IP connection
limit remains the appropriate outer layer for raw connection-flood abuse,
exactly the division of labor the KV path documents for request-rate
abuse. Durable mutations made over the relay ride the EXISTING per-room
and per-app byte caps unchanged (they go through `PutValue`),
so the relay opens no new path to grow the durable store past its caps.

The relay adds NO state that survives a room's deletion: once a room's KV
is gone, any still-open connections to it are connections to a now-empty
room, and the next durable read returns an empty snapshot. Live hubs are pure in-memory
state, GC'd when the last connection leaves; they are never persisted and
never participate in the sweep.

### Multi-pod relay: broadcast fan-out ordered by a durable per-room sequence

A single-pod relay is complete on one process: every connection to a
room terminates on the pod, the room's hub IS the room, and a broadcast
reaches everyone. A multi-pod deploy (the sharded shale backend runs
several hostthisd pods behind one non-sticky ingress) breaks that
silently: two clients in the same room land on different pods, each
pod's in-memory hub sees only its own sockets, and a durable PUT
handled by pod A mirrors only to A's connections - with N pods and
random routing, roughly (N-1)/N of live mirrors never reach a given
client. The durable KV stays correct throughout (every pod routes reads
and writes through the one storage cluster); it is only the LIVE delta
that splits. This section is the cross-pod design.

**The shape: every pod fans out to every pod, and ordering rides the
data, not the topology.** A frame's origin pod broadcasts it locally and
publishes it to every peer pod, which broadcasts it to its own local
connections. That alone would be unordered and lossy (two pods' fan-outs
race; a peer can be down), so the durable stream is made correct by the
**per-room sequence**: a dense counter the storage backend assigns at
commit, carried on every mirror frame and on every snapshot. Subscribers
order by seq, de-duplicate by seq, and DETECT loss by seq (dense means a
hole is visible); a detected hole is healed by re-snapshotting. Delivery
is best-effort; correctness is a property of the data.

#### Rejected alternatives

- **Sticky-by-room (route a room's sockets to its shard owner) -
  rejected for deploy re-homing.** A rolling deploy of the sharded
  backend migrates shard ownership between pods mid-roll (the surge
  deploy hands every unit off with zero interruption). Connections
  pinned to the owner would re-home on every rollout and every ring
  change, turning routine deploys into room-wide reconnect storms, and
  the ingress would have to resolve slug -> current owner (a ring lookup
  no standard ingress does, racing the very handoffs it must follow).
  Sticky also caps a room at one pod's connection budget and makes that
  pod's death the whole room's outage. The clean co-location property it
  promised (the node that commits pushes the delta) is instead recovered
  by the sequence: ANY pod can commit, because the order is in the data.
- **A pub/sub backplane (Redis / NATS) - rejected as a second
  distributed system.** It decouples placement from fan-out, but adds a
  new deployment to run and secure, a per-message hop, and its own
  ordering surface, to solve a fan-out of N where N is a handful of
  pods. Direct peer fan-out costs O(pods) per frame and reuses transport
  the deploy already has. The recipient list is deliberately behind a
  port (see "The peer transport") so **interest-based fan-out** - publish
  a room's frames only to pods with live subscribers to that room - can
  replace "all peers" later as a pure optimization, with no protocol or
  contract change.

#### The per-room sequence: assignment at commit

The sequence lives ON the room's authoritative record
(`rooms/<app-slug>/<uuid>`), which every backend ALREADY rewrites inside
every durable mutation (the clock touch on PUT and DELETE). It
is a `uint64` starting at 0 for a fresh room; each committed mutation
assigns `seq = prior + 1` in the same transaction that commits the
value:

- **shale**: a `seq` field on the room record, incremented inside the
  existing single-shard `{app-slug}` CAS. The record is already in every
  mutation's CAS read-set (the strict per-room cap mechanism), so two
  concurrent writers to the same room - even to distinct keys - conflict
  on the record, the loser retries, and each commit observes the prior
  seq and writes exactly prior + 1. Density and uniqueness fall out of
  the same conflict that makes the per-room cap strict; no new race
  surface is added.

- **local**: the shale mechanism above; only the storage engine beneath
  the cluster differs.

`RoomRepo.PutValue` and `RoomRepo.DeleteValue` return the assigned seq
to the caller: the storage layer is the assignment point because the seq
is durable room state (it must survive the pod that assigned it), and
storage owns the record it rides on. Two invariants make gap detection
sound, and both are contract, pinned by the backend-agnostic conformance
suite:

- **Every committed mutation has exactly one seq, and every seq has
  exactly one mirror frame.** This includes the idempotent DELETE of an
  absent key: it already commits (it touches the clock) and
  already mirrors, so it assigns a seq like any other mutation. A seq
  bump with no frame would read as a permanent hole (a subscriber would
  re-snapshot for nothing); a frame with no seq could not be spliced.
- **`ScanRoom` reports the exact seq its snapshot reflects.** The
  snapshot's `S` must satisfy: every mutation with seq <= S is in the
  state, no mutation with seq > S is. The single-transaction backends read the seq
  inside the same transaction / stripe as the scan, so the fence is
  free. shale cannot put a prefix scan inside a CAS, so `ScanRoom` runs
  a **seq fence**: read the record's seq, scan the namespace, re-read
  the seq; equal means no commit interleaved and the scan is exactly the
  state at S; changed means retry (bounded; on exhaustion the join fails
  and the client reconnects - correctness is never traded for a stale
  fence).

Deleting a room removes its record and its seq with it; a room
UUID is never reused (creation mints a fresh UUIDv4), so no subscriber
can ever observe a room's sequence regress.

#### The wire format: seq on every durable frame

The two server-originated control envelopes gain a `seq` field:

```
{"type":"snapshot", "seq":S, "state":{...}}          the late-join snapshot
{"type":"put",      "seq":N, "key":"...", "value":...}  live mirror of a PUT
{"type":"delete",   "seq":N, "key":"..."}            live mirror of a DELETE
```

Ephemeral peer frames are untouched: payload-opaque, no envelope, no
seq. They are stale-the-instant-they-are-sent signals with no ordering
contract; the room sequence orders only the durable stream.

This is a **coordinated frame-format and client-contract change**, and
the spec says so explicitly rather than pretending compatibility: the
added `seq` field is additive JSON (an old client ignores it and keeps
working against a single-pod deploy), but the no-dup guarantee MOVES
from a server-side lock to the client's discard rule, so a client that
predates the sequence can observe duplicates on a multi-pod deploy. The
tier's consumers are few and version with the service; client and
server adopt the sequence in the same release.

#### The client splice contract

The client keeps `lastSeq`, initialized by the snapshot:

1. **On snapshot**: replace local state with `state`, set
   `lastSeq = S`. (The snapshot is always the first frame the server
   sends on a connection.)
2. **On a durable frame with seq n**:
   - `n <= lastSeq`: discard (already reflected; this is the no-dup
     rule).
   - `n == lastSeq + 1`: apply, advance `lastSeq`.
   - `n > lastSeq + 1`: hold the frame in a small pending set and start
     a short gap timer. Out-of-order arrival is NORMAL, not
     exceptional - two writes committed via different pods race their
     fan-outs - so the client splices, it does not panic: when the
     missing seqs arrive, apply the run in order and clear the timer;
     if the timer fires (a couple of seconds), the frame is lost -
     resync.
3. **Resync = reconnect.** Close the socket and rejoin; the fresh
   snapshot-then-stream is the one resync path and it is already
   correct. There is deliberately no in-band "resend me seq N" request:
   every client -> server frame is broadcast to peers as an ephemeral
   frame (the relay is payload-opaque and has no client control
   channel), and the server retains no frame history to serve a replay
   from.

A client's own PUT comes back to it as a seq'd mirror frame (the mirror
is server-originated and fanned to everyone, sender included). That is
by design: the HTTP 204 says "durable"; the frame says where the write
landed in the room's order.

#### The peer transport

- **Protocol.** A hostthis-owned gRPC service (its proto lives in the
  relay bounded context), one RPC: publish a frame to a peer, carrying
  `(app_slug, room_id, binary, data)`. The frame body is opaque to the
  transport - a durable mirror's seq rides inside `data`, an ephemeral
  frame has none - so the peer tier needs no schema knowledge and the
  envelope can evolve without touching the proto.
- **Receive path.** The receiving pod resolves its LOCAL hub for
  `(app_slug, room_id)` and broadcasts the frame as server-originated
  (from = 0: every local connection receives it; the originating socket,
  if any, lives on the origin pod, which already excluded it from its
  own local fan-out). No local hub means no local subscribers: the frame
  is dropped, correct because the live path never carries correctness.
  A received frame is delivered locally ONLY - never re-forwarded. The
  origin pod is the single fan-out point (full mesh, TTL 1), so no
  routing loops exist by construction.
- **What fans out.** Both flavors: a durable mirror (after its commit)
  and an ephemeral client frame (as it is broadcast locally). Ephemeral
  frames get cross-pod delivery for free on the same path; their loss
  or reordering needs no machinery because they carry no contract.
- **Listener: registered on the gRPC server the sharded backend already
  runs.** In multi-node mode hostthis itself constructs the peer
  forwarding server (`internal/storage.NewShaleRepo` binds the
  listener, creates the `grpc.Server`, registers shale's node service
  on it, and serves it; the server is hostthis-owned code, not buried
  in the shale library). The relay's peer service registers on that
  same server via a generic registrar hook on the storage config,
  wired at the composition root. One advertised address per pod, one
  listener lifecycle, and the address is one every peer can already
  reach - it is the same one shale forwarding uses. The storage package
  stays relay-agnostic (an opaque `func(*grpc.Server)` hook); the relay
  stays storage-agnostic (it implements a gRPC service and consumes two
  small ports). The receiver's local-delivery target is late-bound: the
  relay is constructed after the repo, so the receiver holds a settable
  delivery hook, and a frame arriving before wiring completes (a boot
  race) is dropped - correct, since no client can be connected before
  the HTTP server is up.
- **Peer discovery: the ring membership the cluster already gossips.**
  The cluster's member list carries each live pod's advertised gRPC
  address - exactly the address the relay should dial - kept current by
  the same gossip that tracks joins, leaves, and deploy churn, fresher
  than any DNS view and free of a second discovery mechanism. The relay
  consumes it through a narrow `Peers` port (the current peer addresses,
  self excluded) so tests inject a static list and a future non-shale
  multi-pod shape could plug a DNS-based provider without touching the
  relay. (The operator-side headless service that seeds gossip remains
  just that - the seed; membership is the live truth.)
- **Delivery semantics: best-effort per peer, isolated per peer, and
  never on the commit path.** The origin enqueues the frame on a
  bounded per-peer outbound queue (the enqueue never blocks; a full
  queue drops the frame) drained by a per-peer sender goroutine over a
  long-lived client connection. A slow, full, or unreachable peer costs
  the writer NOTHING: the commit already returned (the HTTP 204
  reflects durability, never liveness), the local mirror already ran,
  and other peers' queues are independent. No peer error ever fails or
  delays a durable write. A dropped durable frame is DETECTABLE at
  every affected subscriber via the dense seq (the splice contract
  re-snapshots); a dropped ephemeral frame is harmless by definition.
  The known bound: a subscriber behind a missed durable frame learns of
  the gap only when the NEXT durable frame arrives, so a then-quiet
  room can stay visually stale until then - the durable KV is never
  wrong, only the live view is late. Accepted for now; the client
  lifecycle's heartbeat + visibilitychange reconnects bound it in
  practice, and a periodic room-seq beacon is the named future fix if
  it bites. A second, rarer producer of the same symptom is an
  **ambiguous commit**: the storage write LANDS but the committer
  observes an error (a timeout that raced the CAS round trip), so a seq
  was consumed and no mirror frame is ever broadcast for it - not
  locally, not to any peer (the handler surfaces the error to the app,
  yet the write is durable). Every subscriber then sees a hole at that
  seq that only the NEXT durable frame exposes, and a then-quiet room
  stays visually stale until one arrives. Same accepted bound, same
  named future fix: the periodic room-seq beacon closes both.
- **Trust boundary.** The peer service rides the same cluster-internal
  listener the shale forwarding port already uses: pod-to-pod traffic
  inside the deployment's network boundary, never exposed on the public
  ingress. Every per-connection abuse limit (frame size, inbound rate)
  is enforced at the ORIGIN pod against the client socket BEFORE any
  peer fan-out, so peer input is trusted to the same degree shale's own
  forwarded writes are; the receiver re-checks a frame size cap on
  arrival as cheap defense in depth. That receiver cap is sized to the
  LARGEST legal frame on this channel, which is NOT the client-socket
  cap: a durable mirror carries a committed room value verbatim (up to
  the room value cap, set by the HTTP PUT path, several times the
  client-socket frame cap) inside a JSON envelope whose string encoding
  can inflate non-JSON bytes up to 6x (worst-case escaping). Sizing the
  receiver to the client-socket cap would silently sever cross-pod
  mirrors for every legal value above it - a whole value class whose
  remote subscribers would stall until the next durable frame exposed
  the gap.

#### Drain hint: reconnect-before-shutdown

WebSockets die with their pod - that is accepted, and the reconnect +
snapshot path heals it. The drain hint makes the heal proactive: a new
server-originated control envelope

```
{"type":"reconnect"}
```

(no seq - it is not a room mutation) is broadcast once to EVERY local
connection the moment the process receives its termination signal,
BEFORE the HTTP server stops accepting and before the final close of
live connections. In a rolling deploy the terminating pod keeps serving
through its grace window, so a client acting on the hint reconnects
while its old socket still works - the new join lands on a surviving
pod through the normal ingress, a make-before-break re-home instead of
a hard cut. The process-side half of that window is
`HOSTTHIS_DRAIN_GRACE` (a Go duration, default `3s`): after the hint is
broadcast the process keeps serving - existing sockets flow, new joins
are still admitted - for that long before the final close, so the hint
has time to flush and a hint-acting client re-homes make-before-break.
`0` disables the pause (hint then immediate close). Clients SHOULD apply small random jitter (a few seconds)
before reconnecting so a large room does not thundering-herd the
survivors. The hint is an optimization, never load-bearing: a client
that ignores it is closed at actual shutdown with a normal closure and
heals through the standard reconnect + snapshot + splice path.

#### The degenerate case: zero peers

Every single-pod deploy (local, single-node shale) has
an empty peer set: no peer gRPC service is registered (there is no
multi-node server to register on), the sender is inert, and the relay
is exactly the single-pod relay - same seq assignment, same
register-then-snapshot join, same client contract (the splice
degenerates to "discard <= S, frames arrive in order"). One code path,
no mode flag; the multi-pod machinery is the peer set being non-empty.
The drain hint fires on a single pod too: there is nowhere else to go,
so clients bounce back after the restart, which is today's behavior
made explicit.

#### Acceptance criteria

The multi-pod gate, stated as observable behavior:

- **Two clients on different pods receive every put/delete mirror.**
- **Late-join during concurrent cross-pod writes has no gap and no
  dup.**
- **A killed pod's clients resync via reconnect+snapshot with the
  splice holding.**

Pinned by an in-process two-relay harness (two `Relay` instances over
the same storage backend, bridged by an in-memory implementation of the
peer port - no real network needed for the correctness core), plus a
real-gRPC seam test for the transport adapter. The storage conformance
suite additionally pins the sequence semantics on every backend: dense
+1 per committed mutation with no gaps at the source, `PutValue` /
`DeleteValue` return it, concurrent same-room writers never share or
skip a seq, and `ScanRoom`'s S is exact under concurrent writes.

### Sandbox and security posture

The relay introduces no new trust boundary beyond the room-UUID
capability and the origin isolation the rest of hostthis relies on:

- **Origin isolation is unchanged.** The WebSocket is served on the app's
  own subdomain (`<app-slug>.hostthis.dev`), the same origin as the app's
  files and its KV API. A browser's same-origin policy keeps one app's
  relay unreachable as a cross-origin target from another paste's JS in the
  normal way; the relay rides the per-subdomain origin boundary that
  already separates pastes and sites. (Path mode collapses origins for dev,
  the same documented caveat the rest of hostthis carries - path mode is
  dev-only and breaks origin isolation; a production relay deploy runs
  subdomain mode.)
- **Origin / Host checks on upgrade.** The WebSocket upgrade validates that
  the request's `Host` resolves to a real app slug (the existence check
  above) and applies an Origin policy appropriate to a same-origin app API:
  cross-origin upgrade attempts that a browser would gate by CORS are
  refused, so a third-party page cannot open a victim app's relay from a
  visitor's browser. The room UUID remains the capability for any party who
  legitimately holds it (the app's own JS, a shared link), exactly as the
  KV verbs treat it.
- **The payload is opaque and never executed.** hostthis relays bytes; it
  never renders, parses, or runs a relayed message. A relayed frame is data
  fanned out to peers' JavaScript, which the app's own code interprets
  inside its own origin - the same "treat any URL on hostthis.dev as
  untrusted user content" posture the HTML-sandboxing section sets, now
  extended to "treat any relayed message as untrusted app data," handled
  entirely by the app's client code, never by hostthis.
- **No amplification past the caps.** The per-frame size cap, the
  per-connection rate limit, the per-room / per-app connection caps, and
  the bounded send buffer together bound the fan-out amplification (one
  inbound frame -> N outbound frames) so the relay cannot be turned into a
  DoS multiplier. A relay frame is never persisted, so it cannot grow the
  durable store; a durable write over the relay rides the existing byte
  caps; the capability + the caps are the whole abuse posture, no new auth.

### DDD shape: the hub as a bounded context

The relay is its own bounded context, kept thin at the edges and pure
where it can be, matching the domain-pure / infra-separate / services-on-
top discipline the rest of the codebase follows:

- **The hub / relay service is the bounded context** (connection registry,
  broadcast fan-out, lifecycle). It depends on the room KV ONLY through the
  existing small `service.RoomRepo` interface - the same snapshot
  (`ScanRoom`, which reports the snapshot's seq) and durable-write
  (`PutValue` / `DeleteValue`, which return the assigned seq) verbs the
  HTTP KV handlers use - so the relay reuses the durable tier's caps and
  quota without re-implementing them. Its only other dependencies
  are the two small outbound ports of the multi-pod tier (the `Peers`
  address provider and the per-peer frame publisher; see "Multi-pod
  relay"), both trivially faked in tests.
- **The connection is an interface, not a concrete socket.** The hub talks
  to a connection abstraction (send a frame, close, identity) so the hub
  logic - register, broadcast, drop-a-laggard, reap-on-heartbeat-timeout,
  tear-down-the-empty-hub - is unit-testable WITHOUT real sockets, with a
  fake connection that records what it received and can be made to block /
  fill its buffer to exercise the backpressure path. The pure hub logic is
  the testable core; the real `coder/websocket` connection is one adapter.
- **The HTTP WS-upgrade handler is thin.** It authenticates the room
  (validate slug exists + UUID parses + room exists, the same checks the KV
  path runs), enforces the connection caps, performs the upgrade, and hands
  the connection to the hub. It carries no app logic and no relay state of
  its own. This is the same translation-layer-only shape the existing
  `/api/rooms` handlers have.
- **The library.** The relay uses a maintained, context-native Go
  WebSocket library - **`coder/websocket`** (the maintained successor to
  `nhooyr.io/websocket`), whose `context.Context`-first read/write API and
  built-in ping/pong fit the lifecycle above and the codebase's
  context-aware shape. It is added via `go.mod`. (The long-lived WebSocket
  connection is hijacked out from under the `http.Server`'s
  `ReadTimeout` / `WriteTimeout`, which bound the short request/response
  paths and must NOT reap a live relay connection; the relay manages its
  own per-connection read/write deadlines via the heartbeat instead.)

### Testing: the multi-client harness is the gate

Per the TDD discipline, the relay does not ship without the integration
test that pins the spec'd behavior. The gate is a **multi-client harness**:
two or more clients join one room and the test asserts the observable
contract end to end - a message from one client reaches the others and not
itself; a late joiner gets the snapshot then the live stream with no gap
and no dup; a reconnecting client re-syncs cleanly; a slow client is
dropped without stalling the room's broadcast to the others; a heartbeat-
timeout connection is reaped; strict isolation holds (a connection in room
A never sees room B's or another app's traffic); and the connection / room
/ frame-size / rate limits each reject past their bound. The pure hub
logic is additionally unit-tested against a fake connection (no real
socket) for the register / broadcast / backpressure / reap / teardown
paths. The WebSocket tests run under `-race` so a data race in the hub's
concurrent register / broadcast / unregister path fails the build. The
multi-pod tier has its own gate on top of this one - the two-relay
peer harness and the seq conformance pins - specified under "Multi-pod
relay" (Acceptance criteria).

## Persistence

**Pastes, sites and rooms persist indefinitely.** Nothing expires, there is
no retention window, and no operator setting controls one.

A retention window is not a setting that defaults off, because a disabled
feature costs what an enabled one does. Expiring content requires
time-ordered indexes, a periodic scan of each, and a sweep whose failure
modes are all destructive. That machinery is a standing hazard, and this
service has no use for what it buys.

What follows:

- **Storage only grows.** A paste is removed when its owner deletes it, and
  never otherwise. The quota is what bounds an identity, and it is now the
  *only* thing that does.
- **No clock affects correctness.** Nothing is a function of elapsed time, so
  no answer changes because a sweep did or did not run.
- **A link never dies.** Which is the property a paste service is for: a URL
  shared today resolves in a year.

The Sybil keygate still forgets: it admits a bounded number of new keys per
IP subnet per rolling window, and that window only means anything if old
entries are dropped. That is a rate limiter's sliding window, not content
retention - it stores no user content and deleting it would lock people out
rather than free space.

## Verbs (the `ssh hostthis.dev <verb>` surface)

Every verb is the first positional argument after the SSH connection.
With no command and no stdin, the server prints the help banner.

### Upload (new)
```
cat index.html | ssh -T hostthis.dev
https://abc12345.hostthis.dev
<QR code of the URL, on stderr>
```
Reads stdin until EOF or the per-paste cap (10 MiB after compression;
see "File handling → Per-paste hard cap" for the bytes-counted detail).
Validates content type (HTML or Markdown in v1). Generates a fresh
random slug.

**`-T` on piped uploads**: `cat file | ssh hostthis.dev` makes the ssh
*client* request a pseudo-terminal (the no-command-arg form defaults to
asking for a PTY), but stdin is a pipe, so the client prints
`Pseudo-terminal will not be allocated because stdin is not a terminal.`
to its own stderr. `-T` disables that client-side PTY request and
silences the warning. It is a client flag only - it changes nothing on
the server. The documented upload examples therefore use `ssh -T`. Verb
commands (`list`, `get`, …) pass a command argument and never trigger
the warning, so their examples stay plain.

**QR code on create (stderr, always on)**: on a successful upload the
server also renders a terminal QR code of the URL. The URL stays the
*only* thing on stdout (the `slug=$(… | ssh -T hostthis.dev)` capture
contract is unchanged); the QR is written to stderr. Per clig.dev, stdout is the
machine-readable datum and stderr is human-facing narration, so
`2>/dev/null` cleanly drops the QR for scripts. There is no flag to
toggle it. The same QR can be re-shown for any existing paste later via
the `qr` verb (below).

Optional `--name`:
```
cat demo.html | ssh -T hostthis.dev --name "Acme prototype v3"
https://abc12345.hostthis.dev
"Acme prototype v3"
<QR code of the URL, on stderr>
```
The name is owner-only metadata for `list`; it never appears in the
URL. Names are 1–60 chars, any printable Unicode except newlines.

**stdout vs stderr discipline**: the URL is the *only* thing on stdout -
one line, no trailing whitespace, no formatting - so pipes Just Work:

```
cat foo.html | ssh -T hostthis.dev | pbcopy   # → URL only on the clipboard
```

Everything else (the QR code, key-onboarding nudge,
warnings) prints to stderr. Pipes lose it, but the user's terminal still
renders it because stderr is a TTY by default.

An ssh key is required on every session - there is no anonymous mode.
A session opened without a key gets `ssh key required` on stderr and
exit code 3. See the `Identity` section above.

### Upload (update an existing slug)
```
cat v2.html | ssh -T hostthis.dev abc12345
https://abc12345.hostthis.dev
v2 saved
<QR code of the URL, on stderr>
```
The update path mirrors create: the URL is the only thing on stdout, and
the QR code of the (unchanged) URL is rendered to stderr alongside the
`vN saved` narration.
Slug as positional arg means "update this one". Server checks ownership
against the key fingerprint. The same format gate the create path uses
decides paste-vs-site: a gzip-tar archive piped to an OWNED site slug
re-deploys that SITE in place (see "Static site archives → Reuse →
Deploy to an existing site slug"); anything else updates a paste as
described here. The slug and URL are unchanged either way. Failure modes
(exit codes; SSH stderr message in italics):

- *not found* (exit 4): slug doesn't exist OR exists but the connecting
  ssh key isn't its owner. Indistinguishable on purpose - the owner-check
  fail surfaces as "not found" so a non-owner can't probe for the
  existence of slugs they don't own.
- *upload exceeds 10 MiB compressed cap* (exit 1): payload too large
  even after zstd compression; rejected before any bytes hit the
  blob store. Stderr surfaces the actual compressed size so the
  caller knows how far over they were.
- *upload too large to consider* (exit 1): raw input exceeded the
  100 MiB hard-fast-fail. No compressed-size check was attempted.
- *usage error* (exit 2): malformed args, bad flag value.

See "Exit codes" below for the canonical mapping.

Update creates a new immutable version under the hood (SHA-keyed blob
ref). What the URL serves next
depends on pin state:

- If the paste is *unpinned* (default for new uploads): the new
  version becomes the served version immediately. Standard "head"
  semantics.
- If the paste is *pinned* to a specific version (via `pin`): the pin
  holds. The new version is recorded in history but is NOT served.
  Stderr emits a `note: this paste is pinned to v1, so the URL still
  serves v1, not v2` along with hints for `unpin` or `pin <newver>`.

See the Pin / Unpin sections below for the full sticky semantics.

### List your pastes
```
ssh hostthis.dev list
SLUG       NAME                  SIZE    KIND      VERS
abc12345   Acme prototype v3     1.2k    html      v2
x7y8z9q0   -                      540B   markdown  v1
qrs78901   bugfix.diff           2.1k    diff      v1
mnop4567   Onboarding email      3.8k    html      v3 (pinned, latest v5)
zwy11122   -                     800B    html      v3 (pinned)
portfolio2 -                    213.0k   site      -
```
**SIZE is what the item COSTS the owner, not the size of what it serves.**
For a paste that is every LIVE version summed, so a paste at v3 shows more
than the v3 bytes alone; for a site it is the deduped stored total. That is
the only reading under which the SIZE column sums to the usage `whoami`
reports, which is what makes `list` a per-item breakdown of the quota.

When at least one row is charged for more than it serves, stderr carries a
note pointing at `versions <slug>` for the per-version breakdown. It is
conditional because a single-version list has nothing to explain. The note
is needed because `VERS` cannot carry the explanation: it shows the SERVED
version NUMBER, not how many versions are stored, and a paste whose v1 was
deleted is charged for two while still displaying `v3`.

Lists BOTH text pastes AND deployed static **sites** (a site shows
`KIND=site`, its stored byte total, and `-` in `VERS` since
sites are not versioned). This matters because a site counts against the
same 100 MiB per-identity quota as pastes: if `list` omitted sites, an owner
could hit `would exceed your 100 MiB total quota` with no visible way to see
or free what is using it (deleting the visible text pastes reclaims almost
nothing). Listing sites makes the quota legible and the slugs copyable for
`delete`. Sorted most recently updated first. `NAME` column shows the user-supplied
label or `-` if none (sites have no label, so `-`). Columns are space-padded so they stay aligned in the terminal no
matter how long a `NAME` runs (a raw single-tab separator overflows the
8-column tab stop as soon as one label is wide, shoving every following
column out of true). The header line is on stdout (top of the output) so
it appears reliably before the rows; scripts wanting headerless output
can pipe through `tail -n +2`. Because `NAME` (and the `VERS` note) can
themselves contain spaces, field-splitting is not a stable machine
contract - consumers that need the fields should parse by the fixed
column layout, not by whitespace.

The `VERS` column reports the version the URL currently serves:

- **Unpinned (default)**: bare `v<N>` where N = `MAX(ver_num)`, the
  same version every `update` advances.
- **Pinned to the latest version**: `v<N> (pinned)` - the pin matches
  the latest, so behavior matches unpinned but the pin is still set.
- **Pinned to an older version**: `v<served> (pinned, latest v<max>)` -
  surfaces both the served and the latest in one column so the owner
  can spot stale-pin situations at a glance.

When the user has zero active pastes, the command prints a single
`no active pastes` line to stderr and exits 0.

### Machine-readable output (`-o` / `--output`)

The read commands that emit a table or a structured summary accept a
kubectl-style output selector so scripts get a stable, parseable shape
instead of the human table. One flag, an enum value - NOT a pile of
boolean `--json`/`--yaml` flags:

```
ssh hostthis.dev list -o json
ssh hostthis.dev versions abc12345 -o json
ssh hostthis.dev whoami --output json
```

- **Flag**: `-o <fmt>` or `--output <fmt>`, the `=`-joined forms
  `-o=<fmt>` / `--output=<fmt>`, and the glued short form `-o<fmt>`
  (e.g. `-ojson`) - matching kubectl / pflag shorthand parsing. It may
  appear anywhere in the verb's arguments; it is parsed out and the
  remaining positionals are handled as usual (so `versions -o json
  abc12345` and `versions abc12345 -o json` are equivalent).
- **Values**: `table` (the default when the flag is absent) and `json`.
  The default is `table` even when stdout is a pipe - matching kubectl,
  which never silently switches format based on the terminal. An
  unrecognized value is an error: stderr gets
  `hostthis: unknown output format "<v>" (want: table, json)` and the
  command exits with the usage exit code (nonzero). The enum is
  deliberately open so `yaml` / `wide` / `name` can be added later
  without introducing new flags.
- **Applies to**: `list`, `versions`, `whoami`. It does NOT apply to
  `get` (raw paste bytes - the content IS the payload), `qr` (a visual
  render), or `url` (already a bare machine datum on stdout). Upload does
  not take it yet; a future pass may add a JSON upload result.
- **`-o` is safe through ssh**: although `-o` is also an ssh client flag,
  the local ssh client stops parsing its own options at the hostname, so
  everything after the verb (`list -o json`) is forwarded verbatim as the
  remote command. The flag only works when it follows a verb (never as
  the first token after the host).

**Output contract in `json` mode.** stdout carries ONLY the JSON value
(per the stdout=machine-datum / stderr=narration split used elsewhere),
so `... -o json | jq` is clean. Any human footer a command normally
writes to stderr (e.g. the `versions` pin footer) is folded into
the JSON object instead of being printed separately. Timestamps are
RFC 3339 (`2006-01-02T15:04:05Z`). Sizes are integer bytes (`size_bytes`), not the
human `2.4k` strings. The JSON is marshaled from a dedicated view shape,
not the internal domain types, so the wire contract is stable across
refactors.

`list -o json` - a JSON array (empty `[]` when there are no active
pastes, and in json mode that is stdout, not the `no active pastes`
stderr line):

```json
[
  {
    "slug": "abc12345",
    "name": "Acme prototype v3",
    "size_bytes": 1234,
    "kind": "html",
    "served_version": 3,
    "latest_version": 5,
    "pinned_version": 3
  },
  {
    "slug": "portfolio2",
    "name": "",
    "size_bytes": 218000,
    "kind": "site",
    "served_version": null,
    "latest_version": null,
    "pinned_version": null
  }
]
```

`name` is the empty string when unset (not the `-` table sentinel).
`pinned_version` is `0` when the paste follows latest (unpinned);
`served_version` is `pinned_version` when pinned, else `latest_version`.
A static **site** is discriminated by `kind: "site"`: it has no versions,
so `served_version` / `latest_version` / `pinned_version` are `null`.
`size_bytes` is the item's CHARGED total: every live version for a paste,
the deduped stored total for a site. `served_size_bytes` is the bytes of
the version being served, and is `null` for a site, which has no versions.

Both are emitted because json mode prints only the array - the human
footer never reaches a script - and a consumer cannot infer which figure
it holds: `served_version` does not say how many versions are charged,
since a deleted version leaves a paste billed for fewer than its number
implies.

`versions <slug> -o json` - an object that folds in the stderr footer
(pin state) around the version array:

```json
{
  "slug": "abc12345",
  "pinned_version": 0,
  "versions": [
    { "version": 4, "created_at": "2026-06-05T15:01:00Z", "size_bytes": 1400, "deleted": false, "current": true },
    { "version": 3, "created_at": "2026-06-05T14:32:00Z", "size_bytes": 1200, "deleted": false, "current": false },
    { "version": 2, "created_at": "2026-06-05T12:15:00Z", "size_bytes": null, "deleted": true,  "current": false }
  ]
}
```

`size_bytes` is `null` for a deleted (tombstoned) version - the bytes are
gone. `current` marks the served version (the pin, or MAX non-deleted
ver_num when unpinned).

`whoami -o json` - an object; `quota_bytes` is `null` when the owner has
no quota cap, and `session` is `null` when the keygate isn't wired or the
session has no subnet:

```json
{
  "key": "SHA256:abcd...",
  "first_seen": "2026-06-01T00:00:00Z",
  "active_pastes": 2,
  "used_bytes": 1234,
  "quota_bytes": 10485760,
  "session": {
    "subnet": "203.0.113.0/24",
    "identity_subnets": 2,
    "subnet_fresh_count": 1,
    "subnet_cap": 5
  }
}
```

`used_bytes` is the COMBINED per-identity total the quota cap actually
enforces: the identity's active paste bytes PLUS its active static-site
bytes (both post-compression). It must include sites - the deploy/upload
write-check rejects at the paste+site sum, so a paste-only `used_bytes`
would under-report and disagree with the cap (a user could see "22% used"
while writes are rejected as over-quota). When the metadata backend has no
site repo, `used_bytes` is paste bytes only. `active_pastes` still counts
pastes only (sites are enumerated by `list`).

### Rename
```
ssh hostthis.dev rename abc12345 Acme prototype v4
renamed.
```
Sets / changes the `NAME` for one of your pastes. The label is the
remaining words joined with spaces - ssh flattens the command to one
space-joined string, so a multi-word label arrives as several tokens and
is rejoined; quoting is optional. Omitting the label clears it:
`ssh hostthis.dev rename abc12345` -> `label cleared.`. (The empty-string
form `""` cannot survive the ssh argv-join, so no-label is the invocable
clear path.) Renaming is purely metadata.

### Get content (read back over ssh)
```
ssh hostthis.dev get abc12345
<the html streams to stdout>
```
Owner-only. Use case: piping a paste back through local tooling.
Non-owners (including connecting with a different ssh key than the
one that originally uploaded) see "not found" - the server doesn't
distinguish "doesn't exist" from "exists but not yours" in any verb,
so an attacker can't probe for slugs they don't own.

### Show the link / QR for an existing paste (`url`, `qr`)
```
ssh hostthis.dev url abc12345
https://abc12345.hostthis.dev

ssh hostthis.dev qr abc12345
https://abc12345.hostthis.dev
<QR code of the URL, on stderr>
```

Re-show the shareable link for any existing paste, anytime - without
re-uploading. Both reuse the exact URL-construction logic the create
path uses (subdomain vs. dev path mode), so the URL is byte-identical to
what the original upload returned.

- `url <slug>` prints **just the URL** on stdout (scripting / copy
  friendly). Nothing else.
- `qr <slug>` prints the **URL on stdout and the QR code on stderr** -
  exactly mirroring create, so the same `2>/dev/null` script contract
  holds and you can pipe the URL cleanly while still seeing the QR in a
  terminal.

**No ownership check.** The URL is a public capability - knowing the
slug already grants read access at that URL - so any caller may `url` /
`qr` any slug; there is nothing to leak that the URL itself doesn't
already expose. The verbs DO verify the target **exists and is not
**: an unknown slug returns the standard `not found`
on stderr and exits 4, the same shape as every other not-found, so the
behavior is uniform across verbs. A slug that names a deployed static
site resolves the same way (a site also has a URL).

### Versions
```
ssh hostthis.dev versions abc12345
v4  current  2026-06-05 15:01 UTC  1.4k
v3          2026-06-05 14:32 UTC  1.2k
v2  deleted  2026-06-05 12:15 UTC  -
v1          2026-06-05 11:22 UTC  0.9k
```

Stdout: space-padded aligned rows (same rationale as `list`), newest
first. The middle column carries a status marker:

- `current` - the version the URL is currently serving (the
  pinned ver_num or `MAX(non-deleted ver_num)` when unpinned).
- `deleted` - the blob bytes were freed via `delete <slug> <ver>`. The
  metadata row remains as a tombstone so the version number isn't
  reused; the size column is `-` since no bytes exist anymore.
- empty - non-current, non-deleted version (still occupies quota).

Stderr footer carries the pin state:

```
unpinned
```

or when pinned:

```
pinned to v1
```

Pipe stdout cleanly (`| awk` etc.); footer lives on stderr.

### Pin a version (sticky)
```
ssh hostthis.dev pin abc12345 1      # always serve v1
ssh hostthis.dev pin abc12345 3      # switch to v3
```
Sets the URL to serve a specific version and makes it sticky:
subsequent `update`s record new versions but do not change which one
the URL serves until the user `unpin`s or `pin`s a different one.


A freshly uploaded paste is *unpinned*: the URL always serves the
latest version, and each `update` publishes immediately.

### Unpin (back to "always latest")
```
ssh hostthis.dev unpin abc12345
unpinned. URL now serves the latest version.
```
Reverts the URL to "follow the head" semantics - every future
`update` becomes the served version.

### Delete (permanent)

Two forms - same verb, slug-only vs slug+version:

**Whole-paste delete:**
```
ssh hostthis.dev delete abc12345
deleted.
```
Wipes the slug record + all versions (including any tombstone rows
from prior `delete <slug> <ver>` calls). Reuses the slug for future
random generation. No undo. No confirm prompt (ssh sessions don't
tty cleanly; the verb is explicit enough).

**Per-version delete (free bytes; keep the history row):**
```
ssh hostthis.dev delete abc12345 2
deleted v2. freed 187.3k.
```

Deletes the blob bytes for one historical version, leaving the
metadata row in place as a tombstone. The version number is NOT
reused (a future `update` still bumps to `MAX(ver_num)+1`); `versions`
shows the row with a `deleted` marker.

Use for freeing quota when an older version isn't worth keeping but
the user wants the version-history audit trail.

Refused with a stderr error when:

- The target version is currently served (latest if unpinned, or the
  pinned version). The caller is told to `pin` to a different version
  first (or `unpin` if pinning to v1 with v2 still alive). Exit 2.
- The target version is already deleted (idempotent-but-noisy: exit 0
  with a `version v<N> already deleted` note on stderr).
- The slug doesn't exist or isn't owned by the caller. Exit 4 with
  the standard not-found-or-not-owned message (no information leak
  about other identities' pastes).

Freed bytes are subtracted from the per-identity quota immediately,
so an `upload` can use the space in the same shell session.

Dispatch rule for the verb: `delete <slug>` (one arg) → whole-paste
delete; `delete <slug> <verN>` (two args) → per-version delete.
Anything else → exit 2 with `usage: delete <slug> [<ver>]`.

### Identity
```
ssh hostthis.dev whoami
key:     SHA256:abc...xyz
joined:  2026-06-05
active:  4 paste(s)
```
Sessions without a key never reach this verb - they're rejected at
session startup with "ssh key required" on stderr and exit 3.

### Help

The bare `ssh hostthis.dev` (and `help`) prints the global banner. It is the
canonical user-facing reference and must stay in sync with this section, the
README, and the landing page.

```
Pipe a rendered file in, get a URL out. Pastes persist indefinitely.

UPLOAD

    cat foo.html | ssh -T hostthis.dev
    cat doc.md   | ssh -T hostthis.dev --name "design notes"

    (-T silences the ssh "pseudo-terminal will not be allocated" warning
     on piped uploads. A QR code of the URL prints to stderr on success.)

UPDATE & MANAGE (owner only; ssh key authenticates)

    cat foo.html | ssh -T hostthis.dev <slug>   replace bytes; URL stays the same
    ssh hostthis.dev list                       all your active pastes
    ssh hostthis.dev get <slug>                 read content back
    ssh hostthis.dev url <slug>                 re-show the URL (no QR)
    ssh hostthis.dev qr <slug>                  re-show the URL + QR code
    ssh hostthis.dev rename <slug> "label"      set / change owner label
    ssh hostthis.dev delete <slug> [<ver>]      wipe the paste, or tombstone one version
    ssh hostthis.dev whoami                     identity + active count + quota

VERSION HISTORY

    ssh hostthis.dev versions <slug>            timeline of every version
    ssh hostthis.dev pin <slug> <ver>           stick the URL to <ver> (survives updates)
    ssh hostthis.dev unpin <slug>               URL follows latest again

STATIC SITES

    tar czf - site/ | ssh -T hostthis.dev        deploy a multi-file site
    tar czf - site/ | ssh -T hostthis.dev <slug> re-deploy in place

LIMITS

    100 MiB per identity, counting post-compression bytes across all
    your active pastes. HTML, Markdown, diff, or a gzip-tar site archive.

    Apps can persist + sync state: https://hostthis.dev/  (rooms + realtime API)
```

`get` and `versions`/`pin`/`unpin` per-verb help comes from `help <verb>` /
`<verb> --help`. There is no `show` or `put` verb: reads are `get`, and upload
is verbless.

### Color output

hostthis emits plain text by default and may add ANSI color escapes in
the future for human-targeted output (warnings, refusal messages, the
`whoami` block). When that lands, every emit site routes through one
helper that follows the universal CLI convention
(https://no-color.org):

- No PTY allocated → plain text. Pipes (`ssh ... | foo`), redirections
  (`> file`), and scripted clients never receive escapes.
- `NO_COLOR` set to any value, including the empty string → plain
  text. Per the no-color.org spec, presence alone disables.
- `TERM=dumb` → plain text. Long-standing opt-out for non-ANSI
  terminals (M-x shell, screen readers, log capture).
- Otherwise → color permitted.

`NO_COLOR` and `TERM` are read from the SSH session's client-supplied
environment (the variables the user's local ssh client forwards via
`SendEnv` or sets explicitly), not from the hostthisd process's own
environment. A user opting out on their machine therefore disables
color for their sessions without affecting anyone else.

---

## Apex landing page

`https://<apex>/` serves a single static HTML page styled as a
roff(1)-shaped manpage. Its job: explain what to type to get a URL,
in 10 seconds. Not a marketing page, not a dashboard, not interactive.

The bytes shipped on the public instance live in
[`web/landing.html`](../web/landing.html); the binary loads them at
startup (`HOSTTHIS_LANDING` path) and a reverse proxy in front can
serve the same bytes directly for efficiency. Single file, no JS,
no external assets.

## Limits

A per-identity quota and an SSH-handshake gate, each enforced atomically
at the app layer, plus a durable total-bytes ceiling enforced one layer
down at the object store (see "Durable total-bytes ceiling: an
object-store quota" below).

### Per-identity quota: 100 MiB (compressed)

The sum of an identity's active pastes' COMPRESSED bytes (counting
EVERY non-deleted version of an updated paste) cannot exceed
`UserQuotaBytes` (100 MiB; not operator-configurable). "Identity" is
the SHA256 fingerprint of the uploader's ssh public key. When pastes
get deleted, or have older versions explicitly
deleted via `delete <slug> <ver>`, the cap frees up. Over-quota uploads
error with `would exceed your 100 MiB total quota`.

The per-identity quota and the per-paste cap are DELIBERATELY DIFFERENT
numbers: 100 MiB total, 10 MiB for any single paste. They were equal
historically, on the reasoning that one number is easier to reason
about, but they constrain different things and one of them is bounded by
memory rather than policy. A single upload is staged in RAM before it is
written, so the per-paste cap is what protects a small node from one
large request; the per-identity quota is a fairness limit on accumulated
storage and costs nothing at request time. Raising the total therefore
does not imply raising the per-request ceiling, and they should be
expected to diverge again. Static **sites** count against the same cap and on the same
basis: the total of their files' COMPRESSED sizes, as reported by the
staging that wrote them (see "Static site archives → Quota").

That total is NOT folded by content hash. Nothing in the store
deduplicates: a blob id is minted fresh for every file staged, so two
identical files - in one archive, across re-deploys, or across owners -
are two objects on disk and are charged as two. Summing what staging
reported is therefore the only figure that cannot drift from the disk,
because it is what went to the disk. A user can spend their quota as one 10 MiB
paste, ten 1 MiB pastes, a static site, or any mix.

Deleted versions (via `delete <slug> <ver>`) contribute zero bytes to the
quota even though the metadata row remains as a tombstone - only the
blob bytes are gone.

### Same-identity create admission: a width-2 gate

Paste creates are admitted to the metadata commit through a per-identity
gate of width 2: at most two creates for the SAME identity run their
quota-check + insert concurrently, and further same-identity creates
QUEUE until a slot frees. Queueing preserves no arrival order (FIFO
fairness is not guaranteed; concurrent creates never had an ordering
guarantee to begin with). Different identities are fully independent:
one identity's queue never delays another identity's create.

The gate guards against the failure mode where a burst of same-identity
creates becomes a same-owner write storm in the storage tier. The
metadata backends serialize same-owner commits at a CAS / transaction
boundary, so N concurrent same-owner commits each contend with N-1
rivals, and a CAS layer under that contention amplifies work (retried
and re-run commits) faster than it completes it. Bounding same-identity
admission BEFORE the storage tier keeps the contention the backend sees
at a small constant instead of the burst size.

A lone create - the overwhelmingly common case - passes straight
through: acquiring an uncontended slot is a map lookup, with no queueing
and no added latency. Width 2 exists precisely so admission control is
invisible until an identity is genuinely storming. The default width is
2 (`HOSTTHIS_CREATE_ADMISSION_WIDTH` overrides it; values below 1 are
rejected). The gate is in-process per pod - it bounds each pod's
contribution to same-owner concurrency, not a global total - and applies
to the CREATE path only; updates, deletes, and reads are not gated. Gate
state is transient: an identity with no create in flight holds no gate
entry.

### Durable total-bytes ceiling: an object-store quota

The total durable bytes the whole service can hold are bounded at the
**object store**, not by an app-level scan on the write path. The
operator sets a hard quota on the blob bucket (e.g. a MinIO bucket
quota); the storage layer never adds the bytes up itself. When a blob
`Put` is rejected by the object store because the bucket is at its
quota, the blob store surfaces the `ErrServiceFull` sentinel, and the
upload / site-deploy services translate it into a graceful
"service is at capacity; try again later" response.
Rooms hold no blobs, so a room write never produces `ErrServiceFull`;
the system recovers as owners delete content and the
sweep reclaims their bytes, freeing room under the quota.

**Why this lives at the object store, not in the app.** hostthis
previously enforced the ceiling with an app-level pre-check: before
accepting any paste / site / room write it summed the active bytes
across the entire metadata keyspace (every `versions/*`, every site's
`DedupedSize`, every app's room bytes) and rejected the write if the
total exceeded a configured cap. That design had three problems an
object-store quota fixes:

- **It was O(active rows) on every write.** The sum was a full scan of
  the byte-holding keyspace (a cross-shard aggregate on the sharded
  backend), recomputed on the hot path of every single upload. The
  cost grew with the amount of stored content, so the busiest service
  paid the highest per-write tax.
- **One bad record poisoned every write.** Because the sum had to decode
  every byte-holding row, a single undecodable / corrupt record made the
  aggregate fail, and that failure rejected EVERY write service-wide -
  not just a write touching the bad record. The blast radius of one
  poisoned row was the whole service.
- **It counted the wrong number.** The scan summed LOGICAL
  (uncompressed, pre-dedup) bytes, an estimate of disk pressure that
  could be 5–10× larger than reality after zstd compression and
  content-addressed dedup. A bucket quota counts the REAL physical bytes
  the object store actually holds (post-compression, post-dedup), so the
  ceiling tracks true storage rather than a worst-case overestimate.

A bucket quota is a HARD ceiling enforced durably by the storage layer
with no blast radius: a rejected `Put` fails only that one write, the
quota is always exact, and there is no per-write scan to run or corrupt
record to trip over. Operators worried about disk pressure set the
bucket quota (and can tune the blob backend's storage class / lifecycle
independently); hostthis carries no `--storage-cap-bytes` knob.

### Sybil rate limit: fresh keys per IP subnet per 24h

Every SSH session derives a subnet (`/24` for IPv4, `/48` for IPv6)
from `RemoteAddr` and admits the connection only if the
(fingerprint, subnet) pair is already known OR the subnet has fewer
than `--fresh-keys-per-subnet` (default 20) distinct fresh
fingerprints in the last `--fresh-keys-window` (default 24h).
Otherwise the session is refused at startup with exit code 6 and the
stderr line `too many new keys from this network today`.

Storage is a `key_first_seen(identity, ip_subnet, first_seen_at)`
table. **Rows past the window are dropped LAZILY, by the reads that
already touch them**, not by a background worker: an admission for a
subnet is already scanning that subnet's rows to count them, so it drops
the out-of-window ones on the way past, and the identity-side read does
the same for its own key.

Lazy pruning bounds the table by the set of subnets that are still
CONNECTING, not by the set that ever connected: a subnet that never
returns keeps its rows. That is deliberate. Those rows are outside the
window, so they cannot change any admission decision, and each is a few
dozen bytes. The alternative - a periodic scan of every row in the
cluster to reclaim them - is a cross-shard fan-out running forever to
free bytes nobody is short of.

**Two access patterns, therefore two orderings.** The gate is read
two ways, and they need opposite key orders:

1. SUBNET-leading, `(ip_subnet, first_seen_at)`: "how many distinct
   fresh keys has this subnet admitted in the window?" This is the
   ADMISSION gate, on the connect path.
2. IDENTITY-leading, `(identity, ip_subnet)`: "how many distinct
   subnets has this key been seen in?" This feeds the `whoami`
   session block. DISPLAY ONLY - it is read after admit and gates
   nothing, so a stale answer is a cosmetic inaccuracy, never a
   weakened control.

On a relational backend one composite primary key
`(identity, ip_subnet)` serves the second for free (leading-column
seek) and a secondary index serves the first. **A KV backend cannot
derive one ordering from the other**, so both must be written
explicitly: the subnet-leading row `keygate/<subnet>/<identity>` AND
an identity-leading entry `keygate_id/<identity>/<subnet>`. Omitting
the second does not produce a wrong answer - it produces a FULL SCAN
of every keygate row in the cluster on an interactive command, whose
cost grows with total admissions across all users and is invisible
until the table is large.

The identity-leading entry is a DERIVED index. shale's transactions
are single-shard and the two keys hash to different shards, so it
cannot be written atomically with the authoritative row; it is written
best-effort after the admit. **It is NOT reconciled, and that is the
point.**

The enumeration indexes are kept fresh by their write paths because they
feed a QUOTA, where a wrong number durably admits or rejects real work.
This one feeds a number `whoami` prints. Drift costs a cosmetic inaccuracy, and it is bounded
without any repair pass: every entry ages out of the rolling window, so a
missing entry self-corrects the next time that key connects from that
subnet, and a surplus entry stops counting when the window passes it.
Reconciling it would mean two cluster-wide scans, on a schedule, forever,
to keep a display value exact - a standing cross-shard cost paid for
nothing that can go wrong.

**Implication: same key + different subnet = a fresh registration.**
A user who connects from a new IP (different ISP, mobile network,
VPN, coffee-shop wifi) is "new" to the gate even if their ssh key is
the same as one we've seen before. They need either: (a) an existing
(fingerprint, subnet) slot on the current subnet, or (b) the
operator to bump `--fresh-keys-per-subnet` temporarily. The gate is
deliberately a subnet-scoped abuse control, not a per-key whitelist -
this is the trade-off.

### Where the rules live

Two rules were, until recently, expressed only inside the storage adapters, and
both are stated here because that placement is a design property rather than an
implementation detail.

**The quota decision is the domain's, the byte count is the adapter's.** Whether
a total admits a write is a rule; computing how many bytes an identity occupies
means scanning an enumeration index and is infrastructure. The rule has two
forms and they are NOT the same:

- a plain write ADDS its bytes to the current total.
- a write that REPLACES an existing record (a site redeploy) CREDITS the
  displaced record's bytes first, because they are still counted at the moment
  of the check but are about to stop existing. Without the credit, redeploying
  at the same size is charged twice and a user at their limit can never update
  in place - they would have to delete and re-upload.

The boundary is inclusive on the allowed side: a total landing exactly on the
cap is at the limit, not over it. A cap of zero or less means no limit.

### Atomicity

The per-identity quota check and the Sybil admit run inside a single
atomic transaction so concurrent writers serialize at the transaction
boundary instead of racing a stale SUM check. (The durable total-bytes
ceiling is NOT part of this transaction: it lives at the object store as
a bucket quota and surfaces as `ErrServiceFull` from the blob `Put`, off
the metadata write path entirely.) The underlying
mechanism depends on the metadata backend in use:

- **local backend** - the same single-shard CAS the shale backend
  uses; only the engine beneath the cluster differs.
- **shale backend** - `Db.begin(IsolationLevel.SnapshotIsolation)`
  with a `WriteBatch` for the actual multi-key writes; SlateDB's
  manifest-level fencing ensures only one writer is alive at a time
  across processes.

The service layer treats both backends identically; the atomicity
contract is "the multi-row write either fully lands or doesn't, with
no half-applied state visible to readers."

### Threat model: what's bounded and what isn't

*Bounded by the protocol*:
- One ssh key → 10 MiB of active LOGICAL bytes, ever
- One IP subnet → ~20 fresh keys × 10 MiB = ~200 MiB/day logical
  (~20–40 MiB/day actual storage after zstd)
- All identities combined → the object-store bucket quota on real
  physical bytes (post-compression, post-dedup), enforced by the storage
  layer, not an app-level scan
- Concurrent per-identity quota races → atomic transactions on the
  single-writer local backend; on the sharded shale backend
  the quota check is a scan that is not atomic with the write, so
  same-identity concurrency admits a BOUNDED overshoot (one in-flight
  upload), backstopped by the object-store bucket quota (see
  "Scan-derived quota" / "The correctness argument")

*Not bounded by the protocol* (operator-layer concerns):
- *Multi-IP Sybil via residential-proxy fleets*. An attacker with 100
  distinct subnets gets 100 × ~20 MiB/day potential churn. Reverse-
  proxy per-IP rate limiting at nginx / caddy is the appropriate
  layer; we don't ship one.
- *Hosted-phishing reputation*. Google Safe Browsing can flag
  specific slugs or, worst case, the entire `*.hostthis.dev`
  wildcard. Operators should monitor via Search Console and be ready
  to manually delete abusive slugs; nothing in the protocol prevents
  abuse before flagging.
- *Markdown render CPU*. Rendering now happens in the visitor's
  browser (marked + DOMPurify), not on the server, so a hot markdown
  slug hammered in parallel no longer pegs server CPU; each read just
  streams the raw bytes or the fixed shell.
- *Bandwidth amplification*. No per-slug egress cap. Hetzner-style
  free egress allowances are generous but not infinite. A CDN in front
  (see "Edge caching" below) makes this concern moot for cached reads.

---

## Blob storage backends

Blob bytes are content-addressed by SHA256 and stored via a small
`BlobStore` interface declared by the service layer:

```go
type BlobStore interface {
    Put(sha string, r io.Reader, size int64) error
    Get(sha string) ([]byte, error)
}

type SweepBlobs interface {
    WalkBlobs(fn func(sha string) error) error
    Remove(sha string) error
}
```

The service layer never imports a specific backend - it depends only on
those four methods. The standalone backend is selected via the
`HOSTTHIS_BLOB_BACKEND` env var at startup.

**`Put` and the `ErrServiceFull` sentinel.** When an underlying object
store rejects a `Put` because the bucket is at its configured quota (a
`507 Insufficient Storage` / quota-exceeded response from MinIO or an
equivalent backend), the blob store maps that rejection to the
`ErrServiceFull` sentinel rather than a generic 500. This is the single
place the durable total-bytes ceiling is enforced (see "Limits →
Durable total-bytes ceiling: an object-store quota"): the app runs no
service-wide byte scan, so `ErrServiceFull` originates from the storage
layer's rejection, and the upload / site-deploy services translate it
into the graceful "service is at capacity" response. A backend with no
quota concept (the local `disk` backend) never returns it; the ceiling
is then whatever the host filesystem allows, an operator concern.

### Available backends

`disk` (default, the only standalone backend): bytes live on local disk
under `<data-dir>/blobs/<sha256[:2]>/<sha256>`. Two-character sharding keeps
any single directory's entry count manageable. Linux page cache absorbs
hot blob reads. Fine up to a few tens of GB and a few thousand
identities. This is the dev/test standalone backend.

Production runs the shale metadata backend, whose blobs go through the
**shale-collocated blob plane** (the cluster owns a MinIO/S3 object store
directly; see "Shale-collocated blobs"), not through this standalone
`BlobStore` at all. A detached standalone `s3` backend used to exist as a
third option; it was retired once the shale-collocated path subsumed the
only production use of a cloud object store for blobs. The standalone path
is now disk-only (dev/test); cloud blobs are the shale cluster's concern.

### On-disk format

Every blob written by either backend is zstd-compressed (level 3) and
prefixed with a 4-byte magic header `HZ\0\x01` (`HZ` for hostthis-zstd,
`\0\x01` for format version 1). The compressed body follows the magic.
Layout:

```
0..3      : magic 'H' 'Z' 0x00 0x01
4..N      : zstd-encoded original bytes
```

Reads:
- Decoder inspects the first 4 bytes. If they match the magic, the
  rest is zstd-decoded and returned.
- If they don't match, the blob is treated as a legacy uncompressed
  blob (written before this change) and returned as-is. Backstop for
  the rolling migration; remains in place indefinitely as defensive
  code since the cost is one cheap byte-compare per Get.

Writes:
- Always compressed + prefixed; legacy raw-bytes writes are never
  emitted by the current binary.

Object/file naming is the sha256 of the ORIGINAL (uncompressed)
bytes - dedup happens on logical content, not on the compressed
representation. Two pastes with identical bytes share one stored object.
The same magic+zstd format is used by the shale-collocated blob plane, so
a blob's stored bytes are identical whichever path wrote them.

### One standalone backend (disk)

The standalone blob path ships exactly one backend, `disk`, which is also
the default - there is no backend to switch between. A detached `s3`
standalone backend (and its disk->S3 migration helpers) used to exist; it
was retired once the shale-collocated blob plane became the production
path for cloud object storage. Production blobs live in the shale cluster's
own object store (see "Shale-collocated blobs"), reached through the
metadata backend, not through this standalone `BlobStore`.

### Local-disk write-back cache (optional, opt-in)

A blob `Put` against a remote object store dominates upload latency: most
of a paste's wall-clock time is spent inside the `PutObject` call (hundreds
of ms), while the local hashing + compression and the metadata writes are
each a few ms. For deploys that can tolerate a small durability window in
exchange for a fast upload ack, an optional **local-disk write-back cache**
sits in front of the durable backend.

NB this cache existed to hide the latency of the *detached cloud* standalone
backend, which has been retired - the standalone path is now disk-only, so
its `Put` is already local and the cache is effectively dormant there. The
production blob path (shale-collocated) instead hides the slow object-store
`Put` by STAGING the bytes durably before the metadata commits (it does not
use this cache). The machinery below is retained for any future remote
standalone backend; it is correct but currently has no remote `Put` to
front.

It is **off by default**. The strict, ship-it-durable-before-ack
behavior described above is unchanged unless an operator opts in with:

```
HOSTTHIS_BLOB_WRITEBACK=true            # enable the write-back cache (default false)
HOSTTHIS_BLOB_WRITEBACK_DIR=<path>      # local cache dir (default <data-dir>/blob-cache)
HOSTTHIS_BLOB_WRITEBACK_MAX_BYTES=<n>   # soft cap on cache size in bytes (default 1 GiB)
```

When enabled, the cache wraps the configured durable backend (`disk`) and
changes the blob path as follows:

- **`Put` writes locally first, uploads asynchronously.** The bytes (the
  already-compressed, magic-prefixed stored representation) are written
  to the local cache directory with the same atomic tmp-write + fsync +
  rename the disk store uses, then the SHA is enqueued for a background
  uploader. `Put` returns as soon as the local write is durable on the
  pod's disk - typically a few ms - without waiting for the object store.
  The content-addressed skip still applies: if the durable backend
  already has the object (checked cheaply), the local write and the
  enqueue are skipped.

- **A background uploader drains the queue to the durable backend.** A
  small pool of workers pops SHAs, reads the bytes back from the local
  cache, and `Put`s them to the durable backend with bounded retry and
  exponential backoff. On a successful durable upload the cache entry is
  marked uploaded (eligible for eviction). A failed upload is re-enqueued
  after backoff so a transient object-store outage doesn't lose the blob;
  it just extends the durability window.

- **`Get` / `GetReader` read the cache first, fall back to the durable
  backend.** A blob that is still local (uploaded or not) serves from the
  pod's disk. A blob that has been evicted (or was never cached, e.g.
  written by a different pod or before the cache was enabled) is fetched
  from the durable backend transparently. Reads are therefore always
  correct regardless of upload state.

- **Startup re-scan re-enqueues pending uploads.** On boot the cache
  walks its directory and re-enqueues every entry that has not been
  confirmed uploaded to the durable backend. This makes the uploader
  durable across a process restart: a crash or redeploy that leaves
  un-uploaded blobs on a surviving disk recovers them on the next boot
  rather than stranding them. (Whether the disk survives a restart is the
  deploy's concern; see the durability caveat below.)

- **Bounded cache with eviction.** The cache tracks its on-disk size and,
  once it exceeds `HOSTTHIS_BLOB_WRITEBACK_MAX_BYTES`, evicts
  already-uploaded entries oldest-first until it is back under the cap. An
  entry that has not yet been confirmed uploaded is NEVER evicted - that
  would lose the only copy of a not-yet-durable blob. The cap is therefore
  a soft cap: a burst of uploads that outruns the uploader can push the
  cache temporarily over the cap, and it drains back down as uploads
  complete.

**Durability caveat (operator-facing, read before enabling).** With the
write-back cache on, a blob is durable on the pod's local disk
immediately but NOT durable in the object store until the async upload
completes. If the pod's local disk does not survive a restart - the
production deploy uses an ephemeral `emptyDir`-class volume, so it does
not - then a pod loss between the `Put` ack and the async upload loses
any blob still in flight. The object-store copy is the only cross-pod
durable copy. This is the same narrow durability window class as a
fast-ack metadata write: the ack is honored locally and the durable copy
follows shortly after. The startup re-scan closes the window for a clean
restart (the disk is still there) but NOT for a reschedule onto a fresh
node with an empty volume. Operators who cannot tolerate that window must
leave the cache off (the default), which preserves today's
durable-before-ack guarantee. Operators who enable it on a deploy with a
persistent local volume get the latency win with no durability loss for
process restarts, only for total volume loss.

**Where it sits in the stack.** The cache is a storage adapter that
implements the same inner contract the compression layer wraps
(`Put` / `Get` / `GetReader`) plus the sweep contract
(`WalkBlobs` / `Remove`). It is wired between the compression layer and
the durable backend, so it sees the compressed stored bytes and is
invisible to the upload service, which still depends only on the
`BlobStore` interface. Sweep GC walks the DURABLE backend (authoritative
for what blobs exist), and `Remove` deletes from both the durable backend
and the local cache.

---

## Metadata storage backends

The metadata layer (paste rows, version rows, identity quota
counters, slug-to-identity index, Sybil key_first_seen rows) is
pluggable. Two ship:

- **local** - a single-node shale cluster on this build's local
  storage engine, persisted under `<data-dir>/metadata`. The
  default. Zero configuration and no external services, which is
  what a fresh clone and `make run` want.
- **shale** - the same cluster over an S3-compatible object store
  (MinIO, R2, S3, GCS, ABS), sharded and replicated across nodes.
  Production. Specified in full under "Shale-backed metadata
  storage" below.

Both are the SAME repo over a different storage engine, chosen at
build time (see "The storage-engine seam"), so there is one
implementation of every behaviour rather than one per backend. The
service layer talks to a small set of interfaces (`PasteAdmin`,
`KeyGateRepo`, `SweepRepo`); the domain layer and the HTTP/SSH
adapters are unaware of which is in use. Switching is one env var:

```
HOSTTHIS_METADATA_BACKEND=local                   # default
HOSTTHIS_METADATA_BACKEND=shale                   # production
HOSTTHIS_METADATA_S3_BUCKET=hostthis-metadata     # required for shale
# (S3 endpoint/credentials reused from HOSTTHIS_S3_*)
```

### Why pluggable

- **Stepping stone to multi-region.** SlateDB-on-MinIO validates the
  cloud-object-store metadata path locally. Same code then points at
  R2 in production with one env var change - no app rebuild.
- **Stateless containers.** With SlateDB, the container holds no
  durable state; killing it and bringing it up elsewhere is safe.
  A local engine ties data to a host path.
- **Scaling headroom.** A local engine caps sustained writes at
  single-writer throughput. SlateDB batches writes into SSTables and
  tolerates higher rates.

### Atomicity contract (both backends)

Every write that touches multiple keys is committed atomically:

- New paste = (paste row) + (v1 version row) + (slug-to-identity
  pointer). All three land or none. The owner's used bytes are DERIVED by
  scanning the version rows (the row carries the identity), so there is
  no separate quota counter to bump.
- Update = (new version row) + (paste head pointer update if
  unpinned). Both land or neither.
- Per-version delete = (version tombstone). The tombstoned version drops
  out of the next quota scan; no counter to decrement.
- Whole-paste delete = (paste row delete) + (cascade to all version
  rows) + (slug pointer delete). All land or none; the freed bytes leave
  the owner's quota simply because the next scan no longer sees them.

the local engine enforces this via the cluster CAS; shale via
`Db.begin(IsolationLevel.SnapshotIsolation)` with `WriteBatch`. On the
shale backend the `{slug}`-shard rows above commit atomically as one CAS,
and the owner's `{id}`-shard enumeration index entry is a separate
best-effort write, ordered BEFORE it (see "Scan-derived quota"); the quota is never a stored AGGREGATE on any backend (shale
caches per-record sizes on the enumeration entries, each rebuildable from
its authoritative rows).

### Compose & operator config

The standalone blob backend (`HOSTTHIS_BLOB_BACKEND`, disk-only) and the
metadata backend (`HOSTTHIS_METADATA_BACKEND`) are independent. Reasonable
combos:

| metadata | blob | shape |
| --- | --- | --- |
| local | disk | single-host, no cloud deps (dev) |
| shale | shale-collocated | production: blobs live IN the shale cluster's object store, co-committed with metadata (see "Shale-collocated blobs"); the standalone `BlobStore` is bypassed |

The detached cloud (`s3`) standalone blob backend was retired; production's
cloud blobs are the shale cluster's concern, reached through the metadata
backend, not a separate `HOSTTHIS_BLOB_BACKEND` selection. Bucket-per-domain
is still recommended on the shale path: the metadata bucket and the
collocated blob bucket (`HOSTTHIS_SHALE_BLOB_BUCKET`) are distinct, so
IAM/credential rotation can differ for metadata vs blobs.

### The storage contract and its conformance suite

Whatever backend is in use, the rest of the app depends on it only
through four small Go interfaces declared in `internal/service`:

- `PasteRepo` (upload): `InsertWithQuotaCheck`, `Get`.
- `PasteAdmin` (manage): `Get`, `ListByOwner`, `Delete`, `SetName`,
  `SetPinnedVersion`, `Unpin`, `AppendVersionWithQuotaCheck`,
  `ListVersions`, `GetVersion`, `DeleteVersion`, `CountByOwner`,
  `SumActiveBytesByOwner`, `OwnerFirstSeen`.
- `SweepRepo` (sweep):
- `KeyGateRepo` (keygate): `AdmitNewKey`, `SubnetSnapshot`,
  `SubnetsForIdentity`.

Every backend implements all four identically. The observable contract
those interfaces expose, not the storage internals, is the load-bearing
thing: callers see the same return values, the same sentinel errors,
and the same accounting regardless of which backend is wired. Adding a
backend is only safe if the new backend preserves that observable
contract.

The sentinel error vocabulary those contracts speak (not-found,
slug-taken, over-user-quota, service-full, room-data-full,
app-rooms-full, too-many-new-keys) is OWNED BY THE DOMAIN LAYER
(`internal/domain`): the sentinels are business outcomes every layer
must agree on, not backend internals. The storage package re-exports
each sentinel under its historical `storage.Err...` name as an alias of
the same error value, so `errors.Is` identity holds whichever name a
caller matches against. The message text is stable contract too (it
keeps the historical `storage:` prefix): sentinel messages appear in
user-facing output, so moving the definitions must not change a byte of
them.

One sentinel is domain-owned without being universal:
`ErrConcurrentChange`. It reports that another write to the same record
landed while this one was deciding what to do, leaving a decision that
cannot be salvaged without re-reading. The operation applied NOTHING,
which is what makes a retry safe, and the retry is the CALLER's: an
interactive verb reports it and the user re-runs, the sweep skips
that reference and the next pass picks it up (its index entry is still
standing, because a cascade that failed did not drop it). It lives in
the domain because the sweep must recognise it without importing an
adapter. A backend whose concurrency control cannot lose this way simply
never returns it, so the conformance suite does not require it.

Retrying in the repo instead was rejected: the optimistic-commit layer
already spends a bounded retry budget internally and only surfaces a
conflict once that is exhausted, so a second loop above it re-runs an
already-exhausted one. Where a stale read taken OUTSIDE the transaction
is the thing that must be redone, the only correct place to start over
is the caller.

**Observable contract (what every backend must agree on).** These
behaviors are expressed in terms of inputs and observable outputs:

- **Insert / Get.** A successful `InsertWithQuotaCheck` makes the paste
  readable by `Get` with the same field values; a missing slug returns
  the not-found sentinel; a duplicate slug returns the slug-taken
  sentinel - bare or `%w`-wrapped, matched by `errors.Is` identity, on
  which the upload service retries with a fresh slug. Message text is
  never load-bearing for classification: an unrelated error whose text
  mentions "slug" must surface verbatim, not trigger a retry.
- **Quota (strict, never exceeded).** `InsertWithQuotaCheck` and
  `AppendVersionWithQuotaCheck` reject (over-quota sentinel) when
  accepting the write would push the identity's active bytes above the
  per-identity cap. Active bytes are summed across every non-deleted
  version of the identity's pastes. Quota is freed by
  `Delete` (removes the paste and all its versions) and by
  `DeleteVersion` (tombstones one version). Per-identity quotas are
  independent of each other; the durable total-bytes ceiling is a
  separate concern enforced at the object store, not by this repo.
- **Versions.** `AppendVersionWithQuotaCheck` assigns `MAX(ver_num)+1`,
  counting tombstones so numbers are never reused. An unpinned paste's
  head rolls forward to the new version; a pinned paste keeps serving
  its pin. `AppendResult.NewVer` and `AppendResult.WasPinned` (pin
  state before the append) are returned; both bump the
  clock.
- **Pin / unpin.** `SetPinnedVersion` makes a version sticky and rolls
  the head to it; `Unpin` clears the pin and rolls the head back to the
  latest non-deleted version.
- **DeleteVersion tombstones (content-inaccessible, blob GC-able).**
  Flips a version's deleted flag, leaving the metadata row so the
  number is not reused and history stays auditable. `ListVersions`
  returns tombstones (newest first, marked `deleted`); `GetVersion`
  returns a tombstone too (so the row is still visible for the paste's
  lifetime). The tombstoned version's content SHA is NOT in the
  referenced set, so the sweep reclaims its blob: a deleted version is
  app-final and content-inaccessible, and its bytes stop counting
  against quota. Recoverability of the dropped blob is provided beneath
  the app by object-store versioning plus a noncurrent-version
  lifecycle (an operator-level safety net, not an app feature). The
  repo does NOT enforce refuse-current / refuse-pinned-current: those
  guards live in `Manage.DeleteVersion`, not the repo. Whole-paste
  `Delete` is a full removal (the paste leaves every listing; there is
  nothing left to show versions of) and is unaffected by this rule.
- **Owner-gating is a service-layer concern.** The repos are NOT
  owner-aware: `Get`, `Delete`, `SetName`, and so on operate on a slug
  regardless of who owns it. IDOR protection (a cross-owner read
  surfacing as not-found) lives in `Manage.requireOwner`. A backend
  must NOT add owner checks; doing so would change observable behavior.
- **Sweep convergence guard (unreachable refs).** "One pass drains what
  it scans" holds only when the store the scan READS is the store the
  deletes WRITE. Real deployments have shown states where they diverge:
  records physically placed where the routed delete cannot reach them
  (e.g. bulk-imported legacy data placed under a different sharding
  function than live routing uses) or a diverged replica resurrecting a
  deleted record. In such a state every processing "succeeds" while
  nothing persists, and an unguarded sweep re-processes the same refs
  every pass forever (a constant deleted/cleaned count each cycle and
  millions of pointless metadata ops per day). The sweep therefore keeps
  an in-process guard across passes, in live mode, covering all three
  record kinds (pastes, sites, rooms): it remembers the refs it processed
  in the previous pass, and a ref that RESURFACES in the next scan after
  being processed is classified UNREACHABLE - skipped (not re-processed),
  excluded from the deleted and cleaned counts, and reported once per
  pass as a distinct skipped-count log line so the operator knows
  external cleanup is required. An unreachable ref that stops appearing
  (externally purged, or the store converged) is forgotten, so a later
  legitimate record with the same identity is processed normally; a
  process restart also clears the guard, giving each boot one fresh
  attempt (self-healing when the store converges later). The guard's
  memory is bounded; at the cap it fails open (refs are processed as if
  unguarded, and the overflow is logged). The guard never weakens abort
  semantics: scan/aggregation errors still abort the pass, and dry-run
  (which mutates nothing) neither consults nor updates it.
- **Dry-run (observability).** The sweep has two modes, selected by the
  operator's disable flag, and a "disabled" sweep is NEVER a no-op. In
  DRY-RUN mode it runs the full computation (which blobs are orphaned) and
  LOGS each would-be deletion, but mutates nothing - no blob removed, no
  rate-limit row pruned. In LIVE mode it performs the deletions. Both fail-closed guards
  apply in dry-run too: a dry run against a store with an undecodable
  record logs that the blob GC WOULD abort, surfacing the bad record
  without touching anything. Dry-run is how an operator earns confidence
  before trusting a sweep: deploy a change, watch the dry-run log confirm
  it would clean only what's expected, then flip to live. There is no
  third mode - the disable flag toggles dry-run vs live, and the safety net
  for a live over-deletion is the object store's versioning/soft-delete (a
  wrongly-removed blob is a recoverable prior version, not a hard loss).
- **Owner stats.** `ListByOwner` returns the owner's pastes ordered most
  recently updated first, with `LatestVersion` populated;
  `CountByOwner` counts them; `SumActiveBytesByOwner` matches the quota
  math; `OwnerFirstSeen` is the earliest paste `created_at` (zero time
  when none).
- **KeyGate.** `AdmitNewKey` reports `knownAlready=true` for a
  previously-seen `(identity, subnet)` pair (no accounting), admits a
  fresh pair when the subnet is under its in-window limit, and returns
  the too-many-new-keys sentinel at the limit. Subnets are independent;
  rows aged past the window stop counting, and the admission scan drops
  them as it passes. There is no separate prune entry point: pruning is a
  side effect of the reads that already walk the rows.

**Conformance suite.** `internal/storage/conformance_test.go` is a
backend-agnostic suite that pins exactly the observable contract above.
It takes a backend through a single factory. The backend type is just
the union of the four service interfaces (no backend-specific helpers,
so the suite cannot accidentally pin a behavior one backend has and
another lacks):

```
type conformanceRepo interface {
    service.PasteRepo
    service.PasteAdmin
    service.SweepRepo
    service.KeyGateRepo
}

func runConformance(t *testing.T, name string, newRepo func(t *testing.T) conformanceRepo)
```

Pastes are created through `InsertWithQuotaCheck` / `AppendVersion-
WithQuotaCheck` with caps set to 0 (the documented "no quota
enforcement" path), so no backend needs an extra unchecked helper.

Each backend supplies a tiny factory and calls `runConformance` with
it. The default `go test ./...` run exercises the shale backend on the
local storage engine (no build tag, no cgo, no external services). The
same suite runs against the slatedb backend under `-tags slatedb` (which also needs cgo +
`libslatedb` on the loader path, and a live S3 endpoint via
`MINIO_TEST_ENDPOINT`, skipping cleanly when unset), and is the
acceptance gate the future
shale backend will run to prove it preserves behavior. Because the
suite asserts only the observable contract, a backend that passes it is
a drop-in for the service layer by construction.

**Tombstoned versions release their bytes.** A deleted version is
app-final and content-inaccessible, and "DeleteVersion frees quota"
implies the storage is freed too, so `DeleteVersion` unbinds that
version's blob in the same transaction that writes the tombstone.
Recoverability does not depend on the app keeping a reference: it is
provided beneath the app by object-store versioning plus a
noncurrent-version lifecycle, an operator-level safety net configured
outside this repo.

### The storage-engine seam

`ShaleRepo` does not name a storage engine. Everything it implements -
the key layout, the durable intents, the scan-derived quota, the
guarded index writes - is engine-independent, and one function chooses
what the cluster mounts underneath:

```go
func openBacking(cfg ShaleConfig) (*backing, error)
```

It has two implementations, selected by build tag:

| build | engine | units | durability |
| --- | --- | --- | --- |
| `-tags slatedb` | slate (SlateDB on an object store) | single or `UnitCount` sharded | durable, shared |
| default | pebble, in memory | single only | process-lifetime |

The default build therefore compiles and exercises the WHOLE shale
implementation with no cgo, no native library and no object store, so
`go test ./...` covers code that previously only a tagged build could
even compile.

The shale tests name no engine: each build supplies `newShaleRepoFor-
Test`, and the SAME test bodies run on pebble by default and on slate
under `-tags slatedb` with a live endpoint. Only the tests that
configure sharding or open a raw slate handle stay tagged, because a
local engine has neither.

Pebble refuses `UnitCount > 0` rather than quietly serving one unit.
Units are mounted and FENCED through a `storageunit.BackendFactory`
whose epoch contract is a property of a store that outlives a node; a
process-local engine has nothing to fence against. Answering a request
for sharded storage with unsharded storage would be a silent
downgrade, so it is an error.

The stored row and key shapes are engine-independent by construction
(`internal/storage/rows.go`): a row written by one engine is readable
by the next, which is what makes the engine a build-time choice rather
than a migration.

### Static-site storage

The "Static site archives" feature persists a **Site** (slug -> owner +
Manifest + timestamps) the same way a paste persists, through a small
`SiteRepo` service-layer interface. This section specifies the **KV
layout** the shale backend uses. The blobs a site references
are unchanged: each extracted file is `Put` under its SHA256 into the
content-addressed `BlobStore`. **Only the manifest plus the site
metadata live in the metadata backend.** Identical files dedupe at the
blob layer regardless of which metadata backend is wired.

**The `SiteRepo` and `SweepSites` interfaces are the contract.** Both are
already interfaces in `internal/service` (`deploy_site.go`, `sweep.go`).
Each backend adds a type that satisfies them; the domain layer (`Site`,
`Manifest`, the safe-untar guards) is backend-agnostic and unchanged.
The deploy path's interface is:

- `InsertWithQuotaCheck(s Site, dedupedSize int, userCap, now)`
- `UpdateWithQuotaCheck(s Site, oldDeduped, newDeduped int, userCap, now)`
  - re-deploy to an OWNED slug in place; charges the replace delta
- `Get(slug) (Site, error)`
- `SumActiveBytesByOwner(owner, now) (int64, error)` (the identity's
  active SITE bytes only; the deploy path adds the paste-side sum)
- `ListSitesByOwner(owner) ([]Site, error)` - the identity's active sites,
  so `ssh <apex> list` can show static sites alongside text pastes (a site
  is a paste; without this it would silently consume
  quota the owner cannot see or free). Enumerates the `identity_sites/<id>/`
  index and re-reads each authoritative `sites/<slug>` row (skipping /
  repairing a stale index entry whose row is gone).

and the sweep path's interface is:

`Delete(slug)` is the owner-facing removal path; it unbinds the site's blobs
in the same transaction that removes the row.

#### Site key layout

Sites mirror the paste key families. The names are new but the shapes are
the established ones (`sites/<slug>` parallels `pastes/<slug>`,
`identity_sites/<id>/<slug>` parallels `identity_pastes/<id>/<slug>`).
Values are JSON unless noted; all keys are UTF-8 strings cast to bytes,
the same as the paste layout:

```
sites/<slug>                       JSON {Identity, Manifest, DedupedSize, CreatedAt, UpdatedAt}
identity_sites/<identity>/<slug>   empty value (for "list/sum sites by identity" prefix scan)
```

The `sites/<slug>` row is the authoritative record. The `Manifest` is
encoded as the compact `{"files": {"<path>": {"sha","size","ct"}}}`
JSON every backend stores, so the on-wire manifest shape is identical
across backends (path -> sha + size + content-type). `DedupedSize` is stored on the row (not recomputed on
read) so the quota scans never have to decode every manifest just to sum
bytes; it is `Manifest.CompressedDedupedSize()` at deploy time - the
distinct-blob total of the STORED (post-zstd) sizes, the number charged
against quota (matching the paste compressed basis). The two index
families carry an empty value (the convention for marker keys,
mirroring `identity_pastes`).

The site keys live in the SAME keyspace and the SAME SlateDB instance as
the paste keys, so a single `Db.begin(SnapshotIsolation)` transaction can
touch both families atomically and a single prefix scan over `sites/`
enumerates every site exactly as `pastes/` enumerates every paste.

#### Operations -> KV mapping

- **Deploy (`InsertWithQuotaCheck`).** Holds the per-identity quota
  stripe (the same `lockQuota(identity)` the paste insert uses, so two
  concurrent same-identity deploys cannot both pass the cap), then:
  1. **Per-identity cap pre-check.** If `userCap > 0`, sum the owner's
     active paste bytes (`identity_pastes/<id>/*` -> non-deleted versions
     of the owner's pastes) PLUS the owner's site bytes
     (`identity_sites/<id>/*` -> `DedupedSize`),
     reject with the over-quota sentinel if `owned + deduped` exceeds the
     cap. There is no
     service-wide byte scan here: the durable total-bytes ceiling is the
     object-store bucket quota (see "Limits"), surfaced as `ErrServiceFull`
     from the blob `Put` when a deploy's blobs would overrun it.
  2. **Slug-collision check, BOTH directions.** Inside the transaction,
     read `sites/<slug>` AND `pastes/<slug>`; if either exists, reject with
     the slug-taken sentinel (whose message contains "slug" so the deploy
     service retries with a fresh slug). A slug is EITHER a site or a
     paste, never both, in either backend: the site insert rejects a slug a
     paste already owns, and the paste insert (unchanged) already rejects a
     slug another paste owns. The read participates in snapshot-isolation
     conflict detection.
  3. **Atomic write.** In one transaction, `Put sites/<slug>` (the JSON
     row), `Put identity_sites/<id>/<slug>` (empty marker), and
     (empty marker). Both land
     or none.
- **Re-deploy (`UpdateWithQuotaCheck`).** Re-deploy to a slug the caller
  already owns. Holds the same per-identity quota stripe, then inside one
  transaction:
  1. **Read `sites/<slug>`.** If missing, OR present but owned by a
     different identity, return the not-found sentinel (the service layer
     surfaces it as *not found*, exit 4, no existence leak). The read
     participates in snapshot-isolation conflict detection.
  2. **Quota pre-check on the DELTA.** The per-identity sum subtracts the
     OLD row's `DedupedSize` and adds the new manifest's `DedupedSize`, so
     an in-place re-deploy is charged only the delta (a same-size re-deploy
     is a no-op against quota; a smaller one frees bytes). Reject with the
     over-quota sentinel if the post-delta total exceeds the per-identity
     cap. The durable total-bytes ceiling stays the object-store quota,
     surfaced as `ErrServiceFull` from the blob `Put`.
  3. **Atomic swap.** `Put sites/<slug>` (new manifest, new `DedupedSize`,
     refreshed `UpdatedAt`), leave `identity_sites/<id>/<slug>`
     in place (owner unchanged). All of it lands or none; the
     old manifest serves until the swap commits. Blobs the old manifest
     referenced are NOT eagerly deleted here - the sweep's
     reference-counted GC reclaims any now-unreferenced blob, exactly as
     it does after a paste version churns.
- **Read (`Get`).** Single `Get sites/<slug>`, decode the JSON row,
  decode the manifest. Returns the not-found sentinel for a missing slug,
  and (like the paste `Get`) returns stale rows too: the HTTP layer
  404s them, the sweep deletes them.
- **Per-identity site bytes (`SumActiveBytesByOwner`).** Scan
  `identity_sites/<id>/`, `Get` each `sites/<slug>`, sum `DedupedSize` of
  the owner's rows. Site-only: the service layer adds the paste sum.

#### Shale reuses the layout

The site key names + JSON row schema are shared (the
same way it reuses the paste layout: co-location is by `ShardKeyFn`, not by
renaming keys). Per-owner BYTE accounting is DERIVED by scanning the
per-owner ENUMERATION index `identity_sites/<id>/<slug>` and summing the
cached deduped size each value-bearing entry carries - one
`{id}`-shard scan, zero per-entry row reads, mirroring the paste quota scan
- and the same enumeration index `ListSitesByOwner` uses to surface a
user's sites in `list`. There is no site byte counter and no site
reservation/release marker: the deploy/replace paths write the cached
values in the same step that writes the row, so a drift needs a crash
between the two and costs one record's bytes in the over-count direction. The full site shard
map:

| Key family | Keys | Shard key |
| --- | --- | --- |
| Authoritative (per-slug) | `sites/<slug>` | `<slug>` |
| Site enumeration index (per-identity) | `identity_sites/<id>/<slug>` | `<id>` |

`sites/<slug>` joins the authoritative `{slug}` family (alongside
`pastes/<slug>`), and `identity_sites/<id>/<slug>` joins the
derived `{id}` family so it co-shards with `identity_pastes/<id>/*` (an
owner's paste-index and site-index scans each stay single-shard). The
`_sites` suffix keeps these from matching the bare
`identity_*` prefixes (the trailing-slash anchoring in `shaleShardKey`):
`identity_sites/` is not any other
`identity_*` family.

**The `identity_sites/<id>/<slug>` index is value-bearing**: the entry is
a JSON projection caching the site's deduped size, which
is exactly what `SumActiveSiteBytesByOwner` sums (one `{id}`-shard scan,
zero per-entry row reads - the site mirror of the paste quota scan).
`ListSitesByOwner` renders from the same cached entries with no per-item
read (docs/SPEC.md "Listing is O(1) reads"). Because `<id>` is the first segment it co-shards with
the paste index, so the index write rides a single-shard `{id}` CAS: the
index-maintenance step after a deploy OR an in-place re-deploy writes the
entry (the re-deploy refreshes the cached size), and `DeleteSite`
deletes it. Index touches are best-effort (a lost write leaves a missing
or stale entry, never a failed deploy). A LEGACY entry written before the
index was value-bearing carries a one-byte marker (shale's `Put` rejects an
empty value; an entry written before the index carried one may be empty): the
quota scan recognizes those two shapes explicitly and falls back to reading
that entry's authoritative `sites/<slug>` row, and `ListSitesByOwner`
rewrites the entry with the full projection the first time it walks it.

**The head row carries the paste's live totals.** `pastes/<slug>` stores
`live_bytes` (the sum of its non-deleted version sizes) and `latest_version`
alongside the served descriptor. Both are maintained in the SAME `{slug}`
transaction that writes or tombstones a version row, so they are
transactionally exact rather than a derived figure that can drift - the head
and its versions co-shard, so there is nothing to co-ordinate.

That is what makes `list` one routed read per item instead of two: the read
that proves the paste exists also supplies its kind, pin, latest version and
live bytes, with no prefix scan of the version family. It is also what the
enumeration entry's cached size is verified against, so the repair below
compares to an authoritative number rather than to a recomputation.

**No periodic reconcile: the index entry is written FIRST.** There is no
maintenance pass, no ticker, and no cross-shard repair job. The write order is
what removes the need for one.

An insert spans two shards - the enumeration entry on `{id}`, the
authoritative row on `{slug}` - and a transaction touches only one. Whichever
is written second can be lost to a crash, so the question is only *which
inconsistency you would rather have*:

| written first | a crash leaves | visible how |
| --- | --- | --- |
| the row | a row with **no entry** | invisible: nothing on the `{id}` shard mentions it, so only a scan of EVERY row on EVERY shard can find it |
| the entry | an entry with **no row** | visible: the entry is right there in the owner's own index, and it is listed to its owner |

hostthis writes **the entry first**, so the surviving inconsistency is the
harmless one. An entry with no row is a PHANTOM: it appears in the owner's
list and its slug 404s. It is not repaired, and nothing scans for it - see
"Phantom entries are accepted, not repaired". Its cached bytes keep counting
against the owner's quota, which is the fail-safe direction (it can refuse an
upload, never admit one past the cap), and the owner can clear it by deleting
it.

The rejected order is the dangerous one for a reason worth stating: a row with
no entry is not merely harder to find, it is unfindable without scanning every
row on every shard - and it UNDER-counts, so the cap can be genuinely
breached. Flipping the write order does not make the crash window smaller; it
makes what survives the crash safe to ignore, which is what allowed the repair
job to be deleted rather than replaced.

**The slug is pre-checked before the entry is written.** `pastes/<slug>` and
`sites/<slug>` are read on the `{slug}` shard first, and a taken slug returns
the collision sentinel with no entry written. Without that, every re-mint in
the upload's collision-retry loop would strand an entry. The pre-check is not
atomic with the authoritative insert, so a genuine race still strands one -
bounded, and left as a phantom.

**A failed insert rolls back only an entry that is genuinely an orphan.** When
the authoritative write fails, the entry written first is normally removed, so a
collision does not charge a would-be owner. It is KEPT in two cases: when the
slug's row turns out to belong to the same identity, and when that row cannot be
read at all. Two callers inserting the same artifact write the one entry key, so
the loser's rollback would otherwise delete an entry the winner's row depends on
- producing the row with no entry above, the state this whole ordering exists to
avoid, and reached by a path no crash-window argument covers. It is the worse
failure in that table arrived at deliberately: the artifact serves every file
and reports its versions while being absent from its owner's listing and free of
charge. An unreadable row counts as the same case rather than as absence,
because the two call for opposite actions and only one of them can be undone.

**What a crash costs, and what bounds it.** A crashed insert leaves an entry
whose cached bytes count against the owner's quota and shows a slug in their
list that does not resolve. That is an OVER-count: it can wrongly refuse an
upload, never wrongly admit one. The previous design had the opposite failure -
a live paste that counted for nothing, so the cap could be genuinely breached.

The residue is not permanent, because the operation records a DURABLE INTENT
before it starts (below). Its lifetime is bounded by the owner's next request
rather than by a background pass, so no cross-shard job is reintroduced to get
that bound.

### Staged blob bytes: the half the intent did not cover

The intent above covers the METADATA halves of a write. It does not cover the
BYTES, and the ordering is why: an upload streams and stages its blobs FIRST,
and only then does the insert open the intent. At the moment the intent exists
the bytes are already staged, so a death before that point leaves bytes with no
record that they were ever attempted. That is not a saga yet - a saga records
the compensating action BEFORE performing the action; this recorded it after
the risky part was done.

Closing it needs the staged object keys written down as they are staged, so
recovery reclaims an EXACT RECORDED LIST rather than scanning and deleting
whatever is absent from a computed set. Acting on absence is the failure mode
the whole blob design exists to avoid: a torn scan deletes live data and no
re-run undoes it.

**One record per staged blob**, at `staged/<scope>/<slug>/<blobid>`, written
immediately after the object lands and before the next file is read. It shards
on the SCOPE, so an owner's staged records co-locate with the intent that owns
them and recovery reads both from the shard it is already on.

**The record is the whole ref, encoded verbatim.** shale derives both the guard
read and the delete key from the ref's fields, so a field lost or corrupted in
this round-trip does not fail loudly - it addresses a DIFFERENT key than the
bind wrote, reads "unbound" for a blob that is bound, and deletes committed
bytes. A missing field is caught by shale's validation; a corrupted-but-present
route shard is not. So the encoder takes the struct whole and never names
individual fields, and a field shale adds later rides along without this code
being taught about it.

The records are dropped when the intent completes: a committed write's bytes
are bound and must never be unstaged.

### Durable intent: the saga survives the process

The compensating action already exists - an authoritative write that FAILS
deletes the entry it just wrote. The gap is narrower than "the write order is
wrong": it is that this compensation lives on the handling goroutine's stack,
so a process death is the one failure that loses the knowledge that cleanup is
owed.

The fix is to persist that knowledge BEFORE acting. The ordering property that
makes it work is that the intent is written FIRST, so its ABSENCE is
unambiguous: no intent means nothing was attempted, and there is nothing to
recover. (Contrast two-phase commit, whose decision record sits in the MIDDLE
of the protocol - which is why 2PC needs a resolver for in-doubt state and this
does not.)

```
T0  record intent                      durable
T1  write identity_pastes entry        {id} shard
T2  write pastes row + version rows    {slug} shard
T3  forget the intent                  durable
```

| crash after | on disk | resolution |
| --- | --- | --- |
| T0 | intent only | nothing was written; forget the intent |
| T1 | intent + entry | row absent -> drop the entry, forget the intent |
| T2 | intent + entry + row | the write succeeded; forget the intent |
| T3 | clean | - |

Every state is distinguishable from the intent plus one existence check, and
each has one defined action. That is the property the design turns on: without
the intent, a crashed insert is INDISTINGUISHABLE from a phantom that a
concurrent uploader is legitimately mid-way through creating, which is why
nothing could safely act on one.

**Resolution is a node-local boot sweep.** Each node, once it is serving,
scans the intents on the units it has MOUNTED and resolves them. That scan is
local: it walks this node's own storage, so it involves no network fan-out even
though the intent family logically spans every shard. Across the fleet every
unit is covered, because every unit is mounted by someone.

It runs after the node goes live, not before. Deciding an intent's outcome
requires reading the authoritative row, which lives on a DIFFERENT shard - one
that may not be mounted anywhere yet during a cold start. Gating readiness on
that read would deadlock a cold cluster: no node could serve until it swept,
and no node could sweep until some node served. Going live first costs nothing,
because the residue it cleans up was already there.

**Resolution rolls FORWARD or BACK; it decides by looking.** An incomplete
intent has two shapes and they need opposite treatments:

| authoritative row | meaning | action |
| --- | --- | --- |
| present | the write succeeded; only the bookkeeping was lost | forget the intent |
| absent | the write never landed | drop the entry, then forget the intent |

Treating every incomplete intent as a rollback would DELETE live pastes whose
only fault was losing the final step. The row's existence is the discriminator,
and it is read per intent - affordable because the normal outstanding count is
zero.

**An intent younger than the resolve grace is left alone.** This is the part
that is not optional. Which pod HANDLES a request is chosen by the load
balancer; which pod STORES that request's intent is chosen by hashing the
owner. The two are unrelated, so a node's own local intents are routinely
created by uploads that OTHER nodes are handling right now. A sweeping node
therefore sees in-flight work, and an intent that is mid-flight is
indistinguishable from one whose process died.

The value guard does not help here: a live upload's entry MATCHES the intent
that describes it, so a guarded delete would fire and take the entry out from
under a running request. Only elapsed time separates the two cases. The grace
MUST exceed the longest plausible upload, the same contract the blob plane's
orphan grace already carries.

So the existence check decides WHAT to do, and the grace decides WHETHER it is
safe to act yet. Both are required; neither substitutes for the other.

**Resolution is idempotent and loses to live traffic.** Several nodes can
resolve concurrently - units are replicated, so more than one node may hold a
given intent. Every step is safe to re-run, and the compensating delete is
VALUE-GUARDED: it removes the entry only while that entry still holds the
payload the intent describes, so a re-upload that landed after the crash
survives. There are no locks and no leases.

**The owner's own listing settles their residue too.** `ListByOwner` resolves
that owner's outstanding intents BEFORE it scans, so the listing never renders a
phantom the very next read would have removed, and no restart is needed for an
owner who comes back. It rides a shard the read is already talking to, and in
the normal case is one prefix scan returning nothing.

It is best-effort: a resolver failure is logged and swallowed, because the
caller is serving a user's read. And it obeys the grace exactly as the sweep
does - a read is not a licence to act on an intent another node may still be
mid-write on.

This is an optimization, not the mechanism. Correctness rests on the boot sweep,
which is what covers an owner who never returns.

**The durability mechanism is a port, not a layer.** The intent log is defined
as a narrow interface in terms of intent and resolution - begin, advance,
complete, and list-outstanding-for-one-owner - with no key, value, shard, or
workflow vocabulary in it. The steps are recorded as DATA rather than closures,
because resolution may run in a different process than the one that began the
work. `Outstanding` is scoped to a single owner rather than global, which is
what keeps both this implementation (one prefix scan) and any other one (a
bounded query) cheap.

The default implementation stores intents in the metadata cluster. Nothing
above the repository knows that: the application service and the repository
port are unchanged, and a backend with a single transaction
has no dual write and therefore no intent log at all.

One optimization is deliberately NOT taken. Intents and enumeration entries
both shard by owner, so T0 and T1 could commit as one CAS, removing that gap
entirely. It is declined because it would require intents to live in this
specific cluster, co-sharded with the entries - which is exactly the coupling
the port exists to avoid. The extra recoverable state is the price of keeping
the mechanism swappable.

A site deploy spans the `{slug}` shard (the authoritative `sites/<slug>`
write + the cross-family paste-slug collision read) and the `{id}` shard
(the enumeration index entry), which is two CASes, but there is no counter to
reserve against, so the deploy is a plain sequence: check quota (scan), write
the `identity_sites/<id>/<slug>` enumeration entry (value-bearing: the cached
deduped size the quota scan sums), then write the authoritative `{slug}` row -
entry first, for the reason given above - matching
a single-transaction backend - and `StrictIdentityQuotaUnderConcurrency` is `false` (the
scan-check and the row-write are not atomic; the bounded same-owner
over-admit is the accepted trade, see "The correctness argument").

**The two sums stay disjoint (paste sum + site sum).** The deploy service
computes the per-owner budget as `UserQuota - paste_bytes - site_bytes`,
reading the paste sum and the site sum SEPARATELY and adding them, so the two
scans MUST count disjoint sets: `SumActiveBytesByOwner` sums the owner's
`identity_pastes` entries, and `SumActiveSiteBytesByOwner` sums the owner's
`identity_sites` entries. Because pastes and sites live in disjoint key
families enumerated by disjoint indexes, adding the two sums never
double-counts, and
`SumActiveSiteBytesByOwner` stays site-only (the conformance contract: a
site-only owner sum).

The per-owner cap is SYMMETRIC across both kinds: a deploy of EITHER kind
checks the owner's COMBINED paste + site bytes against `userCap`, so the
ceiling holds no matter how an owner splits their quota between pastes and
sites. This sums both kinds
and is read by BOTH the paste insert and the site deploy. Concretely:
  - a SITE deploy's check scans BOTH the paste sum and the site sum and
    verifies `paste + site + deduped <= userCap`, and
  - a PASTE insert / append's check scans BOTH sums and verifies
    `paste + site + body <= userCap`.
Without the second of those, a paste could be accepted while the owner's
site bytes were ignored: e.g. an 800-byte site plus a 300-byte paste under a
1000-byte cap would wrongly admit the paste (combined 1100 > 1000) even
though the symmetric site direction correctly rejects it. The
`Sites/PerOwnerCapCountsBoth` conformance subtest pins both directions.

The durable total-bytes ceiling is NOT a shale aggregate scan: it is the
object-store bucket quota (see "Limits → Durable total-bytes ceiling"),
surfaced as `ErrServiceFull` from the blob `Put`. The shale site repo runs
no service-wide byte sum on the deploy path; only the per-identity scan
(bounded to one owner's slugs) gates a deploy at the app layer.

The cross-family paste-slug collision read is added to the site
authoritative write (reject a slug a paste owns), and the paste
authoritative write already rejects a slug a site owns, so a slug is EITHER a
site or a paste, never both, in both directions. Neither path leaves a marker
any background pass must complete - the only per-owner `{id}` state a
deploy or delete writes is the enumeration index entry.

**Status: implemented + conformance-tested.** Prod runs shale, which shares
the scan-derived quota shape (the enumeration index + the cross-family
collision read) with the rest of the artifact families. Every backend runs the
SAME conformance site subtests under the SAME factory, so each is a drop-in for
static-site hosting by construction.

#### Wiring: widen the metadata bundle's `Sites` field

`cmd/hostthisd/metadata.go` holds `Sites` as the `service.SiteRepo`
interface (the deploy view) plus the `service.SweepSites` view (the sweep
view), rather than any backend's concrete type, so any backend's site
impl can be assigned. The bundle stays nil-safe: a backend
that does not supply a site impl leaves the field nil and static-site
hosting stays disabled there.

#### Conformance

The backend-agnostic conformance suite is extended with site operations so
every backend is pinned to behave IDENTICALLY for sites, the
same way they are pinned for pastes. The site contract the suite asserts:
deploy a site and read every path back byte-identically (manifest
round-trip), list/sum a site's bytes by identity, the per-identity quota
counts SITE bytes (a site fills the owner's quota a paste then sees, and
vice versa), the slug-collision rejects a slug a paste already owns
(and a paste rejects a slug a site owns). A backend that passes
the extended suite is a drop-in for static-site hosting by construction.

### Room storage

The "Rooms (app persistence)" feature persists a **Room** (the owning
app's slug + a UUIDv4 + a flat key-value namespace) plus a creation-rate
ledger. This section specifies the **KV layout** the shale backend uses. Rooms hold no blobs: a room value is small, mutable app STATE that
lives entirely in the metadata backend (the content-addressed `BlobStore`
is untouched), so unlike pastes and sites a room contributes nothing to the
blob-GC keep-alive set.

**The `RoomRepo` and `SweepRooms` interfaces are the contract.** Both are
already interfaces in `internal/service` (`rooms.go`, `sweep.go`). Each
backend adds a type that satisfies them; the domain layer (`Room`,
`RoomID`, the pure `RoomKV` cap math) is backend-agnostic
and unchanged - only the storage layer is backend-specific. The
room-write / read interface is:

- `CreateRoom(room Room, subnet, appCap, now)` (mint an empty room, record
  the creation-accounting row, enforce the per-app aggregate cap; the
  creation rate-limit DECISION is made by the service via
  `CountRoomCreates` BEFORE this call, so the gate is a soft bound while the
  per-app byte cap is a hard one)
- `GetRoom(appSlug, id) (Room, error)`
- `GetValue(appSlug, id, key) ([]byte, error)`
- `ScanRoom(appSlug, id) (RoomKV, error)`
- `PutValue(appSlug, id, key, val, appCap, now)` (per-room +
  per-app caps; moves `UpdatedAt`)
- `DeleteValue(appSlug, id, key, now)` (idempotent; moves `UpdatedAt`)
- `CountRoomCreates(appSlug, subnet, now, window) (perSubnet, perApp, err)`

There is no sweep-side room interface. Creation-ledger rows past the
rate-limit window are dropped by `CountRoomCreates` itself, on the scan it
already runs to make the admission decision. `DeleteRoom(appSlug, id)` is the
idempotent full cascade the owner-facing removal path uses.

#### Room key families

Rooms have MORE key families than sites because they carry per-key values,
and a creation-rate ledger. Each is co-located in the SAME
SlateDB instance and the SAME keyspace as the paste + site keys, so a room
create or write is one `Db.begin(SnapshotIsolation)` transaction and a
per-app prefix scan over `roomkv/<app-slug>/<uuid>/` enumerates exactly one
room's values. All keys are UTF-8 strings cast to bytes; values are JSON
unless noted:

```
rooms/<app-slug>/<uuid>                    JSON {CreatedAt, UpdatedAt} (the room record)
roomkv/<app-slug>/<uuid>/<key>             raw value bytes (the stored KV pair, verbatim; not JSON)
roomcreate/<app-slug>/<subnet>/<ts>/<uuid> empty value (one per room created; ts is fixed-width; the trailing uuid disambiguates two rooms created at the same ts, see below)
```

The `roomcreate` key carries the created room's `<uuid>` as a trailing
segment: a KV key is unique, so without the `<uuid>` two rooms created
under the same `(app, subnet)` within
the same fixed-width `<ts>` (the same nanosecond, common when a test or a
script mints rooms in a tight loop) would collide on one key and overwrite,
undercounting the rate-limit ledger. The `<uuid>` makes each creation a
distinct key. The `<ts>` is then the SECOND-to-last segment for the windowed
count + prune compares (the `<subnet>` itself contains a `/`, so both parsers
strip the two trailing slash-free segments from the right to recover
`(subnet, ts)`).

The `rooms/<app-slug>/<uuid>` row is the authoritative record. On the
single-writer local backend it holds the clock only: the byte and key counts are
computed by scanning `roomkv/<app-slug>/<uuid>/` at PUT time, which
materializes the namespace for the pure `RoomKV.CanPut` cap math,
serialized by the per-room `lockQuota` stripe. The **shale** backend
additionally
stores a running `byte_total` + `key_count` on this record (the `roomRow`
shale-only fields), because shale validates the per-room cap inside a CAS and a
CAS read-set cannot carry a scan, so it needs a discrete in-record total the
read-set can read-check (see "Shale reuses the layout" -> "Per-room cap
(strict)").

EVERY backend additionally maintains the room's **durable sequence** on
this record: a `seq` field, the dense
per-room counter the relay's cross-pod ordering rides (see "Multi-pod
relay: broadcast fan-out ordered by a durable per-room sequence"). It
is incremented in the SAME transaction / CAS as every value PUT and
DELETE - the record is already rewritten there for the clock
touch - and returned to the caller as the mutation's position in the
room's history. The storage contract's share of the design is exactly
three properties, pinned by the conformance suite: the seq is dense
(+1 per committed mutation, no gaps at the source, concurrent writers
never share or skip one), it is assigned at commit, and `ScanRoom`
reports the exact seq its snapshot reflects (a single-transaction backend reads it
inside the scan's own transaction / stripe; shale, whose CAS read-set
cannot carry a scan, runs the read-scan-reread seq fence and retries on
motion). A legacy record with no `seq` field decodes as 0, so the first
post-upgrade mutation assigns 1 - no migration of existing rooms is
needed on the KV backends.

A value is stored **verbatim** (not
JSON-wrapped): hostthis never parses a room value, so `roomkv/...` holds
the exact bytes the app PUT, and a Get returns them unchanged. The two
marker family (`roomcreate/`) carries an empty value, the
convention for index keys (mirroring `identity_pastes` /
`identity_sites`).

The `<key>` in `roomkv/<app-slug>/<uuid>/<key>` is the app-chosen key
validated by `ValidateRoomKey` (non-empty, `<= MaxRoomKeyLen`); the
`<uuid>` is the canonical lowercase 8-4-4-4-12 form `ParseRoomID` returns,
so a forged or wrong-version id is rejected at the HTTP boundary before it
can ever reach a key builder. The `<app-slug>` is the validated 8-char
slug. None of these three segments can contain a `/` that would let one
room's key prefix collide with another's (see Strict isolation below).

#### Strict isolation is structural in the key shape

Every value key is `roomkv/<app-slug>/<uuid>/<key>`, the namespacing
triple `(app-slug, room-uuid, key)` in path order. The isolation
guarantees fall out of the key shape, not a runtime filter:

- **Cross-room.** A Get / Put / Delete builds the key from the
  request's own `<uuid>`, so a request carrying room A's UUID can only ever
  address keys under `roomkv/<app>/A/`; it cannot read or write
  `roomkv/<app>/B/...`. A whole-room scan is `ScanPrefix
  roomkv/<app-slug>/<uuid>/` - bounded to exactly one room's subtree, so it
  cannot enumerate another room.
- **Cross-app.** The app slug is the outermost variable segment, so
  `roomkv/app1/<uuid>/...` and `roomkv/app2/<uuid>/...` are disjoint
  subtrees even for an identical UUID. The per-app prefix scans
  (`roomkv/<app-slug>/`, `roomcreate/<app-slug>/`) are anchored at the app
  segment, so one app's aggregate / creation count never sees another's.
- **Nonexistent-room 404, no existence leak on the per-key path.** A
  per-key Get is a single `Get roomkv/<app>/<uuid>/<key>`: a missing key
  in a real room and a key under a nonexistent room both return the
  not-found sentinel (the same `ErrNotFound` the paste / site Get
  returns), so the per-key path cannot distinguish "no such room" from "no
  such key." The service layer does the `GetRoom` existence check
  separately where it needs the whole-room-scan 200-vs-404 distinction
  (the existence signal scoped to a holder of the 122-bit UUID, per the
  Strict-isolation section above).

Because the UUID is validated up front and the segments are slash-free,
there is no key whose prefix is another room's, and a guessed or forged id
addresses an empty subtree - the same "the identifier IS the capability,
the storage layer enforces the namespace boundary" posture pastes and sites
rely on.

#### Operations -> KV mapping

- **Create (`CreateRoom`).** Holds the **per-app** quota stripe (the
  `lockQuota(app-slug)` analogue of the paste / site per-identity stripe,
  here keyed on the app slug because the room aggregate cap is per-app, not
  per-identity), then in one transaction:
  1. **Per-app aggregate pre-check.** If `appCap > 0`, sum the app's
     current room bytes (scan `roomkv/<app-slug>/`, sum value lengths of
     rooms) and refuse a new room with the app-rooms-full
     sentinel once the app is already at its byte cap. (A brand-new room is
     empty, but bounding creation here keeps a full app from accumulating
     unbounded empty rooms.)
  2. **Collision check.** Read `rooms/<app-slug>/<uuid>`; an existing row
     surfaces the slug-taken sentinel so the service retries with a fresh
     UUID (astronomically unlikely for a v4).
  3. **Atomic write.** `Put rooms/<app-slug>/<uuid>` (the JSON record:
     `CreatedAt = UpdatedAt = now`),
     and `Put roomcreate/<app-slug>/<subnet>/<ts>` (empty marker, the
     rate-limit ledger row). Both land or neither.
  The creation rate-limit COUNT is read by the service via
  `CountRoomCreates` OUTSIDE this transaction, so the
  creation gate stays a SOFT bound (N concurrent creators can each read the
  same in-window count and all pass) while the per-app byte cap is the hard
  structural bound enforced inside the tx.
- **Per-key read (`GetValue`).** Single `Get roomkv/<app>/<uuid>/<key>`,
  return the bytes verbatim or the not-found sentinel.
- **Whole-room scan (`ScanRoom`).** `ScanPrefix roomkv/<app>/<uuid>/`,
  rebuild the `RoomKV` map (strip the key prefix to recover each app-chosen
  `<key>`). An existing room with no values scans to an empty (non-nil)
  namespace; the service layer's prior `GetRoom` is what turns a
  nonexistent room into a 404 rather than an empty 200.
- **Write (`PutValue`).** Holds the **per-room** quota stripe
  (`lockQuota(app-slug + "/" + uuid)`) across the read + write, so two
  concurrent writes to the SAME room cannot both pass a stale cap check and
  both commit (valid because SlateDB is single-writer: only in-process
  goroutines can race, and the stripe serializes same-room writers). Then,
  inside one transaction:
  1. **Room-exists re-check.** `Get rooms/<app>/<uuid>`; the not-found
     sentinel if the room is gone (re-checked inside the write boundary so
     a concurrent delete cannot remove the room between the service's
     `GetRoom` and this write).
  2. **Per-room cap.** Scan `roomkv/<app>/<uuid>/`, materialize the
     `RoomKV`, run the pure domain `CanPut` (byte total `<= MaxRoomBytes`,
     key count `<= MaxRoomKeys`, value `<= MaxRoomValueBytes`); reject with
     the room-data-full sentinel (413) on a fail, prior value intact.
  3. **Per-app aggregate cap.** Charge only the byte DELTA (replacing a key
     frees its old bytes). If `appCap > 0` and the delta is positive, sum
     the app's room bytes and reject with the app-rooms-full sentinel (507)
     if `total + delta` exceeds `appCap`.
  4. **Upsert + touch.** `Put roomkv/<app>/<uuid>/<key>` (the new value),
     then touch the room: read the room record and write it back with
     `UpdatedAt = now`.
- **Delete (`DeleteValue`).** Re-check the room exists (not-found if gone),
  `Delete roomkv/<app>/<uuid>/<key>` (idempotent: a missing key is a
  no-op - the post-condition "the key is gone" holds either way), then
  touch the room (a delete is a write, so it moves `UpdatedAt`).
- **Per-app room bytes / creation count.** The per-app aggregate sum is a
  `ScanPrefix roomkv/<app-slug>/` summing value lengths: bytes leave the
  per-app cap when a room or value is deleted. `CountRoomCreates` scans
  `roomcreate/<app-slug>/` (per-app) and **drops the markers it finds past the
  window as it goes** - they can no longer change any decision, so the read
  that would skip them removes them, which is what keeps the family bounded
  with no background pass and no fan-out. The per-subnet count walks the same
  per-app family and matches the `<subnet>` segment (the same way
  `SubnetsForIdentity` walks `keygate/`), counting markers whose fixed-width
  `<ts>` (the second-to-last segment, before the trailing disambiguator
  `<uuid>`) is within the window.

#### No service-wide byte scan on the room path

Rooms run NO service-wide byte sum. The room write path is gated only by
the per-room cap and the per-app aggregate; a room holds no blobs, so it
touches no object-store quota, and the durable total-bytes ceiling (the
object-store bucket quota, see "Limits → Durable total-bytes ceiling")
bounds only the blob-holding kinds (pastes + sites). The removed
`sumServiceWideActiveBytes` aggregate - which the room path once
participated in alongside pastes and sites - no longer exists on any
backend, so there is no cross-kind byte scan for a room PUT to run or to
fold its bytes into.

#### Shale reuses the layout

The room key names + JSON record schema
(co-location is by `shaleShardKey`, not by renaming keys), joining the room
families to the existing shard-family scheme so a room read or write is a
single-shard operation. The room shard map:

| Key family | Keys | Shard key |
| --- | --- | --- |
| Room record (per-app) | `rooms/<app-slug>/<uuid>` | `<app-slug>` |
| Room value (per-app) | `roomkv/<app-slug>/<uuid>/<key>` | `<app-slug>` |
| Room-create ledger (per-app) | `roomcreate/<app-slug>/<subnet>/<ts>/<uuid>` | `<app-slug>` |

All three families shard on `<app-slug>`, co-locating an app's rooms, all
their values and its creation ledger on ONE shard - the same co-location
discipline the `{slug}` / `{id}` / `{subnet}` families use, and the reason
"load the whole room," "write one key," and "count this app's creations"
are each single-shard operations rather than cross-shard fan-outs. The
`<app-slug>` is the first segment after every room family's prefix, so
`shaleShardKey` extracts it directly. The `room`-prefixed family names
do not collide with the `pastes/` / `sites/` / `rooms`-vs-`roomkv`
anchoring (the trailing-slash discipline in `shaleShardKey`:
`roomkv/` is not `rooms/`).

The strict-isolation, per-room-cap, and per-app-aggregate-cap properties
carry over unchanged. Both the per-room cap and the
per-app aggregate cap are STRICT under concurrency on shale, but they are
enforced by two DIFFERENT read-set members of the one `{app-slug}`-shard CAS,
because a CAS read-set is a set of discrete key checks (shale forbids
`ScanPrefix` inside a transaction - a scanned range has no cheap phantom
protection), so a cap whose magnitude is "the sum over a key range" must be
backed by a discrete COUNTER the CAS can read-check, not by an in-CAS scan:

- **Per-app aggregate cap (strict).** A per-app room-byte counter
  `roombytes/<app-slug>` is read-checked and incremented inside the
  `{app-slug}` value-write CAS, so two concurrent writers to the SAME app
  cannot both pass a stale per-app sum and overshoot `appCap`. This counter is
  legitimate where the per-identity paste/site byte counter was not: because
  all five room families (the four above + this counter) share the one
  `{app-slug}` shard, a room create or write is already single-shard, so the
  value write + the counter update are ONE atomic CAS - there is no cross-shard
  split between the counted rows and the counter, so it cannot drift the way a
  per-identity counter (rows on `{slug}`, counter on `{id}`) would. That is
  exactly why the per-identity quota is scan-derived while this per-app
  aggregate stays a counter.
- **Per-room cap (strict).** The per-room byte total + key count are stored ON
  the room record (`rooms/<app-slug>/<uuid>`, the `roomRow` `byte_total` +
  `key_count` fields, shale-only) and validated against `MaxRoomBytes` /
  `MaxRoomKeys` INSIDE the same CAS. The room record is already read-checked in
  the read-set (the room-exists re-check) and rewritten on every PUT / DELETE
  (the clock touch), so two concurrent writers to DISTINCT keys of the same
  room - which target different value keys and so would NOT conflict on the
  value-key read-check alone - DO conflict on the shared room-record read-check:
  the second writer's CAS conflicts after the first commits, the retry re-reads
  the now-updated `byte_total` / `key_count`, and the per-room cap is recomputed
  against the fresh totals. So the per-room ceiling holds no matter how the
  writes interleave (`conformCaps.StrictQuotaUnderConcurrency = true`). The
  totals are maintained ONLY by the sharded shale backend (a single-writer one leaves them
  unset and computes the per-room cap by materializing the namespace under a
  serialized writer - its per-room `lockQuota` stripe - so it needs no
  stored total); since a room is only ever
  written by one backend's store, the shale-only fields are inert on the others.

The shale room path runs NO service-wide byte sum either: the durable
total-bytes ceiling is the object-store bucket quota, and a room holds no
blobs, so the room write is bounded only by the per-room and per-app caps.
A room's bytes leave the per-app counter when the room or the value is
deleted: the per-app aggregate IS a maintained counter, whereas the
per-identity quota is a live scan. The
`Rooms/PerRoomCapConcurrentCeiling` conformance subtest fires N concurrent
distinct-key writes into one room against the structural `MaxRoomBytes` cap and
asserts the persisted byte total never breaches it - the gate that pins the
per-room strictness above on every `StrictQuotaUnderConcurrency` backend.

**Empty room values on shale.** shale's `Put` rejects empty values (the
empty payload is reserved for delete tombstones), but a room value may
legitimately be the empty byte string (`PUT`ting `""` is valid app state on
a local engine). To keep the verbatim-round-trip contract IDENTICAL across every
backend, a stored shale room value is prefixed with one sentinel
byte; the decode strips it to return the app's exact bytes (including the
empty string). All room BYTE accounting (the per-app counter, the per-room
cap) charges the DECODED length, so the byte totals
match exactly - an empty value counts as 0 bytes. This is a
shale-internal encoding detail; the observable Get/Scan contract is the same
verbatim bytes on every backend, and the conformance `Rooms/RoundTrip`
subtest exercises an empty value to pin it.

**Status: implemented + conformance-tested.** Prod runs shale, whose room
layout is co-located on the per-app shard. Every backend runs the SAME
conformance room subtests under the SAME factory, the way it already does for
pastes and sites, so each is a drop-in for the room-persistence tier by
construction.

#### Wiring: widen the metadata bundle's `Rooms` field

`cmd/hostthisd/metadata.go` holds `Rooms` as a `roomStore` interface (the
`service.RoomRepo` write/read view plus the `service.SweepRooms` sweep view,
the union the service + sweep layers consume) rather than any backend's
concrete type, so any backend's room impl can be assigned - mirroring how the
`Sites` field is held as the `siteStore` interface. The bundle stays nil-safe:
a backend that does not supply a room impl leaves the field nil and the
`/api/rooms` surface stays disabled there.

#### Conformance

The backend-agnostic conformance suite is extended with room operations so
every backend is pinned to behave IDENTICALLY for rooms, the
same way they are pinned for pastes and sites. The room contract the suite
asserts (each subtest must FAIL on intentionally-weakened code, the TDD
gate):

- **Round-trip.** Create a room, PUT values under several keys, GET each
  back byte-identically, SCAN the whole namespace and observe every pair.
- **Cross-room + cross-app ISOLATION.** A second room's UUID (and an
  identical-UUID-shaped string under a second app) cannot read, write, or
  scan the first room's data; a scan is bounded to exactly one room's
  subtree. This subtest must FAIL if the namespacing is weakened (a key
  builder that dropped the app or room segment, a scan whose prefix was
  broadened).
- **Nonexistent-room 404.** A per-key GET / PUT / DELETE on a well-formed
  but nonexistent room returns the not-found sentinel - the same shape as a
  missing key in a real room (no per-key existence leak).
- **Per-room cap.** A PUT that would push the room past `MaxRoomBytes` or
  `MaxRoomKeys` is rejected, prior value intact.
- **Per-app aggregate cap.** A PUT that would push the app's total room
  bytes past `appCap` is rejected (the per-app structural bound; rooms run
  no service-wide byte scan, the durable ceiling being the object-store
  bucket quota).
- **Creation rate limit.** The per-subnet and per-app in-window counts the
  service gates on are accurate after N creations, and a windowed prune
  drops past-window ledger rows.
- **App-existence 404.** (Pinned at the HTTP layer, where the existence
  gate lives - the room repo itself is not app-existence-gated, the same
  way the paste repo is not owner-gated; the repo-level conformance pins
  that room creation under any slug succeeds, and the HTTP-layer test pins
  the 404 for a slug that names no live app.)
- **Delete cascade.** `DeleteRoom` removes the room record and every value
  in its namespace, and is an idempotent no-op on a room that is already
  gone.

A backend that passes the extended suite is a drop-in for the
room-persistence tier by construction.

---

## Shale-backed metadata storage (horizontal scale)

The local backend above is single-writer: one process
owns the keyspace, transactions serialize through one engine. That caps
sustained write throughput and ties durability to one node's liveness.
A third metadata backend, **shale**, removes both ceilings by sharding
the keyspace across a cluster of nodes, each holding a slice, with
optional replication for high availability.

Shale is a KV cluster (consistent-hash ring over a shared object-store
backend) that exposes per-shard compare-and-swap transactions and a
cross-shard fan-out for admin scans. The metadata layer talks to it
through the same service interfaces as the other two backends, so the
domain, SSH, and HTTP layers are unaware of the choice. This section
describes how the existing key layout maps onto shards, how quota stays
strict across shard boundaries, and how the derived per-identity indexes
stay correct under eventual consistency.

### Same interfaces, same key names

The shale backend (`ShaleRepo`) implements the same four service-layer
interfaces every metadata backend implements: `PasteRepo` (insert +
get on the upload path), `PasteAdmin` (list / versions / flags / delete
+ the per-owner quota and first-seen accessors), `SweepRepo` (the
referenced-blob set), and `KeyGateRepo` (Sybil admission).

The key names:

```
pastes/<slug>                      paste row
versions/<slug>/<NNNN>             version row
slug_owner/<slug>                  raw identity string
identity_pastes/<identity>/<slug>  per-owner enumeration index (value-bearing, see below)
identity_first_seen/<identity>     cached first-seen timestamp
keygate/<subnet>/<identity>        Sybil first-seen timestamp
```

There is deliberately **no per-owner byte counter** and **no reservation
marker**. The per-identity quota is DERIVED by scanning the owner's
`identity_pastes` enumeration index and summing the cached size each
value-bearing entry carries - one single-shard
prefix scan, zero per-entry fan-out (see "Scan-derived quota" below). The
write paths keep the cached values fresh, in the same step that writes the
row they describe. An earlier design kept a
stored `identity_bytes/<id>` counter maintained by a cross-shard
reservation pattern (reserve -> write -> confirm, plus reservation and
release markers, a background reservation reconciler pass, a crash-durable
release-marker delete, and an offline audit). It was removed: a stored
numeric aggregate that lives on a shard SEPARATE from the rows it counts
cannot be idempotently healed (a scan can only OVERWRITE it, and an
online overwrite races the writes it is trying to sum), so it drifts
permanently. A scan over a SET - the enumeration index, each entry written
alongside the row it projects - cannot durably drift the way a foreign-shard
aggregate does: an error is confined to the one record whose write was lost. All the reservation/marker/audit machinery existed ONLY to keep
that counter correct, so it is deleted with the counter.

Co-location across shards is achieved by a custom shard-key function,
**not** by renaming keys. The cluster is opened with a `ShardKeyFn` that
extracts a shard key from each full key per its family.

### Three shard families

Every key belongs to exactly one of three families. The `ShardKeyFn`
extracts the shard key as follows:

| Key family | Keys | Shard key |
| --- | --- | --- |
| Authoritative (per-slug) | `pastes/<slug>`, `versions/<slug>/*`, `slug_owner/<slug>` | `<slug>` |
| Derived (per-identity) | `identity_pastes/<id>/*`, `identity_first_seen/<id>` | `<id>` |
| Sybil gate (per-subnet) | `keygate/<subnet>/*` | `<subnet>` |

The authoritative family is the source of truth for a paste's existence
and content. The derived family is a denormalized projection of it,
sharded by owner so that "list my pastes" is a single-shard scan rather
than a full-keyspace scan. "How many bytes do I own" is the same
single-shard enumeration scan, summing the cached size each entry carries -
no fan-out to the `{slug}` shards at all (the write paths maintain the
cached values; see "Scan-derived quota").
The Sybil gate family is sharded by subnet so admission decisions for one
subnet touch one shard.

Because every key in a family hashes to the same shard key, a
transaction that touches **only one family's keys for one subject** is
a single-shard transaction and commits through shale's per-shard CAS
(`Transact(pinKey, fn)`: read-modify-write under optimistic concurrency,
retried on conflict, returning a conflict-exhausted error if it cannot
converge). A write that spans families (a slug's authoritative row plus
its owner's derived counter) is **not** one transaction: it is a
sequence of single-shard transactions on different shards, and the
design below is what keeps that sequence correct.

### Scan-derived quota (never durably exceeds the cap)

Per-identity quota (the 10 MiB compressed cap) is enforced by **deriving**
the owner's used bytes from a scan at check time - not by a maintained
aggregate counter.

The single-writer backends compute the sum from the authoritative rows
inside the insert's own transaction. The local engine walks the
owner's `identity_pastes` / `identity_sites` index and reads each live
row's size under a per-identity `lockQuota` stripe - cheap, because every
read is a local engine read. On shale those per-record reads are
cross-shard RPCs, so the shale backend sums the CACHED values its
value-bearing enumeration entries carry instead (one single-shard scan;
the write paths keep the cache converged with the authoritative rows,
below). Shale also cannot wrap the check and the write
in one transaction (they touch different shards), so the check and the
write are two steps rather than one, which relaxes strictness under
same-identity concurrency (below) but never allows a DURABLE breach of
the cap.

**How the scan works.** `SumActiveBytesByOwner(owner)` on the shale
backend:

1. Scan the owner's `identity_pastes/<id>/` enumeration index on the
   `{id}` shard (one single-shard prefix scan). Each entry is
   value-bearing: it caches the paste's live byte size (the sum of its
   non-deleted version sizes).
2. Sum the cached sizes: an entry that cannot be read - undecodable, or
   carrying the placeholder marker - counts as ZERO and the scan continues
   (see "Unreadable entries fail OPEN"). No authoritative row is read: the
   check is ONE prefix scan with zero per-entry fan-out. (One deliberate
   exception, the upgrade path: a LEGACY paste entry migrated from a
   older deployment carries an EMPTY value - that layout stored
   `identity_pastes` as bare markers - and is read through its
   authoritative `pastes/<slug>` row plus live version sum, exactly the
   pre-enrichment per-entry semantics, until the owner's next list enriches it
   with the full projection.)
3. Return the total. `SumActiveSiteBytesByOwner` is the site sibling:
   enumerate `identity_sites/<id>/` and sum each entry's cached deduped
   size under the same rules. (A
   legacy site entry that still carries the pre-value-bearing marker byte
   or an empty migrated value falls back to reading its authoritative
   `sites/<slug>` row until the owner's next list enriches it.)

`CountByOwner` uses the same enumeration scan (count the entries), so its
count matches what `ListByOwner` renders.

**Sum the cached index values; the write paths keep them fresh.** The
`identity_pastes` entry is value-bearing (it caches
`name/size/created_at`). The quota scan sums the CACHED size directly -
the index is both the enumeration AND the
measure. The alternative (use the index only to enumerate, read every size
from the authoritative rows) costs one head read plus one version scan per
enumerated slug, so on a high-cardinality owner every upload's quota check
becomes thousands of sequential cross-shard reads; the cached sum is O(1)
scans regardless of how many pastes the owner holds. The freshness
contract that makes the cached sum safe:

- **Every size-changing operation maintains the cached size.** The cached
  `size` is the paste's live byte sum (its non-deleted version sizes, the
  same figure a scan of the authoritative rows computes). The insert's index write seeds it
  (v1's size); `AppendVersionWithQuotaCheck` refreshes it (together with
  after the version commits; `DeleteVersion` refreshes it
  after the tombstone commits; `Delete` and `MarkFailed` drop the entry
  outright. Each refresh is a synchronous best-effort `{id}`-shard CAS
  right after the authoritative `{slug}` write, logged on failure - never
  a failed operation. Because the refreshed sum is recomputed from a
  version scan OUTSIDE the index CAS, each refresh is GUARDED: it captures
  the entry's value before recomputing and commits only if the entry still
  holds it, skipping on conflict (two concurrent same-slug refreshes
  cannot land older-sum-last; the loser skips). No retry loop on the
  response path.
- **A lost refresh is permanent, and bounded to one record.** Nothing
  rebuilds the cached values in the background. A crash or a failed index
  CAS between the authoritative write and the index write leaves that one
  entry's cached size wrong until the next write to that same slug
  refreshes it. The error is confined to one record's bytes - it cannot
  accumulate across an owner the way a foreign-shard aggregate does - and
  the owner can clear it outright by deleting the paste.
- **Stale-entry semantics are honest, not exact.** An entry orphaned by a
  crash mid-`Delete` (or mid-`MarkFailed`) keeps counting its cached bytes
  - a bounded OVER-count that can only wrongly REJECT the owner's next
  upload, never admit an over-cap write. Writing the entry BEFORE the row
  means the mirror UNDER-count (a live row with no entry) can no longer be
  created by a crash.

The trade, stated plainly: the previous shape was exact whenever the index
was COMPLETE but paid O(owner's slugs) cross-shard reads per check; this
shape is exact whenever the index is complete AND fresh, and pays O(1)
scans per check. Both shapes sit on the same non-atomic check-then-write
foundation, so neither ever made the check strict; the cached sum trades a
narrow permanent per-record error for the fan-out (see "The correctness
argument").

**The write path is a plain row write plus index maintenance - no
counter.** The old three-step reserve -> write -> confirm collapses:

- **Check** (before the write): scan the owner's combined paste + site
  used bytes (the two scans above, added) and reject with the over-quota
  error if `used + body > cap`. A zero `userCap` means "no cap" and skips
  the check. This is the SYMMETRIC combined check every backend does
  (the per-identity sum spans both kinds): a paste insert counts the
  owner's site bytes too, and a site deploy counts the owner's paste
  bytes, so the ceiling holds however an owner splits their quota.
- **Authoritative write** (one single-shard CAS on the `{slug}` shard,
  unchanged): write `pastes/<slug>` (or `sites/<slug>`), the version row,
  `slug_owner/<slug>`, with the same
  slug-collision read-check (reject a slug a paste OR a site already owns).
- **Index maintenance** (one single-shard CAS on the `{id}` shard): write
  the value-bearing `identity_pastes/<id>/<slug>` (or
  `identity_sites/<id>/<slug>`) enumeration entry - the cached
  size the quota scan sums - and set
  `identity_first_seen/<id>` if absent. It runs synchronously on the
  response path (the entry is the quota's accounting record: a deferred
  write would leave every freshly-inserted paste invisible to the owner's
  next check). It is ordered BEFORE the authoritative write, so a crash
  cannot leave a live row the scan under-counts; the reverse residue - an
  entry with no row - is the accepted phantom. No byte counter is touched anywhere - there is none.

`Delete` / `MarkFailed` shed a paste's bytes by dropping its enumeration
entry (alongside removing / failing its authoritative rows);
`DeleteVersion` sheds the tombstoned version's bytes by refreshing the
entry's cached size after the tombstone commits. There is NO counter
decrement, so a lost drop/refresh mis-counts exactly one record in the
over-count direction - not a "markerless residual" to crash-durably protect.
`DeleteSite` collapses to (delete the row, the
entry, the file-blob binds, and the enumeration entry - no release marker,
no size-guarded restart loop, no consume).

**How far the cap can be exceeded, and why that is bounded.** The used-bytes figure the
check sums is the owner's enumeration entries - a projection - but the
authoritative rows stay the source of truth: the write paths refresh the
projection around every `{slug}`-shard commit. Two windows exist, both
bounded to one record (detailed under "The correctness argument" below):
(a) two concurrent same-identity uploads can both pass the check before
either writes, a bounded over-admit; (b) a stale cached value or an
orphaned entry - a lost size refresh after an append/tombstone, or an
entry whose delete lost the index drop - mis-counts by that one record's
bytes, in the over-count direction, until that slug is next written.
Writing the entry first removes the third window the earlier order had (a
live row the index does not enumerate, an UNDER-count). Neither remaining
window can make the owner's DURABLE used bytes exceed the cap by more than
the bounded amounts above,
and the global object-store bucket quota (`ErrServiceFull` from the blob
`Put`, see "Limits -> Durable total-bytes ceiling") is the hard backstop
on total bytes regardless. The shale backend runs no service-wide byte
scan on the write path - only the per-owner enumeration scan, bounded to
one identity.

### Derived indexes and repair-on-read

The per-identity family is a derived projection. `identity_pastes` and
`identity_first_seen` are not the source of truth; the `pastes/*` and
`versions/*` rows on the `{slug}` shards are. The derived entries are
written by the index-maintenance step and by the update / delete paths,
in transactions separate from the authoritative write. They are therefore
**eventually consistent**: a crash between the authoritative write and the
index update leaves the index momentarily out of step with the
authoritative rows. The quota sums the CACHED values of this index
("Scan-derived quota"), so the index must uphold two properties for the
quota to be exact: COMPLETENESS (it lists every live slug) and FRESHNESS
(each entry's cached `size` equals the authoritative live sum).
Completeness is structural - the entry is written BEFORE the row, so a
live row without an entry cannot be produced by a crash. Freshness is
maintained by each size-changing write and is not restored in the
background: a lost refresh is a permanent error confined to that one
record, in the over-count direction.

Each `identity_pastes/<id>/<slug>` entry is **value-bearing**: it stores a
denormalized projection of everything a listing renders - name, size,
created-at, kind, latest version, pinned version, updated-at - rather than the
empty marker the single-writer layout uses. The entry is therefore BOTH the
enumeration and the display: `ListByOwner` scans the owner's prefix on the
`{id}` shard and renders each row from the entry it just read, with no
authoritative read at all. `identity_sites/<id>/<slug>` is the same shape for
sites (size plus the two timestamps).

**Listing is O(1) reads.** One single-shard prefix scan serves the whole list,
whether the owner has three pastes or three thousand. This is the property the
fat entry exists to buy. Resolving each entry against its authoritative row
would cost a head read plus a version scan per item - on a high-cardinality
owner, thousands of sequential cross-shard reads to render one screen - and it
is precisely what this layout removes.

**Phantom entries are accepted, not repaired.** Writing the entry before the
authoritative row (above) means a crash between the two leaves an entry whose
paste does not exist. Such an entry is LISTED, and clicking through to it
404s. That is a deliberate choice, not an oversight:

- Detecting a phantom requires reading the authoritative row, which is the
  per-item read the O(1) listing exists to avoid. There is no cheap partial
  version: an entry is only provably phantom once its row has been read, so
  any detection at all restores O(n).
- The failure is visible and self-explanatory - the owner sees a slug that
  does not resolve - rather than silent.
- Its cached bytes keep counting against the owner's quota. That is an
  OVER-count, which can only wrongly REFUSE the owner's next upload, never
  admit one over the cap. The fail-safe direction.
- `Delete` on a phantom drops the entry, so the owner can clear one directly.

The same holds for a paste stuck `pending` past its upload (the detached-store
path's pod-death case): nothing ages it to `failed`, so it keeps its entry and
its charged bytes. The shale-collocated path prod runs commits READY and has
no pending window at all, so this is confined to the detached-store
deployments.

**The one slow path is a one-time upgrade.** An entry written before the
display fields existed cannot be rendered from the cache. The listing detects
that by shape (a paste entry with no `kind`, a site entry with no
`updated_at`), reads its authoritative row once, renders from that, and
rewrites the entry fat. Every subsequent listing takes the fast path. This is
a migration ramp with a fixed total cost - one read per pre-existing entry,
ever - not a validation pass. An entry whose row is unreadable during the
upgrade is skipped rather than repaired or deleted, for the reason above.

The upgrade's rewrite is GUARDED: it commits only if the entry still holds the
value the scan read, so two concurrent listings (or a listing racing a live
write) cannot land the older projection last. The loser skips and logs.

**There is no reconciler and no background reprojection.** An earlier design
ran a periodic pass that rebuilt every entry from the authoritative rows,
pruned orphans, and aged out stuck pendings. It was deleted along with the
counter it once healed. What replaced it is not a cheaper reconciler but the
decision that the drift it healed does not need healing: a stale cached size
over-counts in the safe direction, a phantom is visible and clearable, and the
gap the pass genuinely closed - a live row with NO entry, from a crash between
the two writes - was closed instead by REVERSING the write order, so that gap
can no longer be created.

#### The correctness argument: a bounded, non-compounding error

The counter design chased an **exact, strict** ceiling and paid for it with
cross-shard machinery that still drifted. The scan design targets a weaker
property, and it is worth stating exactly rather than aspirationally:

**The property that matters: the error in an identity's used-bytes figure
is bounded by a per-record amount and cannot compound.** It is NOT that
the cap is never exceeded - it can be, durably, by an unreadable entry's
bytes (see "Unreadable entries fail OPEN") or transiently by a concurrent
same-owner pair. What the design guarantees is that no single failure
propagates beyond the one record it touched, so the figure degrades
gracefully instead of drifting without limit the way a stored aggregate on
a foreign shard does. Total service bytes are separately hard-capped by
the object-store bucket quota, which is what actually bounds resource use.

There are four windows where a scan disagrees with durable truth, all
bounded to one record each.

**Window A - crash between the two writes (bounded OVER-count).** A write
commits the `{id}` enumeration entry first, then the authoritative `{slug}`
row second (two shards, two CASes). Which of the two orderings is used
decides which inconsistency a crash can produce, and this order was chosen
so the surviving one is the harmless direction:

- **entry first** (what hostthis does): a crash leaves an ENTRY WITH NO
  ROW. The quota scan counts its cached bytes, so the owner is
  **OVER-counted** - they may be refused slightly early. Fail-safe: the cap
  cannot be breached this way. The entry is visible in the owner's own
  list, and deleting it clears the charge.
- **row first** (the earlier order): a crash leaves a ROW WITH NO ENTRY.
  The scan cannot see it, so the owner is **UNDER-counted** and can write
  past the cap - and nothing short of a full cross-shard scan of every
  `pastes/*` row can even find it.

The cost of the chosen order is the phantom listing entry ("Phantom
entries are accepted, not repaired"); the benefit is that the only
unbounded-to-find, cap-breaching residue is structurally unreachable. On
the happy path the row write is the very next CAS after the entry write,
so the window is microseconds and widens only on an actual process death.

**Window B - two concurrent same-identity uploads (bounded OVER-admit).**
The quota CHECK (the scan) and the row WRITE are not one transaction, so two
uploads from the SAME identity can both scan the pre-upload state, both pass
`used + body <= cap`, and both write. The ceiling is then exceeded by up to
the smaller upload's size. This is the strictness the counter's atomic
reserve-CAS bought and the scan gives up: `conformCaps.
StrictIdentityQuotaUnderConcurrency` flips to `false` for shale (and so
for the local backend, which is shale on a local engine). A single-transaction backend keeps it
`true` - its per-identity check is atomic with the write, via a
per-identity `lockQuota` stripe. The separate per-ROOM cap `StrictQuotaUnderConcurrency` stays
`true` on shale - a room write is a single-shard CAS with the per-app counter
in the read-set, still strict - so only the per-identity axis relaxes. It is
acceptable for three reasons: (1) one identity = one key = one person, and a person racing
their own uploads to squeak a few hundred KB past a 10 MiB cap is not a
threat model worth atomic cross-shard coordination; (2) the over-admit is
BOUNDED - it is the number of truly-concurrent same-identity uploads times
the per-upload size, not an unbounded leak, and it is a one-time overshoot,
not a permanent drift (every subsequent upload scans the now-larger set and
is measured correctly); (3) the global **object-store bucket quota** is the
hard backstop on TOTAL bytes across all identities (`ErrServiceFull` from
the blob `Put`, see "Limits -> Durable total-bytes ceiling"), so no amount
of per-identity over-admit can exhaust the store.

**Window C - stale cached values and orphaned entries (bounded to one
record, permanent).** The cached sum introduces a window the
authoritative-fan-out shape did not have: the enumeration entry can be
WRONG, not just missing. It opens two ways. CRASH-shaped: a lost
size refresh (crash or failed CAS after an append or
version-tombstone commits) leaves the entry's cached size stale. An entry
orphaned by a crash mid-`Delete` or mid-`MarkFailed` keeps counting a dead
record's cached bytes - an over-count, so admission stays conservative.
RACE-shaped: the cached value is a number recomputed outside the index
CAS, so two writers of the same entry can hold sums computed from
different authoritative states - two concurrent same-slug refreshes, or a
listing's legacy upgrade racing one. Unguarded, the staler sum could land
LAST and silently replace the fresher one. Every such write is therefore
guarded to LOSE: it commits only if the entry still holds the value its
computation started from, and on conflict it skips, so a race costs at
most one skipped refresh - the same stale-cache shape as a lost refresh,
in whichever direction the surviving value errs (too small UNDER-counts,
an over-admit; too large OVER-counts, wrongly rejects, never admits).

Nothing repairs this in the background. The error is bounded by one
record's bytes per crash/race, does not accumulate across an owner, and is
cleared whenever that slug is next written or deleted. That permanence is
the price of deleting the reconciler, and it is affordable precisely
because the error cannot compound: an aggregate on a foreign shard drifts
without limit, a per-record cache is wrong by one record.

**Window D - an unreadable entry (bounded UNDER-count, permanent).** An
entry the scan cannot decode counts as zero, so its bytes are grandfathered
until the record is repaired. Unlike A/B/C this one does not settle on its
own. It is the deliberate cost of not locking an owner out; the reasoning,
the bound, and the logging are in the next section.

**Unreadable entries fail OPEN.** The quota scan runs on the synchronous
write path and sums the owner's enumeration ENTRIES. An entry it cannot
read - undecodable JSON, or the placeholder marker for an undecodable
authoritative record - counts as ZERO and the scan continues.

The owner is therefore UNDER-charged for exactly those bytes, and those
bytes are effectively grandfathered: nothing recomputes them, so they stay
free until that slug is written again or the entry is repaired. That is
the deliberate choice. The alternative, refusing the scan, locks a person
out of uploading anything because of data damage they did not cause and
cannot fix - and the paths that would let them clean it up themselves
(`Delete`, `DeleteVersion`) must decode the same broken record, so they
fail too. A lockout with no self-service exit is a worse outcome than an
under-charge.

What bounds the under-charge: it is one record's bytes per unreadable
entry, it cannot compound across an owner, and the global object-store
bucket quota (`ErrServiceFull` from the blob `Put`, see
"Limits -> Durable total-bytes ceiling") still hard-caps total service
bytes regardless of how any per-identity sum errs. The per-identity cap is
a fairness mechanism, not the thing standing between the service and a
full disk.

Skips are LOGGED as one summary line per scan carrying the count plus a
bounded sample, never one line per entry - an unreadable entry is
permanent until repaired, so per-row logging would be a standing cost
proportional to accumulated debris rather than to anything actionable. A
clean scan logs nothing, so log volume stays the signal: a steady count is
known debris, a growing one means something is still producing them.

The legacy upgrade path is recognized by SHAPE: a SITE entry holding the
pre-value-bearing marker byte or an empty migrated value, or a PASTE entry
holding an empty migrated value - the older layout stored both index
families as bare markers, and pastes never had a marker-byte era, so an
empty value is the only legacy paste shape - is read through its
authoritative row (for a paste, the row plus its live version sum) until
the owner's next list enriches it. A legacy entry whose authoritative row
is GONE contributes zero; one whose row is UNDECODABLE also contributes
zero and is counted as a skip, the same fail-open rule.

**An undecodable authoritative row no longer reaches the quota at all.**
The scan reads only entries, and a corrupt `pastes/<slug>` row leaves its
enumeration entry perfectly readable, so the owner keeps being charged the
right number and keeps being able to upload. Only reads OF THAT PASTE
fail. This is a direct consequence of deleting the reconciler: the pass
used to discover corrupt rows by scanning them, and had to decide what to
do about one, which is why the PLACEHOLDER entry existed (an entry marked
`placeholder: true`, carrying no usable cached value). Nothing writes a
placeholder now.

The placeholder READERS are kept, and are purely an upgrade concern: a
store written by a deployment that ran the reconciler may still hold them.
Such an entry counts as zero and is logged as a skip, so its owner is
under-charged for that paste and otherwise unaffected. Two things clear it:
the owner's next `list`, which reads that entry's authoritative row on the
legacy-upgrade path and rewrites the entry with real values, and any write
to that slug, which holds the head row already. If the record is still
undecodable neither can, and those bytes simply stay free until an
operator repairs the record - which is the whole point of failing open
here, because the self-service ways out are closed too: `Delete` and
`DeleteVersion` must decode that same corrupt row and fail on it as well.
An owner in that state would otherwise be permanently unable to upload
anything, with no action available to them that would fix it.

Taken together, the ceiling can be over-enforced (Window A's phantom
entry, Window C's stale-large cache), transiently over-admitted (Window
B), or under-enforced (Window C's stale-small cache and an unreadable
entry), each by a bounded per-record amount.

Be precise about the resulting guarantee, because failing open weakened
it. An identity's durable used bytes can sit ABOVE the cap, by the total
of whatever entries the scan could not read. That is not a transient
window that settles; those bytes are grandfathered until the record is
repaired. What remains true is the property that actually protects the
service: the error is bounded by one record per unreadable entry, it
cannot compound, and total bytes across every identity are hard-capped by
the object-store bucket quota regardless of how any per-identity sum errs.
The per-identity cap is a fairness mechanism; the bucket quota is the
thing standing between the service and a full disk, and it does not depend
on any of this.

### Cross-shard background operations

**There are none.** No operation in the metadata plane fans out across
shards, and in each case that is a design choice rather than an accident:

- **Quota** is one single-shard scan of the owner's enumeration index.
- **Listing** is the same scan, rendered from the entries it returns.
- **Blob GC** decides reachability from each blob's own co-committed
  pointer, so there is no global set to collect.
- **The keygate** prunes lazily, inside the single-shard reads that already
  walk its rows (see "Sybil rate limit").
- **Room accounting** is one single-shard scan of the app's families.

The single remaining BACKGROUND job is the blob-plane orphan sweep, which
runs per mounted storage unit against the object store rather than
fanning out across the metadata keyspace.

**Nothing acts on ABSENCE.** Every delete in the system is justified by a
positive fact about the record itself - a blob's own staged-and-unbound
pointer, an entry's own expiry - never by a record failing to appear in
some scanned set. That is the property that used to require a fail-closed
decode policy and an abort-on-zero-refs guard, and it is now structural
rather than defended. It is also why a phantom enumeration entry is
LISTED rather than pruned: pruning it would mean deleting on the strength
of an absence.

### Decode tolerance is per-scan-semantics

Every background scan that walks the metadata keyspace decodes each row
it visits, and any row can in principle be corrupt or undecodable (a
truncated value from a partial restore, a schema-version it cannot read,
a torn write). How a scan reacts to one undecodable row is NOT uniform:
it is dictated by the scan's SEMANTICS, because the safe failure
direction differs per scan. There are exactly three policies, and which
one a given scan uses is load-bearing for correctness.

**The invariant that ranks both: no decode-tolerance path may ever cause a
referenced blob to be DELETED.** Destroying someone's bytes is the only
unrecoverable outcome here, so it is the one thing no tolerance policy may
risk. Everything else is a recoverable error and is ranked below
availability: a quota that under-counts costs fairness and is bounded by
the bucket quota; a quota that refuses to compute costs a person the
ability to use the service at all, with no way to fix it themselves. So
the quota scan fails OPEN (see "Unreadable entries fail OPEN") while the
blob paths stay conservative.

**Policy 1 - idempotent lazy prunes: SKIP + LOG, continue.** The keygate's
and the room ledger's lazy in-scan prunes treat an undecodable row as SKIP
+ LOG and CONTINUE the pass. The consequence of skipping is bounded and
self-correcting: that one record is simply not processed by THIS read; a
later read that walks the same range retries it. This is safe ONLY because
these operations are idempotent and re-run: dropping a stale marker
produces the same end state whether it runs once or many times, so
deferring one record costs at most latency, never correctness. A single
corrupt row must NOT be allowed to abort the whole pass: a hard-fail there
would stall the prune for every healthy record too, until an operator
hand-fixes the one bad row. The blast radius of one poisoned row must stay
one row.
This mirrors the keygate admission-count scan, which already does the
right thing for an idempotent counter (a tolerant parse that skip +
continues on a bad row). The skip is LOGGED so a persistently-bad row is
visible to an operator, not silently swallowed forever.

  **A DELETED key is not a corrupt row: tombstones are skipped by the scan.**
  At R>1 a delete is not a removal. shale turns it into an empty-payload
  tombstone write, so the key keeps a stamped envelope carrying no payload.
  `cluster.Get` resolves that to not-found, but a raw prefix scan hands the
  stored bytes back, so a scan consumer sees the deleted key as an item whose
  value is empty. An empty value does NOT present as "absent" to a consumer -
  it presents as CORRUPT, because decoding empty input fails. Every deleted
  record would therefore reappear as a phantom undecodable row on every pass,
  permanently: it has no owner to derive (the owner mapping was deleted with
  it), so it falls in the unrepairable class above, and deletes are ordinary
  traffic, so the phantom set only grows. The scan therefore drops tombstones,
  which simply makes it agree with the semantics `Get` already has.

  **Emptiness alone does NOT identify a tombstone, and conflating the two is
  a quota-correctness bug.** Two distinct things arrive as an empty payload:

  - a TOMBSTONE: a STAMPED envelope with no payload. A deleted key. Skipped.
  - a LEGACY BARE MARKER: a genuinely empty stored value with NO envelope.
    The pre-shale engine stored enumeration-index entries as bare empty
    markers and the migration is in place, so scans still encounter those raw
    empty bytes even though no shale write can produce one (its Put rejects
    empty values, which is why the index families use a one-byte marker). This
    is LIVE DATA - an owner's enumeration entry, which the quota scan sums.

  Conflating them would drop LIVE data from the sum, and unlike a genuinely
  unreadable entry - which fails open deliberately, is counted, and is logged -
  this one is silently avoidable: a bare marker's size IS recoverable by
  reading its authoritative row. Discarding a value the scan could have
  computed is a bug in any failure-direction policy. The test is therefore
  "stamped AND empty", never "empty": a bare value decodes with the zero
  stamp, while a real envelope always carries the commit stamp it was written
  under.

  **The log is a PER-PASS SUMMARY, never one line per row.** This matters
  because "retried next read" is not the same as "eventually repaired": a
  row that stays undecodable is re-found by every pass that walks it,
  forever. Per-row logging is then not a bounded diagnostic but a PERMANENT
  cost proportional to accumulated debris rather than to anything an
  operator can act on. At scale that cost is not cosmetic: the log volume
  alone can consume enough CPU to starve the request path, degrading
  interactive reads by orders of magnitude while every behavioural check
  still passes, because nothing about the service's RESPONSES is wrong.
  Each pass therefore emits at most ONE line per scan, carrying the COUNT
  plus a bounded sample of slugs; a clean pass emits nothing, so log volume
  stays the signal. The actionable reading is the derivative rather than
  the level: a steady count is known debris, a growing one means something
  is still producing corrupt rows.

  Each skip-and-continue pass is therefore PARTIAL by design: it did the
  work for every decodable record and deferred the undecodable ones. A
  partial prune leaves a stale marker one more cycle - already a tolerated
  state, and fail-safe in the over-count direction. That partiality cannot
  delete live content or under-count a quota, so it satisfies the ranking
  invariant.

**Policy 2 - user-facing reads: hard-fail.** The per-request read paths
(`Get`, `ListVersions`, `GetVersion`, the site manifest read, the room
scan / per-key read) are
NOT made tolerant. A user read that hits a corrupt record SHOULD surface
an error to that user, not silently skip the record and return a
plausible-looking-but-incomplete result. These paths are synchronous,
user-observed, and not idempotent retries of a background loop, so the
right behavior is to fail loudly on the one request that touched the bad
row - the user (or operator) sees a real error rather than silent data
loss in the response body.

The quota scan is synchronous too but is NOT covered by this policy,
because the two differ in what the user can do about the failure. A failed
`Get` tells someone one paste is broken; everything else still works. A
failed quota scan tells them they cannot upload at all, including the
uploads that would let them replace or delete the broken thing. Same
tolerance question, opposite safe answer.

**Policy 3 - the quota scan: COUNT AS ZERO + summary log, continue.**
Covered in full under "Unreadable entries fail OPEN". An unreadable entry
contributes nothing and the scan returns a number rather than an error, so
the owner is under-charged and keeps working. Bounded per record, logged
per scan, and backstopped by the object-store bucket quota.

The three policies, side by side:

| Scan kind | Examples | On a bad record | Why |
| --- | --- | --- | --- |
| Idempotent lazy prune | the keygate and room-ledger in-scan prunes | SKIP + LOG, continue; a later read retries | idempotent, re-runs; partial work is safe; one bad row must not stall the whole pass |
| User-facing read | `Get`, `ListVersions`, `GetVersion`, site manifest read, room scan / per-key read | HARD-FAIL | a user read of corrupt data should surface an error, not silently skip |
| Quota scan | `SumActiveBytesByOwner`, `SumActiveSiteBytesByOwner` | COUNT AS ZERO + summary log, continue | refusing locks a person out over damage they cannot fix; the under-charge is bounded per record and the bucket quota still caps total bytes |

### Shale-collocated blobs (transactional blob plane)

By default the blob bytes live in a detached content-addressed store on disk,
decoupled from the metadata: every backend shares one
`BlobStore` and blobs are keyed by content sha alone. **That store has no GC.**
Its bytes are reclaimed only by deleting the store, which is why it is a
dev/test shape: the deployed shape is the collocated plane below.

A shale backend can OPTIONALLY route its blobs THROUGH the cluster, collocated
with the metadata on the owning shard and transactionally co-committed. It is
enabled by `HOSTTHIS_SHALE_BLOB_BUCKET` (a distinct blob bucket on the same
object store the metadata uses); unset keeps the detached-store model.

**The byte plane goes node -> object store directly.** A blob is staged by
streaming its bytes to a final, unit-keyed object OUTSIDE any transaction (no
shard lease held for the multi-second upload). The bytes never cross the cluster
RPC boundary - only a small pointer routes through the ring. After staging, the
bytes are durable but UNREFERENCED: no reader can reach them until a pointer is
bound.

**The pointer co-commits with the metadata.** A staged blob is bound by writing
its pointer at an internal `bref/{<slug>}/<unit>/<blobid>` key in the SAME
single-shard transaction that writes the paste / version / site row. The hash
tag `{<slug>}` routes the pointer to the SAME shard as `pastes/<slug>` (the
custom `ShardKeyFn` honors it), so the bind and the metadata write commit
together in one CAS. The row carries the staged `blob_id` (a paste/version row's
`blob_id` field; a site row's `file_blobs` sha->id side-table) so a read
resolves the blob to fetch. Each write's staged refs are scoped to that single
call (carried on its own context, not a shared per-slug stash), so two
concurrent writes on one slug - two updates, an update vs a delete, two
redeploys - each bind their OWN blob; one call can never bind another's bytes
or commit a row with no bind.

**A site deploy stages every file under a pre-claimed slug.** Because a file's
pointer co-commits with the manifest on the manifest's `{slug}` shard, every
file must be STAGED under that same slug's route key, or its `bref/{<slug>}/...`
hash tag routes to a different shard than the manifest pins and the bind is
rejected by the cross-shard guard. A paste / version already knows its slug when
it stages; a FIRST-time site deploy does not (the slug is random) and the untar
stream is one-shot (it cannot be re-read to re-route). So a first deploy mints
and pre-claims its slug BEFORE the untar, via a metadata-only single-shard claim
(`slug_owner/<slug>` written iff the slug is free as a paste AND a site AND not
already claimed), then stages every file under it. The pre-claim is a cheap
existence stake, NOT a blob reservation: no two-store coupling, no quota charge,
no in-flight-blob protection (there is none to protect - the files are staged
reader-invisible and the bind co-commits with the manifest). A collision re-mints
a fresh slug before the stream is consumed; the authoritative insert remains the
final collision authority. A crash after a successful claim but before the commit
leaves a `slug_owner/<slug>` marker with no site row - a harmless metadata leak
that self-heals when a later paste insert that mints that slug overwrites the key
(there is NO dedicated slug_owner sweep; until such a paste reuses it, the only
effect is that one slug staying un-pre-claimable for a future site deploy, in a
32^8 space), never an unreadable site. A redeploy (`DeployToSlug`) already targets a known existing
slug, so it stages under the real slug directly and needs no pre-claim. On the
detached-store path the slug routes no blob (blobs are content-sha-keyed), so no
pre-claim runs: the slug is minted in the post-untar insert retry loop where the
authoritative insert is the collision authority.

**Reader-atomic create + atomic delete.** A reader sees a row WITH its blob or
neither - never a row pointing at bytes that are not there, and never bytes a
reader can reach without a committed row. A delete unbinds the pointer in the
SAME transaction that removes the metadata, so the bytes go unreferenced exactly
when the row vanishes. (A delete of a paste unbinds every version's blob; a
version tombstone unbinds that version's blob; a site delete or a redeploy
unbinds the dropped files' blobs - all folded into the authoritative `{slug}`
transaction.)

**Pending-collapse: a shale-collocated paste commits READY directly.** The
async pending/finalizer model (below, "Paste lifecycle status") exists because
the detached-store blob write happens AFTER the metadata commits, leaving a
window where the row is live but the bytes are not yet written. On the shale-
collocated path that window does not exist: the bytes are durable (staged)
BEFORE the metadata commit, and the bind makes them visible together. So the
paste commits READY directly - no pending row, no loading page, no background
finalizer, no `MarkReady`/`MarkFailed` flip. If staging fails the row never
commits (the SSH client gets the error, the quota reservation is released); if
it succeeds the bind + row co-commit or neither lands. The pending model is KEPT
unchanged for the detached-store path (local / shale-without-a-blob-
bucket), where it is still correct.

**Orphan-bytes reclamation.** A crash between staging and the bind leaves a
staged-but-unbound object (a unit-local orphan). On this path the global
content-addressed sweep is NOT run (the cluster owns the blobs); instead an
age-gated, mounted-unit-local `SweepOrphans` pass reclaims orphan objects whose
object-store ModTime is older than a generous grace (default one hour, which
exceeds the longest stage->commit window so an in-flight upload's object is never
swept). hostthis schedules it in the same periodic sweep loop, per node.

**Reachability is per blob, so no pass acts on absence.** A bound blob's
pointer is co-committed with the record that owns it and unbound by that
record's delete, so whether a blob is reachable is a fact about the blob, not
a conclusion drawn from a set of everything else.

That removes an entire failure mode rather than guarding it. The retired
alternative computed a cluster-wide keep-set and deleted every blob absent
from it, which meant a partial scan destroyed live data - it needed a
fail-closed decode policy and an abort-on-zero-refs guard just to be safe.
Neither is needed now, because there is no set to be incomplete.

**Within-record byte dedup is deferred.** A blob is staged under a fresh random
blob id each time, so an unchanged file re-staged on a redeploy (or a paste
reverting to prior content) gets a NEW object and the old one is unbound + swept.
This is correct (no leak) but re-uploads unchanged bytes; true content-sha-keyed
dedup is a later follow-up.

### Deploy arc: replication factor 1, then scale out

The backend ships at `ReplicationFactor = 1` first: one node per shard,
no replicas, no last-write-wins envelope cost on the read path. At R=1
a single-node cluster is functionally equivalent to a single-writer one
(same object store, same keys, same single owner of the keyspace), so
the cutover is low-risk and reversible.

Scaling to `N = 2` nodes with `ReplicationFactor = 2` is then a
configuration change, not a code change. The read path uses
`ReadQuorum`: a read collects a quorum of replicas (both, at R=2) and
resolves by last-write-wins, preferring a present value over a
`NotFound`. This is required at R>1: `ReadNearest` decides on the FIRST
replica to answer and treats a `NotFound` as a usable answer, so a read
served by a replica that is still backfilling (a freshly joined node,
before its rebalance pull completes) could return `NotFound` for a key
that demonstrably exists on the other replica. `ReadQuorum` reads both
and the present value wins. The reservation quota pattern is unaffected:
shale's last-write-wins-on-write rule (a replica applies an incoming
write only if its stamp is strictly newer) still makes the owner-local
CAS the single source of write ordering for its shard. At `R = 1` a
quorum IS the single replica, so `ReadQuorum` reads exactly what
`ReadNearest` would (one read, no extra hop, no envelope comparison) -
the change is behavior-identical at R=1 and only takes effect at R>1.

#### Sharded metadata (multi-backend mode)

By default (`UnitCount = 0`) the shale backend opens a SINGLE slatedb
database per node (the deploy arc above). Setting `UnitCount = N` (a power
of two) selects MULTI-BACKEND mode: the keyspace is partitioned into N
units, each a SEPARATE slatedb database, and a key routes to
`UnitForHash(ShardKeyFn(key), N)`. Units distribute across the cluster's
nodes by the consistent-hash ring; at `R > 1` each unit is replicated to R
nodes. Co-location is preserved: the `ShardKeyFn` routes a whole `{tag}`
set's keys to one unit, so a single-shard CAS stays in one database.

The trade-off is concrete: each unit is a full slatedb instance (its own
memtable, WAL, manifest, compaction goroutines) per owning replica, so N
units at R replicas is up to N*R slatedb instances spread across the nodes.
On small deployments keep N small; the cost of a large fixed N is real RAM
+ goroutines, not just on-disk layout. `UnitCount = 0` (single-backend)
stays the default and is byte-for-byte the prior behavior. The mode is
selected per deployment via the operator env `HOSTTHIS_SHALE_UNIT_COUNT`
(`0` = single-backend; a power of two = that many shards). It composes with
replication + relaxed durability unchanged: `ReplicationFactor`,
`ReadQuorum`, and the relaxed-durability knob apply per unit exactly as
they do for the single backend.

`0` is only meaningful for a SINGLE-NODE deployment. Combining it with a
bind address - i.e. asking to join a cluster while declining to shard - is
a configuration error and the daemon refuses to start, naming the env var.
It is not silently downgraded to a single-node backend, because that
failure is invisible in exactly the way that matters: the node comes up
healthy, serves reads and writes, and looks indistinguishable from a
clustered peer right up until the replication it was supposed to provide
is needed. A refused boot is loud, immediate, and attributable; a quietly
un-clustered production node is none of those. Single-node deployments are
unaffected, since the check fires only when a bind address is present.

**Online resharding (declarative).** Once a deployment is in sharded mode with
a shared CAS arbiter (the homogeneous bootstrap, where every pod wires the same
`ConditionalStore`), `HOSTTHIS_SHALE_UNIT_COUNT` is a LIVE target, not just an
initial shape: changing it to another power of two and redeploying drives an
ONLINE, lossless reshard to the new count. The cluster gossips each member's
declared count; when every live member agrees on a new value, shale's
decentralized arbiter retargets and the units split (or merge) WHILE SERVING -
no downtime, no operator copy. Value-separated blobs survive the reshard because
their pointer keys are generation-independent (the token-free bref). This is
distinct from the single-backend -> sharded migration below, which remains a
one-time copy. The runtime enables this whenever the cluster is multi-backend
AND a `ConditionalStore` is present; a single-backend deployment never reshards.

Migration to sharded mode is NOT in-place. Unlike the single-backend
cutover (same bucket, same key names), the multi-backend layout stores each
unit under its own object-store prefix, so an existing single-backend
deployment's data must be COPIED into the sharded layout once. That copy is
a one-time operator step (read the source keys, write them into a fresh
sharded cluster whose normal routing shards them) performed with a brief
downtime; it lives in the operator's infra tooling, NOT in this app. The
runtime + repo carry no migration logic - they only select the mode from
`UnitCount`.

### Migration

Migration is in-place: same bucket, same object store, same key names.
The `ShardKeyFn` provides co-location by routing, so **no key is
renamed or rewritten** on cutover. An existing deployment's keys
are read by the shale backend as-is.

Two compatibility details:

- **Raw values decode as zero-stamp envelopes.** At R>1 shale wraps each
  value in a last-write-wins envelope. A value written without an
  envelope (every pre-cutover value) decodes as a zero-stamp
  envelope: it loses any comparison against a stamped write and is
  re-stamped on its next write. This is graceful, requires no offline
  conversion, and at R=1 the envelope is not used at all.
- **The per-owner enumeration index self-backfills; there is no counter
  to seed.** The per-identity quota is scan-derived, so a cutover needs no
  offline backfill of any stored aggregate. The one derived structure the
  quota scan reads THROUGH is the `identity_pastes` / `identity_sites`
  enumeration index. An existing deployment already maintains
  those indexes, so they carry over as-is - but as EMPTY values (the
  marker convention), so until the owner's next list enriches them
  the quota scan reads each such entry through its authoritative rows (the
  legacy fallback under "Scan-derived quota"), paying the legacy-shaped
  per-entry fan-out for exactly those entries and never hard-failing on the
  shape. This is a graceful, idempotent, no-downtime heal driven by live
  traffic - not a one-time offline step and not a correctness precondition
  for enabling quota. A paste or site that lacks an index entry entirely is
  NOT picked up: nothing scans the authoritative rows to find it, so it
  stays un-enumerated and uncharged. Writing the entry first means normal
  operation cannot produce one; a store carrying such rows from an older
  deployment needs an out-of-band backfill.

**Upgrading from earlier shale code (pre-value-maintained cached sizes) -
deployment note.** Entries written by earlier shale versions decode fine
but their cached `size` was never version-maintained. Expect bounded quota
slack at upgrade: the quota scans sum those stale cached sizes, typically
UNDER-counting a multi-version paste's owner (head size <= live sum). With
no background reprojection, each such entry is corrected by the next write
to its own slug rather than by a pass, so the slack persists on untouched
pastes. It is bounded per record and cannot compound.

**Upgrading to the fat enumeration entry - deployment note.** Entries
written before the listing's display fields existed carry no `kind` (or,
for sites, no `updated_at`). The first `list` per owner resolves each such
entry against its authoritative row and rewrites it fat, so that one
listing costs the old per-item reads and every later listing is O(1). No
migration step is required and no downtime is involved; the cost is one
slow listing per owner, once.

### Multi-node shale (horizontal write scaling)

Everything above runs correctly on a single shale node: one process owns
the whole ring, every shard resolves to the local backend, and the
shard-key function plus the scan-derived per-identity quota are already in
place so the same code is correct at any node count. This subsection describes the
multi-node shape that turns that single-writer-equivalent deployment into
a horizontally write-scaled one, and the data-safety contract the cluster
must honor when nodes join or leave.

#### The two-node shape

A multi-node deployment runs N identical hostthisd processes, each
configured as one shale node:

- **Its own backend.** Each node opens its own slatedb database (a
  distinct `DbName` / object-store prefix). A key's bytes physically live
  in exactly one node's database (at replication factor 1). No two nodes
  share a database.
- **Gossip membership.** Each node binds a memberlist address (`BindAddr`,
  host:port). A **non-empty `BindAddr` is what enables multi-node mode**;
  with it empty the node runs the single-node path described above, every
  op local, no gossip, no ring routing. This is the back-compat guarantee:
  an existing single-node deployment that sets none of the multi-node
  config behaves exactly as it does today.
- **Peer forwarding.** Each node advertises a gRPC service address
  (`GRPCAddr`, host:port) that it broadcasts to peers as their forwarding
  target. A request that hashes to a shard another node owns is forwarded
  over gRPC to that owner and served from the owner's local backend; the
  caller sees a normal result and never learns the op crossed a node
  boundary. `GRPCAddr` is required whenever `BindAddr` is set.

  The cluster layer advertises `GRPCAddr` via gossip but does **not** itself
  stand up the listener that peers forward to: serving that address is the
  host process's responsibility. In hostthis the metadata adapter owns it.
  When `BindAddr` is non-empty the adapter binds a TCP listener, passes the
  listener's **actual** bound address into the cluster as `GRPCAddr` (so the
  advertised address is exactly the one served, which matters when the
  configured port is `:0` / OS-assigned), registers the cluster's RPC
  handlers (Put/Get/Delete/ScanPrefix/CommitCAS/MigrateRange) on a gRPC
  server, and serves in the background. Closing the adapter gracefully stops
  that server and closes the listener, releasing the port with no leaked
  goroutine. When `BindAddr` is empty the adapter binds **no** listener and
  starts **no** server: the single-node path is byte-for-byte today's
  behavior, the back-compat guarantee. Without this serving step a
  multi-node deployment would gossip a live-looking `GRPCAddr` that no
  process answers, so every forwarded request would hit a dead port; the
  rebalance safety contract below depends on the forwarding path being real.
- **Discovery via seeds.** A joining node is given one or more `Seeds`
  (the `BindAddr` of already-running nodes) to bootstrap gossip. An empty
  seed list means "this node is the founder / seed"; the first node starts
  with no seeds and later nodes seed off it. Once gossip converges every
  node holds the same ring snapshot.
- **Homogeneous bootstrap (optional).** The seed-based discovery above has
  an asymmetry: one node is the *founder* (empty seeds, forms the cluster
  generation) and the rest are *joiners* (seed off the founder's address).
  That asymmetry is a deploy wart - the founder is a special pod, and if it
  is recreated while joiners are live they cannot learn the generation from
  a fresh founder. The homogeneous alternative removes the special pod: when
  a shared `ConditionalStore` (a create-if-absent / compare-and-set object
  arbiter over the metadata object store) is configured, **every** node
  carries the *same* seed list (a headless Service that resolves to all
  pods) and decides form-vs-join at runtime against a `__cluster/init`
  marker in the shared store. On boot a node tries to join; the first one up
  reaches no peer, so `AllowSoloStart` (active only when a `ConditionalStore`
  is wired) lets it come up solo and contend to **form** by writing the
  marker `{gen, count}` with `PutIfAbsent`; exactly one node wins, and the
  rest read the existing marker and **join**, adopting its durable
  `{gen, count}`. No founder, no role split: one StatefulSet of identical
  pods. The marker is also the restart-safe generation source - a full
  cluster restart reads it and resumes the right generation instead of
  re-forming gen 0.

  hostthis opts in via `HOSTTHIS_SHALE_HOMOGENEOUS=true` (multi-backend
  sharded mode only): the metadata adapter builds a MinIO-backed
  `ConditionalStore` over the **same** metadata bucket the units use,
  namespaced by the metadata DB name (so the marker is the same object for
  every pod), and passes it into the cluster. Unset (the default) keeps the
  seed-based bootstrap above byte-for-byte, so existing seed/joiner
  deployments are unaffected; the marker is only consulted when the store is
  wired. Adopting an existing seed-formed cluster is a matter of pre-seeding
  the `__cluster/init` marker to that cluster's live `{gen, count}` before
  the homogeneous pods boot, so they all join the live data with no form
  contention.

The shard-key function (`{slug}` / `{id}` / `{subnet}` co-location) is
unchanged across node counts: it decides which shard a key belongs to,
and the ring decides which node owns that shard. Co-location still holds,
so a single-family-single-subject transaction is still a single-shard CAS,
now resolved to whichever node owns that shard.

#### Where the throughput win comes from

On a single node every write serializes through one backend. On N nodes
the ring spreads shards roughly evenly across nodes, so writes to keys on
**different shards** are committed in parallel by **different nodes'
backends**. A workload of independent uploads (different slugs, different
owners) fans its CAS commits across the cluster instead of queueing them
behind one writer, and sustained write throughput scales roughly with
node count. Writes that contend on the **same** shard (the same owner's
quota counter, the same slug's authoritative row) still serialize, by
design: that serialization is what keeps quota strict and the
authoritative rows consistent. The win is on the independent-write axis,
which is where hostthis's write load actually lives (many small uploads
from many owners), not on hot-key contention.

#### R=1 vs R=2: throughput versus availability

Replication factor is a deployment choice with a direct tradeoff:

- **R=1 (shard, no replica).** Each key lives on exactly one node. Maximum
  write throughput (no replica fan-out, no last-writer-wins envelope on
  the read path) and minimum storage. The cost is availability: if a node
  goes down, the shards it owned are unreadable and unwritable until it
  comes back, because there is no second copy to promote. A node-down event
  is a gap, not data loss, **provided the node returns** with its backend
  intact (the bytes are durable in that node's object-store database). At
  R=1 a permanently-lost node's shards are lost.
- **R=2 (replicate).** Each key lives on two nodes; a write is acked per
  the write-consistency setting and reads resolve a last-writer-wins
  winner. A single node down does not interrupt service (the surviving
  replica serves the shard) and a permanently-lost node loses nothing.
  The cost is that R=2 does **not** add write throughput: every write now
  lands on two backends, so the per-write work doubles even as the node
  count grows. R=2 buys high availability, not horizontal write scaling.

The two goals (throughput, availability) pull in opposite directions; a
deployment picks the point on that axis it needs. The horizontal-write-
scaling deployment this section motivates is R=1. A deployment that values
uptime over peak write rate runs R=2 and accepts that the throughput
ceiling is the per-node ceiling.

#### Relaxed durability: fast-ack at the memtable

Durability is a *backend* concern: the shale cluster layer is durability-
agnostic, and the slate backend decides when a write is acked. By default a
write is acked only after the underlying slatedb flushes it durably to
object storage (`AwaitDurable=true`), which adds a flush round-trip
(~100ms) to every commit. Since a single paste upload is several commits,
that durable-flush latency, not the sharding, is the dominant write-
throughput ceiling.

`HOSTTHIS_METADATA_AWAIT_DURABLE` (default `true`) exposes the slate
backend's relaxed-durability mode. Set it to `false` and the backend acks
at memtable insert (microseconds) and flushes to object storage in the
background, removing the per-commit object-store round-trip from the hot
path. This is the largest single write-throughput win available, larger
than the horizontal sharding gain.

The tradeoff is a bounded loss window: a write that has been acked but not
yet background-flushed is lost if its node crashes before the flush. This
is **only safe paired with R>=2 on anti-affinity-separated nodes**: a
second replica holds the write through the flush window, so the write
survives unless *every* replica crashes inside the same flush interval (a
correlated failure). Relaxed durability at R=1 is unsafe - a single crash
loses un-flushed writes with no second copy - so the knob is intended for
the same HA deployment that runs R=2 across distinct nodes. The flag is
threaded to the slate backend's per-write `WriteOptions`; leaving it at the
default keeps the byte-exact durable path.

#### Dispatch deadlines: the read/write timeout knobs

Every clustered metadata op runs under a per-dispatch deadline: the shale
cluster bounds each read dispatch and each write dispatch, defaulting both
to 5s. Two operator envs override them:

```
HOSTTHIS_SHALE_READ_TIMEOUT    per-dispatch read deadline    (unset = shale default, 5s)
HOSTTHIS_SHALE_WRITE_TIMEOUT   per-dispatch write deadline   (unset = shale default, 5s)
```

Both parse as Go durations (`8s`, `500ms`). Unset or empty leaves the
corresponding `ShaleConfig` field zero, so the shale default applies and a
deployment that sets neither is byte-for-byte unchanged. A value that does
not parse as a Go duration, or a negative one, is a configuration error:
the daemon refuses to start rather than running with a silently
substituted default.

The knob that earns its keep is the read budget. During a rolling deploy a
shard briefly hands off between nodes, and a read that lands inside that
sub-second handoff window re-polls until the shard settles - but only for
as long as the read deadline allows. Under the 5s default a rare handoff
that outlives the budget surfaces as a client error; raising the read
deadline (e.g. `8s`) converts that tail case into added latency instead.
The write deadline exists for the same class of reason (a heavyweight CAS
commit on a bloated unit can stall past 5s; the bulk migration tooling has
always raised these budgets internally), but serving deploys normally
leave it at the default.

#### Coordination is a pluggable choice

Who owns which storage unit is decided by a coordination adapter, not by the
cluster itself. Two exist upstream: gossip (SWIM membership plus a
consistent-hash ring) and a CAS/lease adapter that keeps one membership
document in the conditional store and renews per-node leases against it.

This deployment passes the GOSSIP adapter explicitly. That is a deliberate
choice rather than a default: a single-node deployment passes no coordinator
at all (which is what an empty bind address meant before the port existed),
and any change of adapter is a separate, independently verifiable change from
a dependency upgrade. Keeping those in different blast radii means a surprise
after either one never raises the question of which caused it.

The coordinator is ADVISORY: it says who SHOULD hold a unit. The storage
epoch is AUTHORITATIVE - a unit is only writable by whoever opened its
database at the highest epoch. So a coordinator that is wrong, slow, or
partitioned is an availability problem, never a correctness one. That
separation is what makes swapping adapters a bounded risk.

#### Retrying the handoff refusal

The read budget above is the FIRST line of defence for the handoff window:
shale re-polls inside it, so a blip shorter than the budget never reaches
the caller. This is the second line, for a window that outlives it.

When a unit is mid-handoff, a routed op can refuse with a typed signal
meaning "this is bounded by a mount completing, not by an outage" -
nothing external has to recover for a retry to succeed. shale exposes that
as a matchable sentinel identically whether the refusal was raised locally
or forwarded from a peer, so the caller never has to know which. It does
NOT re-route within the same op, so without a retry the refusal reaches the
client as a request failure even though the work is about to become
possible.

Every non-transactional cluster read therefore retries on exactly that
signal, and on nothing else:

- A bare `Unavailable` is NOT retried. It is overloaded with genuine
  peer-down, and retrying a real outage converts a clean fast failure into
  a slow one while adding load precisely when the cluster is struggling.
- A deadline expiry is NOT retried. When some legs are acquiring and others
  are genuinely down and the window outlives the read budget, the op
  terminates as a deadline, and from the caller's side that is
  indistinguishable from ordinary slowness. It falls through to normal
  error handling.

Reads on the request path are bounded by arithmetic, not taste: an attempt
can burn the entire read budget before refusing, so the attempt count times
the budget, plus backoff, must fit inside the HTTP response deadline with
margin left for rendering. A test pins that relationship, so raising either
the attempt count or the read budget without re-examining the response
deadline fails the build rather than silently shipping a retry that
outlives the response.

The budget is chosen by the CALLER's context, not by the shape of the
call. Whether anything is waiting on a result is a property of the caller,
which the mechanism cannot see, so the cross-shard fan-out is exposed as
two operations rather than one with a tunable: a request-path form that
gives up quickly, and a background form that waits. A single shared entry
point invited the opposite mistake - an interactive command inherited the
patient budget and blocked for the full background span retrying a
best-effort lookup whose error it then discarded, so a user waited half a
minute for a value that was thrown away either way.

Cross-shard background scans (the referenced-blob set,
the key-gate prune) retry more patiently, because no request deadline
bounds them. How MUCH more is set by measurement rather than by feel: a
node holds its positions unmounted for the length of a handoff, so a
background retry span shorter than a realistic handoff is not patience, it
merely postpones the same failure. The span is pinned by a test against an
observed handoff window, and blocking a periodic background job for that
long is free because none of the three consumers is latency-sensitive. They and they retry as a WHOLE CALL rather than per-peer: a refused
peer's slice is absent from every other peer's result, so a partial fan-out
is never a usable answer. The patience is bought for one consumer in
particular. Blob GC acts on ABSENCE - it deletes any blob NOT in the
referenced set - so consuming a truncated set destroys live data, whereas
the other two consumers merely under-report and self-correct on the next
pass. That asymmetry is the general rule for any fan-out consumer: a
partial result is dangerous exactly when the code acts on absence rather
than on presence.

#### The value envelope and the strip-on-read invariant

At R>1 shale wraps every stored value in a last-writer-wins envelope (a
magic byte + a (timestamp, node-id) stamp + the opaque payload) so the read
fan-out can pick a winner across replicas. The cluster layer adds the
envelope on write and removes it on exactly ONE read path: the single-key
**replicated** Get. Every other read primitive `ShaleRepo` uses returns the
**raw stored bytes**, envelope included:

- a cross-shard aggregate scan (`aggregatePrefix`),
- a single-shard prefix scan (`scanPrefix`),
- a CAS transaction's `tx.Get` (counters, markers, JSON reads), and
- even a single-key Get on an R=1 or multi-backend node (the cluster only
  unwraps in the replicated-Get path, not the plain backend read).

`ShaleRepo` therefore treats every raw read as potentially-enveloped and
strips the envelope before decoding it, via a single `stripEnvelope` step
that is a **no-op for raw / pre-envelope values** (they carry no magic byte,
so they pass through unchanged). This invariant holds at every decode site:
both scan helpers, the per-owner and room-byte counters, the reservation
markers, the JSON CAS reads, the room values, and the single-key paste /
version reads. No hostthis payload begins with the envelope magic byte (JSON
rows begin `{`, counters are ASCII digits, markers / timestamps / the
`slug_owner` pointer are text, room values carry a `v` sentinel), so the
strip never misfires on a legitimate raw value.

The invariant is **not optional even on an R=1 deployment**: a value written
while the cluster briefly ran at R>1 stays enveloped on disk until it is next
overwritten, and an R=1 reader must still decode it. Stripping on every read
path is what makes a mixed R=1 / R>1 value population transparent to every
consumer, so a deployment can change replication factor without an offline
rewrite of existing values.

#### The rebalance safety contract (lossless data movement)

The crux of multi-node operation is what happens when membership changes.
Today (single node) all data lives in one node's backend. When a second
node joins, the consistent-hash ring reassigns roughly half the shards to
it, and the cluster **physically migrates** the keys of the reassigned
shards from the old owner's backend to the new owner's backend. This is a
live movement of authoritative data between two storage engines, the same
class of operation as the cutover migration above, and it must be
**lossless**: no key may be dropped, and no key's value may regress to a
stale or empty state, across the transition.

The contract the cluster guarantees, and that the groundwork tests below
must demonstrate on hostthis's actual data shapes:

1. **Copy-before-delete cutover.** A migrating shard is streamed from the
   source node's backend to the destination's backend and the
   destination's copy is verified (a checksum over the streamed
   key/value bytes) **before** the source deletes its local copy. The
   source keeps serving reads of the shard until the destination
   acknowledges the stream; only after that acknowledgement does a grace
   window elapse and the source sweep the now-foreign keys. The source
   never deletes a key it has not confirmed the destination received.
   A failed or interrupted stream (destination crash, checksum mismatch,
   timeout) leaves the source's copy in place and the next evaluation
   retries; it does not advance to the delete step.
2. **Reads stay correct throughout.** During the window in which a shard
   is in flight, a read that lands on the destination (because the ring
   already names it the new owner) is transparently served from the
   source, which still holds the authoritative copy. A reader never sees
   not-found for a key that exists; it sees the source's value until
   cutover completes, then the destination's identical copy.
3. **Writes are guarded, not lost.** A write that lands on a shard
   mid-migration is rejected with a retry-after signal rather than being
   silently applied to a copy that is about to be superseded. The client
   retries after the shard settles on its new owner. A write is never
   applied to the losing side of a cutover.
4. **The quota-relevant keys survive the move.** The per-identity quota
   is scan-derived, so what must survive a rebalance is the authoritative
   `pastes/*` / `versions/*` / `sites/*` rows (the source of truth the
   scan sums) and the per-owner `identity_pastes/<id>/*` /
   `identity_sites/<id>/*` enumeration index (the set the scan walks). All
   are ordinary keys on their shards; when a shard migrates they move with
   it like any other key and arrive on the new owner byte-for-byte
   identical, still readable via gRPC forwarding from any node. Because the
   quota reads live rows through the index rather than a stored aggregate,
   there is no counter that could drift across the move - an entry moves
   with its shard carrying the same cached values it already held. The same
   survives-intact contract holds for `slug_owner`, the enumeration index
   entries, and the value-bearing `identity_pastes` projections.

This contract is the property the groundwork must **prove on real data**
before any live multi-node deployment. The proof is an integration test
that populates a node with the full set of hostthis data shapes, joins a
second node, lets the ring rebalance, and asserts that every shape, the
quota counter most pointedly, is still readable through the cluster with
its original value after roughly half the keyspace has physically moved
to the new node. A rebalance that loses or corrupts data fails this test;
that failure, if it occurs, is the single most important finding of the
groundwork and gates the deploy step below.

#### Deploy shape is a separate, gated step

Wiring the multi-node config into the application (the `BindAddr` /
`GRPCAddr` / `Seeds` / replication-factor surface) and proving the
rebalance is lossless is application-level groundwork. **Reshaping the
deployment to actually run more than one node is a separate step, gated on
this groundwork being green.** The intended runtime shape, when that step
is taken, is a stable-identity replica set (a StatefulSet, each pod a node
with a durable per-node identity and its own object-store prefix), fronted
by a headless service for peer gossip discovery and a load balancer for
client traffic, scaled out one node at a time so each join triggers one
bounded rebalance. None of that orchestration is built here; this section
specifies only the application behavior and the safety contract the
deploy step depends on.

---

## Edge caching

hostthis has two scaling cliffs that a CDN solves:

- *Egress bandwidth*: a 10 MiB paste served at 100 req/s = ~2.5 PB/month.
  Hetzner free egress is ~20 TB; one viral paste could blow the budget
  in days. A CDN absorbs ~95% of reads at the edge, dropping origin
  bandwidth to a sliver.
- *Render CPU*: a markdown GET no longer renders on the server - it
  streams the raw bytes or the fixed shell, and the browser runs marked
  + DOMPurify. So a hot URL no longer pegs a server CPU core; the CDN
  still absorbs the egress for the raw bytes and the shell.

### Cache-Control posture

A non-negotiated paste read response (an HTML paste) sets:

```
Cache-Control: public, max-age=3600
ETag: "<sha256>"
Last-Modified: <RFC1123 from paste.UpdatedAt>
```

CDNs cache for one hour, then revalidate. Browsers cache and send
conditional `If-None-Match` / `If-Modified-Since` on revisits;
hostthis returns 304 Not Modified when the content SHA matches,
saving body bytes on the wire.

Apex landing page is `Cache-Control: public, max-age=300` (5 min) so
content updates propagate quickly without becoming a no-cache origin
hammer.

### 5xx observability on the read surface

Every 5xx returned by the paste/site read path (a metadata read failure,
a blob read failure, an unsupported stored kind) logs one warn-level line
carrying the slug and the underlying error before the generic
"internal error" body is written. The response body stays generic (no
internal detail leaks to clients), but the operator can always attribute
a read 500 from the logs - a read canary failure with no matching log
line means the request never reached this process. The slug is the only
request-derived value logged (slugs are public identifiers; no client
IPs, no headers, no payload).

#### The bare URL always serves the shell (no `Accept` negotiation)

A client-rendered kind (Markdown, Diff - any kind that ships a
client-render shell) is served at its **bare URL** (no `?raw` query) as a
*single representation*: the render shell, to **every** client - a browser,
`curl`, a link-unfurl bot, any `Accept`. The bare URL does **not** content-
negotiate. Raw bytes come only from the explicit `?raw=1` URL (over HTTP,
which the shell itself fetches) and from `ssh hostthis.dev get <slug>`
(over SSH). "`curl` gets the raw bytes at the bare URL" was never a
requirement - the intended model is that the bare URL serves what a browser
gets, and raw is an explicit opt-in.

Because the bare URL is one representation, it is safe to **edge-cache**: it
keeps the shared `Cache-Control: public, max-age=3600`. There is no per-
`Accept` variant, so the CDN hazard that an earlier `no-store` posture
guarded against is removed **at the root**: a CDN keys its cache on the URL,
not on `Accept` (Cloudflare honors only `Vary: Accept-Encoding`, never
`Vary: Accept`), but with a single representation there is nothing to mis-
pin - whichever client primes the edge, every later client gets the same
shell. `no-store` is therefore no longer needed anywhere in the paste serve
path for this reason.

The tradeoff: the shell is now edge-cacheable, so a shell/style change (a
`mdShellVersion` / `diffShellVersion` bump) propagates within the `max-age`
window (1h) OR immediately via the deploy-time edge purge. That is
acceptable: shell changes only ship on a deploy, and a deploy purges the
edge.

What is edge-cacheable:

- the bare URL of a client-rendered kind - now one representation (the
  shell), so it keeps `public, max-age=3600`.
- the explicit `?raw=1` URL - always raw, a distinct single representation.
  This is the read-throughput path the shells fetch, and it keeps
  `public, max-age=3600`.
- the immutable `/_hostthis/...` assets (the bundled renderer libs).
- an HTML paste's bare URL - HTML is not negotiated (one representation),
  so it keeps `public, max-age=3600`.

Static sites (multi-file) set `Cache-Control: public, no-cache` instead
of `max-age`. A single-file paste is the top-level document, which a
browser revalidates on every reload, so a `max-age` window never hides a
paste update. A site is different: its `index.html` loads sub-resources
(its js/css), and a browser serves those from cache WITHOUT revalidating
while they are fresh under `max-age` - so a re-deploy would not show until
each asset's `max-age` expired (the classic SPA "stale bundle after
deploy" trap). `no-cache` makes every site file revalidate against its
content-SHA ETag on each load: a 304 when the SHA is unchanged (cheap, no
body bytes) and fresh bytes when it changed, so a re-deploy is visible on
the next normal reload with no filename-hashing or version query. The
edge-cache benefit is preserved: a CDN still stores the bytes and serves
them after a 304, so egress stays absorbed; only a cheap revalidation
request reaches origin.

### Active invalidation: CachePurger interface

When the bytes served at a paste's URL change, the cached version at
the CDN edge (and in browsers) becomes stale. hostthis fires a purge
call so the next reader fetches fresh from origin.

Operations that fire a purge:
- `update` - content for the latest version changes.
- `delete` - URL should return 404, not the cached body.
- `pin` - the served version changes (e.g. pinning v1 hides v2 again).
- `unpin` - when a pin was holding the URL on an older version,
  unpinning re-exposes the latest; without a purge the old pinned
  bytes stick until max-age expires.

`rename` does NOT purge - the name is owner-only metadata, not part of
the public response. Same with `versions`/`list`/`whoami`/`get`.

The interface lives in the service layer; no production code - not even
the verb service that performs the mutation - knows which CDN is in
front, or that one is in front at all:

```go
type CachePurger interface {
    PurgePaste(slug domain.Slug) error
}
```

Invalidation is **transparent to the business logic.** The verb service
(`Manage`) performs the mutation and knows nothing about caching - it has
no `CachePurger` field and no purge calls. A thin `CacheInvalidating`
decorator wraps the verb service at the composition root
(`cmd/hostthisd`): it delegates every verb to the inner service and,
after a *successful* `Update` / `Delete` / `Pin` / `Unpin`, fires
`PurgePaste(slug)`. The mutation use-cases stay pure; cache invalidation
is a cross-cutting concern layered on by composition, not woven into the
domain logic. `rename` / `versions` / `list` / `whoami` / `get` /
`deleteVersion` are delegated without a purge - they don't change the
bytes served at the public URL (`deleteVersion` is refused outright when
the target is the currently-served version).

The purge is best-effort: a purge error is logged but never fails the
underlying operation (the paste IS updated/deleted on origin; the CDN
just keeps stale content for the remaining max-age).

Three implementations ship:

| Impl | When used | Behavior |
| --- | --- | --- |
| `noop` (default) | No CDN, or CDN with adequate max-age | No-op; relies on cache TTL expiry |
| `cloudflare` | Cloudflare in front | POSTs the slug's public URL variants to `/zones/<id>/purge_cache` |
| `fastly` (not shipped, easy add) | Fastly in front | POSTs to Fastly's purge API |

**Purge every served URL variant.** A paste is reachable at more than one
cache key, and the adapter must purge all of them or an edit leaves stale
content behind. In subdomain mode the variants for a slug are:

```
https://<slug>.<apex>/          the page (an HTML paste, or the markdown/diff shell)
https://<slug>.<apex>/?raw=1    the raw bytes the markdown/diff shell fetches
```

The markdown (and diff) render shell is a fixed, content-independent page
served at the bare `/`; the actual bytes live at `/?raw=1` (the shell
fetches them client-side - see "Client-rendered markdown"). Both the bare
`/` and `/?raw=1` are now edge-cacheable (`max-age=3600`, see "The bare URL
always serves the shell" above). The editable content lives at `/?raw=1`,
so purging only the bare `/` would leave stale content cached at `/?raw=1`
and an edited markdown/diff paste would show its OLD content until max-age
expired. The adapter therefore purges both: the `/?raw=1` purge is the one
that matters for an edit (the bare `/` is the content-independent shell,
which only changes on a deploy), and purging the bare `/` keeps the edge
consistent. The URL-variant policy lives in the adapter (which owns the
apex / scheme / URL-mode config); the service layer only ever names the
slug.

Env vars when `HOSTTHIS_CACHE_BACKEND=cloudflare`:

```
HOSTTHIS_CF_PURGE_TOKEN  CF API token, scoped ONLY to 'Cache Purge' on the zone
HOSTTHIS_CF_ZONE_ID      zone id of the apex domain
```

The adapter derives the per-slug purge URLs from the apex domain, public
scheme, and URL mode hostthis is already configured with
(`HOSTTHIS_APEX_DOMAIN` / `HOSTTHIS_PUBLIC_SCHEME` / `HOSTTHIS_URL_MODE`),
so no separate base-URL var is needed.

The purge token is the only long-lived credential hostthis needs for
the CDN; it's narrowly scoped (zone-level cache-purge only) so leakage
worst-case is "attacker can purge our cache (slowing us down briefly)".

### Switching CDN providers

Replacing Cloudflare with Fastly / Bunny.net / a different provider is:

1. Add an `internal/cache/<provider>.go` implementing `CachePurger`.
2. Wire it in `cmd/hostthisd/main.go` by extending the `HOSTTHIS_CACHE_BACKEND` switch.
3. Change nameservers / DNS at the registrar.
4. Reconfigure cache rules in the new provider's dashboard.

Total: ~100 lines of Go + dashboard work. The service layer is
unchanged; this is hexagonal-architecture portability in action.

### Apex must stay DNS-only when a CDN is in front

A subtle but critical setup detail: only the wildcard `*.<apex>` DNS
record is proxied through the CDN. The apex `<apex>` itself must remain
DNS-only (CF terminology: gray cloud) so the SSH listener on the origin
remains reachable. CDNs proxy HTTP/HTTPS only; they don't forward SSH.
The two surfaces don't overlap (ssh is always on apex, paste reads
are always on subdomains), so the split is clean.

---

## HTML sandboxing

**Origin isolation is the security boundary, not CSP.** Subdomain-per-paste
means each user-uploaded HTML lives on its own origin. Browsers enforce
the same-origin policy: cookies, storage, and JS from `abc12345.hostthis.dev`
cannot reach `xyz67890.hostthis.dev` or the apex. The apex `hostthis.dev`
never sets a `Domain=.hostthis.dev` cookie, so subdomain pastes cannot
read apex cookies either. This is the same model major user-content hosts
(codepen, jsfiddle, codesandbox, gh-pages) rely on.

Within a paste's own origin, we do NOT impose a Content-Security-Policy.
JS can do anything any same-origin script can do: load libraries from any
CDN, fetch any HTTPS endpoint, render WebGL, talk to APIs. The pragmatic
default matches the industry - codepen ships no CSP on user pens at all.

Response headers on paste reads:

- `X-Frame-Options: DENY` - no embedding the paste in iframes elsewhere
  (clickjacking defense)
- `Referrer-Policy: no-referrer` - visiting a paste leaks nothing about
  who sent it
- `Permissions-Policy: camera=(), microphone=(), geolocation=(), usb=(), payment=()`
  - deny everything that needs explicit user grant

### What this means for the visitor

A paste's HTML can:

- Load JS, CSS, fonts, images from any CDN
- Fetch any HTTPS API
- Render WebGL, Canvas, Web Audio, anything browsers support
- Inline `<script>`, `<style>`, modules
- Run user-supplied JS that does anything that JS can do

A paste's HTML cannot:

- Read cookies from `hostthis.dev` apex or other paste subdomains
- Touch the visitor's filesystem, camera, mic, or geolocation without
  the explicit prompt the browser shows (and Permissions-Policy denies
  some categories outright)
- Be embedded in another site's iframe (X-Frame-Options: DENY)
- Tell other sites where the visitor came from (Referrer-Policy)

Treat any URL on hostthis.dev as untrusted user content - same as you'd
treat a codepen, a gist, or a github.io page.

### Markdown rendering

Markdown is rendered to HTML in the visitor's browser. A Markdown read
returns a fixed, content-independent HTML shell that loads a bundled
client-side renderer (`marked`) and sanitizer (`DOMPurify`); the shell
fetches the raw Markdown bytes (via `?raw`) and renders them into the page. DOMPurify strips event handlers,
`javascript:` URLs, and dangerous tags before the HTML is inserted, so
uploaded Markdown still can NOT execute JS even though uploaded HTML can
- DOMPurify is the safety net for the markdown path, replacing the old
server-side bluemonday pass. The server never renders Markdown on the
read path, which keeps its memory constant regardless of paste size
(it streams the raw bytes with `io.Copy`, like the HTML path). The
in-repo `internal/render` package and `cmd/render-md` dev tool are
retained for offline use but are no longer on the live read path.

### Diff rendering

A diff paste follows the same model as Markdown. A diff read returns a
fixed, content-independent HTML shell that loads a bundled client-side
renderer (`diff2html`) and syntax highlighter (`highlight.js`), both
vendored as embedded assets served from `/_hostthis/...` (no runtime
CDN); the shell fetches the raw diff bytes (via `?raw`) and renders them
into the page with diff2html.
The view defaults to **line-by-line** with a toggle to **side-by-side**,
the choice persisted in `localStorage`; code is syntax-highlighted, and
the page is dark-mode aware via `prefers-color-scheme`. The diff shell is
served under the same `Content-Security-Policy` as the Markdown shell
(`script-src 'self'`, `connect-src 'self'`, no inline script), so the
only scripts that run are the vendored renderer + bootstrap. The server
never renders the diff: it streams the raw bytes with `io.Copy`, keeping
memory constant regardless of paste size.

### Abuse reporting

Content persists until its owner deletes it, so takedown is an operator
action rather than something the clock does. An operator can delete a
slug's row directly from the metadata store; the next read 404s and the
next sweep GCs the blob. A user-facing "report this paste" UI is out of
scope for v1.

---

## Readiness vs liveness (health endpoints)

The HTTP listener serves two probe endpoints, and they answer two
DIFFERENT questions on purpose:

- **`/healthz` - liveness.** "Is this process up?" Returns `200 ok`
  whenever the HTTP server can respond, and NEVER gates on storage
  state. It echoes `X-Backend-Color` when the replica is color-labeled.
  This is the restart signal: an orchestrator that sees liveness fail
  should restart the process. Storage health stays OUT of it, because a
  restart cannot repair an unmountable backing store - restart-looping
  a pod whose store cannot open only destroys the retry progress its
  running process was making.
- **`/readyz` - readiness.** "Should this replica receive traffic, and
  may a rollout proceed past it?" Returns `200` iff the metadata
  backend's readiness predicate passes, `503 Service Unavailable`
  otherwise. An orchestrator points its READINESS probe here; its
  liveness (and startup) probes stay on `/healthz`. A readiness-failing
  pod must be held out of rotation, never restarted.

Both are served on the same HTTP listener/port as the paste surface,
routed by path ahead of any Host-based routing (they answer on any Host
header), and both are deliberately un-gated: no auth, no keygate - a
probe must never be turned away.

### The failure class /readyz exists for

Degraded boot on the shale backend deliberately lets a pod come up with
storage units still unmounted: the process is up (liveness passes) and
the reconcile keeps retrying the mounts. That is the right per-pod
availability call, but it creates a rollout hazard: if the readiness
probe points at a liveness signal, a fleet-wide config error (a bad
credential, a bad bucket - anything that makes EVERY unit open fail on
every NEW pod) still reports each new pod "ready". A surge rollout then
replaces the entire fleet with pods that have mounted NOTHING and
"completes" while writes are down. Gating readiness on actual mount
state stalls that rollout at the FIRST new pod: it never goes ready,
the rollout cannot proceed, the old pods keep serving, and the operator
reads the cause straight off the probe body.

### The shale readiness predicate (mount floor)

On the shale metadata backend, `/readyz` returns 200 iff
`cluster.Ready(minMountedFraction)` holds: the pod has mounted at least
`ceil(f * desired)` of the storage-unit positions it currently owns.
The predicate's edge contract is shale's (see the shale SPEC "Mount
readiness"): `desired == 0` is vacuously ready, so a pod with no
assigned positions (mid-join, legacy single-backend mode) never wedges
its own rollout.

The fraction is the operator knob:

```
HOSTTHIS_READY_MIN_MOUNTED_FRACTION   mount floor in [0, 1]   (default 0.5)
```

- **Default `0.5`**: HALF the desired units must be mounted. This
  catches the uniform-failure class - a pod where every open fails has
  0 mounted, and 0 never passes any floor above 0 - while tolerating a
  pod briefly below full mounts mid-handoff or mid-join, so a healthy
  rollout keeps moving.
- **`0` disables the floor entirely**: `/readyz` is then always 200 (no
  mount floor - readiness reduces to process-up, the same gate as
  `/healthz`). The semantics live in the shale predicate (`f <= 0`
  clamps to "no floor requested"), not in a hostthis special case, and
  the response body still reports the live counts.
- **`1`** is the strict end: every desired unit must be mounted.
- **Malformed or out-of-range values refuse startup** with an error
  naming the variable - the same fail-loud config discipline as
  the shale dispatch timeouts. A typo in a
  readiness knob must not silently deploy as some other floor.

### Non-shale backends

The local backend has no mount concept: an open failure
there fails startup outright, so a process that is up IS ready.
`/readyz` returns 200 whenever the process is up - equivalent in gate
behavior to `/healthz`, but kept as a distinct endpoint so probe wiring
never changes when the metadata backend does.

### Response body (diagnosable with curl)

`/readyz` responds with the mount counts as JSON in BOTH directions
(200 and 503), so a stalled rollout is diagnosable with a curl against
the stuck pod - no debug endpoint, no shell in the image required:

```
{"ready":false,"desired":8,"mounted":0,"pending":8,"failedOpen":8,"lastAcquireError":"open unit 0: ..."}
```

`desired` / `mounted` / `pending` / `failedOpen` are the shale
mount-readiness counts (all zero on non-shale backends and on a legacy
single-backend shale node); `lastAcquireError` is one representative
acquire error, omitted when there is none. The body is `no-store`: a
probe result must never be cached.

---

## Self-hosting

The public `hostthis.dev` is the default deploy, but the same Go binary
runs on any box. Minimal runtime config (env vars or single TOML):

All operator knobs are flags or env vars on the binary (no config
file). Defaults in parens:

```
--ssh-addr               / HOSTTHIS_SSH_ADDR                listen for ssh                          (:2222)
--http-addr              / HOSTTHIS_HTTP_ADDR               listen for http                         (:8080)
--apex-domain            / HOSTTHIS_APEX_DOMAIN             public apex                             (hostthis.dev)
--mode                   / HOSTTHIS_URL_MODE                subdomain (prod) | path (dev)           (path)
--scheme                 / HOSTTHIS_PUBLIC_SCHEME           https | http                            (https)
--data-dir               / HOSTTHIS_DATA_DIR                where metadata + blobs live             (./data)
--landing                / HOSTTHIS_LANDING                 path to landing.html                    (web/landing.html)
--fresh-keys-per-subnet  / HOSTTHIS_FRESH_KEYS_PER_SUBNET   sybil-gate threshold                    (20)
--fresh-keys-window      / HOSTTHIS_FRESH_KEYS_WINDOW       sybil-gate rolling window               (24h)

# Standalone blob backend (dev/test; disk-only)
                         / HOSTTHIS_BLOB_BACKEND            disk                                    (disk)
# Production blobs go through the shale-collocated plane instead:
                         / HOSTTHIS_SHALE_BLOB_BUCKET       blob bucket on the metadata object store (unset = detached store)

# CDN / cache purger
                         / HOSTTHIS_CACHE_BACKEND           noop | cloudflare                       (noop)
                         / HOSTTHIS_CF_PURGE_TOKEN          CF token (Cache:Purge scope only)       (required if cloudflare)
                         / HOSTTHIS_CF_ZONE_ID              CF zone id for the apex                 (required if cloudflare)
                         / HOSTTHIS_PUBLIC_URL_BASE         base URL used to construct purge URLs   (https://<apex>)
```

The runtime container reads the same env vars. The operator supplies
a docker-compose (or equivalent) file out of band; this repo ships no
sample production compose.

### What's hardcoded vs operator-tunable

*Hardcoded* (product opinions, not knobs):
- Per-paste cap (10 MiB compressed)
- Per-identity quota (10 MiB compressed)
- Raw-input hard fast-fail (100 MiB, prevents unbounded reads)
- Blob compression (zstd level 3, all blobs)
- Sandbox headers (X-Frame-Options, Referrer-Policy, Permissions-Policy)
- Slug alphabet (`abcdefghijkmnpqrstuvwxyz23456789`)

*Operator-tunable*:
- Listen addresses (`--ssh-addr`, `--http-addr`)
- Public surface (`--apex-domain`, `--mode`, `--scheme`)
- Data location (`--data-dir`, `--landing`)
- Durable total-bytes ceiling: a quota on the blob bucket at the object
  store (e.g. a MinIO bucket quota), NOT an app flag - hostthis carries
  no `--storage-cap-bytes` knob (see "Limits → Durable total-bytes
  ceiling")
- Sybil gate (`--fresh-keys-per-subnet`, `--fresh-keys-window`,
  both can be tightened or relaxed for the operator's threat model)
- Same-identity create admission width
  (`HOSTTHIS_CREATE_ADMISSION_WIDTH`, default 2; see "Limits →
  Same-identity create admission")
- Readiness mount floor (`HOSTTHIS_READY_MIN_MOUNTED_FRACTION`,
  default 0.5, shale backend only; see "Readiness vs liveness")
- Standalone blob backend (`HOSTTHIS_BLOB_BACKEND=disk`, disk-only;
  production uses the shale-collocated blob plane via
  `HOSTTHIS_SHALE_BLOB_BUCKET`, not a standalone backend)
- CDN cache purger (`HOSTTHIS_CACHE_BACKEND=noop|cloudflare`) and its
  credential (`HOSTTHIS_CF_PURGE_TOKEN`)

Operators worried about disk pressure set the blob bucket's quota at the
object store (a hard, exact ceiling on real physical post-compression /
post-dedup bytes) and can put hostthis behind a reverse proxy that adds
per-IP rate limiting on top of the Sybil gate. A rejected `Put` past the
bucket quota surfaces to the user as a graceful "service is at capacity"
response, and the system recovers as owners delete content and the sweep reclaims
bytes back under the quota.

---

## Future directions (proposed, not built)

These are bigger bets that would grow hostthis from "host a renderable file
for 30 days" toward "deploy a small real app over SSH, no account." The first
of them (static directory hosting) has SHIPPED - see "Static site archives"
above. The persistence API has now shipped its FIRST CUT too - the no-auth,
capability-based **Rooms** KV store - see "Rooms (app persistence)" above;
what remains a PROPOSAL here is the richer end-user AUTH model layered on
top of rooms (the JWT-verifying / browser-keypair identity spectrum below).
Each deliberately revisits some of the v1 Non-goals below (a scope
expansion, not an accident). The throughline: shale (the distributed K-V)
is the persistence layer for both, and the differentiator across both is
the SSH-native, no-account, your-key-is-your-identity model.

### Static directory hosting (serve a whole site)

**This is now SHIPPED - see "Static site archives" above.** What was a
proposal here is real: a gzip-tar of a static site, piped over the
existing SSH upload surface (no new verb), is detected, safe-untarred,
stored as content-addressed blobs plus a manifest, and served at
`<slug>.hostthis.dev/<path>` under the same identity, quota, 30-day
and origin-isolation model as an HTML paste. The "Static site
archives" section is the authoritative description; this bullet is kept
only as the pointer from the future-directions framing it grew out of.

What shipped vs the original sketch: detection is gzip-tar only (plain
tar and zip stay out of scope); a default-on SPA fallback serves the
root `index.html` for an unmatched ROUTE while a missing ASSET still
404s (see "SPA fallback (route vs. asset)"); and the security story is
the existing origin-isolation boundary (raw files on their own
subdomain), not a "strict CSP" - the same posture HTML pastes already
have, so no new trust boundary was introduced.

This is "Surge.sh / Netlify-drop", but SSH-native and no-signup. It
revisited the "Binary / non-renderable file hosting" non-goal (a site
is still renderable content, just multi-file).

### A persistence API (a backend for small apps)

Pair static hosting with a small backend so users host REAL apps, not just
static pages. The engine already exists: shale.

**The no-auth first cut of this has SHIPPED as Rooms - see "Rooms (app
persistence)" above.** That section is the authoritative description of the
shipped shape: a per-app KV store keyed by an unguessable room UUID
(`<app>.hostthis.dev/api/rooms/<uuid>/<key>`, GET/PUT/DELETE), with strict
per-room isolation, no accounts, persisted over the metadata backend. The
bullets below are the REMAINING proposal - the richer end-user AUTH model
that would layer verifiable identity on top of rooms (so an app can enforce
"user A cannot overwrite user B's record," which the capability-only Rooms
tier deliberately does not). The shape and trust model here describe that
later tier; the Rooms section is what is real today.

- **Shape**: a per-app KV / document store. An app gets a namespace (a key
  prefix) in shale; its frontend hits `<app>.hostthis.dev/api/kv/<key>`
  (GET/PUT/DELETE), persisted in shale. A thin HTTP layer over the K-V. No
  server-side functions and no reactive subscriptions: running arbitrary
  user code is a sandboxing + security cliff, so this is NOT a FaaS.
  (The shipped Rooms tier is this shape with the room UUID as the access
  capability; the auth model below is what it gains next.)
- **The trust model (the thing that actually matters).** You cannot trust
  the client: any value the browser sends can be forged (edit the JS, or
  curl the API directly). Two DIFFERENT problems fall out, with different
  answers:
  - **Data integrity** ("is this score real?") is UNSOLVABLE client-side.
    No rule or auth fixes it; only re-running the app's logic on an
    authoritative server does, which is a full backend and out of scope.
    True of every client-side app: a hostthis-backed app either accepts it
    (fine for a casual leaderboard) or is not a fit.
  - **Access control** ("who may write WHOSE record?") IS solvable, and is
    the real job of the API's rules: user A cannot overwrite user B's
    record. The rules are about IDENTITY + OWNERSHIP, not value validation
    (a `score <= MAX` bound is a weak band-aid; `request.user ==
    resource.owner` is the load-bearing rule). This stops cross-user
    tampering even though it cannot stop A faking A's own value.
- **Identity** (two separate identities, do not conflate them):
  - **Creator** (who deploys) = the SSH key, as today. Optionally a linked
    account (e.g. Clerk) that groups a developer's many SSH keys into one
    identity with recovery; the SSH key stays the auth mechanism, the
    account is an opt-in management layer, the no-account path stays default.
  - **App end-user** (who plays / comments) is a per-app choice on a
    spectrum:
    1. NONE: anonymous + rate-limit (guestbook, public poll).
    2. CAPABILITY TOKEN: an unguessable per-record link; no accounts.
    3. BROWSER KEYPAIR: the user's browser generates a keypair (WebCrypto),
       the public key IS the identity, the app signs writes, hostthis
       verifies. Passwordless, accountless, no third party, no cost. No
       recovery / no cross-device without exporting the key (casual fit).
    4. REAL ACCOUNTS via JWT: the KV API is a JWT-VERIFYING resource server
       with a CONFIGURABLE issuer. The app sends the user's JWT; hostthis
       verifies it against the configured issuer's JWKS and keys the rules
       off the token claims (`sub` = user id). The issuer is either TURNKEY
       ("Sign in with hostthis", a hostthis-hosted issuer with Clerk under
       the hood: zero setup) or BYO (the developer points hostthis at their
       OWN issuer's JWKS: they own their users, pay their own auth, no
       lock-in). This is exactly how Supabase / Firebase / Hasura accept
       external auth: a JWT-verifying resource server.
  - The single hard commitment is the boring-correct foundation: the KV API
    verifies a JWT (or a keypair signature) and the rules read its claims.
    Same verification path for turnkey and BYO; the developer picks per app.
- **What it unlocks** (apps someone would actually ship): a self-hosted
  comments / guestbook widget (a Disqus replacement); a poll / voting app
  (shale's per-shard CAS already does atomic single-shard counts); a high-score /
  save-state for browser games; a form backend ("Formspree over SSH"); URL
  shorteners, visitor counters, feature flags.
- **The product fork to decide first.** Offering "Sign in with hostthis"
  (the turnkey issuer) makes hostthis an identity provider + ECOSYSTEM: a
  shared user identity across all hostthis apps, a sticky network effect,
  but a walled garden hostthis pays for and that couples apps to it.
  Supporting only BYO keeps hostthis a HOST: apps are standalone, the
  developer owns their users. The configurable-issuer design lets hostthis
  ship the turnkey option AS the low-friction default without forcing the
  walled garden, because BYO is always the escape hatch. Leaning ecosystem
  vs host is a product call, bigger than the mechanism.

Revisits the "Comments / threaded discussion" non-goal: we would not build
comments, but the persistence API lets a USER build them.

**Open design questions:**

- Static (now shipped; these are remaining refinements, not blockers):
  the SPA fallback has SHIPPED default-on (serve the root `index.html`
  for an unmatched route, 404 a missing asset - see "SPA fallback
  (route vs. asset)"); what remains open is custom subdomains, and a
  per-site opt-OUT flag if a site ever wants hard 404s on unknown
  routes. Deploy atomicity and the per-identity (rather than per-site)
  quota are already settled - see "Static site archives".
- Persistence: the no-auth Rooms tier has SHIPPED (see "Rooms (app
  persistence)"), which settles per-app namespacing, rate-limiting + abuse
  on the writable public API, and quota accounting for app data vs paste
  data. What remains open is the AUTH tier on top of it: the rule model
  (identity + ownership constraints evaluated server-side); the
  JWT-verifying resource-server design + the turnkey-vs-BYO issuer config +
  the browser-keypair signature path; and who pays for a turnkey "Sign in
  with hostthis" tier (the ecosystem-vs-host product call).

---

## Non-goals (explicitly out of v1 scope)

These are interesting but expand the product beyond "host renderable
content for a short window." Keep the surface small.

- **Binary / non-renderable file hosting**. ZIPs, photos, videos,
  arbitrary blobs are out of scope.
- **Comments / threaded discussion**. Out of scope.
- **Password protection on public pastes**. Signed share links cover the
  "private but shareable" case; password is duplicative friction.
- **View limits / view counts visible to the public**. Owner can see
  totals in `whoami`; no public-facing analytics.
- **Visual editor**. ssh pipe is the only authoring tool. Edit locally,
  re-pipe.
- **Teams / orgs / shared accounts**. Personal use only.
- **Custom domains** (`pastes.mycompany.com`). The wildcard subdomain
  pattern covers branding-via-slug well enough.
- **Email notifications**. The ssh response IS the notification.
- **MCP server**. The apex landing page is already terse, factual, and
  curl-able by any LLM; a separate machine-doc surface would just
  duplicate it.
- **Separate `/llms.txt`**. Same reason - the landing page IS the
  programmatic reference. Duplicating it as plain text would drift.
- **GitHub (or any third-party) account linking / OAuth**. ssh keys
  alone carry identity; we don't need a second source of trust.
- **Operator-configurable per-paste / per-identity caps**.
  Those three are hardcoded as product opinions. The Sybil gate IS
  operator-tunable, and the durable total-bytes ceiling is an
  object-store bucket quota the operator sets at the storage layer (not
  an app flag) - see "Limits" and the self-hosting flag table.

If real demand surfaces for any of these later, they can be added
without breaking v1 semantics. These are explicit no's, not oversights.

---

## Open questions

- **Quota display in `whoami` and `list`**: right now `whoami` shows
  only the active count, not "1.4 MiB / 10 MiB used". Probably worth
  adding so users see the cap approaching before they hit it.
- **Mermaid as first rendered-format expansion**: confirm the goldmark
  + mermaid SVG renderer choice once we get there; for now Mermaid is
  v2+ and out of scope.
- **Render cache for Markdown**: no longer relevant - rendering moved
  to the browser, so there is no server-side render to cache. The raw
  bytes and the fixed shell are both content-addressable and CDN-cacheable
  on their own.
