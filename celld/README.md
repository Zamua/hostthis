# hostthis on celld

This directory contains hostthis's production metadata Worker. The Go service
reaches its HTTP and WebSocket routes through the `internal/celld` adapter;
public SSH and HTTP remain owned by `hostthisd`.

## Local development

Install celld v0.4.0, then run the Worker with celld's local runtime:

```sh
celld dev --port 8087
```

Run that command from this directory. It watches the Worker source and preserves
local durable state under `.celld/dev`. Stop celld before deleting that directory
to reset the state.

From the repository root, the live adapter suites target the local listener:

```sh
CELLD_TEST_ENDPOINT=http://127.0.0.1:8087 \
  go test -count=1 ./internal/storage -run TestConformance_Celld
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

A celld fleet serves one Worker application. Paste and site payloads remain in
the provider-neutral content-addressed blob store; celld owns metadata and room
state only.
