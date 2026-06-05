# hostthis — spec

A self-hostable, dev-first paste service. Pipe a file to ssh, get back a URL.
No signup, no UI, no CLI to install. Your existing ssh key is the account.

```
$ cat index.html | ssh hostthis.dev
https://abc12345.hostthis.dev
```

This document defines the v1 surface. Anything not in it is intentionally
out of scope for v1 — see "Non-goals" at the bottom.

---

## What it is, what it isn't

- It IS: ephemeral-to-permanent file/HTML hosting addressable by URL, with
  ssh-pipe as the primary upload mechanism.
- It IS: dev-first. The mental model is `git push` for documents — your ssh
  key is your identity, every operation is one line in a terminal.
- It IS: a hosting target for LLM-generated artifacts (HTML mockups, ZIPs,
  Markdown reports) that need an immediate sharable URL.
- It IS NOT: a comment/collaboration platform (other tools fill that role).
- It IS NOT: a long-term file vault (elsewhere).
- It IS NOT: a transient blob host (other tools fill that role).

The differentiator from existing tools: `ssh-pipe upload` + `versioned HTML
hosting` + `ssh-key identity` + `self-hostable` + `dev-first API/CLI surface`.
No tool combines all of those.

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
  other pastes. Standard sandbox pattern (matches `multi-tenant subdomain hosts`,
  `(another such host)`, `(another such host)`).
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

---

## Identity

**SSH key fingerprint IS the account.** No signup form, no email, no
password. First time a key fingerprint connects, the server creates an
account row keyed on the SHA256 fingerprint. Every subsequent connection
from the same key is "the same user".

Three identity tiers:

| Tier | How | Limits | Retention |
|---|---|---|---|
| **anonymous** | no ssh key offered (SSH `none` auth) | 5 MB per paste | 7 days |
| **new keyed** | first-time key, < 7 days old | 5 MB per paste | 7 days |
| **established keyed** | key seen for ≥ 7 days, no abuse flags | 25 MB per paste, 500 MB total | forever (until `delete`) |

New keys start at anon limits to defeat Sybil abuse (`ssh-keygen ∞ times`
for unlimited quota). They auto-bump after 7 days of no abuse.

Optional uplift: `ssh hostthis.dev verify github` opens an OAuth flow that
links the key to a verified GitHub account → immediate bump to established
tier (skips the 7-day cooldown). Not required, just speeds up trust.

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

- **Per-paste hard cap**: 25 MB (configurable by operator, but a per-paste
  ceiling always exists). Anon and new-keyed get 5 MB.
- **Format**: any binary. Server sniffs the first 512 bytes for content
  type via the standard `http.DetectContentType` algorithm. HTML/MD render
  as pages; images render inline; everything else is `Content-Disposition:
  attachment` by default.
- **No streaming reads from upload**: server buffers the whole stdin into
  memory or a temp file before committing. The 25 MB cap means a small fixed
  buffer is fine.
- **Storage**: SHA256-keyed content-addressed blobs on disk (`data/blobs/<sha256[:2]>/<sha256>`).
  Multiple slugs pointing to the same blob share the storage.

---

## Verbs (the `ssh hostthis.dev <verb>` surface)

Every verb is the first positional argument after the SSH connection.
With no command and no stdin, the server prints the help banner.

### Upload (new)
```
cat index.html | ssh hostthis.dev
→ https://abc12345.hostthis.dev
```
Reads stdin until EOF or 25 MB. Generates a fresh random slug. Returns
the URL to stdout (one line, suitable for pipes).

If the user has no ssh key in their agent, the server still accepts the
upload via SSH's `none` auth method, prints the URL, and appends a one-line
nudge about adding a key to get history.

### Upload (update an existing slug)
```
cat v2.html | ssh hostthis.dev abc12345
→ https://abc12345.hostthis.dev  (v2)
```
Slug as positional arg means "update this one". Server checks ownership
against the key fingerprint. Errors:
- `403`: slug exists but you don't own it
- `404`: slug doesn't exist
- `413`: payload too large

Update creates a new immutable version under the hood (SHA-keyed blob
ref). The slug always serves the currently-pinned version (defaults to
latest after an update).

### List your pastes
```
ssh hostthis.dev list
SLUG       SIZE    KIND    UPDATED       VERSIONS
abc12345   1.2k    html    2h ago        v2
x7y8z9q0   540B    text    1d ago        v1
mnop4567   3.8k    html    3d ago        v1
```
Sorted by last-updated desc. Output is tab-separated for easy `awk`-ing.
Anonymous sessions return empty (no identity).

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
v2           2026-06-04 09:15  1.1k
v1           2026-06-03 18:22  0.9k
```

### Pin a version (rollback or roll-forward)
```
ssh hostthis.dev pin abc12345 v1     # roll back
ssh hostthis.dev pin abc12345 v3     # roll forward
```
Sets which version `<slug>.hostthis.dev` serves. No version is ever deleted
unless `delete` is called on the whole slug. Reads symmetric in either time
direction — "rollback" framing is intentionally avoided.

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

### Touch (refresh retention TTL)
```
ssh hostthis.dev touch abc12345
touched. TTL reset.
```
Resets the "untouched for N days → expire" backstop. Keyed pastes have a
365-day backstop by default; this prolongs another 365 days. Auto-applied
on `update` and `pin` too.

### Identity
```
ssh hostthis.dev whoami
key:     SHA256:abc...xyz
joined:  2025-12-01
uploads: 4
tier:    established
quota:   312/500 MB
```

### Verify GitHub (optional uplift)
```
ssh hostthis.dev verify github
visit: https://hostthis.dev/oauth/github?k=...
```
OAuth flow; on success, account is marked `verified:github:<username>`
and tier bumps to established immediately.

### Help
```
ssh hostthis.dev
hostthis.dev — pipe a file, get a URL

  cat file.html | ssh hostthis.dev          upload
  cat file.html | ssh hostthis.dev <slug>   update an existing upload
  ssh hostthis.dev list                     your uploads
  ssh hostthis.dev show <slug>              read content (owner only)
  ssh hostthis.dev versions <slug>          history
  ssh hostthis.dev pin <slug> <ver>         set served version
  ssh hostthis.dev delete <slug>            permanent
  ssh hostthis.dev unpublish <slug>         public 404s
  ssh hostthis.dev publish <slug>           undo unpublish
  ssh hostthis.dev link <slug> --expires …  signed share URL
  ssh hostthis.dev unshare <slug>           revoke signed links
  ssh hostthis.dev touch <slug>             refresh retention TTL
  ssh hostthis.dev whoami                   your identity
  ssh hostthis.dev verify github            link a GitHub account

You: SHA256:abc... (4 uploads, established tier)
```

---

## HTTP / curl fallback

For environments without ssh (some CI, some sandboxes):

```
curl --data-binary @index.html https://hostthis.dev/u
https://abc12345.hostthis.dev
```

Anonymous always. For keyed actions over HTTP, the user issues an API token
via `ssh hostthis.dev token create` (one-time output), then passes it as
`Authorization: Bearer <token>` on `/u`, `/list`, `/delete/<slug>`, etc.

The HTTP surface mirrors the ssh verbs 1:1.

---

## Abuse defenses

Layered, defense-in-depth — no single mechanism is bulletproof but combined
they bound worst-case damage.

1. **Per-IP daily cap**: 100 MB uploaded per `/24` (IPv4) or `/48` (IPv6)
   per day. Bypassable with residential proxies but raises the bar.
2. **New keys start at anon tier**: 7-day cooldown before bumping to
   established. Defeats trivial `ssh-keygen` rotation for unlimited quota.
3. **Per-paste hard cap**: 25 MB ceiling. No path to upload a 1 GB file
   ever, by any user, regardless of trust state.
4. **Retention TTL backstop**: even keyed pastes expire after 365 days of
   no `touch`. Caps long-term storage explosion.
5. **Optional GitHub verify** for instant trust uplift. Doesn't add hard
   defenses but makes the trust signal cheaper for real users.

For self-hosted instances on trusted networks (friends-only, behind a
VPN, etc.) the operator can disable the IP cap and the new-key cooldown
via config. Public-internet deploys keep all defenses on by default.

---

## HTML sandboxing

Subdomain-per-paste means each user-uploaded HTML lives on its own origin.
Browsers enforce the same-origin policy: cookies, JS, and CSP from
`abc12345.hostthis.dev` cannot reach `xyz67890.hostthis.dev` or the apex.

Additional hardening on the response side:

- `Content-Security-Policy: default-src 'self' data: blob:; frame-ancestors 'none'`
  on HTML responses (operator can override per-paste later)
- `X-Frame-Options: DENY` (no embedding)
- `Referrer-Policy: no-referrer`
- `Permissions-Policy` disabling camera / microphone / geo by default
- `Content-Disposition: attachment` for any unknown content-type (forces
  download, doesn't render)

HTML rendering is opt-in by content type; everything else is opt-out
(safer default).

---

## MCP server surface

Expose a Model Context Protocol server at `https://hostthis.dev/mcp` so
Claude / Cursor / n8n can publish directly. Tools mirror the ssh verbs:

- `hostthis_upload(content, slug?, content_type?)` → returns URL
- `hostthis_update(slug, content)` → returns URL
- `hostthis_list()` → returns array
- `hostthis_delete(slug)` → returns null
- `hostthis_show(slug)` → returns content
- `hostthis_versions(slug)` → returns array
- `hostthis_pin(slug, version)` → returns URL

Auth: API token from `ssh hostthis.dev token create`, passed as Bearer.

---

## Self-hosting

The public `hostthis.dev` is the default deploy, but the same Go binary
runs on any box. Operator config (TOML):

```toml
[server]
ssh_listen = ":2200"
http_listen = ":8080"
apex_domain = "hostthis.dev"

[limits]
anon_per_paste_mb = 5
keyed_per_paste_mb = 25
keyed_quota_total_mb = 500
anon_retention_days = 7
keyed_retention_days = 365  # backstop, refreshable with `touch`
ip_daily_cap_mb = 100
new_key_cooldown_days = 7

[features]
github_verify = true
mcp_server = true
curl_fallback = true
```

A trusted-network deploy (LAN-only, VPN-fronted) can relax all of these
defaults — no IP cap, no cooldown, longer retention — via the
`features.trusted_network` toggle.

---

## Non-goals (explicitly out of v1 scope)

These are interesting but pull the product toward "over-scope".
Keep the surface small.

- **Comments / threaded discussion**. not a goal.
- **Password protection on public pastes**. Signed share links cover the
  "private but shareable" case; password is duplicative friction.
- **View limits / view counts visible to the public**. Owner can see counts
  via `ssh whoami` if we add it later, but no public-facing analytics.
- **Visual editor**. ssh pipe is the only authoring tool. Edit locally,
  re-pipe.
- **Teams / orgs / shared accounts**. Personal use only.
- **Custom domains** (`pastes.mycompany.com`). The wildcard subdomain
  pattern covers branding-via-slug well enough.
- **Email notifications**. The ssh response IS the notification.

If real demand surfaces for any of these later, they can be added without
breaking v1 semantics.

---

## Open questions

- **GitHub verify v1 vs v2?** If we skip GitHub verify in v1, the trust
  ramp is 7 days of waiting — annoying for legit users. Including it costs
  ~2h of OAuth wiring. Probably v1.
- **Signed-link tokens: rotate on `unpublish`?** Probably yes — `unpublish`
  should invalidate active signed links automatically (defense in depth);
  user can re-issue with `link`.
- **Quota display**: show in `whoami` and in `list` footer? Probably yes —
  no surprises when hitting the cap.
- **CSP override per paste**: do owners need to relax CSP for legit pastes
  that load external CDN scripts? Punt to operator config for v1.
