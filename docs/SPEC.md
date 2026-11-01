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
DOMPurify sanitizes the rendered output in the browser.

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
browser, with [Pele](https://github.com/obsidianmd/pele), which reads
Mermaid syntax and draws in its own layout and visual style. Pele escapes
diagram text, drops script links and click callbacks, and filters styles;
a source it cannot parse shows its parse error instead of a diagram, with
no second renderer to fall back to. Mermaid also renders **inside markdown
pastes**: a fenced ` ```mermaid ` block in any markdown paste becomes a
diagram, and a block that fails to parse shows its source. The renderer is
~0.7 MB, so the markdown shell loads it **only when the fetched source
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
| `#<heading-slug>` | markdown, knowledge base | scroll to that heading |
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
  text payload ever hits this; tight enough to bound the read. It
  binds single-file pastes only; a site archive has its own input cap
  (see "Safe-untar").
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
  allocation) through two sinks: a zstd writer into a temporary spill
  file and a raw-byte counter (the 100 MiB fast-fail). After EOF, the
  spill file's size is compared against the 10 MiB cap. If it fits, the
  file streams to the configured `BlobStore` under the upload's own
  prefix (see "Blob storage backends → Object keys"). If not, the upload
  is rejected with `upload exceeds 10 MiB compressed cap; your bytes
  compress to <actual> - try removing binary data` and the spill file is
  discarded.
- **Storage backend**: the Worker's payload binding, under the
  upload's own `uploads/<upload-id>/<n>` keys. See "Blob storage backends"
  below.
  Markdown and diff are never rendered on the read path: such a read
  either streams the raw bytes (when the client asks for them via `?raw`)
  or serves the fixed client-render shell, so server memory is constant
  regardless of paste size.
- **Storage compression**: all blob bytes are persisted zstd-encoded
  (level 3, 1 MiB window) by the storage layer, identically on every blob
  backend. Encoders are pooled and single-threaded, so an in-flight upload
  costs about one window of memory however many arrive at once.
  Compression is invisible above the BlobStore interface. See "Blob
  storage backends → On-disk format" for the header and how a read treats
  an object without it.

## Paste lifecycle status (async blob write)

A paste has a **status**: `pending`, `ready`, or `failed`, and moves only from
`pending` to one of the other two; a request for any other status is refused
`invalid-status` (400) with the row unchanged. The status
exists to hide the slow part of `Create` (the blob write to object
storage, ~250 ms of a ~400 ms paste) behind a fast metadata-only
acknowledgement, so the uploader gets a URL back as soon as the paste is
durably reserved + named, not after the bytes finish landing in the
object store.

The bytes and the metadata are written in two steps, and the status models
the window between them.

### The three states

- **`pending`**: the metadata is committed (slug claimed, quota checked,
  paste row written) but the content has not finished landing in the
  object store. A GET on a pending paste serves a lightweight
  **loading page** that auto-refreshes (a `<meta http-equiv="refresh">`
  ~1 s poll) until the paste resolves. A pending paste counts against
  quota exactly like a ready one.
- **`ready`**: the blob write succeeded and the background finalizer
  flipped the paste `pending -> ready`. A GET serves the content. This is
  the terminal success state.
- **`failed`**: the background blob write failed. A GET serves a small
  **error page**. The transition releases the paste's quota charge.

A row with no persisted status is read as **`ready`**.

### Create: the synchronous half

`Create` does, synchronously, before returning the URL:

1. mint the upload id, then stream + compress into a spill file on local
   disk,
2. detect the content kind,
3. **check quota + write the authoritative paste row with
   `status=pending`**, whose manifest already names the object key
   `uploads/<upload-id>/0`,
4. return the URL.

Quota is enforced here, inside the serialized metadata commit, so an
over-quota upload is rejected before any URL is handed out.

### Finalize: the asynchronous half

After `Create` returns the URL, a background goroutine (owned by the
upload service) runs the finalizer:

1. stream the spill file to `uploads/<upload-id>/0`,
2. on success: flip the paste `pending -> ready`; when the paste is gone
   (deleted meanwhile, or its slug re-minted under another generation), delete
   `uploads/<upload-id>/` instead,
3. on failure: flip the paste `pending -> failed`, then delete
   `uploads/<upload-id>/`.

The transitions are guarded: the finalizer only advances a paste that is
still `pending`, so a late finalize against a row already moved off
`pending` is a no-op rather than a resurrection. Graceful shutdown waits
for in-flight finalizers.

### Durability trade (read this)

Between the metadata commit and the blob write, the compressed bytes exist
only in the handling pod's spill file. **If the process dies in that
window, those bytes are lost**: the metadata says `pending` and no bytes
will arrive.

- The window is the blob-write latency, not the whole request.
- A paste stuck `pending` STAYS pending: nothing ages it out (see
  "Phantom entries are accepted, not repaired"). It keeps its charged
  bytes and shows a loading screen until its owner deletes it, which also
  deletes whatever part of its prefix landed.
- A `ready` paste is fully durable: bytes in the object store, metadata
  committed. Only bytes the uploader has not yet been told are permanent
  are at risk.

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
it, stores each file under the deploy's own upload prefix plus a manifest, and
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


### Detection: gzip-tar as a format

The format gate (`DetectKind`, see "File handling → Format gate") gains
one branch. After the existing HTML / Markdown sniffing, the detector
checks the captured upload prefix for the **gzip magic** (`0x1f 0x8b`),
and if present, peeks one tar header out of the decompressed stream to
confirm a tar inside (a gzip-tar = `.tar.gz` / `.tgz`). The detection
is by content, never by filename - the SSH pipe carries no filename, so
this matches how every other format is recognized.

The shape is decided after the safe-untar (below), on the extracted
manifest:

- a root `index.html` -> **site**,
- otherwise -> **knowledge base**.

That is the whole rule. An archive surviving the safe-untar is served
whatever it holds, and the untar guards (path safety, the
decompression-bomb bound, the file-count and manifest-size caps) are the
only bound on what gets in.

The chosen shape is stored on the row and never re-derived at serve
time (see "Serving a directory"). Each deploy decides from its own
manifest, so a redeploy that adds a root `index.html` lands as a site.

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
- **Archive input cap.** The compressed archive read is capped at the
  1 GiB max site size plus 64 MiB of framing headroom. The uncompressed
  bound alone does not bound the read: gzip members and deflate blocks
  that inflate to nothing can stream forever. Crossing it fails with
  *site archive too large to consider*, never as a corrupt stream. A
  site is NOT held to the 100 MiB raw-byte fast-fail for single-file
  pastes; that cap applies only once the upload sniffs as a paste.

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
- `Manifest` - the value object mapping each safe relative path to its file
  (object key, size, compressed size, content-type-by-extension).
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

**Stored shape.** The version row carries the encoded manifest, its upload id,
and a flat root descriptor (kind, size). A row without a manifest, or whose
manifest fails to decode, keeps its metadata but has no content to serve (see
"Blob storage backends → Entries without an object key").

Every version is written with a manifest, including a single document, whose
manifest is of length one at `/`. That is what lets a reader stop asking which
shape it holds.

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
but an object shared by several live versions is charged once (see "Redeploy
reuse"). An owner reclaims bytes only by deleting versions they no longer
retain.

**One artifact appears in one listing.** A directory is an artifact, so it is
enumerated by the artifact index. Anything that also reports it as a site would
show it twice and charge it twice - the same trap in two places.

### Storage

Each upload's files live under that upload's own prefix in the `BlobStore`
(see "Blob storage backends → Object keys"). A manifest entry carries its
object key, so resolving a file needs the manifest alone. A redeploy stages
every changed file under a new prefix, and a file unchanged from the version it
was computed against points at that version's object instead (see "Redeploy
reuse"). Shared objects never cross slugs.

A read resolves the root through the manifest: its `/` entry, else
`index.html`. The flat descriptor keeps kind and size and never names bytes, so
a stale flat field can never serve bytes the manifest does not.

A directory is written through the SAME insert a document uses, and every
caller supplies a manifest, carried into the stored descriptor verbatim. A
document supplies a one-entry manifest at `/` holding its object key. There is
no site-specific write path.

### Serving a directory

One head read answers every request. The head carries the served version's
whole descriptor including its manifest, so resolving a request path is a
manifest lookup on a value already in hand - not a head read followed by a
version read, and not a site lookup followed by a paste lookup.

**The shape is DECLARED, never inferred.** An artifact whose kind is `site` or
`knowledgebase` is a directory; anything else is a document. Counting manifest
entries would get a one-file directory wrong, serving it rendered instead of
handing back its bytes.

A document answers only at its own URL. A deeper path under it is a 404: paths
inside an artifact are a directory's affair.

Lookup resolves either shape. A document keys its single entry at `/`, having
no filename to be known by; a directory keys files by path and answers `/` with
its index. The root lookup checks `/` first and then `index.html`, so one
function serves both.

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
rendered - a site serves its files; rendering markdown inside a
directory is the knowledge base shape, and a single-file markdown paste
renders its own bytes).

Site reads carry the **same sandbox headers** as HTML paste reads
(`X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
`Permissions-Policy: ...`) and the same cache posture. Files are served
**raw**: the site's own HTML/CSS/JS runs exactly as uploaded, secured
by per-subdomain origin isolation, not by sanitizing the bytes.

### Knowledge bases (a directory with no root index)

An archive with no root `index.html` is a **knowledge base**: the same
directory artifact, browsable and searchable as a set of documents
rather than served as a site. Pipe a folder of notes the way you pipe a
built site:

```
$ tar czf - docs/ | ssh hostthis.dev
https://abc12345.hostthis.dev
```

Its kind is `knowledgebase`, decided at deploy time from the extracted
manifest (see "Detection: gzip-tar as a format") and stored on the row,
so serving never re-derives it.

**What the shell serves.** A knowledge base is served by a fixed,
content-independent HTML page, exactly like the markdown, diff and log
shells: its ETag is the shell version and never varies with content. The
shell answers

- the root,
- any path resolving to a `.md` file in the manifest, and
- any path with no manifest entry that looks like a route rather than a
  missing asset, by the same last-segment rule the SPA fallback uses
  above. A missing `.js` or `.png` still 404s.

`?raw=1` on a `.md` path serves that file's raw bytes, the contract every
shell uses to fetch what it renders.

**What renders, and what is linked.** Markdown renders inside the shell.
An HTML file does not: it appears in the navigation and links out to its
own URL, where it serves raw exactly as a site's file does, because the
shell sanitizes what it renders while a site's HTML is meant to run as
itself on its own origin, and linking preserves both. Every other file
type (images, PDFs, CSS, data) likewise appears in the navigation and
links to its raw URL, served with the content-type its extension
implies.

**The root document.** The root renders `README.md` when the manifest
holds one, else the `.md` whose path sorts first, else a generated
listing of the base's files, which is what a base holding no markdown at
all shows. The listing is the shell's own: no response templates content
into a page.

**Rendering is client-side.** The shell loads the same vendored `marked`
plus `DOMPurify` the markdown paste shell loads and renders in the
browser; the server never renders markdown on the read path. Heading
anchors and the `#<heading-slug>` deep-link contract behave as they do
there, including the load-order rule (see "Deep links").

**Navigation.** The shell draws a left sidebar that browses the file list
ONE level at a time - the folder the reader is in, its child folders and
its files, with a control for the parent folder - breadcrumbs for the
current path, and a right sidebar table of contents built from the
current document's headings. A whole tree is not drawn: depth costs
indentation and rows without bounding either, while search reaches any
file at any depth. Entering a child folder moves the sidebar alone; the
document changes only when a file is opened, and opening one from
anywhere puts the sidebar in that document's folder. A search bar matches
file paths, headings and body text, entirely in the browser. Both
sidebars collapse to drawers on a narrow screen, so a base reads on a
phone.

The breadcrumb trail occupies ONE line. Where the path does not fit, its
middle collapses into a control that expands the whole path on demand and
collapses it again, and the first and last segments always survive.
Fitting is measured rather than counted, since four long names overflow a
bar that ten short ones sit in. Every visible segment moves the sidebar to
that folder.

Opening a document replaces the rendered content in place rather than
reloading the page, and the URL follows it, so a link to any document
loads the same thing directly. A link inside a document to another `.md`
file in the base navigates the shell the same way, without a reload. A
link that LEAVES the base says so before it is followed: it carries the
same external mark the file navigation uses and opens in a new tab, so
the base being read is never replaced. That covers a link to another
origin, a link elsewhere on this one, and a link to a non-markdown file
in this very base, which the shell cannot render and which serves raw at
its own URL. A non-web target (`mailto:`, `tel:`) carries the mark but is
followed in place, since a new tab for a mail client leaves an empty one
behind. A link whose target the sanitizer removed renders as plain text,
because a link that cannot go anywhere should not look like one. A path
naming a directory renders a listing of what is under it, the same
listing the root falls back to.

Bodies are indexed lazily in the background, bounded at **500 markdown
files or 8 MiB of markdown**, whichever comes first, and indexed in
path-sort order so the bound falls in a deterministic place. Beyond
either limit, or where a document could not be read, the interface
reports the index as partial and names which applies: a search box that
silently returns fewer results than the base holds is worse than
one that states its limit. Path matching still covers every file in the
base, because paths come from the file list rather than the bodies;
heading and body matching cover the indexed files.

**Security is unchanged.** Same sandbox headers, same origin isolation,
and the shell runs under the same `Content-Security-Policy` as the other
shells (`script-src 'self'`, `connect-src 'self'`, no inline script).
Non-markdown files are served raw, as they are for a site.

#### The file list (`?files=1`)

`?files=1` on a knowledge base URL returns the base's file paths as JSON.
That is how the shell builds its tree and its search index without the
server templating anything into the page. The list covers the whole base
whichever of its paths carries the query.

It is a **query on the existing slug route**, not a new path namespace,
so no file in the base can shadow it: a base holding `files.json` or a
`_files/` directory still serves those at their own paths. Like every
response from the base it is `public, no-cache` (see "Cache-Control
posture"), and a purge of the base covers it.

### Same security model as HTML pastes (not a new posture)

A static site introduces **no new security posture**. An HTML paste is
already served RAW (its JS runs) and is secured by **origin isolation**:
each paste gets its own subdomain, its own browser origin, and the same
response headers (see "HTML sandboxing"). Only the Markdown path is
sanitized, and in the browser: its shell renders the raw bytes through
DOMPurify. Raw HTML is never sanitized.

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
  compressed size. Each entry keeps its uncompressed size, as `Size` for
  display and as `DecodedSize` for the read path (see "On-disk format");
  `CompressedSize` is the quota basis. The untar guard
  separately bounds the uncompressed running total against the owner's remaining
  allowance. The persisted charge is computed from the final normalized
  manifest, so duplicate archive entries that overwrite one path do not charge
  discarded staging attempts.
- **Versioning** reuses the paste-versioning shape where it is low-cost:
  a deploy to an OWNED site slug re-deploys the site in place (same slug,
  same URL, new immutable manifest), so rollback / history ride the
  existing machinery; otherwise a deploy lands as a fresh slug, matching
  whatever pastes do. Either way a deploy is ATOMIC - the new manifest
  only becomes the served one once every file is written and the
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
- **Atomic append.** Every file is staged under a new upload prefix first. The
  metadata operation then admits and publishes one immutable manifest version;
  a refused redeploy deletes that prefix. The old served version remains
  visible until publication finishes, and a half-finished deploy never serves
  a partial site.
- **Quota charges what the appended version adds.** A redeploy retains every
  prior live version, so it receives no replacement credit. Admission evaluates
  the owner's existing all-live charge plus the new version's charge: its
  stored bytes that the base does not already hold (see "Redeploy reuse").
  Deleting a version later releases exactly the bytes it frees.
- **Purges the slug.** Like a paste update, a re-deploy purges the slug's
  cached responses (see "Active invalidation"), so the next load of any file
  shows the new version.

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
real `DeploySite` use case over a real repo + blob store, then fetches every built file back over the real HTTP serving
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
system and without any server-side app code: a per-app KV store fronted
by a thin HTTP layer over the metadata backend, in its no-auth,
capability-based form (the auth tier is under "Future directions").

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

Two servers answer these routes with the same rules: `hostthisd`, and the
Worker's public router once the Worker has `HOSTTHIS_APEX_DOMAIN` set
(see "Public and internal hosts"). Both take the app from the host (or the
path-mode prefix) and the room from the parsed UUID, never from a body or
query. Unless a server is named, every rule in "Rooms", "Real-time room
relay" and "Room push" holds for both.

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
  to the room-creation rate limit (see "Quota and abuse"). An id whose Room
  cell already holds a room is never handed out, since its holder would share
  that room: creation mints another, up to five ids, and then fails 500.
- **GET /api/rooms/<uuid>** scans and returns every key+value pair in the
  room as a single JSON object, so an app loads the full room state in one
  request on join. Each value is embedded as raw JSON when the stored bytes
  parse as JSON - so a value the app PUT as a JSON object comes back as a
  NESTED object, not a JSON-string of escaped text - and as a JSON string of
  the verbatim bytes otherwise. Keys are listed in the order of their UTF-8
  bytes. (The WebSocket `snapshot` and `put` frames carry each value byte for
  byte as this GET does; the single-key GET below instead returns the raw
  bytes.) A well-formed UUID that names no room returns **404**, on both
  servers also when the room goes away while the request is reading it.
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

The remaining rules, shared by every route above:

- A key is the rest of the decoded path after the UUID: 1 to 256 bytes of
  UTF-8, and never `ws`, `push` or anything under `push/`, which are the relay
  and push routes. A key over 256 bytes is a **400**, and so is one whose
  decoded bytes are not UTF-8 (`%FF`, a truncated or overlong sequence, an
  encoded surrogate), on GET, PUT and DELETE alike, before any storage call:
  such bytes have no single text form, so a key written under one could not
  be read back under the same one.
- A route answers only its listed methods; any other method is a **405** with
  an `Allow` header naming them.
- A PUT body is read to at most 256 KiB plus one byte, so an oversized body
  reaches the per-room cap and is refused **413** there. Push bodies are read
  to at most 64 KiB.
- A refused creation is **429** with `Retry-After: 3600`; the per-room cap is
  **413**; the per-app aggregate is **507**.
- "Parses as JSON" means Go's `json.Valid`: nesting at most 10000 deep, a
  byte-order mark is not whitespace, and bytes that are not UTF-8 are allowed
  inside a string. Every surface that embeds a value, the whole-room GET and
  the WebSocket frames, emits only UTF-8: each byte of an invalid UTF-8
  sequence becomes one U+FFFD, as Go decodes it, whether the value is embedded
  as JSON or becomes a JSON string, so no client decoder (whose replacement
  rules differ) ever sees the raw bytes. Only the single-key GET returns the
  stored bytes as they are.
- The text a value is encoded to is computed when it is written and stored
  with it. The whole-room GET on both servers, the `snapshot` frame and the
  `put` frame all carry that stored text byte for byte, in every mode, off
  mode included: no reader parses and re-serializes it, so `1e400`, `-0`,
  `12345678901234567890`, `1.50`, whitespace and the order of an object's
  names reach every client exactly as the value holds them. Text stored by
  either server keeps its own spelling (`hostthisd` writes `<` in a string as
  `<`). A value stored without text, or with text that is not JSON, is
  encoded from its bytes by the rule above wherever it is read. A key's own
  spelling may still differ between the servers' GETs (`hostthisd` escapes
  `<`, `>`, `&`, U+2028 and U+2029 in it); the `snapshot` frame's `state` is
  the Worker's whole-room GET object, byte for byte.
- Every response under `/api/rooms` carries `Cache-Control: no-store`, errors
  included, since values change under fixed URLs and a CDN caches an
  undirected 404 by its own default. So does every `/api/push/key`
  response, its 404 and 405 included.

### Storage: a room-namespaced KV over the metadata backend

Room data is small JSON/bytes blobs - app STATE, not files - so it lives
in the **metadata backend** hostthis already runs (the Worker's cells), NOT
in the BlobStore. The BlobStore holds the larger file bytes of pastes and
sites; room values are small,
mutable, and per-room, so they belong with the metadata. Large blobs are
explicitly out of scope for rooms - an app that needs to host files uses
the archive/site feature, not a room value.

The implementation follows the existing repo-behind-service pattern:

- Pure `Room` and `RoomKV` domain types live in `internal/domain` and import no
  infrastructure. They own identifiers, values, byte/key counts, and cap checks.
- The `RoomRepo` port in `internal/service` exposes creation, point reads,
  snapshots, PUT, DELETE, and creation-ledger counts. The service applies use-case
  policy; HTTP only translates requests and responses.
- The Worker adapter implements the port. It stores one whole Room document per
  Room cell and coordinates app-wide allocations through the app's Paste cell.
- Every address includes `(app_slug, room_id)`, so cross-app isolation is
  structural rather than a filter. The conformance suite pins the observable
  contract.

The slug-must-name-a-live-app **existence requirement** is enforced at the
HTTP layer (`hostthisd` reads the site + paste readers its router already
holds; the Worker asks the app's Paste cell, whose read-only `exists` op
reports whether a row is present), NOT inside the room repo, so the room
repo needs no separate existence reader: room creation 404s a slug that
names no live site or paste.

The Worker's public router reaches the same cell ops through its bindings:
`exists`, `roomcounts` and `pushkey` on the app's Paste cell, and on the
Room cell (`<slug>|<uuid>`) `create`, `meta`, `scan`, `get`, `put`, `del`,
the push ops and the join; `scan` answers each value's stored text beside its
bytes. It computes the value's wire encoding itself and sends it with each
PUT, as `hostthisd` does, and the Room cell stores it with the value.

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
  proxy - not the left-most value, which is fully attacker-controlled. The
  list is every `X-Forwarded-For` header line, in order, read as one, since a
  client may send lines of its own ahead of the proxy's. An empty right-most
  value names no address, and the TCP peer counts instead. An
  operator who terminates TLS at a proxy MUST set this flag (otherwise
  every request's `RemoteAddr` is the proxy's own IP and the per-IP gate
  collapses to a single global bucket); an operator with hostthis directly
  on the public internet MUST leave it unset.

  `HOSTTHIS_HTTP_CLIENT_IP_SOURCE` names the source explicitly and wins over
  the flag: `peer` (the default), `xff-rightmost` (what the flag means), or
  `cloudflare`. `cloudflare` is `xff-rightmost` for a deployment whose proxy
  sits behind Cloudflare for some hosts and faces clients directly for
  others: when the right-most hop is inside Cloudflare's published address
  ranges, the address is the `CF-Connecting-IP` header (Cloudflare's edge
  sets it and overwrites a client's own); any other hop is the client
  itself, and its `CF-Connecting-IP` is ignored, since a client reaching the
  proxy directly can send one. An unknown value stops startup.

  The Worker has no TCP peer address to read, so `HOSTTHIS_CLIENT_IP_SOURCE`
  on the Worker names where the address comes from: `cf-connecting-ip` reads
  the `CF-Connecting-IP` header Cloudflare's edge sets (a client cannot forge
  it through the edge); `xff-rightmost` reads the right-most
  `X-Forwarded-For` value across every line, as `hostthisd` does, for a
  Worker behind a trusted proxy that appends it. Unset, or any other value,
  or no address found, puts every client in one bucket. A
  forwarded request (see "Public and internal hosts") takes the address from
  `X-Hostthis-Client-IP`, which only a token holder can send. An address
  that does not parse is the `unknown` bucket. The subnet strings are the
  ones `hostthisd` derives (`a.b.c.0/24`, the canonical IPv6 text of the
  `/48` prefix plus `/48`, and an IPv4-mapped IPv6 address counts as IPv4),
  so both servers count against the same per-app ledger.
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
- **Room bytes count against the app owner's quota.** An app's room bytes
  (the per-app total above) are charged to the identity that owns the app's
  paste or site, inside the same 1 GiB `UserQuotaBytes` as its pastes and
  sites. A room mutation that grows the app's total is granted only when the
  owner's pastes, sites and every app's room bytes stay at or below the
  quota; otherwise it is refused **507** with its prior room state intact,
  the same answer as a full app. An upload that would push the owner past
  the quota is refused as today, room bytes included. Shrinking and deleting
  values are always granted. The per-app 64 MiB ceiling stays as a backstop
  under the owner quota. A deleted app's rooms stay charged to the app's last
  owner; a slug that never had an owner (rooms are only created under a live
  app, so only an operator reaches one) is bounded by the app ceiling alone.
  A deployment that sends no app ceiling (`appCap` 0)
  enforces neither. `whoami` reports room bytes inside `used_bytes` and on
  their own as `room_bytes`.
  Bytes that rooms held before this rule are charged to the owner as they
  stand, even past the quota (grandfathered): nothing is deleted or made
  read-only, but such an owner can neither grow a room nor upload until it
  is back under the quota.
- **No service-wide cap.** Rooms hold no blobs (a room value lives entirely
  in the metadata backend, not the `BlobStore`), so room data is bounded only
  by the per-room and per-app caps above. hostthis carries no service-wide
  byte cap for rooms or for the blob-holding kinds (pastes + sites). The
  per-app aggregate is therefore the structural bound on a room's growth, and
  a reverse-proxy per-IP rate limit remains the appropriate layer for raw
  request-rate abuse, exactly as for the paste path.

### Scope fence

This tier is **KV persistence plus a real-time relay**. The KV verbs
above are the durable surface; the WebSocket relay below adds live push
on top of the SAME room. Two things are still explicitly NOT in it, each
a later tier with its own design:

- **NO per-user auth / JWT / "Sign in with hostthis."** The room UUID IS
  the access capability; there are no accounts, no roles, no token
  verification in this tier. "End-user auth on rooms" under "Future
  directions" describes a richer end-user identity spectrum (capability token,
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
404. A request that is not a valid WebSocket upgrade gets the answer
`hostthisd`'s WebSocket library gives, from the first of these checks it
fails: a 426 for a `Connection` header without the `Upgrade` token, a 426 for
an `Upgrade` header without the `websocket` token, a 405 for a method other
than GET, a 400 for a `Sec-WebSocket-Version` other than 13, and a 400 for a
`Sec-WebSocket-Key` that is missing, repeated, or not 16 bytes of padded
base64 (the Worker, which sees repeated header lines joined by commas, reads
a key holding a comma as repeated). Two cases are the runtime's on the
Worker. workerd hands the Worker any request carrying `Upgrade: websocket` as
a GET, whatever its method, and fails a non-GET one with its own 500 once the
Worker accepts it, so a handshake sent with another method is a 500 there
rather than a 405. And workerd upgrades only a request whose `Upgrade` is
`websocket` alone, so the Worker refuses one that lists other protocols
beside it (`h2c, websocket`) with a 426, after the method check, where
`hostthisd` accepts it. The origin is checked
before the upgrade: a missing `Origin`, one whose host equals the request's
host, and one whose host (port included, compared lowercase) matches
`<apex>` or the glob `*.<apex>` are allowed; any other origin, or one that
does not parse, is a 403. The room UUID remains the entire participant
capability.

### Cell-owned fan-out

Two front ends reach the same Room cell. `hostthisd` terminates the public
socket and opens one cluster-internal socket to the Room cell; its Go proxy
owns per-process connection admission and a client-facing heartbeat, both
`hostthisd` only. It pipes text and binary frames in both directions without
interpreting them. The upstream socket is not pinged, so a quiet hibernatable
Room is not woken merely to prove liveness.

The Worker's public router makes the checks above and then sends the Room cell
a new upgrade request, `/room/join?room=<slug>|<uuid>` with only
`Upgrade: websocket`, built from the host's slug and the parsed UUID. The
cell's answer is the client's answer: a 101 hands the client's socket to the
cell itself, with no proxy between them. The client's URL, query, headers and
body never reach the cell, so no request can name a cell op.

The Room cell is the only broadcast point. Exactly one cell owns one room, so
there is no pod mesh and no cross-stream ordering problem. Sockets from both
front ends share it, so clients of the two interoperate in one room.

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
  close the sending socket (1008) rather than reaching peers. Durable changes
  continue to use the HTTP KV path so the byte caps and sequence exist in one
  place.

The Room cell's own liveness and admission rules (the ping frame, its frame
limit, its socket cap and stale eviction, below) hold only while the Worker
serves the public surface, that is, has `HOSTTHIS_APEX_DOMAIN` set (see
"Public and internal hosts"). Without an apex the Worker is in off mode and the
cell relays every client frame to its peers exactly as `hostthisd`'s proxy
expects: a ping is an ordinary ephemeral frame, no frame size closes a socket,
and every join is accepted. The proxy's own bounds are then the only ones.

One client frame is reserved for liveness: the text frame `{"type":"ping"}`,
exactly those 15 bytes. The runtime answers it on the same socket with the text
frame `{"type":"pong"}` without waking the cell, and it never reaches a peer.
Any other spelling of a ping is an ordinary ephemeral frame.

A client-sent ephemeral frame may carry at most 32 KiB, bounding one sender's
fan-out amplification explicitly rather than inheriting the WebSocket library's
default. The Room cell enforces it for every socket: a larger frame closes the
sender with 1009 and reaches no peer. `hostthisd`'s proxy applies the same
limit at its client read boundary in every mode. Server-generated snapshot and
mirror frames may carry up to 2 MiB so a full 256 KiB room still fits after
worst-case JSON escaping; the cell writes them, so only `hostthisd`'s proxy
limits them, at its upstream read boundary. A dead or closing peer is skipped
without blocking the remaining fan-out.

### Snapshot and sequence contract

The first server frame is:

```json
{ "type": "snapshot", "seq": 17, "state": { "key": "value" } }
```

In every mode, `state` is the room's whole-room GET object (keys in the order
of their UTF-8 bytes) and a `put` frame's `value` is the value's stored text,
each spliced in byte for byte rather than parsed and re-serialized (see "The
HTTP API"). The frames are written as `{"type":"snapshot","seq":<n>,"state":<object>}`
and `{"type":"put","seq":<n>,"key":<key>,"value":<text>}`, the key a JSON
string.

The Room attaches the hibernatable socket and reads the local Room document in
one input turn, so no local mutation can splice between attachment and snapshot.
Every later durable mirror has a sequence strictly greater than the snapshot it
follows. The sequence is dense: exactly +1 for every committed PUT or DELETE,
including deletion of an absent key.

A client applies mirrors above its snapshot sequence, discards a duplicate at
or below it, and reconnects for a new full snapshot when it detects a gap. No
frame history or per-client durable cursor exists. A reconnect is a fresh join,
which makes browser reload and foreground recovery the same path.

When a peer closes, the Room answers the close frame itself. A runtime that
does not reply on its own leaves the socket closing until the handler closes it
too; one that does has already closed it.

### Lifecycle and bounds

With the public surface on, a room holds at most 64 sockets, counted by its
Room cell across both front ends. The cell decides at each join, in the same
turn that would accept the socket: it counts the open sockets it holds, and at
the cap first evicts stale ones (below) and counts again; a room still full
answers the join with 429 instead of a 101. On the Worker path that 429 is the
client's own handshake response. `hostthisd` accepts its client before it
dials the cell, so there a refused join closes the client with 1011. The
Worker path has no per-app cap: the creation limits bound how many rooms an
app has. `hostthisd`'s proxy also admits at most 64 connections per room and
1024 per app per process, answering 429 before its own handshake; these bound
that process, not the room, and go away with the proxy. In off mode they are
the only caps. Each of its connections is released exactly once when dialing
fails or either side closes.

Neither runtime pings or times out a hibernatable socket, so a client that
vanishes without closing stays counted until a write to it fails. Liveness is
therefore the client's job: the canonical browser client sends the ping frame
every 20 seconds. When a join finds the room at its cap, the cell closes every
socket whose last ping answer is more than 120 seconds old, with the
application code 4001 and reason `stale`, so a vanished client stops counting
against the cap. A join under the cap evicts nothing, so a client whose pings
are only late (a throttled background tab) keeps its socket while the room has
room. A client that sees 4001 knows the server dropped it as silent, not that
the room failed, and reconnects for a fresh snapshot. A socket that has never
pinged is not evicted this way; it leaves when it closes or a write to it
fails.

`hostthisd`'s proxy also pings its public client every 20 seconds at the
WebSocket protocol level and requires a pong within 10 seconds. This is below
normal reverse-proxy idle timeouts and reaps dead mobile connections; it exists
only on the `hostthisd` path. The canonical browser client also reconnects with
exponential backoff and jitter, reconnects immediately on foreground recovery,
and buffers only bounded durable actions while disconnected.

The live Worker harness is the acceptance gate: multiple clients receive durable
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
specified - so this constrains only how the client must be invoked. The server
skips a bare `--` wherever it arrives, so `ssh hostthis.dev <slug> -- --type
csv` works as well as `ssh hostthis.dev -- <slug> --type csv`.

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
- *another change to this paste is still settling; retry shortly* (exit 1):
  an earlier change to the paste has not settled yet. Nothing was saved and
  the upload's bytes are deleted.
- *busy storing other uploads; try again in a minute* (exit 1): the upload
  waited too long for a free upload slot (see "Upload admission") or for the
  blob store to admit its bytes (see "Worker upload admission"). Nothing was
  saved.

See "Exit codes" below for the canonical mapping.

Update creates a new immutable version under the hood, stored under
its own upload prefix. What the URL serves next
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
than the v3 bytes alone; for a site it is its stored total. That is
the only reading under which the SIZE column sums to the usage `whoami`
reports, which is what makes `list` a per-item breakdown of the quota.

When at least one row is charged for more than it serves, stderr carries a
note pointing at `versions <slug>` for the per-version breakdown. It is
conditional because a single-version list has nothing to explain. The note
is needed because `VERS` cannot carry the explanation: it shows the SERVED
version NUMBER, not how many versions are stored, and a paste whose v1 was
deleted is charged for two while still displaying `v3`.

Lists BOTH text pastes AND deployed **directories** (a directory shows its
own kind - `site` or `knowledgebase` - and its stored byte total; one
carrying no version timeline shows `-` in `VERS`). This matters because a site counts against the
same 1 GiB per-identity quota as pastes: if `list` omitted sites, an owner
could hit `would exceed your 1 GiB total quota` with no visible way to see
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
A **directory** is discriminated by its kind: `"site"` for a static site,
`"knowledgebase"` for a browsable base. A row with no version timeline has
`served_version` / `latest_version` / `pinned_version` `null`.
`size_bytes` is the item's CHARGED total: every live version for a paste,
the stored total for a directory. `served_size_bytes` is the bytes of
the version being served, and is `null` where there are no versions.

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
while writes are rejected as over-quota). `active_pastes` still counts
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
- `deleted` - the version's bytes were deleted via `delete <slug> <ver>`. The
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
from prior `delete <slug> <ver>` calls), then deletes every version's
bytes (see "Blob storage backends → Deleting bytes"). Reuses the slug for future
random generation. No undo. No confirm prompt (ssh sessions don't
tty cleanly; the verb is explicit enough).

**Delete heals its own lost tail (row absent, index entry present).** A
whole-paste delete is two transactions: the `{slug}` CAS removes the
paste row, its versions and the slug binding; a
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

Tombstones one historical version, then deletes its bytes. The
metadata row stays in place as the tombstone. The version number is NOT
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

### Pod
```
ssh hostthis.dev pod link https://alice.example/profile/card#me
open within 15 minutes to link SHA256:abc...xyz:
https://hostthis.dev/_solid/link/3f9c...?k=...

ssh hostthis.dev pod status
pod:     https://alice.example/hostthis/
webid:   https://alice.example/profile/card#me
linked:  2026-10-22

ssh hostthis.dev pod unlink
unlinked. new uploads are stored by hostthis; versions in your pod stay there.
```
Links a Solid pod so new uploads store their bytes in it (see "Pod storage").
`pod link` prints the URL on stdout and its QR code on stderr, as an upload
does.
`pod status` prints `no pod linked.` when there is none; `pod unlink` with no
link is the same no-op answer. `-o json` on `status` gives `{"pod", "webid",
"linked"}`, or `null`. A second `pod link` replaces the first once its login
completes.

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
    ssh hostthis.dev have <slug>                 keep list of the latest version; put
                                             unchanged files' lines in .hostthis-keep
                                             to skip sending them

LIMITS

    1 GiB per identity, counting post-compression bytes across all
    your active pastes. One paste: 10 MiB after compression. One site:
    1 GiB unpacked. HTML, Markdown, diff, or a gzip-tar site archive.

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

- `tz` is always present and names an IANA zone the runtime knows. The Room
  cell, which schedules in `tz`, is the one judge of that: both servers pass
  any non-empty `tz` to it (`utc` included, if the runtime knows it), so the
  answer never depends on a server process's own zone database. `at` is
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

Statuses: an unknown app is 404 on every push route and on `/api/push/key`,
checked before anything else; a path under `push/` other than the three
above is 400; a body that is not one JSON document of the right shape is 400
(`malformed JSON body`), and one that breaks a rule above is 400 naming the
rule. The Room cell checks a subscription or schedule again when it stores
it, and is the last judge: a document a server passed but the cell refuses (a
zone the runtime does not know, a `when` whose fraction follows a comma, a
port out of range) is 400 too, `push: invalid: <the cell's rule>` (for
example `push: invalid: invalid-tz`), never a 500. A missing room is 404; a
17th subscription is 413; a second test within the minute is 429 with
`Retry-After` in whole seconds, rounded up. Bodies
decode as Go's `encoding/json` decodes them into the documents above: the
first JSON value counts and anything after it is ignored, field names match
case-insensitively with the last match winning, unknown fields are ignored, a
`null` leaves a field unset, and a field of the wrong JSON type (a number
with a fraction or exponent where a weekday is expected, among others)
refuses the document.

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
`exp` is 12 hours out) and caches nothing across fires. The `sub` is the one
the latest subscription or schedule write carried: `https://<apex>` from
`hostthisd`, `<scheme>://<apex>` from the Worker, its scheme
`HOSTTHIS_PUBLIC_SCHEME` (`https` when unset).

### Timer

A room with a non-empty schedule keeps one due instant: the earliest next fire
across its items, computed in `tz`. A room without a schedule keeps no timer
and costs nothing. The Room cell has exactly one alarm, already used by the
budget recovery protocol, so both concerns share it: the alarm handler resumes
any pending budget operation and then fires any due push items, and every
re-arm sets the alarm to the earlier of the two deadlines rather than clearing
it. A budget concern that no longer needs the alarm must not disarm a pending
push fire. The pending budget operation stores its own retry instant
(`retryAt`), so either side computes the deadline from stored state; a pending
operation recorded before `retryAt` existed keeps the instant the alarm holds.

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
room byte ceiling, and is bounded only by its own caps.

### Caps

- 16 subscriptions and 16 schedule items per room.
- 2 KiB payload; subscription endpoint at most 1 KiB.
- 8 sends per subscription per local day.
- One `test` per room per minute (429 with `Retry-After`).

## Limits

A per-identity quota and an SSH-handshake gate, each enforced atomically
at the app layer. Storage is bounded per identity (1 GiB) and, for rooms,
per room and per app; hostthis carries no service-wide total-bytes cap.
Room push adds its own caps (16 subscriptions and 16 schedule items per
room, 2 KiB payload, 8 sends per subscription per local day, one test per
minute); see "Room push".

### Per-identity quota: 1 GiB (compressed)

The sum of an identity's active pastes' COMPRESSED bytes (counting
EVERY non-deleted version of an updated paste) cannot exceed
`UserQuotaBytes` (1 GiB; not operator-configurable). "Identity" is
the SHA256 fingerprint of the uploader's ssh public key. When pastes
get deleted, or have older versions explicitly
deleted via `delete <slug> <ver>`, the cap frees up. Over-quota uploads
error with `would exceed your 1 GiB total quota`.

The per-identity quota and per-paste cap are distinct: 1 GiB total and
10 MiB for one paste. The per-paste cap bounds one request's compressed
payload; the per-identity quota bounds accumulated logical storage.

**Writes are constant-memory.** A single-document upload compresses into a
temporary spill file while counting it, then streams that file to
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

Every upload stores its own copy of each file, so quota and physical storage
agree: every manifest path in every live version is charged its compressed
size, and that is what it occupies.

Deleted versions contribute zero bytes to quota and are inaccessible. Their
metadata remains as a tombstone, and their bytes are deleted once the
tombstone commits.

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

### Operator denylist: banned keys and networks

An operator can ban a key fingerprint or an IP network outright. A banned
session is refused at SSH admission, before the Sybil gate, so it records
nothing: no keygate row, no subnet note, no verb. It exits 7 with the stderr
line `hostthis: this key or network is banned from hostthis`, plus
`  reason: <reason>` when the entry has one.

An entry is one of:

- **a key**: `SHA256:<64 lowercase hex>`, the fingerprint `whoami` prints.
  It matches every session presenting that key, from any network.
- **a network**: a CIDR prefix, `/8` to `/32` for IPv4 and `/16` to `/128`
  for IPv6, stored in canonical masked form. It matches any session whose
  peer address it contains, whatever key it presents. The Sybil gate's
  `/24` and `/48` are the usual widths; a wider prefix covers a provider's
  block.

Each entry carries a `reason` (at most 200 bytes, shown to the refused client,
so it must be fine for them to read) and its `addedAt` time.

**Storage.** One `Denylist` cell holds every entry; the list is small and
operator-written, so one cell needs no fan-out. The Worker's internal router
serves it at `/denylist/list` (GET), `/denylist/add` and `/denylist/remove`
(POST `{entry, reason}` and `{entry}`). Add is idempotent and replaces the
reason of an entry already present; removing an absent entry succeeds. A
malformed entry answers 400 `invalid-entry`. The cap is 1000 entries
(507 `denylist-full`).

**Operator path.** Entries change through the Worker's internal API with its
bearer token, so a ban needs no deploy and no restart.

**Admission reads a cached copy.** Each hostthisd replica refreshes the list
every 30 seconds and checks sessions against its last good copy, so admission
costs no Worker call and a ban takes effect within one refresh. A failed
refresh keeps the last good copy. Before the first successful refresh the list
is empty: the denylist fails open, because refusing every session while the
Worker is unreachable would turn a metadata outage into a full SSH outage,
and an unreachable Worker already fails every verb that needs it.

The denylist governs SSH admission only. Content already published stays
until the operator takes it down ("Abuse reporting").

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
serialized inside their owning aggregate. celld v0.4 may serve several events
concurrently, so every check-and-mutate route in Identity, Paste, Room, and
Subnet runs under `blockConcurrencyWhile`. Local multi-key transitions use one
storage transaction or batch. Room's cross-cell byte invariant additionally
uses the versioned escrow protocol above.

To the service layer, a local multi-record transition fully lands or remains at
its prior state, and concurrent admission cannot race a stale check.

### Threat model: what's bounded and what isn't

*Bounded by the protocol*:
- One ssh key → 10 MiB of active LOGICAL bytes, ever
- One IP subnet → ~20 fresh keys × 10 MiB = ~200 MiB/day logical
  (~20–40 MiB/day actual storage after zstd)
- Concurrent per-identity and per-app quota races → serialized aggregate-local
  decisions; the Room escrow protocol preserves the cross-cell app ceiling

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

Blob bytes are independent of metadata. The service uses one blob port,
implemented over the Worker's payload binding (below). The port has
three operations:

- `Put(key)` streams one object in.
- `GetReader(key)` streams it back out.
- `GetRange(key, offset, length)` streams one slice of an object, for a file
  stored inside a pack.
- `DeletePrefix(prefix)` deletes every object under a prefix. An absent prefix
  deletes nothing and succeeds.

Metadata stores object keys, never a provider URL or filesystem path, so
changing providers does not change the domain or metadata shape.

### Object keys: one prefix per upload

```
uploads/<upload-id>/<n>
```

- `upload-id` is a random id minted when an upload starts, before any bytes
  are written.
- `n` numbers the upload's objects from `0`: a document is `0`, and a site's
  objects are its packs and its unpacked files, in the order they are written.
- Each manifest entry stores its object key, so a reader never derives one. A
  version records its upload id, which is how a delete finds its bytes.
- The key carries no slug or generation. A site deploy stages every file before
  its slug is chosen, and a slug collision retries with the same staged files.
- Storage involves no hashing, and uploads share nothing: an identical
  re-upload writes new objects under new keys.

A stored file's ETag is `sha256-<digest>` when its entry records a digest, so
cache identity follows the bytes wherever they are stored; otherwise it is its
object key, plus its offset when it lives in a pack (see "Edge caching"). An
upload's objects never change, so both are stable validators.

### Site packs: small files share an object

A site deploy writes its small files into shared objects called packs, so the
number of object writes follows a deploy's bytes rather than its file count.
A site of hundreds of small files costs a handful of writes, not one per file.

- **Which files pack:** a file whose declared size is at most 1 MiB joins the
  open pack. A larger file is its own object, as before. The declared size is
  only the routing hint: the quota guard still meters the bytes actually read.
- **When a pack closes:** once its stored length reaches 8 MiB, the pack is
  written as one object and the next small file opens a new one. The last open
  pack is written when extraction finishes, before any metadata commits.
- **Member format:** each member is a complete at-rest blob (the magic, then
  its zstd frame), so a slice decodes exactly like a whole object.
- **The manifest entry** records `Key`, `Offset` and `Length` (stored bytes).
  An entry without a `Length` names the whole object, which is every entry
  written before packs and every unpacked file. Old sites need no migration.
- **Quota:** a packed entry's `CompressedSize` is its `Length`, the same
  stored bytes an unpacked file would charge.
- **Reads** fetch exactly the entry's slice with one ranged request, so a
  packed file costs the same single read as an unpacked one.
- **Staging spills to disk:** a pack is built in a temporary file and sent
  with its length, so memory does not grow with it.
- **Packs are never shared:** a pack belongs to the one version that wrote it,
  even when a redeploy carries one of its files (see "Redeploy reuse").

### Redeploy reuse

A redeploy does not store or charge again a file the site already holds, and a
client can skip sending it at all. There are no reference counts, sweepers, or
background state: a delete still names exactly the bytes it frees, in its own
commit.

- **Digest.** Every site file entry records `Digest`, the hex SHA-256 of its
  decoded bytes, computed as it streams in. Entries written before it have
  none and are never reused.
- **Base.** A redeploy is computed against the slug's head: its latest live
  version, which is not the served one when the URL is pinned. A redeploy
  reuses only from the base.
- **Reuse.** A file whose digest matches a standalone (unpacked) file of the
  base points at that object: nothing is stored, nothing new is charged. A file
  matching only a packed base file is stored again in this deploy's own pack,
  so a pack is never kept alive by another version.
- **Keep list.** An archive may hold `.hostthis-keep` at its top level or one
  directory down. Its directory IS the site root: the sent files are stripped
  to it (never by the shared-directory guess, which would misplace files sent
  from one subdirectory), and a sent file outside it refuses the deploy. It is
  never stored or served. Its first
  line is `base <n>`; each further line is `<digest>  <path>`, the format
  `sha256sum` prints, with paths relative to the site root. Each listed path is
  carried from the base: a standalone file by reference, a packed one copied
  server-side into this deploy's pack. The copy reads each base pack once and
  moves its members' stored bytes unchanged, so carrying a file costs no
  per-file read and no decode. The deploy is refused whole, with
  nothing published, when `base` is not the head (the error says which version
  is, and to run `have` again), a listed path is absent from
  the base or has another digest, a path is listed twice or is also in the
  archive, or a line is malformed (including `sha256sum`'s escaped form, a
  leading `\`). A base file neither sent nor listed is dropped. `.hostthis-keep`
  anywhere else is ignored and never stored.
- **`have <slug>`** (owner only; not found otherwise, shaped as any not-found)
  prints the head as a ready keep list: `base <n>`, then a `<digest>  <path>`
  line for each file with a digest and a path free of backslashes and
  newlines, sorted by path. `-o json` gives `{"base": n, "files": [{"path",
  "digest"}]}`.

**Why deletes stay local.** Reuse reaches only the head, and a key absent from
the head is never picked up again. So the live versions referencing any object
are one unbroken run, starting at the version that stored it. Deleting a
version therefore needs only its live neighbours: the previous live version
*p* and the next live version *n*.

**The Paste cell decides both checks in its single transaction**, so nothing
coordinates across machines:

- An append that carries `base` is refused with `base-moved` (409) unless
  `base` is the latest live version, and with `bad-reuse` (409) when any entry
  names an object outside its own upload prefix that is not a standalone object
  of the base. The service deletes its prefix and answers *the site changed
  while this deploy ran; nothing was saved, retry* (exit 1). It never rebases.
- A version delete frees the objects of *v* referenced by neither *p* nor *n*.
  An upload prefix is deleted whole when its version is deleted and neither
  *p* nor *n* references a key under it; that also clears staging strays no
  manifest named. Other freed objects are deleted one by one.

**Charge.** Each live version records a charge: the stored size of its objects
that its previous live version does not hold. The artifact's charge is the sum,
which is the stored size of its distinct live objects. An append adds the new
version's charge; a version delete recomputes the next live version's charge
against its new predecessor and releases exactly the freed bytes, which
`delete <slug> <ver>` reports as `freed`. A version written before this has no
charge field and charges its full size, which is exact because it shares
nothing. A version's `size`, shown by `versions`, stays its full logical size.

The first redeploy of a site written before digests is a full upload; every
redeploy after it can reuse.

### Deleting bytes

Metadata commits first, then bytes, so a read that starts after the commit
never follows a row to missing bytes. A GET that read the head before the
commit and opens an object after its prefix is deleted answers 500.

- **Delete a version:** commit the tombstone, then delete the prefixes and
  objects its receipt names (see "Redeploy reuse"); with no sharing, that is
  that version's `uploads/<upload-id>/`.
- **Delete a paste:** commit the removal, then delete each removed version's
  prefix.
- **The delete names its own bytes.** The metadata delete answers with the
  upload ids of exactly what it removed, taken inside the same commit and kept
  in its receipt, so a replayed request gets the same answer. The service
  never re-reads versions afterwards: once a slug is removed it can be
  re-created, and a later read could name the new owner's uploads.
- **A crash between the two steps** leaks that prefix but never breaks a read.

Nothing asks afterwards whether anything else still uses the bytes: the commit
already decided it from the version's neighbours, and nothing outside the slug
can reference them.

An upload that is definitively refused deletes its own prefix before returning
its error:

- an untar abort (bomb guard, unsafe entry, too many files, empty archive);
- a site insert refused (slug taken through every retry, over quota);
- a refused redeploy or update append (over quota, not found, version too
  large, another change still settling);
- a create whose background blob write fails and marks the paste failed;
- a create whose paste is gone when its background blob write lands, as the
  ready transition reports.

An ambiguous outcome keeps the bytes: a lost response, a transport failure, an
accounting conflict the cell keeps pending, or any answer the metadata adapter
does not positively identify may still publish the version, and deleting its
bytes then would break a live read. A leak is always preferable to that.

The Worker adapter therefore maps only identified answers to the definitive
errors: an over-quota (507), too-large (413), or operation-pending (423)
status, a conflict whose body names `slug-taken`, `create-aborted`, or
`generation-mismatch` (the paste was replaced, so it reads as not found), and a
success answer stating `reason: "absent"`. Any other status or body, empty ones included, is an error
the service treats as ambiguous.

A process killed mid-upload leaks its prefix. That rare leak is accepted: there
is no sweep.

### The celld payload store

Each object is stored at `r2/payloads/<key>` in the celld fleet bucket,
through the Worker's `PAYLOADS` R2 binding (`bucket_name` `payloads`).
`hostthisd` reaches it at `HOSTTHIS_WORKER_ENDPOINT` and needs no object-store
credential of its own. The Worker exposes three stateless, cluster-internal
routes, served outside any cell:

- `PUT /blob/<key>` streams the request body into the binding and answers
  204. The request carries the object's length as `Content-Length`.
- `GET /blob/<key>` streams the object back, or answers 404 when it is
  absent. The runtime sends a streamed body chunked, so the stored size
  travels in an `X-Blob-Size` header; a response without it is an error.
- `DELETE /blob?prefix=<prefix>` pages through the binding's listing of that
  prefix and deletes in batches of up to 1000 keys, answering 204. An absent
  prefix deletes nothing and succeeds.

The Worker validates every key with the same rule as the Go adapters and
answers 400 otherwise. A delete prefix must also start with `uploads/` and
end on a segment boundary, so no route can empty the binding. The Worker
stores opaque bytes: compression stays in `hostthisd`, and no route reads a
whole body into memory.

The celld fleet bucket must never carry a storage quota. Payloads share it with
celld's own state and lease writes, so a full quota would refuse those too and
stop the fleet, not just new uploads.

### Worker upload admission: a byte-weighted queue

The celld runtime holds an upload's bytes in memory while it forwards them to
the bucket, so the bytes in flight across concurrent writes set its memory
peak. Every `hostthisd` process therefore admits its Worker writes through a
byte-weighted queue, which gives that peak a fixed ceiling however many
uploads arrive at once:

- **Weight is the stored size.** A write weighs its at-rest (compressed)
  length, clamped to `[1, budget]`. A body larger than the budget is admitted
  alone and counts as the whole budget; an unknown size counts as the whole
  budget.
- **Budget.** The admitted writes of one process never weigh more than the
  budget together. The default is 16 MiB (`HOSTTHIS_WORKER_PUT_BUDGET_BYTES`).
  The queue is per process, so the bound the runtime sees is the budget times
  the number of `hostthisd` processes. The runtime's memory is paid on its
  node, which also runs the bucket and the app, so the default is sized for
  node headroom rather than for throughput.
- **Waits, never rejects.** A write that does not fit waits in arrival order
  (FIFO) for earlier writes to finish. Waiting reads none of the body: it stays
  in the request's spill file.
- **Bounded wait.** A write still waiting after the wait limit (default 30s,
  `HOSTTHIS_WORKER_PUT_WAIT`) fails with the busy error and writes nothing. The
  SSH user sees `hostthis: busy storing other uploads; try again in a minute`
  (exit 1) on an update or site deploy. A new paste has already returned its
  URL when its write runs (see "Paste lifecycle status"), so a busy write there
  fails the paste like any other background write failure.
- **Reads and deletes are not queued.** `GetReader` streams without growing
  the runtime's memory, and `DeletePrefix` carries no body.

Both settings are validated at startup: a budget below 1 or a wait that is not
a positive duration stops the process.

### Upload admission: bounded concurrent uploads per process

Every in-flight upload holds a compressor (its history window and tables) plus
transport, decompression and archive buffers for as long as it streams. The
process's working memory therefore grows with the number of uploads running at
once, not with the request rate, so it is bounded by count. The celld queue
above bounds only the encoded bytes on their way to the store; it cannot bound
the encoders that produce them.

- **What counts as an upload.** Every SSH command that streams a body through
  the upload pipeline: a new paste (single file or site archive), a paste
  update, and a site redeploy. Every other verb (`list`, `get`, `versions`,
  `delete`, ...) and every HTTP read is not gated.
- **Limit.** Each `hostthisd` process runs at most 8 uploads at once
  (`HOSTTHIS_UPLOAD_CONCURRENCY`). The gate is per process, so the total
  bound is the limit times the number of processes.
- **Waits, never rejects.** An upload beyond the limit waits in arrival order
  (FIFO). While it waits the server reads none of its body, so SSH flow control
  holds the bytes on the client once the channel's receive window fills, and a
  waiting upload costs no encoder.
- **Bounded wait.** An upload still waiting after the wait limit (default 30s,
  `HOSTTHIS_UPLOAD_WAIT`) fails with the same busy error as a Worker write:
  `hostthis: busy storing other uploads; try again in a minute` (exit 1).
  Nothing is read or saved. A client that disconnects while waiting leaves the
  queue.
- **Held for the whole upload.** An admitted upload keeps its slot until the
  command finishes, including the synchronous blob writes of an update or site
  deploy, and releases it on every exit: success, error, or client
  disconnect. A new single-file paste keeps it while its body is read and
  encoded; the background write that follows (see "Paste lifecycle status")
  runs outside the slot.

Both settings are validated at startup: a limit below 1, a wait that is not a
positive duration, or an unparseable value stops the process.

### On-disk format

Every object written by any backend is zstd-compressed (level 3) and
prefixed with a 4-byte magic header `HZ\0\x01` (`HZ` for hostthis-zstd,
`\0\x01` for format version 1). The compressed body follows the magic.
Layout:

```
0..3      : magic 'H' 'Z' 0x00 0x01
4..N      : zstd-encoded original bytes
```

Writes are always compressed and prefixed, so an object's stored bytes are
identical whichever adapter wrote them. Reads inspect the first 4 bytes and
zstd-decode the rest. An object that does not start with the magic, an empty
one included, is damaged, and so is one holding the magic and no zstd frame
(the encoder writes a frame even for an empty file): its read fails before any
byte is served (the HTTP read surface logs the slug and answers 500, `get`
exits 1), while every other object stays readable. The Worker's read path
decodes the same format (see "The public surface").

A manifest entry records its object's decoded (uncompressed) length in up to
two fields. `Size` is for display only: an entry written before `DecodedSize`
existed may hold a wrong one there (0, or the object's stored length).
`DecodedSize` is set by every current write path, a document's from the bytes
read from its input and a site or knowledge base file's from the bytes read
out of its archive entry, and is absent on older entries; an empty file
records none either, since 0 is absent. It is the only length a read trusts.

A read response that streams stored bytes (an HTML paste, `?raw` bytes, a site
or knowledge base file) declares the entry's `DecodedSize` as its
`Content-Length`, on GET and HEAD alike, when the entry carries one (greater
than 0). An entry without one streams with no declared length, whatever its
`Size`. The first chunk is decoded before the status is sent, so damage found
there (a bad frame header) is a 500 like an unframed object. Damage found
later aborts the response and logs one warning naming the slug, and the last
byte is held back until the stream ends cleanly, so damage the frame checksum
finds at the very end still breaks the transfer: a damaged object never reads
as a complete body. A decoded length that disagrees with the declared one is
logged too, and the response ends short of its `Content-Length`.

### Entries without an object key

A manifest entry names its bytes by its object key and nothing else. An entry
without a key, like a paste whose row carries no manifest, is unreadable:

- The HTTP read surface answers 404, the same shape as an unknown path, for a
  document's URL, its `?raw=1` URL, a knowledge base document's `?raw=1` URL,
  and a site or knowledge base file alike. No shell is served for a document
  paste's keyless root; a knowledge base's shell, which answers every document
  path whatever it holds, still answers there and finds the bytes not found.
- `get` answers the standard not found (exit 4).
- Neither touches the blob store.

Such a record still decodes: stored or wire fields outside the current shape
are ignored, so listing, versions, and deletes keep working for it. A version
recorded without an upload id owns no prefix, so deleting it, or its paste,
removes metadata only.

A stored manifest that fails to decode (a string, a number, an entry field of
the wrong type) reads as no manifest on the paste row. Its paste is then
unreadable as above, while listing, versions, and deletes keep working for it,
and the metadata adapter logs one skip per decode naming the slug. The
tolerance covers the manifest alone: any other field of the wrong type still
fails the read.

A version listing carries metadata only (number, kind, upload id, size,
creation time, tombstone), never a manifest: the Worker reads no manifest to
build it and the adapter ignores one if sent. Listing a site's history therefore
costs the same whatever the site's file count, and a damaged version manifest
cannot affect `versions`, `pin`, or version deletes.

### Pod storage (bring your own storage)

An owner may link a [Solid](https://solidproject.org) pod. From then on the
bytes of that key's new uploads are stored in the pod instead of the payload
bucket. Nothing else changes: the same verbs, URLs, headers, output, quota and
reuse rules apply, and metadata (manifests, versions, ownership) stays in the
Worker's cells. Only where an object's bytes live differs.

**hostthis is its own Solid agent.** It never holds an owner's credentials.
The deployment has a WebID, `<scheme>://<apex>/_solid/id#me`, whose profile
names `<scheme>://<apex>/_solid` as its OIDC issuer. hostthis signs its own
DPoP-bound access tokens for that WebID with `HOSTTHIS_SOLID_KEY`, a Worker
secret holding an ES256 private JWK, and publishes the public half at
`/_solid/jwks`. A pod verifies them as it verifies any Solid-OIDC token: from
the issuer the WebID names. The owner grants that WebID read and write on one
container of their pod, `<storage>hostthis/`, and on nothing else. Whoever
holds `HOSTTHIS_SOLID_KEY` can therefore reach only those shared containers.
`HOSTTHIS_SOLID_ORIGIN` replaces `<scheme>://<apex>` for a deployment whose
public origin carries a port (dev and tests). Without the secret the Worker
has no pod support: `pod link` answers that pods
are not available, and every other path behaves as if no owner had linked one.

**The apex endpoints.** The Worker's public router serves these on the apex
host, and `hostthisd` forwards every apex `/_solid/` request to it (see
"Public and internal hosts"):

| Path | Answer |
| --- | --- |
| `/_solid/id` | the WebID profile (Turtle): `<#me> solid:oidcIssuer <…/_solid>` |
| `/_solid/.well-known/openid-configuration` | `{"issuer", "jwks_uri"}` |
| `/_solid/jwks` | the public key set |
| `/_solid/client` | the Client ID Document used when an owner links |
| `/_solid/link/<link-id>` | the link page (below) |
| `/_solid/callback` | the OIDC redirect target |

**Linking.** `ssh hostthis.dev pod link <webid>` prints a one-time URL,
valid for 15 minutes, of the form `/_solid/link/<link-id>?k=<secret>`. The
link id and secret are random; the Worker keeps the secret only as a hash.
Opening the URL shows the SSH key fingerprint it will be linked to and says
to continue only if you ran `pod link` yourself. When the profile lists more
than one `pim:storage` (a provider that gives one WebID several pods), the page
also lists them by host and URL for the owner to pick one. It preselects the
first on the WebID's own host, marked as holding the WebID, or else the first
listed; a single storage shows no choice. Switching pods later is another
`pod link`: only a sign-in can write the share rule. Continuing:

1. reads the WebID profile for its `solid:oidcIssuer` and its storages, uses
   the chosen one (a choice the profile does not list is refused), and
   redirects to that issuer: authorization code,
   PKCE, scopes `openid webid`. hostthis signs in as a client the issuer
   registers on the spot when its configuration offers
   `registration_endpoint` (client secret sent as HTTP Basic), and as the
   `/_solid/client` document otherwise or when registration fails: some
   providers lose the authorization result for a Client ID Document client. No `offline_access` is requested, so no
   refresh token is ever issued. Pressing Continue again starts another sign-in;
   each stays valid until one completes, so a later press never strands an
   earlier one, with the pod chosen for it.
2. On the callback, exchanges the code with a DPoP key made for this link
   alone, checks that the token's `webid` is the WebID that was named, and
   with that session finds the storage when the profile named none (the
   nearest container above the WebID whose `Link` header types it
   `pim:Storage`), creates `<storage>hostthis/` and writes its access rule:
   the owner keeps read, write and control, hostthis's WebID gets read and
   write, both for the container and everything in it. The rule is ACP when
   the container's access resource advertises
   `acp:AccessControlResource`, and WAC otherwise.
3. Discards the session and records the link. Only then does the page say
   linked.

The login is what binds the pod to the key. One hostthis identity can reach
every container shared with it, so a key may only be linked to a pod whose
owner signed in through that key's own link URL. A link URL that expires, was
already used, or whose secret is wrong fails without contacting the pod.

**Where an object goes.** An upload made while its key has a link mints an
upload id of the form `pod_<link-id>_<random>`, so its keys stay
`uploads/<upload-id>/<n>` (see "Object keys") and every prefix, receipt and
reuse rule is unchanged. The Worker's payload store sends a key whose upload
id names a link to `<storage>hostthis/uploads/<upload-id>/<n>` in that link's
pod, and every other key to the bucket. The link's record never changes once
written, so the Worker caches it per isolate.

- `PUT` writes the object with `Content-Type: application/octet-stream`.
- `GET` reads it, with a `Range` header for a pack member; a pod that answers
  200 to a range request fails the read.
- A prefix delete reads the upload's container listing (`ldp:contains`),
  deletes each object, then the container. Pod objects are never listed on
  the request path otherwise.

Each pod request carries a fresh DPoP proof and a hostthis access token the
Worker reuses for up to two minutes, one per pod origin. The token is valid
at every pod that shares with hostthis, so this relies on each pod enforcing
its DPoP binding, as Solid-OIDC requires: without our DPoP key a token taken
from one pod cannot be used at another.

**History may mix.** Versions written before a link, or after `pod unlink`,
keep their bytes in the bucket; versions written while linked keep theirs in
the pod. Reuse references an object wherever it lives, since it copies keys,
not bytes.

**Failure.** A pod that refuses a request (401, 403), is unreachable, or
answers 5xx makes the Worker answer 502 with a body naming the pod's
answer. On a site deploy or a paste update that is a definitive failure:
nothing is published, the upload's prefix is deleted as for any refused
upload, and `hostthisd` prints `your pod refused the write (<status>); check
that its hostthis/ folder is still shared with hostthis, or run pod link again`
and exits 1. A new paste's bytes are written after its URL is printed (see
"Paste lifecycle status"), so there it marks the paste failed. On a read it
is a 500, the same as a missing object.

**Unlinking.** `pod unlink` stops new uploads from going to the pod. It does
not touch the pod or existing versions, which keep reading from it for as
long as the owner leaves the container shared. Removing the share in any pod
app revokes hostthis at once; versions stored there then fail to read.

**Scope.** Paste and site bytes. Rooms stay in the Worker's cells.

---

## Metadata storage backends

The domain lives once, in the Worker (`worker/src`): Identity, Paste,
Room, and Subnet cells own durable metadata over a celld fleet's object store,
and the `internal/worker` adapters implement the application's metadata ports
over HTTP. There is no second implementation of the domain in Go.

```text
HOSTTHIS_WORKER_ENDPOINT=http://worker:8787         # required
```

The same Worker runs everywhere: on celld, on Cloudflare Workers, and, for
development, the Go tests and the browser suite, on this machine under
Miniflare (workerd) from `worker/localrt`, with cells in memory or a local
persist directory and payloads in a local R2 store. On Cloudflare the
`PAYLOADS` binding names an R2 bucket; on celld it lives inside the fleet
bucket.

Its source is split into modules under `worker/src`, entry `worker/src/index.js`.
celld loads a single module, so every runtime runs one bundled ES module,
`worker/dist/index.js`, built by `make bundle` (esbuild, pinned in
`worker/package.json`; unminified, names kept). `dist/` is a build output and is
not committed; `worker/wrangler.jsonc` points `main` at it with `no_bundle`, so a
deploy builds the bundle first. The local runtime refuses to start on a missing
bundle or one older than its sources.

The service and transport layers depend only on domain-shaped ports. The
conformance suite pins their behavior against the Worker.

### Atomicity contract

The Worker serializes mutating events per cell and commits local
multi-key transitions with one storage transaction or batch. Cross-cell invariants use the
durable protocols specified in the celld section below; they are not one global
transaction.

### Blob and operator boundary

Metadata and payload storage are independent ports, both served by the Worker:
payloads live in the fleet bucket under `r2/payloads/`, beside celld's own state
and outside every cell.

### The storage contract and its conformance suite

Whatever backend is in use, the rest of the app depends on it only
through small Go interfaces declared in `internal/service`:

- `PasteRepo` (upload): `InsertWithQuotaCheck`, `Get`.
- `PasteAdmin` (manage): `Get`, `ListByOwner`, `Delete`, `SetName`,
  `SetPinnedVersion`, `Unpin`, `AppendVersionWithQuotaCheck`,
  `ListVersions`, `GetVersion`, `DeleteVersion`, `SumActiveBytesByOwner`,
  `OwnerFirstSeen`, `OwnerSummary`.
- `KeyGateRepo` (keygate): `AdmitNewKey`, `SubnetSnapshot`,
  `SubnetsForIdentity`.

Every backend implements them identically. The observable contract
those interfaces expose, not the storage internals, is the load-bearing
thing: callers see the same return values, the same sentinel errors,
and the same accounting regardless of which backend is wired. Adding a
backend is only safe if the new backend preserves that observable
contract.

The sentinel error vocabulary those contracts speak (not-found,
slug-taken, over-user-quota, room-data-full,
app-rooms-full, too-many-new-keys) is OWNED BY THE DOMAIN LAYER
(`internal/domain`): the sentinels are business outcomes every layer
must agree on, not backend internals. The storage package re-exports
each sentinel under its historical `storage.Err...` name as an alias of
the same error value, so `errors.Is` identity holds whichever name a
caller matches against. The message text is stable contract too (it
keeps the historical `storage:` prefix): sentinel messages appear in
user-facing output, so moving the definitions must not change a byte of
them.

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
  independent of each other, and no service-wide total caps their sum.
- **Versions.** `AppendVersionWithQuotaCheck` assigns `MAX(ver_num)+1`,
  counting tombstones so numbers are never reused. An unpinned paste's
  head rolls forward to the new version; a pinned paste keeps serving
  its pin. `AppendResult.NewVer` and `AppendResult.WasPinned` (pin
  state before the append) are returned; both bump the
  clock.
- **Pin / unpin.** `SetPinnedVersion` makes a version sticky and rolls
  the head to it; `Unpin` clears the pin and rolls the head back to the
  latest non-deleted version.
- **DeleteVersion tombstones (content-inaccessible).**
  Flips a version's deleted flag, leaving the metadata row so the
  number is not reused and history stays auditable. `ListVersions`
  returns tombstones (newest first, marked `deleted`); `GetVersion`
  returns a tombstone too, upload id included. A deleted version is
  app-final and content-inaccessible, and its bytes stop counting
  against quota. The repo does NOT enforce refuse-current /
  refuse-pinned-current: those guards live in `Manage.DeleteVersion`, not
  the repo. `DeleteVersion` returns the tombstoned version's upload id.
  Whole-paste `Delete` is a full removal (the paste leaves every listing;
  there is nothing left to show versions of) and returns the upload ids of
  every version it removed.
- **Owner-gating is a service-layer concern.** The repos are NOT
  owner-aware: `Get`, `Delete`, `SetName`, and so on operate on a slug
  regardless of who owns it. IDOR protection (a cross-owner read
  surfacing as not-found) lives in `Manage.requireOwner`. A backend
  must NOT add owner checks; doing so would change observable behavior.
- **Owner stats.** `ListByOwner` returns the owner's pastes ordered most
  recently updated first, with `LatestVersion` populated;
  `OwnerSummary.Active` counts them; `SumActiveBytesByOwner` matches the
  quota math; `OwnerFirstSeen` is the earliest paste `created_at` (zero
  time when none).
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
the union of the service interfaces (no backend-specific helpers,
so the suite cannot accidentally pin a behavior one backend has and
another lacks):

```
type conformanceRepo interface {
    service.PasteRepo
    service.PasteAdmin
}

func runConformance(t *testing.T, name string, newRepo func(t *testing.T) conformanceRepo)
```

Pastes are created through `InsertWithQuotaCheck` / `AppendVersion-
WithQuotaCheck` with caps set to 0 (the documented "no quota
enforcement" path), so no backend needs an extra unchecked helper.

**The metadata contract never deletes bytes.** It records upload ids and
names the removed ones in each delete's answer; the service deletes those
prefixes after the metadata commits (see "Blob storage backends → Deleting
bytes").

### The conformance gate

The conformance suite runs against the Worker under Miniflare on every
`go test ./...`, and against a real celld node when `WORKER_TEST_ENDPOINT` names
one (CI runs that node against an object store, the production shape). The
Worker code is the same in both; the second run pins celld itself.


### Static-site storage

A static site is a paste whose version kind is `site` and whose manifest maps
safe relative paths to object keys. The root manifest
entry is `/`. Site files live in the configured `BlobStore`; metadata stores
only the paste row, version descriptors, and manifest. A knowledge base is the
same representation under version kind `knowledgebase`; storage treats the two
directory kinds identically.

`storage.Sites` translates the `service.SiteRepo` vocabulary onto the same
`PasteRepo` used by documents. There is no second site key family, owner index,
or quota sum:

- A first deploy inserts one site-kind paste.
- A redeploy appends a manifest version. Prior live versions remain available
  for pin, rollback, roll-forward, and per-version deletion.
- Each version is charged by every manifest path's compressed size.
- List and owner-byte accounting use the paste projection, where sites already
  appear, so the site adapter returns no duplicate listing or sum.
- Files are staged under the deploy's upload prefix before a slug is chosen. A
  slug collision retries the metadata insert with the same staged files; an
  insert that fails or exhausts its retries deletes the prefix.
- Reads reject a document-kind paste as not found rather than treating its
  one-file manifest as a directory.

This representation makes paste/site slug collision impossible by construction:
one slug names one paste aggregate and its version history. The site
conformance suite covers manifest round-trip, version charging, quota
interaction, ownership privacy, and slug reservation.

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

Each incarnation of a slug carries an opaque `generation`, and every artifact
row has one. Every accounting, projection, delete, and recovery call includes
it. The Paste cell answers a mutation whose generation is empty or differs from
the row's with `generation-mismatch` (409); Identity rejects a generation
that does not match its current entry, and permanent operation receipts remain
keyed by `(slug, generation, operation ID)`. A delayed call from an artifact that was deleted
and re-minted can therefore neither charge, release, nor rewrite the new
artifact; its caller is told the paste it named is not found.

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

A failed attempt, including one that throws, leaves the operation pending and
re-arms the alarm with exponential backoff from one second to a five-minute
ceiling, so a stuck operation costs a bounded number of commits instead of a
tight retry loop. An append whose Identity decision is neither a grant nor a
quota refusal keeps its operation pending and answers
`artifact-accounting-conflict` (409), whatever status Identity returned. A
mutation that finds a different operation pending persists nothing and answers
`artifact-operation-pending` (423). A repeat of the pending operation itself
that is still unresolved after resuming answers `artifact-operation-pending`
(502), because it may yet complete. The alarm re-arms at that deadline before any outbound call,
so a handler the runtime kills mid-call cannot fire again at once; the count
restarts when the operation advances a stage. A candidate version too large to
persist is refused with `version-too-large` (413) before any charge. One the
storage layer refuses only at publish can never succeed on retry: the append
ends with a permanent receipt carrying the same answer and settles the granted
charge back down through the same version fence.

Cell storage caps a single value at about 2 MiB, so a Paste cell never keeps a
growing history in one value. Each version's metadata lives under `ver:<n>` and
its manifest under `manifest:<n>`, and `maxVer` bounds the key range. A history
stored as a single `versions` value is split into those keys by the first write
that touches it, in the same transaction as that write.

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

An app's room bytes are charged to its owner's Identity cell under
`roomCharge:<app slug>`, a `{ version, allocated, target }` decision record
with the same fence as an artifact's. The owner's total, which reserve,
artifact decisions and `bytes` all use, is every entry's charged size plus
every room charge's `allocated`. The app's Paste cell keeps the owner side in
`roomOwnerCharge`: `{ owner, version, target, settled }`. A room decision that
raises the app total records the next owner version and target unsettled
first, then asks the owner's Identity cell (`roomcharge`), then commits the
app decision only on a grant; a refusal persists as the room's refused
decision. A decision that lowers the total commits first and settles the
owner charge after. Every room decision first replays an unsettled owner
charge, so response loss and a crash between the steps leave the owner
charged for at least what the app holds, never less. A Paste row whose
identity differs from `owner` (the slug was re-minted) moves the charge:
the old owner's record is set to zero and the new owner's is seeded with the
app's total. `roomchargesync` seeds or refreshes the owner charge with no cap,
which is how bytes that predate the rule are charged.

There is no room-retention or full-room-delete interface. Rooms persist
indefinitely. Creation-ledger entries are the only room records removed by
elapsed time, because they are rate-limit state rather than user content.

The Worker adapter uses two cells:

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
retained data. A value with no stored text (a room written before the relay
kept one) is encoded from its bytes wherever it is read, so its snapshot,
frames and whole-room GET still agree. There is no offline Room
reconciliation pass.

The conformance cases cover round-trip, reserved
object-property keys, cross-room and cross-app isolation, nonexistent-room
shape, room byte/key ceilings, exact serial and concurrent app ceilings,
immediate cross-room capacity release, creation refusal at a full app, creation
ledger behavior, and dense sequences. Deterministic Worker tests additionally
sabotage every durable boundary and replay accepted, refused, stale, reordered,
and same-version/different-target coordinator calls.

---

## Worker-backed metadata storage (production)

The production metadata adapter is a Worker reached over cluster-internal
HTTP. Each named cell owns a private SQLite database, and celld fences ownership
and durability through the fleet's object store. The public Go service remains
the translation layer: it owns SSH/HTTP policy and calls service ports whose
Worker adapters translate to Worker requests.

### Cell taxonomy

Four active classes are permanent because class names participate in stored
identity:

- **Identity**, one per owner: quota entries, listing summaries, first-seen,
  durable intents, and the keygate reverse index.
- **Paste**, one per app slug: paste row, per-version keys, claim, the app-scoped
  room creation and byte-budget coordinator, and the app's VAPID key pair. Room accounting works without a
  paste row, so a static site can own rooms under the same slug.
- **Room**, one per `(app slug, room UUID)`: room document, dense sequence,
  pending budget operation, push subscriptions, schedule and send counters, the
  shared alarm, and hibernatable sockets.
- **Subnet**, one per source network: Sybil-admission rows.

The deprecated `IntentLog` class remains only so the original migration stays
resolvable. No request routes to it, but it keeps a binding (`INTENT_LOGS`):
celld registers only classes that have one, and a cell left over from the
original migration must still be able to activate.

Paste artifact state and app-room accounting are distinct logical aggregates
co-located in one physical cell because both are addressed by the app slug. This
reuses the existing permanent class and avoids a second app-slug coordinator;
room methods do not depend on a paste row.

An operator tool reaches a Paste cell by id over celld's internal `/do/<cell>`
route. That route takes no path after the cell, so the op is named in an `op`
query instead (`/do/Paste:<id>?op=get`, `?op=versions`); a path that names an
op wins over the query.

Two read-only Paste ops answer a read without handing out the row:

- `resolve` (`GET /paste/resolve?slug=&path=`, plus `raw=1` and `files=1` for
  the `?raw` and `?files` flags) runs the read rules ("Serving a directory",
  "SPA fallback (route vs. asset)", "Knowledge bases") against the row.
  `path` is the request path, percent-decoded once, starting with `/`; any
  other is 400 `invalid-path`. The answer is `{found: false}` without a row,
  `{status}` for a pending or failed document at its root, or
  `{action, kind, updatedAt, uploadId}` with, by action, the served entry's
  `{key, contentType, size}`, the `shell` whose template renders, or a
  knowledge base's `files` in byte order. `action` is one of `html`, `shell`,
  `raw`, `file`, `kbshell`, `kbraw`, `files` and `notfound`. A manifest that
  fails to decode reads as none (see "Entries without an object key").
- `exists` (`GET /paste/exists?slug=`) answers `{exists}`: whether a row is
  present, whatever its status.

Neither carries the owner identity, the name, the generation, or any version
but the served one.

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

### Moving cells between runtimes

A deployment moves between runtimes (celld and Cloudflare Workers) by copying
each cell whole. Every cell class answers four internal paths on its own
`fetch`, which the Worker's router never forwards, so only code holding the
class binding reaches them:

- `GET /__cell/name` answers the name the cell was addressed by, as the
  runtime reports it (`state.id.name`), or `null` when it does not.
- `GET /__cell/image` answers the cell's every key-value entry and its alarm
  time as JSON: `{ format: "hostthis.cell-image", version: 1, kv: [[key,
  value], ...], alarm: <epoch ms> | null }`. A value JSON cannot carry (bytes, a
  `Date`, a `Map`) travels tagged and comes back as the same type:
  `{ "$t": <type>, v }` for `undefined`, a non-finite number or `-0`, `bigint`,
  `date`, `map`, `set`, `bytes` (a `Uint8Array`, base64url), `buffer` (an
  `ArrayBuffer`), and `view` (any other typed array or a `DataView`, named in
  `k`); a plain object that owns a `$t` key travels as `object`. A value of any
  other type fails the image with 500 `unsupported-value` naming its key, so no
  entry is ever dropped.
- `POST /__cell/restore` applies an image to a cell that holds nothing, sets
  its alarm (a time already past fires at once), and answers
  `{ restored: true, entries: <n> }`; a cell that holds anything answers 409
  `{ restored: false, reason: "not-blank" }` and is left as it was. A malformed
  image answers 400 `invalid-image` before any write. The entries and the alarm
  commit in one transaction, at most 128 entries per put, so a write the
  storage refuses answers 500 `restore-failed` and leaves the cell blank.
- `POST /__cell/disarm` deletes the cell's alarm and answers `{ alarm: <epoch
  ms> | null }`, the time it was set for. A mover disarms the original before
  restoring its copy with that alarm, so a scheduled push never fires from
  both. Entries are untouched.

A cell too large for one image (a site with hundreds of versions holds tens of
megabytes of manifests, past what one isolate can hold as JSON) moves in parts:

- `GET /__cell/image?start=<key>&limit=<n>` answers a page: the entries whose
  key sorts at or after `start`, in key order, at most `n` of them, the alarm,
  and `next`, the first key the page leaves out, or `null` at the end. Without
  the parameters the image is the whole cell and `next` is `null`. Pages
  describe one state only while nothing writes to the cell, so a mover pages a
  frozen copy.
- `POST /__cell/restore` takes an image carrying `part: { id, index, count }`.
  Index 0 needs a blank cell, as a whole image does. Every later index needs
  the cell to be mid-restore of the same `id` and waiting for that index,
  otherwise 409 `out-of-order`. A part whose entries include a key the cell
  already holds answers 409 `overlap`. Each part commits in one transaction;
  every part before the last also records the progress under the reserved key
  `__cell:restore`, and the last removes it and sets the alarm (only the last
  part's alarm counts). The answer adds `part` and `complete`. A failed part
  leaves the cell where the previous part left it, so a retry of that part
  resumes the move.
- Until the last part lands the cell serves whatever it holds, so a mover sends
  the entries a cell's reads start from (a paste's `row`) in the last part.

The routes carry no token of their own: they are unreachable through the
Worker's public router. Subnet cells hold only a rolling admission window and
are not moved.

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
- R2 does not replace the provider-neutral BlobStore contract. It backs one
  adapter of that contract (the celld blob backend), and compression, keys,
  and lifecycle stay in `hostthisd`.
- Service bindings do not remove hostthis's app/room protocol or public policy
  boundary.

The unified peer tunnel is a runtime transport improvement. It replaces no
hostthis adapter or domain rule.

### Blob and operational boundaries

Paste and site payloads live in the fleet bucket under `r2/payloads/`, reached
through the Worker's stateless `/blob` routes and its `PAYLOADS` R2 binding
(see "Blob storage backends"). No cell reads or writes them: `hostthisd` writes
and deletes payload bytes around its metadata calls, and the byte routes run in
the Worker outside any cell. Room values are small mutable metadata and remain
in Room cells.

The fleet bucket must never carry a storage quota. Payload growth shares it
with celld's state and lease writes, and a quota that fills would refuse those
as well.

One fleet bucket serves one Worker application. `hostthisd` serves all public
content unless a deployment routes public hosts to the Worker's public surface
(see "Public and internal hosts"). The Worker's internal routes accept an optional shared token: when the Worker
has `HOSTTHIS_WORKER_TOKEN` set, every route except `/healthz` (the cell
routes, `/blob`, and the room join) requires `Authorization: Bearer <token>`
and answers 401 without it, compared in constant time. `hostthisd` sends the
header when its own `HOSTTHIS_WORKER_TOKEN` is set. A deployment whose Worker is
reachable only inside its cluster may leave both unset and rely on
NetworkPolicy; a Worker reachable across the internet sets both, and a Worker
with a public apex (below) must set the token. Worker health and runtime health are
separate: `/healthz` checks the hostthis Worker, while
`/.well-known/worker/health` is the node lifecycle probe.

### Public and internal hosts

The Worker has two routers. The internal router is everything above: the cell
routes, `/blob` and the room join behind the optional token, and `/healthz`.
The public router answers the public hostnames. Which router a request reaches is
decided from its host, before any path is looked at.

`HOSTTHIS_APEX_DOMAIN` on the Worker turns the split on. Unset or empty, the
Worker runs only the internal router, exactly as above, whatever the Host and
headers, and its Room cells relay every frame as `hostthisd`'s proxy expects
(see "Real-time room relay"); every existing deployment runs this way. Set,
each request is classified in this order:

1. **Public host.** A request whose host is the apex or any name under it
   (`<apex>`, `<slug>.<apex>`, `www.<apex>`, `a.b.<apex>`) goes to the public
   router, whatever its path, query, method or headers, a valid token
   included. Nothing a public request carries reaches the internal router or
   the payload bucket, and it reaches a cell only as values the public router
   parsed (below).
2. **No token, no internal router.** With `HOSTTHIS_WORKER_TOKEN` unset or
   empty, every other request, `/healthz` included, answers 503: a Worker with
   a public surface never runs an open internal router.
3. **Forwarded.** A request carrying `X-Hostthis-Forwarded-Host` and a valid
   token goes to the public router as if addressed to that host. The header is
   read only after the token check, so it gives a caller nothing the token did
   not already give. A forwarded host that is not a well-formed hostname
   (dot-separated labels of letters, digits and hyphens) equal to or under the
   apex answers 400. Without a valid token the header is ignored and the
   request is internal (401, or `/healthz`). A forwarded request's client
   address is the `X-Hostthis-Client-IP` header, trusted for the same reason;
   a direct request's is never read from it.
4. **Internal.** Everything else reaches the internal router, unchanged.

Hosts compare after normalization: lowercase, no port, no trailing dot. The
apex value is normalized the same way, so `Example.COM.:443` names
`example.com`.

The public router first does what Go's server and `ServeMux` do before
`hostthisd` routes, for every request: a malformed percent escape is a 400;
`/_hostthis` without its slash is redirected 307 to `/_hostthis/`; a path that
cleaning changes (a doubled slash, a `.` or `..` segment; a trailing slash is
kept) is redirected 307 to its cleaned form with the query kept; and the path
is percent-decoded once. Cleaning works on the escaped path, as `ServeMux`'s
does, and the `Location` is that cleaned escaped path: an escaped `/` or `\`
stays escaped (`/%2Fevil.com//` redirects to `/%2Fevil.com/`), so the
`Location` never starts with `//` or `/\` and never leaves the host. On every
host it then answers the renderer assets under `/_hostthis/` and `/healthz`
and `/readyz` (see "The public surface").

It serves an app on a slug host, `<slug>.<apex>` whose one label parses as a
slug, and, with `HOSTTHIS_URL_MODE=path` (dev and tests only; `subdomain` is
the default), under `/p/<slug>/` on the apex host. An app's rooms API, room
push and room WebSocket (`/api/rooms[/...]` and `/api/push/key`, see "Rooms",
"Real-time room relay" and "Room push") come first, as in `hostthisd`; every
other path is the app's read path (see "The public surface"). It builds every
cell request from parsed values: the slug from the host or the path-mode
prefix, the room from the parsed UUID, the key or read path from the decoded
path, the read flags from the parsed query, and a body it decoded, checked and
encoded again. Neither the client's URL nor its query, headers or body reaches
a cell, so no request can choose a cell op. Its other Worker settings are
`HOSTTHIS_PUBLIC_SCHEME` (the push `sub`) and `HOSTTHIS_CLIENT_IP_SOURCE`
(the room creation limit's client address, see "Quota and abuse"). With it
unset, or naming neither `cf-connecting-ip` nor `xff-rightmost`, every direct
client shares one room creation bucket: requests are still served, and the
first direct public request an isolate sees logs one warning naming the
variable. A forwarded request carries its own client address, so it never
warns.

Every other request answers 404 with `404 page not found`,
`Content-Type: text/plain; charset=utf-8` and `X-Content-Type-Options:
nosniff`, the 404 `hostthisd` gives an unknown slug. The apex `/` is one: the
landing page stays `hostthisd`'s. The apex `/_solid/` paths are the one
exception (see "Pod storage"). The Worker never redirects between schemes;
that is the front door's job.

### The public surface

With an apex set, the Worker answers paste and site reads itself, by the
rules `hostthisd`'s read path follows, so the same URL gives the same answer
from either server (statuses, the content headers, bodies byte for byte).
Nothing changes for a deployment until it routes public traffic to the Worker
(a Workers route, or forwarded mode); `hostthisd` keeps its own read path.

The URL parser resolves `.` and `..` segments (`%2e` included) before the
Worker sees the path, so such a request is served at the resolved path, where
`hostthisd` redirects a literal dot segment to that path and looks an escaped
one up as part of the path.

**The read path.** Reads answer GET and HEAD; any other method gets `405`
with `Allow: GET, HEAD`. Each request makes one call to the slug's Paste cell,
the read-only `resolve` op, built from parsed values only: the slug from the
host (or the path, in path mode), the decoded path, and the `raw` and `files`
flags. The flags are read as Go's `URL.Query().Has` reads them: pairs split on
`&`, a pair holding `;` dropped, keys percent-decoded, any value (`?raw`,
`?raw=0` and `?r%61w` all ask for raw bytes). The client's URL and query are
never forwarded. The answer maps to a response as `hostthisd` maps the same
case:

| answer | response |
| --- | --- |
| no row, `notfound` | 404 |
| `pending` | 200 loading page, `no-store`, `Retry-After: 1` |
| `failed` | 410 error page, `no-store` |
| `html`, `raw`, `file`, `kbraw` | 200, the stored bytes, ETag the entry's validator (`sha256-<digest>` when it has a digest, else its key, plus its offset when packed), `Content-Length` the entry's decoded size when it has one |
| `shell`, `kbshell` | 200, the kind's shell with its CSP, ETag the shell version |
| `files` | 200, the sorted path list as `json.Encoder` writes it, ETag `files-<upload id>` |
| a kind with no shell | 500 `unsupported kind` |
| a metadata or blob failure | 500 `internal error` |

Content responses carry the sandbox headers (see "HTML sandboxing"),
`Cache-Control: public, no-cache`, `ETag` and `Last-Modified`, and answer
conditional requests before any payload is read (see "Cache-Control
posture"). Raw bytes take the kind's raw type (`text/plain; charset=utf-8`
when it has none), a site or knowledge base file its stored type
(`application/octet-stream` when empty). Every 500 is bare, as `hostthisd`'s:
its text with `Content-Type`, `X-Content-Type-Options: nosniff` and
`Cache-Control: no-store`, no sandbox headers or validators. It logs one
warning naming the slug and the cause, and nothing else (see "5xx
observability on the read surface").

**Byte ranges.** A stored-bytes response (`html`, `raw`, `file`, `kbraw`)
whose entry records its decoded size answers single byte ranges, since media
players (iOS Safari's `<audio>` and `<video>` among them) refuse to play from
a server without them. Such a response carries `Accept-Ranges: bytes`. After
the conditional check, a `Range` header of exactly one `bytes=` spec
(`a-b`, `a-`, or `-n`) is served as:

- `206` with `Content-Range: bytes a-b/size` and `Content-Length` the
  range's length, the end clamped to the last byte;
- `416` with `Content-Range: bytes */size` and no body, when `a` is at or past
  the end or `n` is zero.

Any other `Range` (several ranges, another unit, a malformed spec) is ignored
and the whole body is a `200`, as is a `Range` sent with an `If-Range` that is
not the response's ETag. An entry without a recorded size ignores `Range`.
The stored object is decoded from its start and the bytes before the range
are discarded, so a range costs decode time up to its end. Only a range that
reaches the last byte can still be failed by the frame checksum.

The bytes stream from `PAYLOADS` through a check of the at-rest magic and a
zstd decoder (see "On-disk format"), never held whole: libzstd's decompressor
compiled to WASM, vendored with its build in `worker/vendor/zstd`. It decodes
only as the client reads, at most 128 KiB per step, refuses a frame whose
window exceeds 8 MiB before allocating it, and verifies the frame checksum.
Each isolate reuses up to four idle decoder instances; WASM memory never
shrinks, so an instance whose memory grew past 16 MiB (a frame with a window
of several MiB) is dropped when its decode ends instead of kept idle.
The first chunk is decoded before the response starts, so a missing object, a
missing magic or a damaged frame header is a 500. Damage found later (the
checksum sits at the end of the frame) errors the response's body stream and
logs one warning naming the slug. A GET of stored bytes whose entry carries a
`DecodedSize` (see "On-disk format") states it as `Content-Length`, held to
it by the runtime's `FixedLengthStream` where it has one. Decoded bytes that
would run past that size, or end short of it, are damage too, and the chunk
that would run past is not sent. The chunk that completes the size is sent
only once the stream has ended cleanly (the decoder verifies the frame
checksum after its last output), so damage found at the checksum also leaves
the body short. A body that ends early either aborts the connection or ends
short of its `Content-Length`, and no client takes it for whole. An entry
without a decoded size (none, 0 or negative, whatever its `Size`; a missing
field decodes as 0, as Go reads it) streams without a `Content-Length`. Its
damage found after the headers still errors the body stream and logs, but a
runtime may end a chunked response cleanly when its body errors (Cloudflare
does), so there a damaged body can read as whole: only the decoded size
guards against that. Shells and file lists are generated and carry none. A
HEAD never opens the object.

**Renderer assets.** On Cloudflare, the build's static assets answer every
known `/_hostthis/<name>` before the Worker runs. A request that reaches the
Worker resolves the name as `hostthisd` does, by the path's last segment, so
`/_hostthis/mdshell/md.js` serves `md.js`; an unknown name is the 404. A name
too large for static assets (listed in `static-r2.json`) streams from
`PAYLOADS` at `static/<shell version>/<name>`, stored as is; so does every
asset when the Worker has no `ASSETS` binding. Either way the response carries
the asset's Content-Type and `public, max-age=31536000, immutable`. Assets
answer GET and HEAD, and a HEAD reads nothing. The static assets layer
answers a known name on every host, the internal endpoint's included, with a
validator of its own (an `ETag`, which `hostthisd`'s assets do not carry), and
answers any other method with a 405 of its own, without `Allow`.

**Deliberate differences** from `hostthisd`, each where the Worker is
stricter or the runtime decides:

- Reads and assets answer only GET and HEAD (405 otherwise).
- A HEAD never opens the object, so a damaged object is 200 to HEAD, 500 to
  GET.
- `If-None-Match` compares weakly (RFC 9110): an edge that compresses a
  response turns its ETag into `W/"..."`, which must still match.
- A stored kind with no shell is a 500 even to a conditional request.
- Hosts compare lowercased, and `/p/` is served only on the apex host.
- `.` and `..` segments are resolved by the URL parser rather than redirected
  (or, escaped, looked up as written).
- An asset the static assets layer answers carries that layer's `ETag`, and
  its 405 no `Allow`.

**Caches.** Both are decorators around the read path, wired at the entrypoint,
off unless configured, and used only where the runtime has a Cache API
(celld's always misses, so a celld deployment leaves them off):

- `HOSTTHIS_EDGE_CACHE_TTL_S` (default `0`, off) caches whole read responses
  in the data center's cache. An entry's key is on the public host, so a
  hostname purge of the slug's host reaches it (in path mode, so does a prefix
  purge of `<apex>/p/<slug>`), and names only the build and what the read
  path answers from: the decoded path and the `raw` and `files` flags as
  parsed. Any other query shares the entry, so a client's random query
  neither adds an entry nor misses. The build is a hash, made when the bundle
  is built, of the Worker's code and the shells: a deploy starts a fresh key
  space, so a deploy that changes shells, or how any read is answered, takes
  effect at once, with no purge. An unconditional GET that misses fills it:
  the stored copy carries `Cache-Control: public, max-age=<ttl>` and
  `Cache-Tag: <slug>`, while the
  client gets the browser headers. A hit answers GET, HEAD and conditional
  requests (the cache answers `If-None-Match` and `If-Modified-Since` with a
  304 itself, by RFC 9110's rules) without a cell call or a payload read, and
  reaches the client with `Cache-Control: public, no-cache`; a 304, or the
  answer to a HEAD, carries no body. A conditional request or a HEAD that
  misses goes to the read path and fills nothing. Only a `200` with the
  content posture is stored: never a 5xx, a 404, or a pending or failed page.
  A fill is capped by `HOSTTHIS_EDGE_CACHE_MAX_BYTES` (default 8 MiB): a
  response whose `Content-Length` (the entry's decoded size) is over it starts
  no fill, and one that states none (a shell, a file list, an entry without a
  decoded size) is abandoned once it runs past it. A write purges the slug's
  host (see "Active invalidation"), which also drops these entries; the TTL
  bounds a missed purge. A read that
  began before a write can still be filling when the purge lands, so a fill
  must end within 10 s of its read beginning (timed from before the read's
  cell call), and one that would end later is abandoned and stores nothing:
  `hostthisd` purges the slug a second time 15 s after each purge, and that
  second purge then finds every entry such a read stored.
- `HOSTTHIS_BLOB_CACHE_MAX_BYTES` (default `0`, off) caches the decoded bytes
  of each object whose entry carries a decoded size (`DecodedSize`) of at most
  that many bytes, keyed by the object key; an entry without one is not
  cached. A key never names different bytes, so these entries need no
  invalidation; they serve the miss right after a purge when the bytes did
  not change (a pin back to an older version). A decode that fails stores
  nothing.
- A cache that fails to answer is a miss, never an error.

A response stored while it streams to the client is split in lockstep: the
source is read only when both the client and the cache have taken the last
chunk, so neither copy is buffered whatever the client's pace. The client's
copy leads: a client that stops reading before the end cancels the source,
which stops the decoder and the payload read, and fails the cache's copy, so
no partial body is stored and no fill outlives its client. A fill that fails
leaves the client to read alone.

## Edge caching

hostthis has two scaling cliffs that a CDN solves:

- *Egress bandwidth*: a 10 MiB paste served at 100 req/s = ~2.5 PB/month.
  Hetzner free egress is ~20 TB; one viral paste could blow the budget
  in days. A CDN that stores responses absorbs nearly all of it: it
  revalidates with the origin, and an unchanged response's 304 carries
  no body.
- *Render CPU*: a markdown GET no longer renders on the server - it
  streams the raw bytes or the fixed shell, and the browser runs marked
  + DOMPurify. So a hot URL no longer pegs a server CPU core; the CDN
  still absorbs the egress for the raw bytes and the shell.

### Cache-Control posture

Every response that serves a paste's or site's content sets:

```
Cache-Control: public, no-cache
ETag: "<validator>"
Last-Modified: <RFC1123 from the paste's updatedAt>
```

That covers an HTML paste, a render shell, `?raw` bytes, a site or knowledge
base file, a knowledge base's shell and its `?files=1` list. The validator is
the served file's object key for stored bytes (a key never names different
bytes), the shell version for a shell page, and `files-<upload id>` for a file
list.

`no-cache` lets a browser or shared cache store the response but makes it
revalidate before every reuse, so a reader always sees the version the slug
serves now: a reload after an update, pin, unpin or delete shows the change,
and a reload of an unchanged paste costs a bodiless 304. A `max-age` window
would let a browser show its own stored copy, without asking, for the length
of the window after an update; for a site it would keep serving cached js and
css sub-resources after a redeploy until each expired.

A request whose `If-None-Match` lists the ETag (or is `*`), or that has no
`If-None-Match` and an `If-Modified-Since` at or after `updatedAt` (to the
second), is answered 304 before any payload byte is read. A present
`If-None-Match` decides alone (RFC 9110 section 13.2.2): one listing only other
tags gets the full response whatever its `If-Modified-Since`. An empty header
counts as absent. The pending and failed pages are `no-store` (see "Paste
lifecycle status").

The versioned renderer assets under `/_hostthis/` are
`public, max-age=31536000, immutable`: their URLs carry the shell version.

The Worker's public surface can also keep whole responses in the edge's own
cache for a TTL, purged with the slug's host on every write (see "The public
surface"); browsers still revalidate every load.

Apex landing page is `Cache-Control: public, max-age=300` (5 min) so
content updates propagate quickly without becoming a no-cache origin
hammer.

The rooms API (`/api/rooms`, see "Rooms (app persistence)") is never
cacheable: every response it writes, success or error, carries
`Cache-Control: no-store`. A room key's value changes under one fixed URL,
so any stored copy is a stale one. Errors matter as much as successes: when
the origin says nothing, a CDN applies its own default and can hold a 404
for minutes, so a key read before it is written would keep answering 404
after the PUT that creates it.

### 5xx observability on the read surface

Every 5xx returned by the paste/site read path (a metadata read failure,
a blob read failure, an unsupported stored kind) logs one warn-level line
carrying the slug and the underlying error before the generic
"internal error" body is written. The response carries that text and
`Cache-Control: no-store` and nothing else from the content response it
replaced: no `ETag`, `Last-Modified`, cacheable `Cache-Control` or sandbox
headers, whether the failure came before or after they were chosen, so no
cache stores the error or revalidates it against the content. The body stays
generic (no internal detail leaks to clients), but the operator can always
attribute a read 500 from the logs - a read canary failure with no matching
log line means the request never reached this process. The slug is the only
request-derived value logged (slugs are public identifiers; no client
IPs, no headers, no payload).

### Command log on the SSH surface

Every SSH session logs one line when it ends: the verb, the slug it targets
(`-` for none), its outcome, and its duration. The verb is the registry name
(`upload` for the implicit upload, `update` for the bare-slug shortcut,
`unknown` otherwise) and the slug is logged only when the argument parses as
one, so no free-form client input reaches the log: no names, labels, flags,
payload, or key material.

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

Because the bare URL is one representation, it is safe to keep in a shared
cache: there is no per-`Accept` variant to mis-pin. A CDN keys its cache on the
URL, not on `Accept` (Cloudflare honors only `Vary: Accept-Encoding`, never
`Vary: Accept`), but whichever client primes a cache, every later client gets
the same shell. Like every content response it is `public, no-cache`, and a
shell change (a version bump in `web/shells/shells.json`) changes its ETag, so
it shows on the next revalidation after the deploy that ships it. The
Worker's edge cache keys every entry on the build, so a deploy that changes
shells takes effect there at once too (see "The public surface").

### Active invalidation: CachePurger interface

When the bytes served at a paste's URL change, the cached version at
the CDN edge (and in browsers) becomes stale. hostthis fires a purge
call so the next reader fetches fresh from origin.

Every write that changes what a slug serves fires a purge:
- `update` - content for the latest version changes.
- `delete` - URL should return 404, not the cached body. This covers a
  site or knowledge base deleted through the site-delete fallback as well
  as through the paste delete.
- `pin` - the served version changes (e.g. pinning v1 hides v2 again).
- `unpin` - when a pin was holding the URL on an older version,
  unpinning re-exposes the latest; without a purge, a cache that keeps
  responses for a TTL of its own holds the old pinned bytes.
- a site or knowledge base deploy to an existing slug (re-deploy in
  place) - every file of the slug may change, and the shape itself can
  (adding a root `index.html` turns a knowledge base into a site).

`rename` does NOT purge - the name is owner-only metadata, not part of
the public response. Same with `versions`/`list`/`whoami`/`get`. A new
upload or deploy mints a slug nothing has cached, so it does not purge
either.

**A write purges even when it reports failure.** A write can report an
error *after* it committed - a transport error that loses the cell's
answer, a transient storage error partway through a delete - and then the
URL serves the old state (or deleted content) from the edge until the
edge's own TTL ends. Since purging a slug that did not change is harmless
(the next reader simply re-fetches it), every purging write purges unless
its error proves nothing was touched.

"Nothing was touched" means the rejections raised before any write is
attempted: `ErrNotFound` from the ownership pre-check,
and the refusals of an update's body while it is staged
(`ErrRawTooLarge`, `ErrCompressedTooLarge`, an empty upload, a local
staging failure). Excluding them is not an optimisation - it is what stops
an unrelated caller from forcing purges on slugs they do not own: an
update stages its body before it checks ownership, so without the staging
refusals in the list, an empty or oversized update of any slug would spend
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
after an `Update` / `Delete` / `Pin` / `Unpin` that succeeded or may have
(see above), fires `PurgePaste(slug)`. A `SiteCacheInvalidating` decorator
does the same around the site deploy service for `DeployToSlug` and its
`Delete`, by the same rule. The mutation use-cases stay pure; cache
invalidation is a cross-cutting concern layered on by composition, not
woven into the domain logic. `rename` / `versions` / `list` / `whoami` / `get` /
`deleteVersion` are delegated without a purge - they don't change the
bytes served at the public URL (`deleteVersion` is refused outright when
the target is the currently-served version).

The purge is best-effort: a purge error is logged but never fails the
underlying operation (the paste IS updated/deleted on origin; the CDN
just keeps stale content until its TTL ends).

**Purged twice.** A read that started before a write can resolve with the
old state and store that response in the edge cache after the write's
purge went out. The Worker abandons any such store that completes more
than 10 seconds after its read resolved, so a second purge of the slug 15
seconds after its last purge drops whatever raced the first. A
`Repurging` decorator around the CDN adapter, wired at the composition
root, does this for every purge: it purges at once and arms a 15-second
timer per slug, which a further purge of the same slug re-arms, so a burst
of writes costs one delayed purge, timed from the last. The bound this
gives: **a stale edge entry can survive a write by at most about 15
seconds** (plus the purge's own pacing and propagation), not until the
edge TTL ends. Shutdown cancels the timers still armed; a write in its
last 15 seconds before a restart keeps only its first purge.

Three implementations ship:

| Impl | When used | Behavior |
| --- | --- | --- |
| `noop` (default) | No CDN, or one that revalidates every request | No-op |
| `cloudflare` | Cloudflare in front | Queues the slug's hostname and POSTs batches to `/zones/<id>/purge_cache` |
| `fastly` (not shipped, easy add) | Fastly in front | POSTs to Fastly's purge API |

**Purge the slug's whole host.** A slug answers at many cache keys: the
bare URL, `?raw=1` (the bytes a render shell fetches), `?files=1` (a knowledge
base's file list), every file of a site or knowledge base, and any other
query a client sends. The Cloudflare adapter purges them all by naming the
slug's hostname:

```
{"hosts": ["<slug>.<apex>", ...]}
```

so no list of variants has to stay in step with what the shells fetch. A
hostname purge also reaches responses a Worker stored with the Cache API under
that host. In path mode (dev only) a slug has no host of its own, so the
adapter purges the path prefix `<apex>/p/<slug>` instead
(`{"prefixes": [...]}`); slugs have a fixed length, so that prefix names one
slug. A port on the apex is dropped: purges name hosts, not origins. The
scope policy lives in the adapter (which owns the apex and URL-mode config);
the service layer only ever names the slug.

**Coalesced and paced.** Below the enterprise plan, hostname and prefix
purges have tighter API rate limits than URL purges and take at most 30
entries per call. So a purge only queues the slug's hostname (once, however
many writes name it before it is sent), and one loop sends the queue in
calls of at most 30 entries, at most one call every 2 seconds; a call after
an idle spell goes out at once. A 429, a 5xx or a transport failure is
retried with backoff doubling from 1 second (longer when `Retry-After` asks),
for at most 5 calls; a batch still failing, or refused outright (another
4xx), is logged and dropped, bounded by the edge TTL. Every call is logged
with its hosts and its outcome. A write never waits for its purge. At
shutdown the queue is sent within the process's shutdown bound (see
"Process shutdown"); what is left at the deadline is dropped.

Env vars when `HOSTTHIS_CACHE_BACKEND=cloudflare`:

```
HOSTTHIS_CF_PURGE_TOKEN  CF API token, scoped ONLY to 'Cache Purge' on the zone
HOSTTHIS_CF_ZONE_ID      zone id of the apex domain
```

The adapter derives a slug's host (or path prefix) from the apex domain and
URL mode hostthis is already configured with (`HOSTTHIS_APEX_DOMAIN` /
`HOSTTHIS_URL_MODE`), so no separate base-URL var is needed. The zone's plan
must accept purges by hostname (and by prefix, for path mode).

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

Within a paste's own origin, the only policy imposed is
`Content-Security-Policy: form-action 'self'`. JS can do anything any
same-origin script can do: load libraries from any CDN, fetch any HTTPS
endpoint, render WebGL, talk to APIs. What a page cannot do is submit a form
to another origin, which is how a hosted page is turned into a cross-site
request forgery or credential-harvesting relay against a third party. A
`srcdoc` iframe inherits the policy, so a page cannot nest its way around it.

The policy rides every full response of stored bytes (an HTML paste, a site
or knowledge-base file, any `?raw` body) and never a 304: a cache applies a
304's headers to the response it stored, and a client-rendered shell's own,
stricter policy (`form-action 'none'`) must not be replaced by it.

Response headers on paste reads:

- `X-Frame-Options: DENY` - no embedding the paste in iframes elsewhere
  (clickjacking defense)
- `Referrer-Policy: no-referrer` - visiting a paste leaks nothing about
  who sent it
- `Content-Security-Policy: form-action 'self'` on stored bytes, as above
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
- Submit a form to another origin, from the page or a `srcdoc` iframe
  (`form-action 'self'`)

Treat any URL on hostthis.dev as untrusted user content - same as you'd
treat a codepen, a gist, or a github.io page.

### Markdown and diff rendering

Markdown and diff pastes render in the visitor's browser. A read returns
a fixed, content-independent HTML shell that loads a vendored client-side
renderer served from `/_hostthis/...` (no runtime CDN): `marked` plus
`DOMPurify` for Markdown, `diff2html` plus `highlight.js` for diffs. The
shell fetches the raw bytes via `?raw` and renders them; the server never
renders on the read path and streams those bytes with `io.Copy`, so its
memory stays constant regardless of paste size. DOMPurify strips event
handlers, `javascript:` URLs and dangerous tags, so uploaded Markdown
cannot execute JS even though uploaded HTML can. Both shells run under the
same `Content-Security-Policy` (`script-src 'self'`, `connect-src 'self'`,
no inline script). The diff view defaults to line-by-line with a
side-by-side toggle persisted in `localStorage`, is syntax-highlighted,
and follows `prefers-color-scheme`.

### Abuse reporting

Content persists until its owner deletes it, so takedown is an operator
action rather than something the clock does. An operator can delete a
slug's row directly from the metadata store; the next read 404s. That route
deletes no bytes, so the operator also deletes the upload prefixes its versions
record. A user-facing "report this paste" UI is out of
scope for v1.

### Quarantine

Quarantine hides a slug without destroying it, so a suspected abuse page can
come down at once and the decision to delete can wait for a person. It is
reversible: release restores the slug exactly as it was.

While a slug is quarantined:

- every public read answers 404, `?raw=1` and `?files=1` included, and the
  slug's host is purged from the edge cache when it is quarantined and again
  15 seconds later (the "Purged twice" bound);
- the Paste cell's `get` answers 451 `{"error":"quarantined"}`, so every SSH
  verb on it fails with `hostthis: this paste is quarantined by the
  operator` (exit 4) and its owner can neither change nor delete it. 451 is
  not 404 on purpose: a delete that took the slug for absent would heal the
  owner's listing entry away while the row still exists;
- the row, its versions and its bytes stay, and still count against the
  owner's quota.

The quarantine record (`reason`, `actor`, `at`) lives in the Paste cell beside
the row. An operator deletes a quarantined slug with the takedown procedure
above; the cell's `operatorrow` read returns the row regardless of
quarantine for that purpose.

**The Abuse cell.** One `Abuse` cell serializes every quarantine, release and
watcher ban, and keeps the quarantine index (slug, owner, reason, actor, time)
and an audit log of the last 1000 actions, refused ones included.

**Two credentials.** The Worker's internal token reaches everything,
including the operator routes:

| route | body | answer |
| --- | --- | --- |
| `POST /abuse/quarantine` | `{slug, reason}` | 200 `{slug, quarantined, already, purged}`, 404 `no-such-paste` |
| `POST /abuse/release` | `{slug}` | 200 `{slug, released, purged}` |
| `GET /abuse/list` | | `{quarantined: [{slug, owner, reason, actor, at}]}` |
| `GET /abuse/audit` | | `{entries: [...]}`, newest last |

A second secret, `HOSTTHIS_WATCHER_TOKEN`, is accepted on exactly two routes
and nowhere else, for an automated abuse watcher:

| route | body |
| --- | --- |
| `POST /watcher/quarantine` | `{slug, reason}` |
| `POST /watcher/ban-key` | `{identity, reason}` (a `SHA256:` fingerprint, `key:` prefix optional) |

It cannot release, delete, or ban a network. Its actions pass two guards the
operator routes skip:

- **Protected owners.** `HOSTTHIS_PROTECTED_OWNERS` lists owners the watcher
  may never act on: each item a full fingerprint or the first 16 hex digits
  of `sha256("key:<fingerprint>")`. A slug they own, or their key, answers
  403 `protected-owner`.
- **Rate cap.** At most 5 watcher quarantines and 5 watcher bans in any
  rolling hour. Past the cap the route answers 429 `rate-capped` and does
  nothing, so the watcher escalates to a person instead.

Every action, accepted or refused, is appended to the audit log. The purge
uses `HOSTTHIS_CF_PURGE_TOKEN` and `HOSTTHIS_CF_ZONE_ID`; without them the
answer says `purged: false` and the edge keeps what it held until its TTL.

---

## Health endpoints

The HTTP listener serves two process-health endpoints ahead of Host-based routing:

- **`/healthz`** returns `200 ok` whenever the HTTP server responds and echoes
  `X-Backend-Color` when the replica is color-labeled. Kubernetes uses it for
  startup, readiness, and liveness.
- **`/readyz`** is a compatibility endpoint returning `{"ready":true}`. The
  metadata adapter has no application-side activation phase: celld activates
  cells on demand.

Both answer on any Host without authentication. They expose no metadata, storage
counters, or operator controls. The Worker's public router answers both on every
public host with the same bodies and headers (it has no color to echo); its
internal router keeps its own `/healthz`.

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
--data-dir               / HOSTTHIS_DATA_DIR                where the ssh host key lives            (./data)
--landing                / HOSTTHIS_LANDING                 path to landing.html                    (web/landing.html)
--fresh-keys-per-subnet  / HOSTTHIS_FRESH_KEYS_PER_SUBNET   sybil-gate threshold                    (20)
--fresh-keys-window      / HOSTTHIS_FRESH_KEYS_WINDOW       sybil-gate rolling window               (24h)

# Storage (metadata and blobs)
                         / HOSTTHIS_WORKER_ENDPOINT          Worker base URL                  (required)
                         / HOSTTHIS_WORKER_TOKEN             bearer token for Worker calls          (unset)

# Limits
                         / HOSTTHIS_CREATE_ADMISSION_WIDTH  same-identity create admission width    (2)
                         / HOSTTHIS_WORKER_PUT_BUDGET_BYTES  in-flight Worker write bytes per process (16777216)
                         / HOSTTHIS_WORKER_PUT_WAIT          max wait for Worker write admission      (30s)
                         / HOSTTHIS_UPLOAD_CONCURRENCY      concurrent uploads per process          (8)
                         / HOSTTHIS_UPLOAD_WAIT             max wait for an upload slot              (30s)

# CDN / cache purger
                         / HOSTTHIS_CACHE_BACKEND           noop | cloudflare                       (noop)
                         / HOSTTHIS_CF_PURGE_TOKEN          CF token (Cache:Purge scope only)       (required if cloudflare)
                         / HOSTTHIS_CF_ZONE_ID              CF zone id for the apex                 (required if cloudflare)
```

The runtime container reads the same env vars. The operator supplies
a docker-compose (or equivalent) file out of band; this repo ships no
sample production compose.

The Worker reads its own variables (wrangler `vars` and secrets, or the
runtime's equivalent), separate from `hostthisd`'s:

```
HOSTTHIS_APEX_DOMAIN   public apex; turns on the public/internal host split     (unset: off)
HOSTTHIS_WORKER_TOKEN   bearer token for the internal routes; required with an apex   (unset)
HOSTTHIS_URL_MODE      path also serves /p/<slug> on the apex host (dev only)   (subdomain)
ASSETS                 static assets binding for /_hostthis/; absent serves them from PAYLOADS
HOSTTHIS_EDGE_CACHE_TTL_S      edge TTL of cached read responses; 0 is off      (0)
HOSTTHIS_EDGE_CACHE_MAX_BYTES  largest read response the edge cache stores      (8388608)
HOSTTHIS_BLOB_CACHE_MAX_BYTES  largest decoded payload cached; 0 is off         (0)
```

See "Public and internal hosts". A deployment runs the bundled Worker,
`worker/dist/index.js`: run `make bundle` before `celld deploy` or
`wrangler deploy`.

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
SSH can no longer start one. CDN purges still queued are sent last, since
the drain before them can still queue some. The whole sequence has a
35-second hard bound. Work still blocked at that point is left to the
documented pending-state and startup recovery protocols (a purge still queued
is dropped, bounded by the edge TTL). The 20-second and 35-second limits are
product constants, not operator knobs.

### What's hardcoded

Everything not in the table above is a product opinion, not a knob: the
per-paste cap (10 MiB compressed), the per-identity quota (1 GiB
compressed), the raw-input fast-fail (100 MiB, prevents unbounded reads),
blob compression (zstd level 3, all blobs), the sandbox headers
(X-Frame-Options, Referrer-Policy, Permissions-Policy), and the slug
alphabet (`abcdefghijkmnpqrstuvwxyz23456789`). There is no service-wide
total-bytes cap; space returns as owners delete content. Per-IP rate
limiting on top of the Sybil gate is a reverse-proxy concern.

---

## Future directions (proposed, not built)

What remains proposed is an end-user AUTH model layered on top of Rooms
(see "Rooms (app persistence)" for the shipped, capability-only tier):
verifiable identity per app user, so an app can enforce "user A cannot
overwrite user B's record," which the Rooms tier deliberately does not.
The throughline holds: the metadata plane is the persistence layer, and
the differentiator is the SSH-native, no-account, your-key-is-your-identity
model. This revisits the "Comments / threaded discussion" non-goal:
hostthis would not build comments, but a USER could.

### End-user auth on rooms

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

**Open design questions:** the rule model (identity + ownership
constraints evaluated server-side); the JWT-verifying resource-server
design, the turnkey-vs-BYO issuer config and the browser-keypair signature
path; and who pays for a turnkey "Sign in with hostthis" tier (the
ecosystem-vs-host product call).

---

## Non-goals (explicitly out of v1 scope)

These are interesting but expand the product beyond "host renderable
content." Keep the surface small.

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
- **MCP server, or a separate `/llms.txt`**. The apex landing page is
  already terse, factual, and curl-able by any LLM; it IS the programmatic
  reference, and a second machine-doc surface would duplicate it and drift.
- **GitHub (or any third-party) account linking / OAuth**. ssh keys
  alone carry identity; we don't need a second source of trust.
- **Operator-configurable per-paste / per-identity caps**. Hardcoded
  product opinions; see "Self-hosting" for what is a knob.

If real demand surfaces for any of these later, they can be added
without breaking v1 semantics. These are explicit no's, not oversights.
