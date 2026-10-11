# hostthis on celld

This directory contains hostthis's metadata and payload Worker: the one
implementation of the domain. The Go service reaches its HTTP and WebSocket
routes through the `internal/celld` and `internal/storage` adapters; public SSH
and HTTP remain owned by `hostthisd`.

## Build

The source is split into modules under `src/`, entry `src/index.js`. celld
loads a single module, so every runtime runs one bundle, `dist/index.js`, which
`wrangler.jsonc` names as `main` with `no_bundle`. Build it before running or
deploying the Worker, and again after every change under `src/`:

```sh
npm ci && npm run build    # from this directory; `make bundle` from the root
```

esbuild is pinned in `package.json`. `dist/` is a build output and is not
committed. `npm test` builds it first.

## Local development

`localrt/` runs the bundle under Miniflare (workerd) with the bindings
`wrangler.jsonc` declares. The Go tests (`internal/celldtest`), `make run` and
the browser suite all use it; `npm ci --prefix localrt` installs it. It refuses
to start when the bundle is missing or older than `src/`.

To run the Worker on celld itself, install celld v0.6.1 and use its local
runtime:

```sh
celld dev --port 8087
```

Run that command from this directory. It runs the bundle, so rebuild after
editing `src/`, and preserves local durable state under `.celld/dev`. Stop
celld before deleting that directory to reset the state.

On Linux the dev watcher can observe its own `.celld/dev` writes and restart
the application continuously, dropping connections. For an automation-grade
local fleet, run the production shape against any S3-compatible store instead:

```sh
npm run build
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

`npm test` builds the bundle and runs the Worker-level suites (atomicity, crash
boundaries, routing) without a runtime.

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

## Access

With `HOSTTHIS_CELLD_TOKEN` set on the Worker, every route except `/healthz`
requires `Authorization: Bearer <token>` and answers 401 without it. `hostthisd`
sends the header when its own `HOSTTHIS_CELLD_TOKEN` is set, and `localrt`
passes the variable through to the Worker. Left unset, the routes are open, for
a Worker reachable only inside its cluster.
