# Site fixtures (byte-identical validation harness)

These are small, real static-site builds used to prove the static-site
hosting pipeline serves a deployed site **byte-identically**: the same
bytes you upload come back out, with the content-type the extension
implies, and the SPA fallback serves the root `index.html` for a
client-side route while a genuinely-missing asset 404s.

Each fixture is deployed through the **real** archive pipeline (the same
`DeploySite` use case an `ssh` tar upload hits) by
[`internal/sitevalidation`](../../internal/sitevalidation), then every
built file is fetched back over the real HTTP serving surface and
compared byte-for-byte against the file on disk here.

## Layout

```
testdata/sitefixtures/
  react/         vite + React + react-router-dom (BrowserRouter)
  vue/           vite + Vue 3 + vue-router (createWebHistory)
  svelte/        vite + Svelte + svelte-routing (history mode)
  plain-static/  hand-written multi-file site, no framework, no build step
```

Each framework demo holds:

- the demo **source** (`package.json`, `package-lock.json`, `vite.config.js`,
  `index.html`, `src/`) - committed, so the build is reproducible,
- the **build output** `dist/` - committed as the known-good fixture the
  harness compares against,
- `node_modules/` - **gitignored**; restored by `npm ci` only when you
  regenerate the fixtures.

The `plain-static` demo has no build step: its `dist/` IS the
hand-written source (`index.html`, `about.html`, `css/app.css`,
`js/app.js`). It exercises the same round-trip for a non-framework site
(real multi-page links, a real second page served from its own file,
never via the SPA fallback).

## Each framework demo has client-side routing

So the SPA fallback is exercised: a home route (`/`) plus an `/about`
route (and `/users/:id`) that are NOT real files on disk. A direct GET of
`/about` against the server misses the manifest and must fall back to the
root `index.html` with a `200`. See the harness for the asserted cases.

## Regenerating the fixtures

CI does **not** run npm - it byte-compares the served bytes against the
committed `dist/` here. Regenerate the snapshots only when a demo's source
(or a pinned dependency) changes on purpose:

```
make rebuild-site-fixtures
```

This runs `npm ci` (reproducible install from the committed
`package-lock.json`) then `npm run build` for each vite demo. The vite
build is deterministic - content-hashed asset names mean identical source
yields identical `dist/` bytes - so a clean rebuild leaves the committed
tree unchanged unless the source actually changed. Review
`git status testdata/sitefixtures` afterward and commit any intended dist
change alongside the source change that caused it.

## Why commit the build output?

So the validation test needs no Node toolchain to run. `go test ./...`
exercises the full deploy + serve round-trip against a real, framework-
built `dist/` without any `npm install`, which keeps CI fast and
hermetic. The committed `dist/` is the contract; `make rebuild-site-fixtures`
is the one way to change it.
