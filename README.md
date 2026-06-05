# hostthis

Pipe HTML or Markdown to ssh, get a URL. Pastes expire 24 hours after
their last update.

```
$ cat index.html | ssh hostthis.dev
https://hostthis.dev/p/abc12345
expires in 24h
```

Your ssh key is your account. No signup, no password. Without a key you
can still upload anonymously; you just can't list, update, or delete
your pastes from another machine.

## Commands

```
cat file | ssh hostthis.dev [--name "..."]      upload
cat file | ssh hostthis.dev <slug>              update an existing upload
ssh hostthis.dev list                           your active pastes
ssh hostthis.dev show <slug>                    read content (owner only)
ssh hostthis.dev rename <slug> "<name>"         set / change a paste's label
ssh hostthis.dev versions <slug>                history within the 24h window
ssh hostthis.dev pin <slug> <ver>               set served version
ssh hostthis.dev delete <slug>                  permanent
ssh hostthis.dev whoami                         your identity + active count
ssh hostthis.dev token create                   issue an HTTP API token
```

Limits: 5 MB per paste. 24h retention. HTML and Markdown only.

The URL itself is the secret — an 8-char random slug from a 32-char
alphabet, so ~10^12 possibilities. Share the URL with whoever you want
to see the paste; nobody who doesn't have it can find it. Same posture
as YouTube unlisted, GitHub secret gists, Drive "anyone with the link."

The public instance is at [hostthis.dev](https://hostthis.dev). The spec
that defines the behavior lives at [docs/SPEC.md](docs/SPEC.md).

## Self-hosting

You need Docker, a domain, and a TLS-terminating reverse proxy (nginx,
caddy, traefik). One container, one sqlite db, one blob directory.

```
git clone https://github.com/Zamua/hostthis.git
cd hostthis
make deploy VPS_HOST=myvps VPS_PATH=/opt/hostthis VPS_USER=apps \
            HOSTTHIS_APEX_DOMAIN=example.com
```

Local dev:

```
make run       # ssh :2222 / http :8080
make docker-up # ssh :12222 / http :18080
```

See [`CLAUDE.md`](CLAUDE.md) for the full layout and conventions.

## License

MIT — see [LICENSE](LICENSE).
