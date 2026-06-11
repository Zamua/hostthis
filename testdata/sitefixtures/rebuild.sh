#!/usr/bin/env bash
# Regenerate the committed site-fixture dist/ trees from the demo source.
#
# The byte-identical validation harness (internal/sitevalidation) deploys
# each demo's dist/ through the real archive pipeline and asserts the
# served bytes match the committed fixture EXACTLY. CI does NOT run npm:
# it byte-compares against the dist/ committed here. This script is the
# one way to regenerate those known-good snapshots when a demo's source
# changes (or a pinned dependency is bumped on purpose).
#
# For the three vite demos it runs `npm ci` (reproducible install from the
# committed package-lock.json) then `npm run build`. The vite build is
# deterministic: content-hashed asset names mean identical source yields
# identical dist bytes, so a clean rebuild leaves the committed tree
# unchanged unless the source actually changed.
#
# The plain-static demo has no build step (its dist/ IS hand-written), so
# this script leaves it untouched.
#
# Usage: testdata/sitefixtures/rebuild.sh   (or `make rebuild-site-fixtures`)
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if ! command -v npm >/dev/null 2>&1; then
  echo "rebuild-site-fixtures: npm not found on PATH." >&2
  echo "Install Node.js (>= 18) to regenerate the vite fixtures." >&2
  exit 1
fi

for demo in react vue svelte; do
  dir="$here/$demo"
  echo "==> rebuilding $demo fixture"
  ( cd "$dir" && npm ci --no-audit --no-fund && rm -rf dist && npm run build )
done

echo "==> plain-static has no build step (dist/ is hand-written); left as-is"

# Regenerate the committed SHA256SUMS manifest over every fixture dist/
# file. The harness (TestFixtureSnapshot_Matches) verifies each committed
# file hashes to the value pinned here, so an accidental dist/ corruption
# (a botched merge, a stray editor save) is caught loudly even though the
# round-trip alone reads whatever is on disk.
echo "==> regenerating SHA256SUMS"
{
  for demo in plain-static react svelte vue; do
    ( cd "$here/$demo/dist" && find . -type f | LC_ALL=C sort | sed 's|^\./||' | while read -r f; do
        printf '%s  %s/%s\n' "$(shasum -a 256 "$f" | cut -d' ' -f1)" "$demo" "$f"
      done )
  done
} > "$here/SHA256SUMS"

echo "done. Run 'git status testdata/sitefixtures' to see if any dist/ changed."
