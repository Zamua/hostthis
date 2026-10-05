# hostthis on celld

This directory contains hostthis's metadata and payload Worker: the one
implementation of the domain. The Go service reaches its HTTP and WebSocket
routes through the `internal/celld` and `internal/storage` adapters; public SSH
and HTTP remain owned by `hostthisd`.

## Local development

`localrt/` runs the Worker under Miniflare (workerd) with the bindings
`wrangler.jsonc` declares. The Go tests (`internal/celldtest`), `make run` and
the browser suite all use it; `npm ci --prefix localrt` installs it.

To run the Worker on celld itself, install celld v0.6.1 and use its local
runtime:

```sh
celld dev --port 8087
```

Run that command from this directory. It watches the Worker source and preserves
local durable state under `.celld/dev`. Stop celld before deleting that directory
to reset the state.

On Linux the dev watcher can observe its own `.celld/dev` writes and restart
the application continuously, dropping connections. For an automation-grade
local fleet, run the production shape against any S3-compatible store instead:

```sh
celld deploy . --bucket s3://<bucket> --endpoint <s3-endpoint>
celld --bucket s3://<bucket> --endpoint <s3-endpoint> --listen 127.0.0.1:8087
```

From the repository root, the adapter suites target that node instead of
Miniflare:

```sh
CELLD_TEST_ENDPOINT=http://127.0.0.1:8087 \
  go test -count=1 ./internal/storage -run 'TestConformance_Celld|TestCelldBlobContract'
CELLD_TEST_ENDPOINT=http://127.0.0.1:8087 \
  go test -count=1 ./internal/http -run TestLiveRoom
```

`npm test` runs the Worker-level atomicity and crash-boundary suite without a
runtime.

## Cell topology

| cell | owns |
| --- | --- |
| Identity | one owner's index, quota state, and durable intents |
| Paste | one slug's metadata and versions, or one app's room allocation ledger |
| Room | one room's metadata, key/value state, sequence, and sockets |
| Subnet | one subnet's fresh-identity admission window |

A celld fleet serves one Worker application. Paste and site payloads live in
the `PAYLOADS` R2 binding (`r2/payloads/` in the fleet bucket), reached through
the stateless `/blob` routes outside any cell. The Worker stores them as opaque
bytes; keys, compression, and lifecycle stay in `hostthisd`.
