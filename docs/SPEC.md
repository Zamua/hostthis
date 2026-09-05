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
where the blob write happens after
the metadata commit. On a backend that binds bytes inside the metadata
commit this model COLLAPSES: the bytes are staged
durably before the metadata commits and the pointer co-commits with the row, so
there is no window between row and bytes - such a paste commits
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
before (a serialized in-transaction
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
  detached-store path only - a bind-inside-commit backend commits
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

It therefore charges like an update: every live version counts against quota.
Each version's charge is the sum of every final manifest path's compressed size.
The same content hash may refer to one physical blob, but logical quota charges
each retained path in each retained version. An owner reclaims bytes only by
deleting versions they no longer retain.

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

**Unchanged files reuse physical storage, not logical quota.** Blobs are
content-addressed across paths, versions, artifacts, and owners. Reusing an
existing hash writes no second physical object. It still contributes its
compressed size to every final manifest path that retains it, so physical
storage deduplication can never weaken a user's quota ceiling.

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
- **Quota** counts every path in every retained version against the SAME
  per-identity cap, using each final manifest entry's stored (post-zstd)
  compressed size. It does not deduplicate repeated content hashes for quota:
  content-addressing is a physical storage optimization, while quota is the
  logical amount retained for the owner. Each entry keeps its uncompressed
  `Size` for display; `CompressedSize` is the quota basis. The untar guard
  separately bounds the uncompressed running total against the owner's remaining
  allowance. The persisted charge is computed from the final normalized
  manifest, so duplicate archive entries that overwrite one path do not charge
  discarded staging attempts.
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
- **Atomic append.** Every blob in the final manifest is staged first. The
  metadata operation then admits and publishes one immutable manifest version.
  The old served version remains visible until publication finishes, and a
  half-finished deploy never serves a partial site.
- **Quota charges the appended version in full.** A redeploy retains every prior
  live version, so it receives no replacement credit. Admission evaluates the
  owner's existing all-live charge plus the final manifest's logical compressed
  size. Deleting a version later releases exactly that version's charge.

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
called for a per-app KV store fronted by a thin HTTP layer over the metadata backend,
and this section makes the no-auth, capability-based form of it real.

The deliberately-scoped commitment for this tier is **a key-value store keyed
by an unguessable room UUID, with strict per-room isolation, plus a generic
same-room WebSocket relay under the deployed app's own origin.** No accounts,
JWTs, or server-side app functions ship with it. This is enough to build a
collaborative app with no signup: a when2meet, shared list, poll, or retro board.

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
metadata store: the memory engine for dev, celld for the
object-store-backed and horizontally-scaled deploys), NOT in the
content-addressed BlobStore. The BlobStore is for the larger,
dedupe-worthy file bytes of pastes and sites; room values are small,
mutable, and per-room, so they belong with the metadata. Large blobs are
explicitly out of scope for rooms - an app that needs to host files uses
the archive/site feature, not a room value.

The implementation follows the existing repo-behind-service pattern:

- Pure `Room` and `RoomKV` domain types live in `internal/domain` and import no
  infrastructure. They own identifiers, values, byte/key counts, and cap checks.
- The `RoomRepo` port in `internal/service` exposes creation, point reads,
  snapshots, PUT, DELETE, and creation-ledger counts. The service applies use-case
  policy; HTTP only translates requests and responses.
- The memory adapter and celld adapter implement the same port. Memory keeps exact
  app aggregates under one mutex. Celld stores one whole Room document per Room
  cell and coordinates app-wide allocations through the app's Paste cell.
- Every address includes `(app_slug, room_id)`, so cross-app isolation is
  structural rather than a filter. The backend-agnostic conformance suite pins
  the observable contract.

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
  discipline the SSH side uses for `HOSTTHIS_SSH_PROXY_PROTOCOL`, and on
  the SSH side the trust is enforced both ways: with the flag set, the
  listener REQUIRES a PROXY header and refuses a connection that arrives
  without one. A headerless connection there means something bypassed the
  reverse proxy, and serving it would attribute the client to the proxy's
  own address - precisely the mis-accounting the flag exists to prevent.
  Deployments that enable the flag must therefore route ALL ssh traffic
  through the header-sending proxy; a direct dial to the listener is
  refused by design. Trusting
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
  **default 64 MiB of room value bytes per app** (and the room-creation rate
  limit caps the growth rate). The ceiling is exact across sibling rooms and
  concurrent writes. A mutation that would exceed it returns **507** with its
  prior room state intact. A new room is also refused with 507 while the app is
  already at its ceiling, so a full app cannot accumulate empty room records.
  Deletes and smaller replacements commit first. Capacity release is attempted
  in the same turn and a durable alarm retries it, so success may be reported
  before a sibling observes the freed capacity. The coordinator may temporarily
  charge MORE than the committed room bytes while recovering an interrupted
  mutation, but it must never charge less. This is the room-tier analogue of
  the per-identity paste quota: one app cannot consume the whole service.
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

Rooms expose one generic same-origin WebSocket endpoint:

```text
GET (Upgrade: websocket) /api/rooms/<uuid>/ws
```

The app slug must name a live paste or site, and the UUID must name an existing
room. A malformed UUID is refused before upgrade; an unknown app or room is a
404. The normal same-origin policy rejects a third-party browser origin. The
room UUID remains the entire participant capability.

### Cell-owned fan-out

`hostthisd` terminates the public socket and opens one cluster-internal socket
to the Room cell. The Go proxy owns origin policy, per-process connection
admission, and the client-facing heartbeat. It pipes text and binary frames in
both directions without interpreting them. The upstream socket is not pinged,
so a quiet hibernatable Room is not woken merely to prove liveness.

The Room cell is the only broadcast point. Exactly one cell owns one room, so
there is no pod mesh and no cross-stream ordering problem.

Two frame classes share the socket:

- **Durable mirror.** A successful HTTP room PUT or DELETE commits the Room
  document and dense sequence first. The cell then sends a JSON `put` or
  `delete` frame carrying that sequence to every connected socket. Output gating
  prevents a send from escaping before the local commit is durable. The send is
  not itself durable: a cell stop after commit can omit it. Clients reconnect for
  a fresh snapshot when they observe a sequence gap or resume from suspension.
- **Ephemeral relay.** A client-sent text or binary frame is copied
  byte-identically to every OTHER socket in that Room, never back to its sender,
  never to another room or app, and never to storage. Text objects whose `type`
  is `snapshot`, `put`, or `delete` are reserved for server control frames and
  close the sending socket rather than reaching peers. Durable changes continue
  to use the HTTP KV path so the byte caps and sequence exist in one place.

A client-sent ephemeral frame may carry at most 32 KiB, bounding one sender's
fan-out amplification explicitly rather than inheriting the WebSocket library's
default. Server-generated snapshot and mirror frames may carry up to 2 MiB so a
full 256 KiB room still fits after worst-case JSON escaping. The Go proxy applies
these asymmetric limits at the two read boundaries. A dead or closing peer is
skipped without blocking the remaining fan-out.

### Snapshot and sequence contract

The first server frame is:

```json
{ "type": "snapshot", "seq": 17, "state": { "key": "value" } }
```

The Room attaches the hibernatable socket and reads the local Room document in
one input turn, so no local mutation can splice between attachment and snapshot.
Every later durable mirror has a sequence strictly greater than the snapshot it
follows. The sequence is dense: exactly +1 for every committed PUT or DELETE,
including deletion of an absent key.

A client applies mirrors above its snapshot sequence, discards a duplicate at
or below it, and reconnects for a new full snapshot when it detects a gap. No
frame history or per-client durable cursor exists. A reconnect is a fresh join,
which makes browser reload and foreground recovery the same path.

### Lifecycle and bounds

Each Go process admits at most 64 connections for one room and 1024 across one
app. These are process resource bounds, not global audience limits. A refused
admission returns 429 before the WebSocket handshake. Each connection is
released exactly once when dialing fails or either side closes.

The Go proxy pings the public client every 20 seconds and requires a pong within
10 seconds. This is below normal reverse-proxy idle timeouts and reaps dead
mobile connections. The canonical browser client also reconnects with
exponential backoff and jitter, reconnects immediately on foreground recovery,
and buffers only bounded durable actions while disconnected.

The live celld harness is the acceptance gate: multiple clients receive durable
frames in sequence; a late join receives an exact snapshot splice; ephemeral
text and binary frames reach peers only; room and app isolation hold; deletes
mirror correctly; reconnect works; and admission slots are reclaimed.

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

Optional `--name`. The `--` terminator is required: without it `ssh` parses
the flag as its own option and fails before the server sees it (see "Flags
need a `--` terminator" below).
```
cat demo.html | ssh -T hostthis.dev -- --name "Acme prototype v3"
https://abc12345.hostthis.dev
"Acme prototype v3"
<QR code of the URL, on stderr>
```
The name is owner-only metadata for `list`; it never appears in the
URL. Names are 1–60 chars, any printable Unicode except newlines.

**A flag directly after the host needs a `--` terminator.** An upload has no
verb, so its flag would be the first thing after the destination, and `ssh`
eats it. Upload flags therefore require `--`:

```
cat doc.md | ssh -T hostthis.dev -- --name "design notes"
```

Verb commands do NOT need it, and must not be written with one, because the
verb itself protects the flags that follow:

```
ssh hostthis.dev list -o json
```

The rule comes from `ssh`, not from hostthis. After `ssh` consumes the
destination it *resumes* parsing the remaining arguments as its own options,
but `getopt` stops at the first argument that does not begin with `-`. A
verb (`list`, `whoami`, `versions`) is such an argument, so parsing halts
there and everything after it is passed through untouched. An upload has no
verb, so a bare `--name` is the first token `ssh` re-examines, and it is
consumed client-side - the server is never contacted. Quoting the flags as a
single argument does not help, since `getopt` only tests for a leading `-`.

To tell which side rejected a flag, run it with no stdin so no paste is
created, and read the error:

```
ssh hostthis.dev -- --not-a-real-flag
hostthis: unexpected argument "--not-a-real-flag"     <- reached the server

ssh hostthis.dev --not-a-real-flag                    (WRONG, for contrast)
hostthis.dev: illegal option -- -                     <- ssh ate it
```

An error naming the *host* as the program is `ssh`'s own `getopt` failing,
because by then it has advanced past its `argv[0]`.

Verified on OpenSSH 10.2p1 (macOS) and 9.6p1 (Linux); the behaviour comes
from portable OpenSSH itself, so it is not platform-specific. Server-side
parsing is unaffected - the server accepts `--name` and `--type` exactly as
specified - so this constrains only how the client must be invoked.

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

**Delete heals its own lost tail (row absent, index entry present).** A
whole-paste delete is two transactions: the `{slug}` CAS removes the
paste row, its versions, the slug binding and the blob binds; a
follow-on `{id}`-shard CAS drops the owner's enumeration row and
owner-document entry together. No durable intent covers a delete, so a
process death (or a refused handoff) between the two strands the second
half: the paste is gone, but the caller's own index still carries the
slug. Every owner read counts that entry (list, whoami, the quota sum -
no read validates entries against rows, see "Phantom entries are
accepted, not repaired"), and every owner verb refuses at its ownership
read, which needs the very row that is gone. Insert's entry-first order
can strand the same shape (entry written, crash before the row, intent
lost).

`delete <slug>` is the repair verb for that residue. When the paste row
is ABSENT but the slug still sits in the CALLER'S OWN index (their
owner document, or their enumeration row for a pre-doc owner), the
delete drops BOTH index representations in the same guarded
single-shard `{id}` CAS the normal delete tail uses, and reports
`deleted.` (exit 0). Success, not not-found, is the honest answer: the
caller's intent - this slug no longer exists under my account - is
exactly the state the verb just made true. Not-found would leave an
entry counted forever that no verb can reach.

The heal cannot leak or touch anyone else's paste:

- When a paste row EXISTS under the slug - whoever owns it, including a
  re-mint after the caller's copy was removed - the response stays
  exactly the standard not-found and nothing is dropped. The heal fires
  only on row-absent + slug-present-in-the-caller's-own-index, so
  existence never leaks.
- The drop is guarded by the index entry's immutable `created_at`
  stamp, the same guard the normal delete tail carries: a same-owner
  re-mint racing the heal holds a fresh stamp and keeps its entry.
- Cost is point reads plus one single-shard CAS. No scans.

Both representations must go in the one CAS. Dropping only the document
entry would leave the renderable enumeration row behind, and a later
document rebuild (the heal-on-write walk) copies renderable entries
from their cached fields without consulting the paste row - the phantom
would come back. The contract is service-level and backend-independent:
a delete whose paste row is gone but whose entry lingers in the
caller's own index succeeds and removes the entry. A backend that
cannot strand such residue (a single-transaction delete) satisfies it
vacuously.

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
    cat doc.md   | ssh -T hostthis.dev -- --name "design notes"

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

## Room push (scheduled Web Push)

A static app can persist and sync state through rooms, but it cannot say
"time to clean" at eight in the morning: a page cannot schedule a notification
for when it is closed, and Web Push needs a server that holds subscriptions,
keeps VAPID keys, and sends at the right moment. Room push is that server, kept
to the rooms model: no accounts, no server-side app code, the room UUID is the
only capability.

### Surface

All under the deployed app's own origin, next to the rooms API:

```text
GET    /api/push/key                            -> { "key": "<VAPID public key, base64url>" }
PUT    /api/rooms/<uuid>/push/subscriptions     PushSubscription JSON          -> 204
DELETE /api/rooms/<uuid>/push/subscriptions     { "endpoint": "..." }          -> 204
GET    /api/rooms/<uuid>/push/subscriptions     -> [{ "endpoint": "...", "added": "<RFC 3339>" }]
PUT    /api/rooms/<uuid>/push/schedule          Schedule                       -> 204
GET    /api/rooms/<uuid>/push/schedule          -> Schedule (empty items when unset)
POST   /api/rooms/<uuid>/push/test              -> 202 { "sent": n, "pruned": n }
```

`push` and every key under `push/` are reserved path segments of the room
API, carved out before the KV verbs exactly as `ws` is; a KV key named `push`
or starting with `push/` is refused with 400. Room keys with other shapes
(`push:2026-09-06`) are ordinary data.

A `PushSubscription` is the browser's JSON: `endpoint` (an `https` URL of at
most 1 KiB whose host belongs to a known push service: Apple, Google, Mozilla
or Microsoft), `keys.p256dh` (the 65-byte uncompressed P-256 point, base64url)
and `keys.auth` (16 bytes, base64url). Subscriptions are deduplicated by
endpoint; a repeated PUT refreshes the keys. Anyone holding the room link can
add a device, which is the trust model rooms already have.

A `Schedule` is:

```json
{
  "tz": "America/New_York",
  "items": [
    { "id": "morning", "at": "08:00", "days": [1, 2, 3, 4, 5, 6],
      "title": "Rota", "bodyKey": "push:{date}", "url": "/?room=<uuid>#/", "tag": "rota-today" },
    { "id": "once", "when": "2026-09-12T08:00:00-04:00", "title": "Rota", "body": "Deep clean" }
  ]
}
```

- `tz` is always present and names an IANA zone the runtime knows. `at` is
  `HH:MM` local time in `tz` and `days` is a non-empty set of distinct weekdays
  (0 = Sunday); a one-shot item carries `when` (RFC 3339 with offset, in the
  future) instead of `at` and `days`.
- `id` is 1 to 32 characters of `[A-Za-z0-9_-]`, unique within the schedule.
  `title` is 1 to 64 bytes of UTF-8, `url` at most 512 bytes, `tag` at most 64
  bytes; `url` and `tag` are optional.
- Exactly one of `body` (at most 1 KiB of UTF-8) or `bodyKey` (a valid room key, in
  which `{date}` is substituted with the local date in `tz`, e.g.
  `2026-09-06`) is present. The body is read from the room at send time, so
  the server runs no app logic: the app pre-writes the next days' summaries
  and a missing key means nothing is sent for that day.
- PUT replaces the whole schedule; an empty `items` list clears it. Validation
  refuses the whole document; nothing is applied partially.

### Delivery

Delivery is Web Push (RFC 8030) with `aes128gcm` content encoding (RFC 8291)
and a VAPID `Authorization` header (RFC 8292), sent with `TTL: 86400` and
normal urgency. The notification payload is the JSON object
`{ "title", "body", "url", "tag" }`; a payload over 2 KiB is skipped, never
truncated. A `404` or `410` from the push service deletes that subscription,
unless it was added less than a minute earlier: a push service can answer
`404` for a registration it has not finished propagating, so inside that
window the send counts as failed and the subscription stays.
Other failures are not retried within a fire; the next scheduled fire is the
retry. A send waits at most 10 seconds and never follows a redirect: a 3xx is
a failure, so ciphertext and the VAPID token reach only the endpoint the
subscriber registered. The sends of one fire run concurrently. Nothing is
recorded about who received what.

The Room cell owns the whole feature: the subscription list, the schedule,
per-subscription daily send counters, the timer, encryption, and the outbound
HTTP to the push services. The runtime's cells make outbound requests and carry
the WebCrypto primitives the protocol needs (ECDH and ECDSA on P-256, HMAC for
HKDF, AES-GCM); a subscriber's raw `p256dh` point is imported through its SPKI
wrapping. A `test` send runs inline in the request and reports what it did.

One P-256 VAPID key pair exists per app, generated on first use and held by the
app's own cell beside its room coordinator. No route returns the private key.
A Room cell never holds it either: it asks the app cell for a signed VAPID
token per push-service origin (`aud` is the origin, `sub` names the apex,
`exp` is 12 hours out) and caches nothing across fires.

### Timer

A room with a non-empty schedule keeps one due instant: the earliest next fire
across its items, computed in `tz`. A room without a schedule keeps no timer
and costs nothing. The Room cell has exactly one alarm, already used by the
budget recovery protocol, so both concerns share it: the alarm handler resumes
any pending budget operation and then fires any due push items, and every
re-arm sets the alarm to the earlier of the two deadlines rather than clearing
it. A budget concern that no longer needs the alarm must not disarm a pending
push fire.

When the alarm fires, every item whose due instant has passed fires once. A due
instant more than 15 minutes in the past is skipped rather than replayed: a
stale reminder is worse than none. A fired or skipped one-shot item is removed
from the schedule. A recurring item's next due instant is then recomputed. The
local date that resolves `{date}` and the daily counter is the item's due
instant in `tz`, not the instant the alarm happened to run. Counters count
attempts, including failed ones, so a broken push service cannot turn one
subscription into unbounded outbound traffic; the ninth attempt to one
subscription in one local day is skipped.

Push state lives in the Room cell beside the room document but outside the KV
namespace: it is not part of a scan or snapshot, does not count against the
room byte ceiling, and is bounded only by its own caps. The memory backend
stores subscriptions and schedules with the same validation and reports the
public key, but delivers nothing: push delivery is a celld feature, and the
conformance suite pins the storage contract on both backends.

### Caps

- 16 subscriptions and 16 schedule items per room.
- 2 KiB payload; subscription endpoint at most 1 KiB.
- 8 sends per subscription per local day.
- One `test` per room per minute (429 with `Retry-After`).

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

The per-identity quota and per-paste cap are distinct: 100 MiB total and
10 MiB for one paste. The per-paste cap bounds one request's compressed
payload; the per-identity quota bounds accumulated logical storage.

**Writes are constant-memory.** A single-document upload compresses into a
temporary spill file while hashing and counting it, then streams that file to
the blob store. A static-site file follows the same spill-then-stream shape.
Resident memory is bounded by compressor and copy buffers rather than payload
size. Static sites count against the same identity cap using each manifest
path's compressed size.

**Reads are constant-memory too.** Every path that serves stored bytes
streams them: the HTTP raw read, a static site's files, and the `get`
verb all copy from the blob port's reader straight to the client. A read
therefore costs a copy buffer regardless of whether the paste is a
kilobyte or the 10 MiB ceiling, and the decompressed size never lands in the
heap.

Blob storage is content-addressed by the uncompressed SHA-256, so identical
content shares one physical object. Quota is logical rather than physical:
every manifest path and every live version carries its own compressed-byte
charge even when their content hashes match.

Deleted versions contribute zero bytes to logical quota and are inaccessible.
Their metadata remains as a tombstone; physical content-addressed objects are
not synchronously deleted.

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
Room push adds its own caps (16 subscriptions and 16 schedule items per
room, 2 KiB payload, 8 sends per subscription per local day, one test per
minute); see "Room push".

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

The identity-leading entry is a DERIVED index. Cross-scope transactions
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

The per-identity quota check, Paste transitions, and Sybil admission are
serialized inside their owning aggregate. The durable total-bytes ceiling is
separate: it lives at the object store as a bucket quota and surfaces as
`ErrServiceFull` from blob `Put`.

- **Memory backend.** One mutex covers the store. Maintained per-owner and
  per-app aggregates make quota decisions point-addressed and exact.
- **Celld backend.** v0.4 may serve several events concurrently, so every
  check-and-mutate route in Identity, Paste, Room, and Subnet runs under
  `blockConcurrencyWhile`. Local multi-key transitions use one storage
  transaction or batch. Room's cross-cell byte invariant additionally uses the
  versioned escrow protocol above.

The service layer treats both backends identically: a local multi-record
transition fully lands or remains at its prior state, and concurrent admission
cannot race a stale check.

### Threat model: what's bounded and what isn't

*Bounded by the protocol*:
- One ssh key → 10 MiB of active LOGICAL bytes, ever
- One IP subnet → ~20 fresh keys × 10 MiB = ~200 MiB/day logical
  (~20–40 MiB/day actual storage after zstd)
- All identities combined → the object-store bucket quota on real
  physical bytes (post-compression, post-dedup), enforced by the storage
  layer, not an app-level scan
- Concurrent per-identity and per-app quota races → serialized aggregate-local
  decisions on both backends; the Room escrow protocol preserves the cross-cell
  app ceiling

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

Blob bytes are independent of metadata. The service uses one
content-addressed blob port and selects its adapter with
`HOSTTHIS_BLOB_BACKEND`:

```text
HOSTTHIS_BLOB_BACKEND=disk    # default
HOSTTHIS_BLOB_BACKEND=s3      # production
```

Both adapters key the same compressed representation by the SHA256 of the
original bytes. Metadata therefore stores a content identity, never a provider
URL, filesystem path, or object-store key. Changing providers does not change
the domain or metadata shape.

**`Put` and the `ErrServiceFull` sentinel.** When an object store rejects a
write because its bucket quota is exhausted, the S3 adapter maps that response
to `ErrServiceFull`. Upload and site-deploy services translate the sentinel into
the graceful "service is at capacity" result. The application performs no
service-wide byte scan. The disk adapter has no bucket quota and therefore
cannot produce this result.

### Available backends

- **`disk`** stores each object at
  `<data-dir>/blobs/<sha256[:2]>/<sha256>`. It is the local development and test
  default.
- **`s3`** stores each object at
  `<prefix>/<sha256[:2]>/<sha256>` in the configured bucket. It is the durable
  production byte plane. Endpoint, bucket, region, credentials, TLS, and prefix
  come from `HOSTTHIS_S3_*` settings.

Neither adapter is supplied by the metadata backend. Production combines celld
metadata with S3 blobs; local development normally combines memory metadata with
disk blobs.

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
The same magic+zstd format is shared by every blob backend, so
a blob's stored bytes are identical whichever path wrote them.

### Local-disk write-back cache (optional, opt-in)

A blob `Put` against S3 dominates upload latency, while local hashing,
compression, and metadata writes are comparatively small. For deploys that can
tolerate a local-durability window, an optional **local-disk write-back cache**
can sit in front of either blob adapter. It is useful in practice only for a
remote adapter.

It is **off by default**. The strict, durable-before-ack behavior is unchanged
unless an operator opts in with:

```
HOSTTHIS_BLOB_WRITEBACK=true            # enable the write-back cache (default false)
HOSTTHIS_BLOB_WRITEBACK_DIR=<path>      # local cache dir (default <data-dir>/blob-cache)
HOSTTHIS_BLOB_WRITEBACK_MAX_BYTES=<n>   # soft cap on cache size in bytes (default 1 GiB)
```

When enabled, the cache wraps the configured blob adapter and changes the blob
path as follows:

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

Two adapters implement the application's metadata ports:

- **memory** - the default in-process adapter. It is ephemeral, requires no
  external service, and is used by local development and most tests.
- **celld** - the production adapter. Identity, Paste, Room, and Subnet cells
  own durable metadata over a celld fleet's object store.

The service and transport layers depend only on domain-shaped ports. The
backend-agnostic conformance suite runs the same behavior against memory and a
live celld Worker.

```text
HOSTTHIS_METADATA_BACKEND=memory                  # default
HOSTTHIS_METADATA_BACKEND=celld                   # production
HOSTTHIS_CELLD_ENDPOINT=http://celld:8080         # required for celld
```

### Atomicity contract

The memory adapter serializes every check-and-mutate operation under one mutex.
The celld Worker serializes mutating events per cell and commits local multi-key
transitions with one storage transaction or batch. Cross-cell invariants use the
durable protocols specified in the celld section below; they are not one global
transaction.

### Blob and operator boundary

Metadata and payload storage are independent ports. Local development combines
the memory adapter with the disk BlobStore. Production combines celld metadata
with the content-addressed S3 BlobStore. The celld fleet bucket and payload
bucket are separate security and lifecycle domains.

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

**Tombstoned versions release their bytes.** A deleted version is
app-final and content-inaccessible, and "DeleteVersion frees quota"
implies the storage is freed too, so `DeleteVersion` unbinds that
version's blob in the same transaction that writes the tombstone.
Recoverability does not depend on the app keeping a reference: it is
provided beneath the app by object-store versioning plus a
noncurrent-version lifecycle, an operator-level safety net configured
outside this repo.

### The two backends and the conformance gate

`HOSTTHIS_METADATA_BACKEND` selects the metadata plane:

- **memory** (default) - `storage.MemRepo`, an in-process implementation of
  every port under one mutex. Ephemeral by design; the dev, test and e2e
  engine. Being single-mutex makes it the STRICTEST backend: every
  check-then-write is atomic, so anything admitted concurrently here is
  admissible everywhere.
- **celld** - the production plane, specified under "Celld-backed metadata
  storage".

What keeps two implementations honest is the conformance suite, not
inspection: the same assertions run against MemRepo on every
`go test ./...` and against a live celld fleet when `CELLD_TEST_ENDPOINT`
names one. A behavior difference between backends is, by construction, a
failing test in one of them.


### Static-site storage

A static site is a paste whose version kind is `site` and whose manifest maps
safe relative paths to content-addressed blob descriptors. The root manifest
entry is `/`. Site files live in the configured `BlobStore`; metadata stores
only the paste row, version descriptors, and manifest.

`storage.Sites` translates the `service.SiteRepo` vocabulary onto the same
`PasteRepo` used by documents. There is no second site key family, owner index,
or quota sum:

- A first deploy inserts one site-kind paste.
- A redeploy appends a manifest version. Prior live versions remain available
  for pin, rollback, roll-forward, and per-version deletion.
- Each version is charged by every manifest path's compressed size. Shared
  content deduplicates only the physical object in the `BlobStore`.
- List and owner-byte accounting use the paste projection, where sites already
  appear, so the site adapter returns no duplicate listing or sum.
- Files are staged by content hash before a slug is chosen. A slug collision
  retries the metadata insert without rewriting the archive.
- Reads reject a document-kind paste as not found rather than treating its
  one-file manifest as a directory.

This representation makes paste/site slug collision impossible by construction:
one slug names one paste aggregate and its version history. Memory and celld
backends run the same site conformance suite, including manifest round-trip,
version charging, quota interaction, ownership privacy, and slug reservation.

### Artifact accounting on celld

The Paste cell owns authoritative versions, the served projection, a monotonic
`accountingVersion`, and any pending cross-cell operation. The Identity cell owns
the owner's exact all-artifact charge and point-readable listing. For every
artifact `a`:

```text
publishedCharge[a] <= allocatedCharge[a]
sum(allocatedCharge[a]) <= ownerCap
```

Stable state has equality. Growth reserves first and publishes second. Shrink or
whole deletion tombstones locally first and releases second. Recovery may
conservatively overcharge, but no visible retained version may be uncharged.

Each incarnation of a slug carries an opaque `generation`. Every accounting,
projection, delete, and recovery call includes it. Identity rejects a generation
that does not match its current entry, and permanent operation receipts remain
keyed by `(slug, generation, operation ID)`. A delayed call from an artifact that was deleted
and re-minted can therefore neither charge, release, nor rewrite the new
artifact.

The Identity decision record is:

```text
{ version, allocated, target }
```

Its transition matches the Room escrow protocol: older versions are stale; the
current version is an idempotent replay only for the same target; skipped
versions and target mismatches are errors; shrink is always granted; growth is
granted only when replacing the artifact's allocation keeps the owner's total at
or below the cap. Granted and refused decisions persist. The generic listing
projection endpoint cannot mutate `allocated`.

An append stores the complete candidate version and pending operation with an
alarm, obtains the Identity grant, publishes the version and served head, then
updates the guarded Identity projection. A refusal clears the pending operation
without publishing. A version delete first writes its tombstone and rolls the
served head, then settles the lower absolute charge. Pin and unpin keep the
absolute charge unchanged but use the same version fence and recovery path to
refresh served size, kind, pin, and latest-version projection. A `pending` to
`failed` transition publishes the failed row and a target-zero operation together;
the alarm drives the same guarded decision and drop path, so response loss cannot
strand either a charge or a projection. Whole deletion
keeps a tombstone fence until Identity confirms a guarded zero allocation and
removes the current listing entry. Only then can the Paste cell accept a new
incarnation.

Every pending operation and its immediate alarm are persisted together. The
alarm and the next mutation resume pending work before doing anything new.
Permanent operation receipts make a repeated request return the original result
instead of appending a second version. Absolute targets, generation guards, and
monotonic versions make response loss and delayed delivery safe.

A first-version create reserves the Identity charge and persists a create intent
with an opaque fingerprint of the exact Paste row in one transaction. Paste
publication records the same fingerprint. While the intent is outstanding,
replaying the same generation succeeds only when its fingerprint matches; a
same-generation request with different content is a conflict, and once the
intent is discharged a repeated reservation for the slug is refused as taken
regardless of fingerprint. Recovery runs once the intent is older than a fixed
grace (30 seconds), so an in-flight create is never mistaken for a crashed one. It
atomically inspects the Paste cell and, when the
matching row is absent, fences that generation against every late `put` before it
releases the reservation. When the row is present, recovery requires the matching
fingerprint and confirms from the row's authoritative status. Confirmation may
advance `pending` to a terminal status but cannot regress a terminal projection.
Malformed or unknown create intents retain their alarm rather than silently
stranding an unowned reservation.

The owner listing exposes two different quantities:

- `StoredBytes` is the sum of all non-deleted version charges for the artifact.
- `Size` is the currently served version's charge.

Pinning changes `Size` and served kind/content, never `StoredBytes`. Owner totals
sum `StoredBytes` only.

A legacy artifact row that predates generations adopts lazily, on first
mutation: the Paste cell assigns a fresh opaque generation, derives the charge
from its live versions, and idempotently seeds the Identity account before the
mutation proceeds; no quota refusal applies to already-retained data. A caller
holding the legacy row (an empty generation) addresses that incarnation; an
empty generation against an adopted row remains a conflict. Reads serve legacy
rows unchanged. There is no offline reconciliation pass and no flag day.

### Room storage

Rooms persist small mutable app state through the `service.RoomRepo` port:

- `CreateRoom(room, subnet, appCap, now)` creates an empty room, records its
  creation-ledger row, and refuses creation while the app is already at its
  byte ceiling.
- `GetRoom`, `GetValue`, and `ScanRoom` read one room.
- `PutValue(..., appCap, now)` enforces the per-room and per-app ceilings and
  returns the mutation's dense room sequence.
- `DeleteValue(..., now)` is idempotent and also consumes one room sequence.
- `CountRoomCreates(..., window)` counts and prunes the soft creation-rate
  ledger.

There is no room-retention or full-room-delete interface. Rooms persist
indefinitely. Creation-ledger entries are the only room records removed by
elapsed time, because they are rate-limit state rather than user content.

The memory adapter stores the room aggregate under one mutex. One critical
section checks both byte ceilings, mutates the namespace, and increments the
sequence, so its cap and sequence are exact.

The celld adapter uses two cells through the same port:

- The **Room** cell owns one room's metadata, KV document, byte count, dense
  sequence, pending budget operation, recovery alarm, and hibernatable sockets.
- The app's **Paste** cell owns the creation ledger and point-addressed byte
  allocation for every room under that app. It does not require a paste row, so
  site-backed apps use the same app-slug coordinator.

This is a distributed invariant, not a transaction. It follows an escrow rule.
For every room `r`:

```text
actual[r] <= allocated[r]
sum(allocated[r]) <= appCap
```

`actual` is the byte count committed in the Room cell. `allocated` is the
coordinator's durable charge. These two inequalities prove that committed room
bytes cannot exceed the app ceiling. Stable state has equality; recovery may
overcharge temporarily but must never undercharge.

#### Versioned room-budget protocol

Each Room document stores a `budgetVersion` separate from the user-visible
mutation `seq`. A rejected or interrupted budget attempt advances the budget
version without consuming a mutation sequence. While an attempt is incomplete,
the document also stores:

```text
pending = {
  targetBytes,
  appCap,
  mutation: { key, value, wire, now } // present only for growth
}
```

The enclosing document's `budgetVersion` is the pending version. A pending
operation with `mutation` is growth; one without it is release. The format does
not persist duplicate version or phase fields that could disagree during
recovery.

The pending operation and its immediate recovery alarm are persisted or removed
in the same local storage transaction. Every Room PUT and DELETE runs under
`blockConcurrencyWhile`, which makes the dense sequence and whole-document
mutation exact under v0.4's concurrent event model; the pending state makes a
cross-cell wait and finite reset safe.

The coordinator stores an aggregate allocated total plus one point-addressed
record per room:

```text
{ version, allocated, target }
```

A replay is granted exactly when `target == allocated`; storing a separate
boolean would create two representations of the same decision.

Its `decide(room, version, targetBytes, appCap)` transition is one local
transaction:

1. An older version is stale and changes nothing.
2. The current version is an idempotent replay only when `targetBytes` equals
   `target`; it derives and returns the persisted decision.
3. Anything other than the next version is a protocol error.
4. A target no larger than the current allocation is always granted, even if an
   operator lowered the cap below current usage.
5. Growth is granted only when replacing the room's allocation with the target
   keeps the aggregate at or below `appCap`.
6. Granted and refused decisions both persist the version and target. A refused
   replay can therefore never become granted later because capacity changed.

`appCap` is persisted with the Room's pending operation so alarm recovery replays
exactly the original decision. A replay of the current version and target returns
the persisted 200 or 507 outcome even if the caller supplies a different cap.
Older versions, skipped versions, and the same version with a different target
return 409 without changing state. Malformed versions or targets return 400.

A zero-byte allocation record is retained as a version fence against delayed
older calls.

Mutation order depends on the byte delta:

- **Growth: reserve, then commit.** The Room transaction advances the budget
  version, stores the complete pending mutation, and arms its alarm. It then
  awaits `decide`. Refusal clears the pending operation without changing KV or
  `seq`. A grant lets one Room transaction apply KV, bytes, timestamp, and
  `seq+1`, then clear the pending operation and alarm. The allocation is never
  below actual bytes.
- **Shrink: commit, then release.** One Room transaction applies KV, bytes,
  timestamp, and `seq+1`, advances the budget version, stores a pending release,
  and arms its alarm. The committed mutation may return success immediately;
  `decide` is attempted in the same turn and the alarm retries it after a
  transient failure. This can delay capacity reuse but cannot undercharge the
  Room or turn a committed user mutation into an error response.
- **Equal-size mutation.** The Room commits locally and increments `seq`; its
  exact allocation does not change, so no coordinator call is needed. Deleting
  an absent key follows this path and still consumes a sequence.

The alarm replays `pending` before any later mutation. It re-arms itself before
an outbound call. On a transient failure it leaves the durable operation in
place and schedules another attempt after a bounded one-second delay, preventing
a tight durable-alarm loop. A pending growth replays the persisted coordinator
decision and either applies or clears the exact saved mutation. A pending
release replays the release and clears. `(room, budgetVersion)` is the internal
idempotency key, so callers need no operation token and delayed calls cannot
overwrite newer accounting.

The runtime output gate is part of this proof. A cell-to-cell decision cannot be
observed before the Room's pending state is durability-proven, and its response
cannot be observed before the coordinator decision is durable. Output gating
does not replace the pending operation, version fence, local transaction, or
recovery alarm.

Room creation performs a serialized coordinator preflight. That decision is the
creation's byte-admission linearization point: a sibling growth serialized after
it may fill the final byte without retroactively invalidating the earlier empty
room. If the app's allocation is already at the cap when the preflight runs, it
returns 507 before the empty Room record is created. It records the soft creation-ledger row before the Room document, so an
unavailable ledger cannot leave a persisted room whose ID was never returned.
A failed Room commit may leave one conservative ledger row until its normal
window expires. The creation-rate decision remains a soft service-layer gate;
the byte ceiling remains hard.

#### Upgrade and conformance

Legacy Rooms adopt the versioned protocol lazily. A document without a budget
version reads as version zero, and its first growth charges the room's full
absolute target through the normal decide path, so the coordinator's aggregate
converges toward exactness as rooms are touched; until then the app ceiling is
enforced against the touched subset, which can only under-count, never block
retained data. Missing wire entries on pre-relay rooms serve as `null` in
snapshots, exactly as they did before the relay existed. There is no offline
Room reconciliation pass.

Every backend runs the same observable conformance cases: round-trip, reserved
object-property keys, cross-room and cross-app isolation, nonexistent-room
shape, room byte/key ceilings, exact serial and concurrent app ceilings,
immediate cross-room capacity release, creation refusal at a full app, creation
ledger behavior, and dense sequences. Deterministic Worker tests additionally
sabotage every durable boundary and replay accepted, refused, stale, reordered,
and same-version/different-target coordinator calls.

---

## Celld-backed metadata storage (production)

The production metadata adapter is a celld Worker reached over cluster-internal
HTTP. Each named cell owns a private SQLite database, and celld fences ownership
and durability through the fleet's object store. The public Go service remains
the translation layer: it owns SSH/HTTP policy and calls service ports whose
celld adapters translate to Worker requests.

### Cell taxonomy

Four active classes are permanent because class names participate in stored
identity:

- **Identity**, one per owner: quota entries, listing summaries, first-seen,
  durable intents, and the keygate reverse index.
- **Paste**, one per app slug: paste row, versions, claim, the app-scoped
  room creation and byte-budget coordinator, and the app's VAPID key pair. Room accounting works without a
  paste row, so a static site can own rooms under the same slug.
- **Room**, one per `(app slug, room UUID)`: room document, dense sequence,
  pending budget operation, push subscriptions, schedule and send counters, the
  shared alarm, and hibernatable sockets.
- **Subnet**, one per source network: Sybil-admission rows.

The deprecated `IntentLog` class remains only so the original migration stays
resolvable. No request routes to it.

Paste artifact state and app-room accounting are distinct logical aggregates
co-located in one physical cell because both are addressed by the app slug. This
reuses the existing permanent class and avoids a second app-slug coordinator;
room methods do not depend on a paste row.

### Concurrency and local atomicity

A v0.4 cell may serve several fetches concurrently. Local storage-shaped awaits
remain in the same input turn, while a real cell call or fetch opens an
interleaving point. Code must not rely on the phrase "one event at a time."

A same-cell invariant uses one local commit. Pure grouped puts use one
`storage.put(Map)`; transitions mixing puts and deletes use
`storage.transaction()`. The identity reserve/confirm/release and paste
put/append/delete-version transitions therefore expose either their complete old
state or complete new state after a stop, never a partial combination.

The Room's app-byte protocol is the only path that intentionally spans cells.
It persists a versioned pending operation before the outbound call and uses
`blockConcurrencyWhile` only around that bounded state machine. The protocol is
specified under "Room storage."

### Input and output gates

The input gate keeps storage-shaped awaits in one local turn, but it opens on
outbound cell calls. The output gate holds HTTP responses, cell-call responses,
queue delivery, and WebSocket sends until relevant local durability is proven.
It does not execute an unowned promise, combine several local commits into a
transaction, or create a distributed transaction. Every asynchronous operation
must be awaited or owned by `state.waitUntil`, and every multi-key invariant
must still use one local transaction or batch.

### Worker deployment generations

The fleet reads `deploy/current.json` periodically and can adopt it immediately
through the release-matched operator reload endpoint. Adoption does not restart
the node. New requests use the new Worker generation, existing requests finish
on the old generation, and resident Room cells move at safe points with their
storage and hibernatable sockets. Adjacent generations can call each other, so
an application release must keep its internal cell protocol compatible for the
old-generation residency window.

A failed generation build leaves the prior deployment serving. Hot adoption is
used only when adjacent Worker generations preserve the internal protocol during
the residency window.

### Binding fit

Hostthis keeps its domain cells rather than forcing v0.4's partial service
bindings into different semantics:

- Workers KV does not provide the compound exact quota and ordered mutation
  transitions owned by Identity, Paste, Room, and Subnet.
- Queues and Workflows are asynchronous and cannot decide a synchronous room or
  upload admission response.
- R2 does not replace the existing provider-neutral, content-addressed BlobStore
  contract and its S3 adapter.
- Service bindings do not remove hostthis's app/room protocol or public policy
  boundary.

The unified peer tunnel is a runtime transport improvement. It replaces no
hostthis adapter or domain rule.

### Blob and operational boundaries

Paste and site payloads stay outside celld in the content-addressed S3
BlobStore. Room values are small mutable metadata and remain in Room cells.
Blob deduplication, pending/finalize, and orphan collection are unchanged.

One fleet bucket serves one Worker application. The Worker and operator routes
are unauthenticated and remain cluster-internal behind NetworkPolicy;
`hostthisd` is the only public surface. Worker health and runtime health are
separate: `/healthz` checks the hostthis Worker, while
`/.well-known/celld/health` is the node lifecycle probe.

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

**`delete` purges even when it reports failure.** The other three purge
only on success, which is right for them: if an update did not happen,
the cached bytes are still correct. A delete is different, because it can
report an error *after* the paste is already gone - a transient storage
error partway through, for instance - and then the URL serves deleted
content from the edge until `max-age` expires, up to an hour. Since
purging a paste that still exists is harmless (the next reader simply
re-fetches it), a delete purges unless the error proves nothing was
touched.

"Nothing was touched" means the pre-mutation rejections only:
`ErrNotFound`, `ErrNotOwner` and `ErrEmptyOwner`, all raised by the
ownership pre-check before any write is attempted. Excluding them is not
an optimisation - it is what stops an unrelated caller from forcing
purges on slugs they do not own by attempting deletes, which would spend
the CDN's finite purge budget on request.

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

## Health endpoints

The HTTP listener serves two process-health endpoints ahead of Host-based routing:

- **`/healthz`** returns `200 ok` whenever the HTTP server responds and echoes
  `X-Backend-Color` when the replica is color-labeled. Kubernetes uses it for
  startup, readiness, and liveness.
- **`/readyz`** is a compatibility endpoint returning `{"ready":true}`. Current
  metadata adapters have no application-side activation phase: memory is local,
  and celld activates cells on demand.

Both answer on any Host without authentication. They expose no metadata, storage
counters, or operator controls.

## Metrics

The service publishes Prometheus metrics on a **separate listener**,
`HOSTTHIS_METRICS_ADDR` (default `:9091`).

Separate, not a path on the public mux, and that is a security boundary
rather than a style choice. `/healthz` and `/readyz` answer on any Host
without authentication; a `/metrics` route beside them would publish
request rates, verb mix and failure counts to anyone who asked. The
metrics port is never routed by the ingress.

The same listener serves Go's pprof endpoints under `/debug/pprof/`.
Wait-dominated latency is invisible to counters and to CPU profiles
alike; a goroutine dump taken during a slow command shows exactly which
call it is blocked in, which is the question a latency investigation
actually asks. pprof exposes stacks and heap contents, so it inherits
the same rule as `/metrics`: private listener only, never the public
mux.

### Why the SSH surface needs this

SSH is the primary interface, and it is the one an operator cannot see
from outside. A reverse proxy handles SSH as plain TCP, so it can report
how many connections are open and nothing more: no command counts, no
durations, no failures. Every question about what users actually do -
which verbs run, how long an upload takes, how often the gate refuses a
key - can only be answered from inside the process.

### What is exported

| metric | type | labels |
| --- | --- | --- |
| `hostthis_ssh_commands_total` | counter | `verb`, `outcome` |
| `hostthis_ssh_command_duration_seconds` | histogram | `verb` |

Plus the standard Go runtime and process collectors.

`outcome` is one of `ok`, `refused` (the Sybil gate), `incomplete` (the
session ended without reporting an exit code, i.e. the client hung up),
or `error_<code>` for a structured exit code.

`incomplete` is deliberately distinct from `ok`. Both are "not an error",
and collapsing them would hide clients dropping mid-command.

### Label cardinality is a correctness property

`verb` is derived from the first SSH argument, which is entirely
attacker-controlled. It is mapped through the verb registry: recognised
names pass through, everything else becomes `unknown`. Without that
mapping, anyone could grow the series count without limit by sending
random verbs - a memory-exhaustion path on the metrics server, reachable
by an unauthenticated stranger.

An empty command maps to `upload`, since piping content with no verb is
the service's primary path and deserves its own series.

### Where it is measured

Instrumentation lives in a middleware wrapping the whole SSH chain, not
inside any verb. Two consequences: no verb carries observability code,
and sessions the gate refuses are counted too - they never reach the
dispatcher, and a rising refusal rate is exactly the kind of thing worth
seeing.

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

# Metadata backend
                         / HOSTTHIS_METADATA_BACKEND        memory | celld                         (memory)
                         / HOSTTHIS_CELLD_ENDPOINT          celld Worker base URL                  (required for celld)

# Blob backend
                         / HOSTTHIS_BLOB_BACKEND            disk | s3                              (disk)
                         / HOSTTHIS_S3_ENDPOINT             S3-compatible endpoint                (provider default)
                         / HOSTTHIS_S3_BUCKET               payload bucket                         (required for s3)
                         / HOSTTHIS_S3_REGION               bucket region                         (us-east-1)
                         / HOSTTHIS_S3_ACCESS_KEY           S3 access key                         (required for s3)
                         / HOSTTHIS_S3_SECRET_KEY           S3 secret key                         (required for s3)
                         / HOSTTHIS_S3_USE_SSL              endpoint uses TLS                     (false)
                         / HOSTTHIS_S3_BLOB_PREFIX          object-key prefix                     (blob)
                         / HOSTTHIS_BLOB_WRITEBACK          enable local write-back cache         (false)
                         / HOSTTHIS_BLOB_WRITEBACK_DIR      write-back cache directory            (<data-dir>/blob-cache)
                         / HOSTTHIS_BLOB_WRITEBACK_MAX_BYTES soft cache ceiling                   (1 GiB)

# CDN / cache purger
                         / HOSTTHIS_CACHE_BACKEND           noop | cloudflare                       (noop)
                         / HOSTTHIS_CF_PURGE_TOKEN          CF token (Cache:Purge scope only)       (required if cloudflare)
                         / HOSTTHIS_CF_ZONE_ID              CF zone id for the apex                 (required if cloudflare)
                         / HOSTTHIS_PUBLIC_URL_BASE         base URL used to construct purge URLs   (https://<apex>)
```

The runtime container reads the same env vars. The operator supplies
a docker-compose (or equivalent) file out of band; this repo ships no
sample production compose.

### Process shutdown

`SIGINT` and `SIGTERM` start one bounded shutdown sequence. Room relay admission
stops synchronously; an upgrade racing a new room socket receives HTTP 503.
Public HTTP, private metrics, and existing room relays then drain concurrently.
Active room sockets receive WebSocket 1012 with `service restart`; sockets that
do not close are force-closed before the process exits.

SSH stops accepting new connections at the same time and receives at most 20
seconds for existing sessions. The server then force-closes every remaining SSH
connection. Session drain covers command handlers, not just sockets: a handler
whose connection was force-closed or lost may still be inside metadata work, so
both graceful shutdown and force close return only after every admitted handler
has finished. A handshake accepted before shutdown that completes afterward is
refused before its command dispatches. Upload finalizers are waited only after
SSH can no longer start one, and blob write-back cleanup starts only after those
finalizers finish. The whole sequence has a 35-second hard bound. Work still
blocked at that point is left to the documented pending-state and startup
recovery protocols. The 20-second and 35-second limits are product constants,
not operator knobs.

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
- Metadata backend (`HOSTTHIS_METADATA_BACKEND=memory|celld`) and celld endpoint
- Blob backend (`HOSTTHIS_BLOB_BACKEND=disk|s3`), S3 connection settings, and
  optional local write-back cache
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
expansion, not an accident). The throughline: the metadata plane
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
static pages. The engine already exists: the room KV.

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
  prefix); its frontend hits `<app>.hostthis.dev/api/kv/<key>`
  (GET/PUT/DELETE), persisted in the metadata plane. A thin HTTP layer over the K-V. No
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
  (a cell's event already does atomic counts); a high-score /
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
