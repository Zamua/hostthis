# hostthis

```
hostthis — pipe rendered content (html/markdown), get a URL.
              pastes expire 24h after their last update.

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

uploads accept HTML and Markdown only. 5 MB per paste. 24h retention.
the URL itself is the secret — 8-char random slug, ~10^12 possibilities.
share the URL with anyone you want; don't share it with anyone you don't.
```

## License

MIT — see [LICENSE](LICENSE).
