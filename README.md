# HOSTTHIS(1)

## Name

**hostthis** - pipe a rendered file in, get a public URL out. No signup, no app.

## Synopsis

```
cat <file> | ssh hostthis.dev [--name "label"] [--type html|markdown]
cat <file> | ssh hostthis.dev <slug>            # update an existing paste
ssh hostthis.dev <command> [args ...]
ssh hostthis.dev                                 # show help
```

## Description

Publishes HTML or Markdown for 7 days at a random subdomain. One ssh
pipe, no signup, no install. Useful when you want a shareable URL
for a one-off HTML mock, a Markdown writeup, or anything you need a
teammate or LLM to load in a browser without spinning up a deploy.
Identity is your ssh public key: anyone with a different key can
read the URL but can't update, rename, pin, or delete the paste.

## Commands

The first non-flag argument decides which verb runs. Anything that
parses as a slug (8 chars from `abcdefghijkmnpqrstuvwxyz23456789`)
on stdin's path acts as an `update`; anything else is treated as a
named verb.

### Upload

```
cat foo.html | ssh hostthis.dev
cat foo.html | ssh hostthis.dev --name "prototype v3"
cat foo.html | ssh hostthis.dev --type html
```

Reads stdin, sniffs content type, prints the URL on stdout (one line,
no trailing whitespace - pipes Just Work). Owner-only metadata
(the name label) goes on stderr.

The optional `--type` flag overrides content sniffing - useful when
you're piping something like a Jinja template that confuses sniffers.

### Update an existing paste

```
cat foo-v2.html | ssh hostthis.dev <slug>
```

Adds a new version. Resets the 7-day expiry clock. If the paste is
unpinned (default), the URL immediately serves the new version; if
pinned, the new version is recorded but not served until you `unpin`
or `pin` to it explicitly.

### List your active pastes

```
ssh hostthis.dev list
```

Tab-separated rows, soonest to expire first:
```
SLUG       NAME              SIZE   KIND   EXPIRES_IN  VERS
abc12345   prototype v3      1.2k   html   6d 23h      v2
x7y8z9q0   -                 540B   html   6d 16h      v3 (pinned, latest v5)
```

The `VERS` column shows the served version + pin state.

### Read content back

```
ssh hostthis.dev show <slug>
```

Streams the served version's bytes to stdout. Same content the URL
would render, minus the HTML wrapper. Owner only.

### Rename

```
ssh hostthis.dev rename <slug> "new label"
ssh hostthis.dev rename <slug> ""           # clear label
```

The label is owner-only metadata visible in `list`. It never appears
in the URL.

### Version history

```
ssh hostthis.dev versions <slug>
```

Output:
```
v4   current   2026-06-05 15:01 UTC   1.4k
v3             2026-06-05 14:32 UTC   1.2k
v2   deleted   2026-06-05 12:15 UTC   -
v1             2026-06-05 11:22 UTC   0.9k
```

- `current` - the version the URL is serving right now.
- `deleted` - bytes freed via `delete <slug> <ver>`; row stays as
  a tombstone so the version number doesn't get reused.

### Pin / unpin

```
ssh hostthis.dev pin   <slug> 1     # always serve v1
ssh hostthis.dev pin   <slug> 3     # switch the pin
ssh hostthis.dev unpin <slug>       # URL follows the latest version again
```

Pinning lets you publish updates without changing what the URL
serves. Useful when you want to keep a known-good version stable
while iterating on the next one.

Pinning does NOT reset the 7-day clock - only `update` does that.

### Delete

```
ssh hostthis.dev delete <slug>             # wipe the entire paste
ssh hostthis.dev delete <slug> <ver>       # tombstone just that version
```

Whole-paste delete is permanent - no undo, the slug becomes
available for future random generation.

Per-version delete frees the blob bytes (so they don't count against
your quota anymore) but keeps the metadata row so the version
history stays intact. Refused for the version the URL is currently
serving (pin a different version first if you want to delete the
current one).

### Identity

```
ssh hostthis.dev whoami
```

Prints your ssh key fingerprint + how many active pastes you own.
Sessions without an ssh key are rejected at session startup with
"ssh key required" on stderr - anonymous uploads aren't allowed.

### Help

```
ssh hostthis.dev help
```

Or run `ssh hostthis.dev` with no command and no piped input.

## Examples

```sh
# share an HTML demo, get a URL
cat index.html | ssh hostthis.dev
# → https://7gh3kp29.hostthis.dev

# share a rendered version of your Markdown notes
cat README.md | ssh hostthis.dev --name "alpha notes"

# update in place (same URL, new bytes)
cat index-v2.html | ssh hostthis.dev 7gh3kp29

# look at all your pastes
ssh hostthis.dev list

# look at a paste's version history
ssh hostthis.dev versions 7gh3kp29

# go back to v1 even though v3 is the latest
ssh hostthis.dev pin 7gh3kp29 1

# you typed the wrong file in v2; free those bytes (keep history)
ssh hostthis.dev delete 7gh3kp29 2

# done with it
ssh hostthis.dev delete 7gh3kp29
```

## Limits

- **10 MiB per identity** total, counting post-compression bytes
  across every active version of every active paste. HTML/Markdown
  compresses 5-10x under zstd, so the real ceiling on raw payload is
  much higher (typically 50-100 MiB of text).
- **Per-paste cap = identity cap (10 MiB compressed)**. Same number;
  a user can spend their quota as one big paste or many small ones.
- **7-day retention** from the last `update`. Nothing renews
  automatically - re-pipe the content (or `update` it) to extend.
- **Content types**: HTML and Markdown only. Binaries / images /
  zips are rejected at upload.
- **Per-network rate limit on fresh keys**: 20 new ssh keys per
  `/24` IP subnet per 24h. Stops trivial sybil churn; legitimate
  users hitting this should reuse an existing key.

## Errors

The CLI exits with conventional codes:

- `0` - success
- `1` - service error (quota exhausted, paste size cap exceeded,
  service at storage capacity, unsupported content type)
- `2` - usage error (bad args, malformed flag)
- `3` - auth required (no ssh key offered)
- `4` - not found / not owner (deliberately indistinguishable so
  non-owners can't probe slug existence)
- `6` - sybil gate (too many new keys from this network today)

## Self-hosting

`hostthis` is a single Go binary that runs ssh + http servers on
configurable ports, against either a local disk blob store or any
S3-compatible object store (MinIO, R2, S3). See `docs/SPEC.md` for
the protocol, `cmd/hostthisd/main.go` for the env-var-driven config
surface, and `deploy/vps/` for a working docker compose example.

The project is open source under MIT. Patches welcome.

## License

MIT - see [LICENSE](LICENSE).
