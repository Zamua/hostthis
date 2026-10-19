# HOSTTHIS(1)

## NAME

hostthis - pipe a file in, get a public URL out

## SYNOPSIS

```
cat <file> | ssh -T hostthis.dev [<slug>] [-- <opts>]
tar czf - <dir> | ssh -T hostthis.dev [<slug>]
ssh hostthis.dev <command> [<args>]
```

## DESCRIPTION

Renders HTML, Markdown, diffs, Mermaid, PDF, CSV, JSON, folded stacks (flame
graph), NDJSON logs and plain text at a random subdomain. A gzip-tar is a
static site. No signup: your ssh key is your identity, and only it can change
what it uploaded. Everything persists until you delete it.

## UPLOAD

```
cat <file> | ssh -T hostthis.dev           new paste; URL on stdout, QR on stderr
cat <file> | ssh -T hostthis.dev <slug>    new version; same URL
-- --name "<label>"                        set a label
-- --type <kind>                           force the renderer: html, markdown, diff,
                                           mermaid, pdf, csv, json, flamegraph, log, text
```

`-T` silences ssh's pseudo-terminal warning.

## COMMANDS

Each runs as `ssh hostthis.dev <command>`.

```
list [-o json]               your pastes, newest first
whoami [-o json]             identity, count, quota
get <slug>                   content to stdout
url <slug>                   the URL
qr <slug>                    the URL and a QR code
rename <slug> [<label>]      set the label; none clears it
versions <slug> [-o json]    version history
pin <slug> <ver>             serve <ver> until unpinned
unpin <slug>                 serve the latest version
delete <slug>                delete everything
delete <slug> <ver>          delete one version's bytes
have <slug> [-o json]        a site's files as a keep list (see SITES)
```

## LINKS

```
<url>?raw                    source bytes
#<heading> #page=3 #row=42   markdown, pdf, csv
#L42-L48 #F1L42-L48          text and log lines, diff lines of file 1
#focus=<stack> #q=<term>     flame graph zoom, highlight
```

## SITES

```
tar czf - <dir> | ssh -T hostthis.dev [<slug>]   deploy, or redeploy in place
routing                      a missing path serves index.html
layout                       one top-level directory is flattened
no index.html                browsable knowledge base
redeploy                     stores only changed files
.hostthis-keep               lines from `have` for files not sent;
                             base must still be latest
```

To send only what changed since the latest version:

```
ssh hostthis.dev have abc12345 > held
(cd dist && find . -type f | sed 's|^\./||' | sort | xargs sha256sum) > local
{ grep '^base ' held; grep -Fxf local held; } > dist/.hostthis-keep
grep -vFxf held local | cut -c67- | tar czf - -C dist -T - .hostthis-keep | ssh -T hostthis.dev abc12345
```

## PERSISTENCE

Key-value rooms on the page's own origin. The room id is the only key.

```
POST   /api/rooms             new room: {"id": "<uuid>"}
GET    /api/rooms/<id>        all keys as one JSON object
GET    /api/rooms/<id>/<key>  one value, raw bytes
PUT    /api/rooms/<id>/<key>  write; body is the value
DELETE /api/rooms/<id>/<key>  remove
```

Values are bytes; JSON values nest as JSON in whole-room reads. 256 KiB and
256 keys per room, 64 MiB per app.

## WEBSOCKETS

```
GET /api/rooms/<id>/ws       a snapshot, then every put and delete
your frames                  relayed to other clients, not stored
{"type":"ping"}              every 20 s; answered with pong
close 4001                   dropped for a newcomer after 2 min without
                             ping; reconnect
```

64 sockets per room; frames up to 32 KiB.

## PUSH

```
GET        /api/push/key                       VAPID public key
PUT|DELETE /api/rooms/<id>/push/subscriptions  add or remove a PushSubscription
PUT        /api/rooms/<id>/push/schedule       {"tz", "items": [{"id", "at": "HH:MM",
                                               "days", "title", "body" | "bodyKey",
                                               "url", "tag"}]}
POST       /api/rooms/<id>/push/test           send now
```

`"when"` (RFC 3339) fires once instead of at/days. `bodyKey` reads a room key
at send time. 16 devices and 16 items per room, 8 sends per device per day,
one test a minute. `push` and `push/*` are reserved keys.

## LIMITS

```
quota    1 GiB per key, after compression
paste    10 MiB after compression
site     1 GiB unpacked
```

## EXIT STATUS

```
0  ok              3  no ssh key
1  error           4  not found
2  usage           6  rate limited
```

## ENVIRONMENT

`NO_COLOR` or `TERM=dumb` disables color.

## LICENSE

MIT - see [LICENSE](LICENSE).
