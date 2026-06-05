# hostthis — spec

A self-hostable, dev-first paste service for content that *needs rendering
to be shareable* — HTML, Markdown, and a small future set of rendered
formats. Pipe a file to ssh, get back a URL. No signup, no UI, no CLI
to install. Your existing ssh key is the account.

```
$ cat index.html | ssh hostthis.dev
https://abc12345.hostthis.dev
```

This document defines the v1 surface. Anything not in it is intentionally
out of scope for v1 — see "Non-goals" at the bottom.

---

## What it is, what it isn't

- It IS: a hosting target for **content that needs rendering to share
  well** — HTML pages, Markdown docs — addressable by URL, with ssh-pipe
  as the primary upload mechanism.
- It IS: dev-first. The mental model is `git push` for documents — your ssh
  key is your identity, every operation is one line in a terminal.
- It IS NOT: a general file host. ZIPs, binaries, photos, videos belong
  elsewhere.
- It IS NOT: a comment/collaboration platform.
- It IS NOT: a transient blob host for opaque bytes.

## Supported formats

**v1**: HTML, Markdown.

Detection: by content type sniffed from the first 512 bytes plus optional
explicit `--type` flag at upload time. Markdown is rendered to HTML
server-side at request time (no client JS required); the rendered HTML
follows the same sandboxing rules as user-supplied HTML.

**Future** (post-v1, behind a feature flag):
- Mermaid diagrams (render server-side to SVG / PNG)
- Maybe: PlantUML, GraphViz / DOT, ASCII tables

Uploads of unsupported types are **rejected** with a clear error pointing
at what we accept:

```
$ cat photo.jpg | ssh hostthis.dev
error: hostthis only accepts content that needs rendering (html, markdown).
```

This is deliberate scope: every accepted format expands the surface for
abuse + sandboxing edge cases. Stay narrow.

---

## URL shape

Each paste lives at its own subdomain on the apex:

- `https://abc12345.hostthis.dev` — anon upload, random 8-char slug
  (alphabet: `abcdefghijkmnpqrstuvwxyz23456789` — lowercase, no ambiguous chars)
- `https://abc12345.hostthis.dev?k=<token>` — signed share link for an
  unpublished paste

Apex `https://hostthis.dev` is the homepage / docs / dashboard. Never serves
user content.

**Subdomain-per-paste, not path-per-paste**, because:
- Each paste gets its own origin — cookies, JS, CSP can't reach apex or
  other pastes. Standard sandbox pattern for multi-tenant content hosts.
- Reads cleaner in chats ("check this out: `acme-demo.hostthis.dev`").
- Shorter total URL than `hostthis.dev/p/abc12345`.

Wildcard cert covers `*.hostthis.dev` via Let's Encrypt DNS-01.

### Reserved subdomains

These cannot be slugs. Slug generation retries until it picks something
not in this set:

```
www, app, api, docs, blog, dashboard, status, admin, mail, support,
billing, signin, signup, signout, login, logout, register, dev, staging,
prod, test, console, hostthis, mcp, help, about
```

### Dev-only path mode

For local development where wildcard DNS + certs are friction (the Mac
mini tailnet case), the binary supports a `--mode path` flag (or
`HOSTTHIS_URL_MODE=path` env). In path mode:

- Pastes live at `<apex>/p/<slug>` instead of `<slug>.<apex>`.
- The SSH server emits the path-shape URL after upload.
- The HTTP router accepts both `<slug>.apex` and `apex/p/<slug>`.
- Storage, validation, rendering, sanitization, and response headers are
  identical to subdomain mode — only the URL emission and routing
  differ at the I/O boundary.

**Path mode is dev-only and breaks the origin-isolation property** —
all pastes share the apex origin, so user-uploaded JS could read apex
cookies or talk to other pastes' state. The binary's startup logs a
loud warning when running in path mode, and any deploy script that
ships a production binary with `--mode path` should be rejected by CI.

The integration test suite always runs against subdomain mode (using
Host-header tagging in the test HTTP client — no DNS needed) so the
origin-isolation guarantees are pinned regardless of dev convenience.

---

## Identity

**SSH key fingerprint IS the account.** No signup form, no email, no
password. First time a key fingerprint connects, the server creates an
account row keyed on the SHA256 fingerprint. Every subsequent connection
from the same key is "the same user".

Two tiers, distinguished only by *what you can do* (not size or
retention — see those sections for fixed numbers):

| Tier | How | Can do |
|---|---|---|
| **anonymous** | no ssh key offered (SSH `none` auth) | upload only |
| **keyed** | any ssh key offered | upload + `list` + `show` + `update` + `delete` + `unpublish`/`publish` + `link`/`unshare` against your own pastes |

Anonymous uploads are owner-less: nobody can `list` or `update` them
once submitted. They live their full retention window and then expire.

There is no "new key cooldown" or trust ramp — the per-class quota
(see "Limits & Sybil defense") already bounds abuse via key rotation,
so we don't add a second mechanism for the same problem.

### Security: snooping a public key gives nothing

SSH auth requires proving you hold the matching **private** key — server
sends a random challenge, client signs with the private key, server
verifies the signature against the public key. So a leaked `id_*.pub` is
harmless. Same model as `git push` or ssh-ing into a Linux box.

Implementation must use `golang.org/x/crypto/ssh`'s `PublicKeyCallback`,
which is invoked AFTER the lib has already cryptographically verified the
signature. Never trust a self-asserted username or fingerprint.

---

## File handling

- **Per-paste hard cap**: 5 MB. Universal across tiers. Sized for the
  worst-case legitimate use (a self-contained HTML page with embedded
  base64 images, or a long Markdown doc). Larger than that and the user
  is sharing the wrong thing through hostthis.
- **Format gate**: accept only supported content types (HTML, Markdown
  in v1). Server sniffs the first 512 bytes for content type via
  `http.DetectContentType` and cross-checks any explicit `--type` flag.
  Unsupported content is rejected with a clear error pointing at
  alternatives — no silent fallback to `attachment` rendering.
- **No streaming reads from upload**: server buffers the whole stdin
  into memory (≤ 5 MB so a fixed buffer is fine) before committing.
- **Storage**: SHA256-keyed content-addressed blobs on disk
  (`data/blobs/<sha256[:2]>/<sha256>`). Multiple slugs pointing to the
  same blob share the storage. Rendered output (Markdown → HTML) is
  cached alongside the source blob, keyed by source SHA + renderer
  version, regenerated on renderer-version bumps.

## Retention

**Every paste lives for 24 hours from its last update**, then it's
deleted (slug, all versions, content blob if unreferenced). No
exceptions, no user-facing control, no operator config.

- Initial upload: 24h clock starts.
- Each `update <slug>` resets the clock to 24h from that moment.
- No `touch` verb, no `--expires` flag. Time-based extension only happens
  as a side effect of actually changing content.

Rationale for short + fixed: hostthis is for *shareable rendered content*
(HTML mockups, Markdown reports, demo prototypes). The use case is
"send this link to a coworker today"; nobody is sending an "open this
link in two months" URL through us. 24h forces the asker to re-host if
they need it again, which catches stale-link rot at the source.

Long-term hosting is a deliberate non-goal — see "Non-goals" at the
bottom. If the use case shows up later, it'll be specced and ADR'd then.

---

## Verbs (the `ssh hostthis.dev <verb>` surface)

Every verb is the first positional argument after the SSH connection.
With no command and no stdin, the server prints the help banner.

### Upload (new)
```
cat index.html | ssh hostthis.dev
https://abc12345.hostthis.dev
expires in 24h (2026-06-06 12:34 UTC)
```
Reads stdin until EOF or 5 MB. Validates content type (HTML or Markdown
in v1). Generates a fresh random slug.

Optional `--name`:
```
cat demo.html | ssh hostthis.dev --name "Acme prototype v3"
https://abc12345.hostthis.dev
"Acme prototype v3" — expires in 24h (2026-06-06 12:34 UTC)
```
The name is owner-only metadata for `list`; it never appears in the
URL. Names are 1–60 chars, any printable Unicode except newlines.
Anonymous uploads ignore `--name` (no identity to attach it to) with
a stderr note.

**stdout vs stderr discipline**: the URL is the *only* thing on stdout —
one line, no trailing whitespace, no formatting — so pipes Just Work:

```
cat foo.html | ssh hostthis.dev | pbcopy   # → URL only on the clipboard
```

Everything else (expiry note, key-onboarding nudge, warnings) prints to
stderr. Pipes lose it, but the user's terminal still renders it because
stderr is a TTY by default.

If the user has no ssh key in their agent, the server still accepts the
upload via SSH's `none` auth method, prints the URL, and appends to
stderr a one-line nudge about adding a key to get `list` / `update` /
`delete` capability.

### Upload (update an existing slug)
```
cat v2.html | ssh hostthis.dev abc12345
https://abc12345.hostthis.dev
v2 — expires in 24h (2026-06-06 14:12 UTC)
```
Slug as positional arg means "update this one". Server checks ownership
against the key fingerprint. Errors:
- `403`: slug exists but you don't own it
- `404`: slug doesn't exist
- `413`: payload too large

Update resets the 24h retention clock to "24h from now" and creates a
new immutable version under the hood (SHA-keyed blob ref). The slug
always serves the currently-pinned version (defaults to latest after
an update).

### List your pastes
```
ssh hostthis.dev list
SLUG       NAME                  SIZE    KIND      UPDATED    EXPIRES IN   VERS
abc12345   Acme prototype v3     1.2k    html      2h ago     22h          v2
x7y8z9q0   —                      540B   markdown  8h ago     16h          v1
mnop4567   Onboarding email      3.8k    html      18h ago    6h           v1
```
Sorted by expiry asc (soonest-to-die first, so you notice things about
to disappear). `NAME` column shows the user-supplied label or `—` if
none. Output is tab-separated for easy `awk`-ing. Anonymous sessions
return empty (no identity).

When a paste is within 1h of expiry, the row's `EXPIRES IN` is rendered
in red (ANSI, only when stderr says we're on a TTY).

### Rename
```
ssh hostthis.dev rename abc12345 "Acme prototype v4"
renamed.
```
Sets / changes the `NAME` for one of your pastes. Pass an empty string
to clear: `ssh hostthis.dev rename abc12345 ""`. Renaming does NOT
reset the expiry clock — purely metadata.

### Show content (read back over ssh)
```
ssh hostthis.dev show abc12345
<the html streams to stdout>
```
Owner-only. Use case: reading an unpublished paste, piping back through
local tooling. Anonymous error: `403`.

### Versions
```
ssh hostthis.dev versions abc12345
v3  current  2026-06-05 14:32  1.2k
v2           2026-06-05 12:15  1.1k
v1           2026-06-05 11:22  0.9k
expires in 22h (2026-06-06 14:32 UTC)
```
The expiry footer is on stderr (same convention as upload), so a script
that wants just the version list can pipe stdout cleanly.

### Pin a version (rollback or roll-forward)
```
ssh hostthis.dev pin abc12345 v1     # roll back
ssh hostthis.dev pin abc12345 v3     # roll forward
```
Sets which version `<slug>.hostthis.dev` serves. Pinning does NOT reset
the expiry clock — only `update` does that. Reads symmetric in either
time direction — "rollback" framing is intentionally avoided.

### Delete (permanent)
```
ssh hostthis.dev delete abc12345
deleted.
```
Wipes the slug record + all versions. Reuses the slug for future random
generation. No undo. No confirm prompt (ssh sessions don't tty cleanly;
the verb is explicit enough).

### Unpublish / publish (visibility toggle)
```
ssh hostthis.dev unpublish abc12345
unpublished. URL now 404s.

ssh hostthis.dev publish abc12345
published. URL serves again.
```
While unpublished, `<slug>.hostthis.dev` returns 404 to everyone. Owner can
still read via `show` over ssh, and can issue signed share links via `link`.

### Signed share links
```
ssh hostthis.dev link abc12345 --expires 24h
https://abc12345.hostthis.dev?k=hX9pQ2...

ssh hostthis.dev unshare abc12345
all signed links revoked.
```
Token is HMAC-bound to your ssh-key fingerprint + slug + version. URL-bearer
auth: anyone with the URL views. Revoke any time with `unshare`.

`--expires`: `Nh` (hours), `Nd` (days), `never`. Default 24h. No upper bound
enforced; operators may config one.

### Identity
```
ssh hostthis.dev whoami
key:     SHA256:abc...xyz
joined:  2025-12-01
uploads: 4 active (24h)
quota:   38 / 200 MB this 30d window
```

### Issue an HTTP API token
```
ssh hostthis.dev token create
htst_live_z9k2q...
```
One-time output (we don't store the raw value, only a hash). Use as
`Authorization: Bearer <token>` against the HTTP surface (see "HTTP /
curl fallback"). `token list` / `token revoke <prefix>` round it out.

### Help
```
ssh hostthis.dev
hostthis.dev — pipe rendered content (html/markdown), get a URL.
              pastes expire 24h after their last update.

  cat file.html | ssh hostthis.dev [--name "…"]      upload
  cat file.html | ssh hostthis.dev <slug>            update an existing upload
  ssh hostthis.dev list                              your active pastes
  ssh hostthis.dev show <slug>                       read content (owner only)
  ssh hostthis.dev rename <slug> "<name>"            set / change a paste's label
  ssh hostthis.dev versions <slug>                   history (within the 24h window)
  ssh hostthis.dev pin <slug> <ver>                  set served version
  ssh hostthis.dev delete <slug>                     permanent
  ssh hostthis.dev unpublish <slug>                  public 404s
  ssh hostthis.dev publish <slug>                    undo unpublish
  ssh hostthis.dev link <slug> --expires …           signed share URL
  ssh hostthis.dev unshare <slug>                    revoke signed links
  ssh hostthis.dev whoami                            your identity + quota
  ssh hostthis.dev token create                      issue an HTTP API token

You: SHA256:abc... (4 active uploads, 38/200 MB this 30d window)
```

---

## Apex landing page

`https://hostthis.dev/` serves one static HTML page. Its job is exactly:
*explain how to use this in 10 seconds*. Not a marketing page, not a
dashboard, not interactive — just the docs the user needs to send their
first paste.

Content (final copy can iterate; this is the substance):

```
hostthis.dev

Pipe a file to ssh, get a URL.

    $ cat index.html | ssh hostthis.dev
    https://abc12345.hostthis.dev

Supported: HTML, Markdown.

Your ssh key is your account. No signup.

    $ ssh hostthis.dev whoami    your identity (or run --help for more)
    $ ssh hostthis.dev list      pastes tied to your key

[full verb list as a small <table> or definition list]

Source: github.com/<org>/hostthis
```

Constraints:
- *Single static HTML file*, served from the binary's embedded assets.
  No JS unless we add a tiny copy-button. No external fonts/CSS/CDN.
  Total weight under 20 KB.
- *No content-rendering surface*. The apex never serves user content;
  reserved subdomains include the apex hostname itself.
- *Style*: monospace by default (it's a terminal-flavored tool). Minimal
  CSS, single accent color. Dark + light via `prefers-color-scheme`.

## HTTP / curl fallback

For environments without ssh (some CI, some sandboxes), the HTTP surface
mirrors the ssh verbs 1:1. **The HTTP surface always requires a token** —
no anonymous curl. Tokens are issued only via the ssh path, which means
quota tracking always has a key fingerprint to attribute uploads to.

Get a token first:

```
$ ssh hostthis.dev token create
htst_live_z9k2q...
```

Then use it:

```
$ curl -H "Authorization: Bearer htst_live_z9k2q..." \
       -H "Content-Type: text/html" \
       --data-binary @index.html https://hostthis.dev/u
https://abc12345.hostthis.dev
```

A request to any HTTP endpoint without a valid `Authorization: Bearer …`
header returns `401`, with a body pointing at `ssh hostthis.dev token create`.

Endpoints (all under `https://hostthis.dev/api/`):
- `POST /api/u[/<slug>]` — upload (new, or update existing)
- `GET  /api/list` — your active pastes
- `GET  /api/show/<slug>` — content (owner only)
- `GET  /api/versions/<slug>` — history
- `POST /api/pin/<slug>/<ver>` — set served version
- `DELETE /api/<slug>` — permanent delete
- `POST /api/unpublish/<slug>` / `POST /api/publish/<slug>`
- `POST /api/link/<slug>` — issue signed share link
- `POST /api/unshare/<slug>` — revoke all signed share links
- `GET  /api/whoami` — identity + quota

---

## Limits & Sybil defense

Layered, defense-in-depth. The headline defense is the **union-quota
model**: per-key caps and per-IP caps aren't independent ceilings (which
would let an attacker rotate one axis to bypass the other) — they share
the same quota via an equivalence-class structure.

### Caps

- **Per-paste hard cap**: 5 MB. Universal. Never raised.
- **Per-quota-class total**: 200 MB rolling 30-day window. Covers normal
  use (LLM artifacts, prototypes); abusers hit the wall fast.
- **Per-quota-class upload rate**: 50 MB per day. Smoothing burst usage.

### The quota class — "union of key and IP"

Every upload is tagged with (`key_fingerprint`, `ip_subnet`). For IPv4,
`ip_subnet` is the `/24`; for IPv6, the `/48`. (Anonymous uploads use
`key_fingerprint = null`.)

Quotas are tracked per **equivalence class**, not per axis. The classes
form via simple union: any two upload identities that share *either* a
key fingerprint *or* an IP subnet land in the same class. Quota usage
sums across all uploads in the class.

Concretely:
- Anon upload from IP `1.2.3.0/24` → class `{(null, 1.2.3.0/24)}`.
- A keyed upload from the same subnet → joins the same class
  (`(K1, 1.2.3.0/24)` shares subnet with `(null, 1.2.3.0/24)`).
- That same key from a different subnet `5.6.7.0/24` → joins the same
  class (shares key `K1`).
- A new key from `5.6.7.0/24` → joins the same class (shares subnet).

To escape the class, an attacker must rotate **both** the key fingerprint
**and** the IP subnet *simultaneously*. Rotating only one keeps them
trapped in the existing class with its accumulated quota.

Implementation: a union-find structure keyed on (key, subnet) tuples,
persisted in sqlite. Each upload reads the tuple, finds (or creates) its
class, checks the class's rolling quota, and accepts or rejects.

### Edge case: household merging

Legitimate users on shared networks (family, coworking spaces, dorms)
will see their classes merge across people:

- Roommate A uploads with key K_A from home IP `H/24`.
- Roommate B uploads with key K_B from same `H/24` → joins A's class.
- Now both share a 200 MB / 30d window even though they're different
  people.

This is intentional cost for the Sybil defense. We accept the false-positive
merging because the alternative (independent per-key + per-IP caps with
OR-rejection) lets a determined abuser stay below both ceilings forever
by playing them off each other. If household merging becomes a real
complaint, raise the per-class cap (still keep the union structure) before
splitting axes.

### Other defenses

1. **New keys start at anon tier**: 7-day cooldown before establishing.
   Defeats `ssh-keygen ∞ times` for instant-trust abuse. A class can
   contain a mix of established and new keys; the *behavior* tier
   (ephemeral default, no long-term option) is per-key, but the *quota*
   is per-class.
2. **Per-paste hard cap**: never exceeded regardless of trust. Worst case
   is bounded.
3. **Retention TTL backstop**: even long-term pastes flip to ephemeral
   after 365 days of owner-silence. Caps storage explosion from forgotten
   accounts.
4. **GitHub verify** (optional uplift): doesn't add a hard defense but
   raises the cost of generating a fresh trust-signal for an attacker
   (they need a fresh GitHub account too).
5. **Per-class abuse signal**: if any paste in the class is flagged for
   abuse (DMCA, malware, phishing), the class is throttled or banned —
   the merging that hurts legit households also hurts attackers who
   share infrastructure with each other.

### Self-hosted relaxation

For deploys on trusted networks (friends-only, VPN-fronted, LAN-only)
the operator can disable IP-subnet tracking entirely via the
`features.trusted_network` flag — quotas then become purely per-key,
and anonymous uploads share a single global anonymous class. Public
deploys keep the full union-quota machinery on.

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
default matches the industry — codepen ships no CSP on user pens at all.

Response headers on paste reads:

- `X-Frame-Options: DENY` — no embedding the paste in iframes elsewhere
  (clickjacking defense)
- `Referrer-Policy: no-referrer` — visiting a paste leaks nothing about
  who sent it
- `Permissions-Policy: camera=(), microphone=(), geolocation=(), usb=(), payment=()`
  — deny everything that needs explicit user grant
- `Cross-Origin-Opener-Policy: same-origin` — pastes can't open windows
  to other origins and retain references

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

Treat any URL on hostthis.dev as untrusted user content — same as you'd
treat a codepen, a gist, or a github.io page.

### Markdown rendering

Markdown is rendered to HTML server-side by a memory-safe Go markdown
library (likely `goldmark`). The output is sanitized through `bluemonday`'s
UGC policy to strip event handlers, `javascript:` URLs, and dangerous
tags before being served. Uploaded Markdown therefore can NOT execute JS
even though uploaded HTML can — the sanitizer is the safety net for the
markdown path.

### Abuse reporting

Every paste page can render a small "report" link (it points at an apex
form). The form lets visitors flag phishing / malware / DMCA. Reports
flow to the operator's queue; flagged pastes can be unpublished by ops
without owner action. Since our default-24h retention already evicts
everything in a day, the abuse window is naturally bounded.

---

## Self-hosting

The public `hostthis.dev` is the default deploy, but the same Go binary
runs on any box. Minimal runtime config (env vars or single TOML):

```toml
[server]
ssh_listen = ":2222"
http_listen = ":8080"
apex_domain = "hostthis.dev"
data_dir    = "/var/lib/hostthis"
url_mode    = "subdomain"          # or "path" — dev only, see "Dev-only path mode"

[storage]
max_bytes        = 6_000_000_000   # service-wide hard cap on disk usage
warn_pct         = 80              # log warnings above this fill
reject_pct       = 95              # refuse new uploads above this fill

[tls]
# operator points these at their own wildcard cert for *.<apex_domain>
cert_file = "/etc/hostthis/wildcard.pem"
key_file  = "/etc/hostthis/wildcard.key"

[features]
trusted_network = false
```

### Service-wide storage cap

A single operator knob bounds total disk usage to keep hostthis from
filling the disk on a small VPS. The server tracks total bytes-on-disk
across the sqlite db + the blob store (basically the size of
`data_dir`). Behavior:

- **Below `warn_pct`**: normal.
- **At or above `warn_pct`**: log a warning every minute, surface a
  one-line note in the `whoami` response footer ("⚠ service at 82% of
  its disk budget").
- **At or above `reject_pct`**: refuse new uploads with a 503-shaped
  error ("hostthis is at capacity — try again after the next expiry
  sweep"). Existing pastes are served fine. The expiry sweep runs
  more aggressively (every minute instead of every 10 minutes) to
  reclaim space.
- **At 100%**: hard refuse. Even an in-flight upload that would push
  us over is aborted before flushing to disk.

The cap is service-wide, not per-class. Per-class quotas (200 MB /
30d) sit underneath: a single class can never exhaust the disk by
itself, but the aggregate of all classes can. The service-wide cap
catches that.

Default for `max_bytes` if unset: `0` = no cap (let disk fill). The
binary refuses to start in this mode if it detects it's running in a
container or in production-flagged mode without an explicit cap set.

### Everything else is hardcoded

Caps (per-paste, per-class), retention (24h), quota class behavior,
sandbox headers, slug generation alphabet — none of it is
configurable. The product is opinionated on purpose; operator choice
is limited to where it listens, where data lives, what disk budget
it gets, and the `trusted_network` toggle.

`trusted_network = true` skips IP-subnet tracking in the quota class
(quotas become purely per-key, anonymous uploads share a single global
anon class). Exists for LAN-only / VPN-fronted self-hosts where
household merging would be annoying.

---

## Non-goals (explicitly out of v1 scope)

These are interesting but expand the product beyond "host renderable
content for 24 hours." Keep the surface small.

- **Long-term storage**. Every paste expires at 24h, period. If you need
  a permanent URL, host elsewhere.
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
- **Separate `/llms.txt`**. Same reason — the landing page IS the
  programmatic reference. Duplicating it as plain text would drift.
- **GitHub (or any third-party) account linking / OAuth**. ssh keys
  alone carry identity; we don't need a second source of trust.
- **Operator-configurable limits**. Caps, retention, sandbox headers
  are hardcoded; the only operator knobs are ports / data-dir / TLS /
  trusted-network toggle.

If real demand surfaces for any of these later, they can be added without
breaking v1 semantics. Adding any of them should go through an ADR first
(see [docs/adr/](adr/)) — these are explicit no's, not oversights.

---

## Open questions

- **Signed-link tokens: rotate on `unpublish`?** Probably yes — `unpublish`
  should invalidate active signed links automatically (defense in depth);
  user can re-issue with `link`.
- **Quota display in `list` footer?** Currently shown in `whoami` only.
  Probably yes — no surprises when hitting the cap.
- **Owner notification when a paste expires?** Right now expiry is silent.
  A best-effort "your paste expired" line in the next `list` output could
  remind owners that the 24h clock ticks. Maybe.
- **Mermaid as first rendered-format expansion**: confirm the `goldmark`
  + `mermaid` SVG renderer choice once we get to it; for now Mermaid is
  v2+ and out of scope.
