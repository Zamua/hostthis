# HOSTTHIS(1)

## Name

hostthis — host an HTML or Markdown paste at a URL for 7 days.

## Synopsis

```
cat file | ssh hostthis.dev [slug] [--name label]
ssh hostthis.dev command [args]
```

## Description

Reads HTML or Markdown from stdin, stores it, prints a URL. The URL
serves the content for 7 days from the last update, then it's deleted.

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
<dd>list versions within the 7-day window</dd>

<dt><code>ssh hostthis.dev pin <em>slug</em> <em>ver</em></code></dt>
<dd>stick the URL to <em>ver</em>; survives future updates</dd>

<dt><code>ssh hostthis.dev unpin <em>slug</em></code></dt>
<dd>clear the pin; URL serves the latest version</dd>

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

1 MiB per identity, total across active pastes. 7-day retention. HTML
and Markdown only.

## License

MIT — see [LICENSE](LICENSE).
