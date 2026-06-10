# .githooks

Repo-owned git hooks for hostthis. Plain shell scripts; no third-party hook
framework. Activate them once per clone with:

```
git config core.hooksPath .githooks
```

After that, every commit you make from this clone runs the hooks below.

## pre-commit

Runs against the staged Go files (no-op when none are staged):

1. `gofmt -l` on each staged `.go` file. Fails if any are not formatted.
2. `go vet ./...` across the default build.
3. `golangci-lint run --new-from-rev HEAD --fix=false`, if the tool is on
   `PATH`. The hook prints a warning and skips this step when
   `golangci-lint` is not installed, so a fresh contributor isn't blocked.

Bypass for one commit (use sparingly): `git commit --no-verify`.
