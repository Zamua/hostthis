# HOSTTHIS(1)

## Name

hostthis — host an HTML or Markdown paste at a URL for 24 hours.

## Synopsis

```
cat file | ssh hostthis.dev [slug] [--name label]
ssh hostthis.dev command [args]
```

## Description

Reads HTML or Markdown from stdin, stores it, prints a URL. The URL
serves the content for 24 hours from the last update, then it's deleted.

Your ssh key is your account — no signup, no password. Without a key
you can still upload anonymously; you just can't list, update, or
delete pastes from another machine.

The URL itself is the secret: 8-char random slug from a 32-char
alphabet, ~10^12 possibilities. Share the URL with whoever you want to
see the paste; nobody who doesn't have it can find it.

## Commands

<dl>

<dt><code>cat <em>file</em> | ssh hostthis.dev [--name <em>label</em>]</code></dt>
<dd>upload</dd>

<dt><code>cat <em>file</em> | ssh hostthis.dev <em>slug</em></code></dt>
<dd>replace <em>slug</em>'s content; resets the 24h clock</dd>

<dt><code>ssh hostthis.dev list</code></dt>
<dd>active pastes, soonest to expire first</dd>

<dt><code>ssh hostthis.dev show <em>slug</em></code></dt>
<dd>print content to stdout</dd>

<dt><code>ssh hostthis.dev rename <em>slug</em> <em>label</em></code></dt>
<dd>set label; empty string clears</dd>

<dt><code>ssh hostthis.dev versions <em>slug</em></code></dt>
<dd>list versions within the 24h window</dd>

<dt><code>ssh hostthis.dev pin <em>slug</em> <em>ver</em></code></dt>
<dd>serve <em>ver</em></dd>

<dt><code>ssh hostthis.dev delete <em>slug</em></code></dt>
<dd>permanent</dd>

<dt><code>ssh hostthis.dev whoami</code></dt>
<dd>print identity and active count</dd>

</dl>

## Examples

```
cat index.html | ssh hostthis.dev
cat README.md  | ssh hostthis.dev --name notes
cat v2.html    | ssh hostthis.dev abc12345
ssh hostthis.dev list
ssh hostthis.dev delete abc12345
```

## Limits

1 MiB per identity, total across active pastes. 24h retention. HTML
and Markdown only.

## License

MIT — see [LICENSE](LICENSE).
