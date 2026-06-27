# HOSTTHIS(1)

## NAME

hostthis - pipe a rendered file in, get a public URL out

## SYNOPSIS

```
cat <file>      | ssh hostthis.dev
cat <file>      | ssh hostthis.dev <slug>
tar czf - <dir> | ssh hostthis.dev [<slug>]
ssh hostthis.dev <command> [<args>]
```

## DESCRIPTION

Publishes HTML, Markdown, or a unified diff for 30 days at a random
subdomain. One ssh pipe, no signup, no install. Identity is your ssh
public key: anyone with a different key can read the URL but cannot
update, rename, pin, or delete the paste.

A Markdown paste renders in the browser; so does a diff (line-by-line
or side-by-side, with syntax highlighting). Append `?raw` to any
rendered paste's URL for the raw source.

## COMMANDS

<dl>

<dt><code>cat <em>file</em> | ssh hostthis.dev</code></dt>
<dd>upload a paste. To set a label or force the content type, pass
<code>--name "label"</code> or <code>--type html|markdown|diff</code> after
a literal <code>--</code>. ssh otherwise parses a leading <code>--name</code>
as one of its own options.</dd>

<dt><code>cat <em>file</em> | ssh hostthis.dev <em>slug</em></code></dt>
<dd>replace <em>slug</em>'s content; resets the 30-day clock</dd>

<dt><code>ssh hostthis.dev list</code></dt>
<dd>active pastes, soonest to expire first</dd>

<dt><code>ssh hostthis.dev get <em>slug</em></code></dt>
<dd>print content to stdout</dd>

<dt><code>ssh hostthis.dev rename <em>slug</em> [<em>label</em>]</code></dt>
<dd>set the owner label from the remaining words; omit them to clear it</dd>

<dt><code>ssh hostthis.dev versions <em>slug</em></code></dt>
<dd>list versions</dd>

<dt><code>ssh hostthis.dev pin <em>slug</em> <em>ver</em></code></dt>
<dd>stick the URL to <em>ver</em>; survives future updates</dd>

<dt><code>ssh hostthis.dev unpin <em>slug</em></code></dt>
<dd>clear the pin; URL serves the latest version</dd>

<dt><code>ssh hostthis.dev delete <em>slug</em></code></dt>
<dd>wipe the entire paste; permanent</dd>

<dt><code>ssh hostthis.dev delete <em>slug</em> <em>ver</em></code></dt>
<dd>free one version's bytes; keeps the history row as a tombstone</dd>

<dt><code>ssh hostthis.dev whoami</code></dt>
<dd>identity, active count, and quota usage</dd>

</dl>

## STATIC SITES

Pipe a gzip-tar instead of a single file to deploy a multi-file static
site.

```
tar czf - site/ | ssh hostthis.dev            # deploy, get a URL
tar czf - site/ | ssh hostthis.dev abc12345   # re-deploy in place
```

Served at `<slug>.hostthis.dev/<path>`, with content type by file
extension. A request that matches no file serves `index.html`, so
single-page apps route client-side. A single leading directory is
flattened, so `tar czf - site/` serves at the root. macOS sidecar files
`._*`, `.DS_Store`, and `__MACOSX/` are skipped. Delete a site with
`delete <slug>`, the same as a paste.

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

A value is opaque bytes. The whole-room read and the `snapshot`/`put`
frames embed each value as JSON when it parses as JSON - a value you PUT
as a JSON object comes back as a nested object, not a quoted string - and
as a JSON string otherwise. Reading a single key returns the raw bytes.

Limits: 256 KiB and 256 keys per room; 64 MiB per app; a room expires 30
days after its last write. Deployments without a room store return 404.

## LIMITS

10 MiB per identity, counting post-compression bytes across every
active version of every active paste. Text compresses 5-10x under
zstd, so the real raw-payload ceiling is typically 50-100 MiB.

Pastes are HTML, Markdown, or a unified diff; sites are a gzip-tar
archive. 30-day retention from the last update.

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
# upload, get a URL on stdout
cat index.html | ssh hostthis.dev

# upload with a label; --name and --type follow a literal --
cat notes.md | ssh hostthis.dev -- --name "alpha notes"

# update an existing paste; same URL, bumps to v2, v3, ...
cat v2.html | ssh hostthis.dev abc12345

# force content type when sniffing gets it wrong
cat tricky.html | ssh hostthis.dev -- --type html

# render a unified diff (auto-detected, or force it with --type diff)
git diff | ssh hostthis.dev
cat review.patch | ssh hostthis.dev -- --type diff

# read your content back
ssh hostthis.dev get abc12345

# set the owner label from the words, or omit it to clear
ssh hostthis.dev rename abc12345 design notes v2
ssh hostthis.dev rename abc12345

# stick the URL on v1 even though v3 is the latest
ssh hostthis.dev pin abc12345 1
ssh hostthis.dev unpin abc12345

# free one version's bytes; leaves a tombstone row
ssh hostthis.dev delete abc12345 2

# deploy a static site
tar czf - site/ | ssh hostthis.dev
```

## SEE ALSO

Source: [github.com/Zamua/hostthis](https://github.com/Zamua/hostthis)

## LICENSE

MIT - see [LICENSE](LICENSE).
