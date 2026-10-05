# hostthis - contributor guide

Read this before doing substantive work on the project. It captures the
conventions we've agreed on and the workflow that keeps the repo coherent
over time. Read [`docs/SPEC.md`](docs/SPEC.md) next for what hostthis
actually does.

This project is public (open source). Don't put environment-specific notes, personal
identifiers, or operator-specific configuration here or anywhere else in
the tree - keep those in your local untracked config.

## Workflow

### Spec-first, always

Before any code change that adds or alters product behavior:

1. Open [`docs/SPEC.md`](docs/SPEC.md).
2. Confirm the spec already describes what you're about to build. If not,
   edit the spec first, re-read it, confirm it still hangs together as a
   whole.
3. Then write the code.

In a single commit, the spec should reflect the behavior the code in that
commit implements - not what existed before, not what we plan next. This
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
  its own package(s) and imports *nothing* from infrastructure - no
  SQL, no SSH, no HTTP, no third-party SDKs. It's plain Go data + pure
  functions you can test without spinning up anything.
- Infrastructure adapters (metadata repo, SSH server, HTTP handlers,
  object store) live in separate packages and depend on the
  domain. The domain never depends back.
- Application services orchestrate use cases by composing the domain
  with the adapters via small interfaces. Routes / SSH handlers /
  CLI verbs are thin translation layers that call services and shape
  the response.
- Don't reach for fancy DDD patterns (aggregates, events, unit-of-work,
  specifications) unless the type system or a concrete use case forces
  them. The shape - domain-pure, infra-separate, services-on-top - is
  what matters; the ceremony is optional.

**Test-Driven Development.** Tests are part of the same change as the
code they cover, not a follow-up.

- For new behavior: red → green → refactor. Write the failing test
  that pins the spec'd behavior, make it pass, clean up.
- For modifying existing behavior: write a characterization test that
  pins the *current* behavior first, then change the code + the test
  together. Keeps regressions visible.
- Prefer integration tests over unit tests where the boundary is real
  (a real metadata repo, a real SSH session via in-memory listener, etc.) - they catch
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
docs(spec): clarify version pinning for keyed pastes
```

## Engineering principles

Standing design rules for this codebase. They bind every change; a PR that
violates one either fixes its approach or changes the principle here first,
in the same spirit as spec-first. The list grows as decisions get made.

1. **No in-memory buffering of payloads.** The service is constant-memory by
   design: request and response bodies stream end to end, and resident memory
   must never track payload size. When a size must be known before storage
   accepts the bytes (a checksum, a length header), spill to a temp file -
   disk is the buffer, never the heap. A code path that reads a whole body
   into memory is a defect even when every current caller is small.

2. **No scans on the request path.** Prefix scans over churned key ranges
   cost object-store round trips that grow with an owner's LIFETIME activity,
   because replicated deletes leave markers scans must walk forever. Anything
   a user waits on reads point-gettable state: maintained documents and
   aggregates, updated transactionally at write time. Scans are for boot-time
   sweeps and offline reconciliation only.

3. **Fail open on unreadable data.** One undecodable record may cost accuracy
   (an under-count, a skipped row, a logged skip) but must never cost
   availability. Any path where a single damaged record can block reads,
   writes, or repair for a whole owner is a defect - the repair path
   especially must tolerate the very damage it exists to repair.

## Tests: assert the property, not the spelling

When an assertion compares a SERIALIZED form - JSON text, wire bytes, formatted
output - decode it and assert the value instead, unless the encoding itself is the
property under test (escaping of control characters, say). Two spellings can be
valid JSON for the same string (`"\uFFFD"` and a literal U+FFFD), so asserting the
spelling pins the Go toolchain, not the code, and such a test fails on a toolchain
upgrade while looking like an upstream bug.

## Repo layout

```
cmd/hostthisd/       single binary entry point
internal/
  domain/            pure types + invariants (no I/O)
  celld/             metadata adapters over the celld Worker
  celldtest/         runs the Worker locally for tests
  storage/           site view, blob store layers, storage contract tests
  service/           use cases (upload, manage, deploy, rooms)
  ssh/               gliderlabs ssh server + verb dispatch
  http/              apex landing + paste read surface
e2e/                 Playwright browser suite (Chromium + WebKit)
web/landing.html     embedded apex landing page
docs/SPEC.md         product spec; source of truth for behavior
Dockerfile           multi-stage build; distroless static image
Makefile             build / test / run / docker-up / smoke targets
CLAUDE.md            contributor workflow conventions
README.md            user-facing manpage
```

## Local setup

Go 1.26+ (per `go.mod`), Node 24+, and Docker required.

The Go tests and `make run` run the celld Worker (`celld/src`) locally under
Miniflare, from `celld/localrt`. `make test` and `make run` install its
dependencies on first use; a bare `go test ./...` needs them installed once:

```
npm ci --prefix celld/localrt
```

`internal/celldtest` starts one runtime per test binary and hands each test its
own cell namespace (`celldtest.Endpoint(t)`), so a test builds the production
adapters (`celld.NewPasteRepo`, `storage.NewCelldBlobStore`, ...) against real
Worker code with no shared state.

```
make build         # local Go build → ./bin/hostthisd
make test          # run all tests (domain unit + storage + service + ssh/http e2e)
make run           # run locally, no container; ssh :2222 http :8080, path-mode,
                   # beside the Worker under Miniflare (state in ./data/celld)

make docker-build  # build the container image
make docker-up     # docker compose up; same ports; data persists in ./data
make docker-down   # tear down
```

### Storage: one domain, in the Worker

The domain lives once, in the celld Worker (`celld/src`): metadata in its
cells, payloads in its R2 binding, both reached at `HOSTTHIS_CELLD_ENDPOINT`
(see `docs/SPEC.md` "Metadata storage backends"). There is no Go copy of it to
keep in step. Production runs the Worker on celld; `make run`, the Go tests and
the browser suite run the same code under Miniflare.

The conformance suite pins the adapters' contract: it runs against the local
runtime on every `go test ./...`, and against a real celld node when
`CELLD_TEST_ENDPOINT` names one (the CI `celld` job), along with the blob
contract and the live room suite.

Quick smoke from another terminal once it's live:

```
# make run    - ssh :2222 / http :8080
# make docker-up - ssh :12222 / http :18080 (host ports shifted to avoid common clashes)

echo '<!doctype html><h1>hi</h1>' | ssh -p 12222 -o StrictHostKeyChecking=no localhost
# → prints a URL like http://localhost:18080/p/abc12345
curl <that URL>
```

The binary defaults to `--mode path` (apex/p/&lt;slug&gt; URLs) for dev. Use
`--mode subdomain` only for production deploys with a wildcard cert.

### Browser e2e

The renderers are the product: a paste is served as a fixed HTML shell that
fetches the raw bytes and renders them client-side. A shell whose bundle 404s
or throws serves a blank page while every status code stays 200, so
`scripts/smoke.sh` cannot see it. The `e2e/` suite is what does.

It is a Playwright Test project, and every test runs in both Chromium and
WebKit: one test, every engine, so Safari's engine is covered without a second
suite.

```
make e2e        # run it (installs e2e/node_modules on first use)
make e2e-ci     # same, after installing the browsers and their system libraries
make e2e E2E_FLAGS='tests/mermaid.spec.ts --project webkit'   # one file, one engine
```

The harness is `e2e/fixtures.ts`. A run builds `hostthisd` once and mints one
SSH key; each Playwright worker starts its own celld Worker under Miniflare and
its own daemon against it, on ephemeral ports, and tests upload over SSH the way
a user does. No staging, no celld fleet, no network dependency. The `pageErrors` fixture
records uncaught exceptions, `console.error`, and failed or erroring
sub-resources, so a test that ends in `pageErrors.expectNone()` fails on a
blank page even when every response is a 200.

**A screenshot is evidence, not an assertion.** Every test asserts a semantic
DOM signal (an element, its text, a count, a state change) and attaches
screenshots through `shot(label)` for a human to review. Pixel diffing and
`toHaveScreenshot` are excluded on purpose: font rasterization differs between
a CI runner and a laptop, and that flake ends with the suite being ignored.

Every pull request publishes Playwright's HTML report to hostthis (one site per
PR, redeployed in place) and comments the link. Traces are kept for failures
only, which keeps each report small.

A test adds no product behavior, so the spec-first rule has nothing to bite on
here: extending this suite does not edit `docs/SPEC.md`.

## Deploy

This repo ships application code and a `make smoke` target only. Deploy
mechanics (image publishing, cluster manifests, rollout, log tailing,
takedown) live OUTSIDE this repo, in the operator's private
infra checkout. The shape is intentional: anyone cloning the public repo
gets a clean buildable + testable Go service with no operator paths,
ssh aliases, or sudo invocations baked in.

If you operate a deploy of hostthis, your operator-side concerns belong
next to your production deployment config (one directory per app),
and you reference this repo from there as a source dependency. The
operator-side Makefile shells through to `make -C <hostthis-repo> smoke`
for post-deploy verification.

The runtime config (apex domain, URL mode, scheme, storage backends) is
read from `HOSTTHIS_*` env vars, which
the operator's deployment supplies. The binary refuses to start without
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
