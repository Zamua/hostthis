# HOSTTHIS(1)

## NAME

hostthis - pipe a rendered file in, get a public URL out

## SYNOPSIS

```
cat <file>      | ssh -T hostthis.dev
cat <file>      | ssh -T hostthis.dev <slug>
tar czf - <dir> | ssh -T hostthis.dev [<slug>]
ssh hostthis.dev <command> [<args>]
```

`-T` silences ssh's pseudo-terminal warning on piped uploads; it changes
nothing on the server.

## DESCRIPTION

Publishes HTML, Markdown, a unified diff, a Mermaid diagram, a PDF, a CSV
or JSON document, a profile as an interactive flame graph, structured
logs, or plain text with linkable lines, at a random subdomain. One
ssh pipe, no signup, no install. Identity is your ssh
public key: anyone with a different key can read the URL but cannot
update, rename, pin, or delete the paste.

Append `?raw` to a rendered paste's URL for its raw source.

## COMMANDS

<dl>

<dt><code>cat <em>file</em> | ssh -T hostthis.dev</code></dt>
<dd>upload a paste. The URL prints on stdout; a QR code of it prints on
stderr (drop it with <code>2&gt;/dev/null</code>). To set a label or force
the content type, pass <code>--name "label"</code> or
<code>--type html|markdown|diff|mermaid|pdf|csv|json|flamegraph|log|text</code> after a literal <code>--</code>. ssh
otherwise parses a leading <code>--name</code> as one of its own options.</dd>

<dt><code>cat <em>file</em> | ssh -T hostthis.dev <em>slug</em></code></dt>
<dd>replace <em>slug</em>'s content; the URL stays the same</dd>

<dt><code>ssh hostthis.dev list [-o json]</code></dt>
<dd>your pastes, most recently updated first</dd>

<dt><code>ssh hostthis.dev get <em>slug</em></code></dt>
<dd>print content to stdout</dd>

<dt><code>ssh hostthis.dev url <em>slug</em></code></dt>
<dd>re-show just the URL for any existing paste or site (no ownership
check; not found on an unknown slug)</dd>

<dt><code>ssh hostthis.dev qr <em>slug</em></code></dt>
<dd>re-show the URL (stdout) and a QR code (stderr) for any existing
paste or site</dd>

<dt><code>ssh hostthis.dev rename <em>slug</em> [<em>label</em>]</code></dt>
<dd>set the owner label from the remaining words; omit them to clear it</dd>

<dt><code>ssh hostthis.dev versions <em>slug</em> [-o json]</code></dt>
<dd>list versions</dd>

<dt><code>ssh hostthis.dev have <em>slug</em> [-o json]</code></dt>
<dd>a site's latest version as a keep list: <code>base <em>n</em></code>, then
<code><em>sha256</em>  <em>path</em></code> per file</dd>

<dt><code>ssh hostthis.dev pin <em>slug</em> <em>ver</em></code></dt>
<dd>stick the URL to <em>ver</em>; survives future updates</dd>

<dt><code>ssh hostthis.dev unpin <em>slug</em></code></dt>
<dd>clear the pin; URL serves the latest version</dd>

<dt><code>ssh hostthis.dev delete <em>slug</em></code></dt>
<dd>wipe the entire paste; permanent</dd>

<dt><code>ssh hostthis.dev delete <em>slug</em> <em>ver</em></code></dt>
<dd>free one version's bytes; keeps the history row as a tombstone</dd>

<dt><code>ssh hostthis.dev whoami [-o json]</code></dt>
<dd>identity, active count, and quota usage</dd>

</dl>

## STATIC SITES

Pipe a gzip-tar instead of a single file to deploy a multi-file static
site.

```
tar czf - site/ | ssh -T hostthis.dev            # deploy, get a URL
tar czf - site/ | ssh -T hostthis.dev abc12345   # re-deploy in place
```

Served at `<slug>.hostthis.dev/<path>`, content type by extension. A
request that matches no file serves `index.html`, so single-page apps route
client-side. A single leading directory is flattened. macOS sidecar files are
skipped.

A redeploy stores only files that changed. To skip sending unchanged ones,
put their `have` lines in `.hostthis-keep` at the archive root and send the
rest; the deploy is refused if the site moved past `base` meanwhile.

An archive with no root `index.html` deploys as a **knowledge base**: the URL
browses it as documents, with folders, a table of contents and search.

## ROOMS API

A deployed site or paste can store and sync state in a room, with no
backend of your own. The room's UUID is the only key. The API is served
on the app's own origin.

Durable key-value over HTTP:

```
POST   /api/rooms                -> {"id":"<uuid>"}    mint a room
GET    /api/rooms/<uuid>         -> {key: value, ...}  read the whole room
GET    /api/rooms/<uuid>/<key>   -> value              read one key
PUT    /api/rooms/<uuid>/<key>     write one key, body is the value
DELETE /api/rooms/<uuid>/<key>     delete one key
```

Realtime over WebSocket, on the same origin:

```
GET /api/rooms/<uuid>/ws
```

On connect you receive a `snapshot` of the room, then a live stream:
every durable `PUT`/`DELETE` is mirrored to all clients as
`{"type":"put"|"delete", ...}`. Frames you send are broadcast verbatim
to other clients and never stored, for ephemeral motion.

Send `{"type":"ping"}` every 20 seconds: the server answers
`{"type":"pong"}` and passes it to no one. A room holds 64 sockets. When it
is full, a joining client takes the place of any socket that has pinged but
not in the last two minutes, which is closed with code 4001 (`stale`);
reconnect when you see it. A frame you send over 32 KiB closes your socket.

A value is opaque bytes. The whole-room read and the `snapshot`/`put`
frames embed each value as JSON when it parses as JSON - a value you PUT
as a JSON object comes back as a nested object, not a quoted string - and
as a JSON string otherwise. The read and the frames carry the same text
for a value, byte for byte (`1e400` stays `1e400`). Reading a single key
returns the raw bytes.

Limits: 256 KiB and 256 keys per room; 64 MiB per app. Deployments without
a room store return 404.

Scheduled Web Push, per room, on the same origin:

```
GET    /api/push/key                          -> {"key": "<VAPID public key>"}
PUT    /api/rooms/<uuid>/push/subscriptions     body: PushSubscription JSON
DELETE /api/rooms/<uuid>/push/subscriptions     body: {"endpoint": "..."}
GET    /api/rooms/<uuid>/push/subscriptions   -> [{"endpoint", "added"}]
PUT    /api/rooms/<uuid>/push/schedule          body: Schedule
GET    /api/rooms/<uuid>/push/schedule        -> Schedule
POST   /api/rooms/<uuid>/push/test            -> {"sent", "pruned"}  send now
```

Schedule:

```
{ "tz": "<IANA zone>",
  "items": [ { "id", "at": "HH:MM", "days": [0..6], "title",
               "body" | "bodyKey", "url", "tag" } ] }
```

`at`/`days` fire weekly at local time in `tz` (0 = Sunday); `when` (RFC 3339)
fires once instead. `bodyKey` names a room key read at send time, `{date}`
replaced by the local date; a missing key sends nothing. Payload:
`{"title","body","url","tag"}`. 404/410 endpoints are dropped.

Limits: 16 subscriptions, 16 items per room; 2 KiB payload; 8 sends per
device per day; one test per minute. `push` and `push/...` are reserved keys.

## LIMITS

1 GiB per identity, counting post-compression bytes across every
active version of every active paste and site. Text compresses 5-10x
under zstd, so text can use several GiB of raw payload before the cap.

Pastes are HTML, Markdown, a unified diff, a Mermaid diagram, a PDF, a CSV
or JSON document, folded stacks as a flame graph, NDJSON logs, or plain
text; sites are a gzip-tar archive. Pastes and sites persist indefinitely.

## EXIT STATUS

```
0   success
1   generic failure
2   usage error: bad arguments or unknown verb
3   identity required: no ssh key presented
4   not found, or not yours
6   refused by the per-subnet rate limit
```

## ENVIRONMENT

```
NO_COLOR    set to any value to disable color output
TERM=dumb   also disables color output
```

Read from the environment your ssh client forwards.

## EXAMPLES

```
# capture just the URL; 2>/dev/null drops the QR + narration
url=$(cat index.html | ssh -T hostthis.dev 2>/dev/null)

# upload with a label; --name and --type follow a literal --
cat notes.md | ssh -T hostthis.dev -- --name "alpha notes"

# force content type when sniffing gets it wrong
cat tricky.html | ssh -T hostthis.dev -- --type html

# every format is auto-detected
git diff | ssh -T hostthis.dev           # a diff
cat cpu.folded | ssh -T hostthis.dev     # folded stacks -> flame graph
cat app.ndjson | ssh -T hostthis.dev     # NDJSON logs -> log viewer

# link to a place inside a paste
#   <url>#some-heading   <url>#page=3   <url>#row=42   <url>#F1L42-L48

# redeploy a site, sending only what changed since the latest version
ssh hostthis.dev have abc12345 > held
(cd dist && find . -type f | sed 's|^\./||' | sort | xargs sha256sum) > local
{ grep '^base ' held; grep -Fxf local held; } > dist/.hostthis-keep
grep -vFxf held local | cut -c67- | tar czf - -C dist -T - .hostthis-keep | ssh -T hostthis.dev abc12345
```

## SEE ALSO

Source: [github.com/Zamua/hostthis](https://github.com/Zamua/hostthis)

## LICENSE

MIT - see [LICENSE](LICENSE).
