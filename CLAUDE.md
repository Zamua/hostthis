# hostthis — contributor guide

Read this before doing substantive work on the project. It captures the
conventions we've agreed on and the workflow that keeps the repo coherent
over time. Read [`docs/SPEC.md`](docs/SPEC.md) next for what hostthis
actually does.

This project is currently private but is being written with the assumption
it will be open-sourced. Don't put environment-specific notes, personal
identifiers, or operator-specific configuration here or anywhere else in
the tree — keep those in your local untracked config.

## Workflow

### Spec-first, always

Before any code change that adds or alters product behavior:

1. Open [`docs/SPEC.md`](docs/SPEC.md).
2. Confirm the spec already describes what you're about to build. If not,
   edit the spec first, re-read it, confirm it still hangs together as a
   whole.
3. Then write the code.

In a single commit, the spec should reflect the behavior the code in that
commit implements — not what existed before, not what we plan next. This
means SPEC.md is never stale relative to code, by construction. The spec
is the source of truth for what the project does. The code is the
implementation.

Every other long-running project has watched its spec drift until nobody
trusted it. The discipline of editing the spec first is the only thing
that keeps the spec useful.

### Implementation discipline: DDD + TDD

Two non-negotiable practices when writing code.

**Domain-Driven Design.** Organize packages by *bounded context*, not by
technical layer.

- The domain layer (pure types, value objects, business rules) lives in
  its own package(s) and imports *nothing* from infrastructure — no
  SQL, no SSH, no HTTP, no third-party SDKs. It's plain Go data + pure
  functions you can test without spinning up anything.
- Infrastructure adapters (sqlite repo, SSH server, HTTP handlers,
  filesystem blob store) live in separate packages and depend on the
  domain. The domain never depends back.
- Application services orchestrate use cases by composing the domain
  with the adapters via small interfaces. Routes / SSH handlers /
  CLI verbs are thin translation layers that call services and shape
  the response.
- Don't reach for fancy DDD patterns (aggregates, events, unit-of-work,
  specifications) unless the type system or a concrete use case forces
  them. The shape — domain-pure, infra-separate, services-on-top — is
  what matters; the ceremony is optional.

**Test-Driven Development.** Tests are part of the same change as the
code they cover, not a follow-up.

- For new behavior: red → green → refactor. Write the failing test
  that pins the spec'd behavior, make it pass, clean up.
- For modifying existing behavior: write a characterization test that
  pins the *current* behavior first, then change the code + the test
  together. Keeps regressions visible.
- Prefer integration tests over unit tests where the boundary is real
  (sqlite, real SSH session via in-memory listener, etc.) — they catch
  more, mock less. Reserve unit tests for pure domain logic where
  there's nothing to integrate.
- A PR that "adds a feature without tests" doesn't ship. The spec edit
  + the code + the tests are one change.

### Commits

Conventional Commits style, single line. No co-author trailers, no
agent-attribution lines. Examples:

```
feat(ssh): accept anonymous uploads via 'none' auth method
fix(http): set Content-Disposition: attachment on unknown content types
docs(spec): clarify retention TTL behavior for keyed pastes
```

## Repo layout

```
cmd/hostthisd/       single binary entry point
internal/
  domain/            pure types + invariants (no I/O)
  storage/           sqlite repos + on-disk blob store
  service/           use cases (upload, manage, sweep)
  ssh/               gliderlabs ssh server + verb dispatch
  http/              apex landing + paste read surface
  render/            markdown → sanitized HTML
web/landing.html     embedded apex landing page
docs/SPEC.md         product spec; source of truth for behavior
Dockerfile           multi-stage build; distroless static image
Makefile             build / test / run / docker-up / smoke targets
CLAUDE.md            contributor workflow conventions
README.md            user-facing manpage
```

## Local setup

Go 1.25+ and Docker required.

```
make build         # local Go build → ./bin/hostthisd
make test          # run all tests (domain unit + storage + service + ssh/http e2e)
make run           # run locally, no container; ssh :2222 http :8080, path-mode

make docker-build  # build the container image
make docker-up     # docker compose up; same ports; data persists in ./data
make docker-down   # tear down
```

### Testing the S3 blob backend locally

The S3-compatible backend (`HOSTTHIS_BLOB_BACKEND=s3`) is exercised by an
integration test that needs a live S3 endpoint. The dev compose at
`deploy/dev/docker-compose.yml` brings up a local MinIO + auto-creates
the test bucket.

```
make dev-minio-up  # starts MinIO at :9000 (s3) + :9001 (console)
make test-s3       # runs ./internal/storage TestS3BlobStore_RoundTrip
make dev-minio-down  # teardown (with volume wipe)
```

The default `make test` skips this test if `MINIO_TEST_ENDPOINT` is unset,
so CI runs that don't have Docker available don't fail.

### Migrating live blobs from disk to S3

Two operator binaries handle the one-way disk → S3 migration:

```
# 1. Copy blobs (idempotent: re-Putting an existing key is a no-op).
HOSTTHIS_DATA_DIR=/path/to/data \
HOSTTHIS_S3_ENDPOINT=https://… HOSTTHIS_S3_BUCKET=… \
HOSTTHIS_S3_ACCESS_KEY=… HOSTTHIS_S3_SECRET_KEY=… \
make blob-migrate

# 2. Verify byte-for-byte (re-hashes every S3 object).
make blob-verify   # exits non-zero on any mismatch / missing object

# 3. Flip the env var on the deploy and restart.
HOSTTHIS_BLOB_BACKEND=s3
```

Both binaries read the same `HOSTTHIS_S3_*` env vars hostthisd does, so
there's no second config surface to maintain.

Quick smoke from another terminal once it's live:

```
# make run    — ssh :2222 / http :8080
# make docker-up — ssh :12222 / http :18080 (host ports shifted to avoid common clashes)

echo '<!doctype html><h1>hi</h1>' | ssh -p 12222 -o StrictHostKeyChecking=no localhost
# → prints a URL like http://localhost:18080/p/abc12345
curl <that URL>
```

The binary defaults to `--mode path` (apex/p/&lt;slug&gt; URLs) for dev. Use
`--mode subdomain` only for production deploys with a wildcard cert.

## Deploy

This repo ships application code and a `make smoke` target only. Deploy
mechanics (rsync to a host, build the image remotely, rolling restart,
log tailing, takedown) live OUTSIDE this repo, in the operator's private
infra checkout. The shape is intentional: anyone cloning the public repo
gets a clean buildable + testable Go service with no operator paths,
ssh aliases, or sudo invocations baked in.

If you operate a deploy of hostthis, your operator-side concerns belong
next to the production `compose.yml` + `.env` (one directory per app),
and you reference this repo from there as a source dependency. The
operator-side Makefile shells through to `make -C <hostthis-repo> smoke`
for post-deploy verification.

The runtime config (apex domain, URL mode, scheme, S3 credentials,
MinIO root creds) is read from `HOSTTHIS_*` and `MINIO_*` env vars by
the operator-side compose file. The binary refuses to start without
`HOSTTHIS_APEX_DOMAIN` set.

## Don'ts

- Don't commit environment-specific paths, IPs, hostnames, account IDs,
  or operator credentials. The repo is meant to be a clean implementation
  that anyone can clone and run.
- Don't add personal preferences or session-specific notes here. Those
  belong in your local (untracked) config.
- Don't bypass the spec-first rule. If a fix is "too small to spec," it's
  either small enough that the spec was already correct, or it changes
  behavior and the spec needed updating. Either way: read the spec before
  the change.
