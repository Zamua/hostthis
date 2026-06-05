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

### ADRs for shaping decisions

When you make a design choice that:

- has multiple defensible alternatives,
- is hard to reverse later (URL shape, wire protocol, identity model, data
  shape, on-disk layout, public API), and
- would surprise a reader of the code who hadn't seen the discussion,

file an Architecture Decision Record under [`docs/adr/`](docs/adr/).
[`docs/adr/README.md`](docs/adr/README.md) is the template + the
when-to-write guide.

ADRs are immutable once accepted. To change a decision, write a new ADR
that supersedes the old one. The old one stays in the tree so future
readers can see what was considered last time and why.

Don't ADR every small thing. Library choice, directory layout,
code-style — skip. ADR when the decision *shapes* the project.

**ADRs are not autonomous work.** If you're an AI agent, do not write
an ADR on your own initiative and commit it. ADRs codify decisions, and
decisions require a human in the loop. The process is:

1. *Surface the question*. When you hit a design fork that looks
   ADR-worthy, stop and tell the user: "this looks ADR-worthy because
   X, Y. The alternatives are A, B, C." Wait for them to agree it's
   worth an ADR. They may decide it's small enough to skip.
2. *Draft for review*. Once they agree, draft the ADR following the
   `docs/adr/README.md` template and share it for review. Do not commit
   it yet.
3. *Wait for approval*. Iterate on the draft until the user explicitly
   approves it. Only then commit and proceed to implementation.

The "spec-first" rule still applies — if the decision changes product
behavior, the spec edit is part of the implementation step *after* the
ADR is approved.

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
docs/
  SPEC.md          product spec — source of truth for behavior
  adr/
    README.md      ADR template + when-to-write guide
    NNNN-*.md      individual decision records (none yet)
CLAUDE.md          this file — contributor workflow conventions
README.md          (not yet) human-facing intro
```

Once code lands, expect `cmd/` (Go binaries), `internal/` (private
packages), `pkg/` (anything intended for external import), a `Makefile`,
deploy scripts under `deploy/`, integration tests under `tests/`.

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

## Deploy (single host over ssh)

The make targets rsync the working tree, build the image, and restart
the container on a remote host you reach via ssh. All host-specific
settings come from `VPS_*` variables passed on the command line.

```
make deploy VPS_HOST=myvps VPS_PATH=/opt/hostthis VPS_USER=apps \
            HOSTTHIS_APEX_DOMAIN=example.com
```

Available targets:

| target | what it does |
| --- | --- |
| `make deploy` | sync + build + restart in one go |
| `make deploy-sync` | rsync working tree; chown checkout to VPS_USER, data to container uid |
| `make deploy-build` | rebuild the image on the host |
| `make deploy-restart` | docker compose up -d on the host |
| `make deploy-logs` | tail the container logs |
| `make deploy-down` | docker compose down on the host |

The runtime config (apex domain, URL mode, scheme) is read from
`HOSTTHIS_*` env vars by [`deploy/vps/compose.yml`](deploy/vps/compose.yml).
The compose file refuses to start without `HOSTTHIS_APEX_DOMAIN` set.

For repeated deploys to the same host, drop a (gitignored) shell
script at `.env.deploy` and `source` it before running `make`.

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
