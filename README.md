# HOSTTHIS(1)

## Name

hostthis - pipe a rendered file in, get a public URL out. No signup, no app.

## Synopsis

```
cat <file> | ssh hostthis.dev [--name <label>] [--type html|markdown]
cat <file> | ssh hostthis.dev <slug>            # update an existing paste
ssh hostthis.dev <command> [<args>]
ssh hostthis.dev                                # show help
```

## Description

Publishes HTML or Markdown for 7 days at a random subdomain. One ssh
pipe, no signup, no install. Useful when you want a shareable URL
for a one-off HTML mock, a Markdown writeup, or anything you need a
teammate or LLM to load in a browser without spinning up a deploy.
Identity is your ssh public key: anyone with a different key can
read the URL but can't update, rename, pin, or delete the paste.

## Commands

<dl>

<dt><code>cat <em>file</em> | ssh hostthis.dev [--name <em>label</em>]</code></dt>
<dd>upload</dd>

<dt><code>cat <em>file</em> | ssh hostthis.dev <em>slug</em></code></dt>
<dd>replace <em>slug</em>'s content; resets the 7-day clock</dd>

<dt><code>ssh hostthis.dev list</code></dt>
<dd>active pastes, soonest to expire first</dd>

<dt><code>ssh hostthis.dev show <em>slug</em></code></dt>
<dd>print content to stdout</dd>

<dt><code>ssh hostthis.dev rename <em>slug</em> <em>label</em></code></dt>
<dd>set label; empty string clears</dd>

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

## Examples

```
# upload, get a URL on stdout
cat index.html | ssh hostthis.dev

# upload with an owner-only label visible in `list`
cat notes.md | ssh hostthis.dev --name "alpha notes"

# update an existing paste (same URL, new bytes; bumps to v2, v3, ...)
cat v2.html | ssh hostthis.dev abc12345

# force content type when sniffing gets it wrong
cat tricky.html | ssh hostthis.dev --type html

# see your active pastes, plus your current quota usage
ssh hostthis.dev list
ssh hostthis.dev whoami

# walk a paste's version history
ssh hostthis.dev versions abc12345

# read content back (owner only)
ssh hostthis.dev show abc12345

# stick the URL on v1 even though v3 is the latest
ssh hostthis.dev pin abc12345 1
ssh hostthis.dev unpin abc12345

# free one version's bytes; the history row stays as a tombstone
ssh hostthis.dev delete abc12345 2

# delete the paste entirely
ssh hostthis.dev delete abc12345

# set or clear the owner label
ssh hostthis.dev rename abc12345 "new label"
ssh hostthis.dev rename abc12345 ""
```

## Limits

10 MiB per identity, counting post-compression bytes across every
active version of every active paste. Highly redundant text
compresses 5-10x under zstd, so the real ceiling on raw payload is
typically 50-100 MiB of text.

HTML and Markdown only. 7-day retention from the last `update`.

## See Also

Source: [github.com/Zamua/hostthis](https://github.com/Zamua/hostthis)

## License

MIT - see [LICENSE](LICENSE).
