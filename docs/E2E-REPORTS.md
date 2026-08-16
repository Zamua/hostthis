# e2e reports on pull requests

Every pull request can publish a browsable report: pass or fail, the failure
output, and whatever screenshots the tests recorded, at one URL that opens on a
phone. GitHub's own artifacts are login-gated zips, which is why this exists.

The infrastructure is in place and currently **inert**. It switches on the day
an `e2e-ci` target appears in the Makefile.

## The contract

`.github/workflows/e2e-pr.yml` runs `make e2e-ci` and expects two things from it.

**1. JUnit XML at `results.xml`.**

For Go, `gotestsum` writes it directly:

```
gotestsum --junitfile results.xml -- ./...
```

The renderer is framework agnostic, so `pytest --junit-xml` or
`vitest --reporter=junit` work identically if a lane ever needs them.

**2. Optional screenshots under `$E2E_ARTIFACTS`.**

One directory per flow. Numeric prefixes order the steps:

```
$E2E_ARTIFACTS/
  markdown-render/
    01-uploaded.png
    02-rendered.png
    meta.json
```

`meta.json` names the test the flow belongs to, which is how the report attaches
the filmstrip to a result:

```json
{ "flow": "markdown-render", "nodeid": "TestMarkdownRender" }
```

Match `nodeid` to the test name JUnit reports. For Go that is the test function
name. A flow whose name matches no result still appears, under "unmatched
filmstrips", rather than being silently dropped.

Screenshots are optional. Without them the report is still a readable result
page; the filmstrip is the part that needs a browser driver.

## What the report shows

Verdict first, then identity (commit, branch, PR, link to the run), then each
test with its filmstrip and, on failure, its output. Then the command to
reproduce it locally, and the environment. Section order is deliberate: the
question a reviewer has at second zero is "did it pass", and the question they
have on day thirty is "what produced this".

## Publishing

The report is uploaded as a static site and the URL is posted as a PR comment.

- One slug per pull request, redeployed in place. The URL stays valid for the
  life of the branch and each run becomes a new **version**, so `versions <slug>`
  is a timestamped history for comparing runs.
- Authentication is the `HOSTTHIS_SSH_KEY` repository secret, a dedicated key
  used for nothing else. Pull requests from forks do not receive secrets, so
  they skip publishing and fall back to the build artifact. That is intended.
- Quota is 100 MB per key. A report carrying a full filmstrip runs a few MB, so
  a busy branch will need old versions pruned eventually. Nothing prunes
  automatically today, on purpose.

**The published URL is unguessable but not authenticated.** Fine for fixture
content; do not screenshot anything that should stay private.

## Notes

- `scripts/render_test_report.py` is the only Python in this repo and runs in CI
  only. A Go port would remove the odd dependency out if that is preferred.
- The workflow itself knows nothing about how hostthis is wired. It runs one
  make target and renders whatever comes back.
