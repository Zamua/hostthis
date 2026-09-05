#!/usr/bin/env bash
# Smoke-test every spec'd ssh verb against a deployed hostthis instance.
# Run via `make smoke` against a live URL, or invoked post-deploy by
# the operator-side deploy tooling (which lives outside this repo).
#
#   ./scripts/smoke.sh                                   # production: HOSTTHIS_HOST=hostthis.dev (subdomain mode)
#   HOSTTHIS_HOST=staging.example.com ./scripts/smoke.sh
#   HOSTTHIS_HOST=hostthis-local ./scripts/smoke.sh      # local dev compose (path mode)
#
# Works against either URL_MODE the server is configured with:
#   subdomain  → upload returns https://<slug>.<apex>/    (production shape)
#   path       → upload returns http(s)://<apex>/p/<slug> (dev compose shape)
# The mode is auto-detected from the URL the first upload prints; the
# script never needs to know which mode the server is in.
#
# Reuses a persistent ed25519 key (HOSTTHIS_SMOKE_KEY, default
# ~/.config/hostthis/smoke_id_ed25519; generated on first run), uploads
# two pastes, runs every verb against them, asserts the expected output /
# http status at each step, and cleans up the pastes it created (the key
# is kept). Reusing one identity keeps the server's per-subnet new-key
# gate from being exhausted: the key is admitted once per subnet, then
# reused, instead of minting a fresh key (and burning a slot) every run.
#
# Exit codes:
#   0  every verb passed
#   1  any verb failed (script logs which one)

set -u  # don't set -e - we want to keep going on individual failures
        # and report a summary at the end

HOST="${HOSTTHIS_HOST:-hostthis.dev}"
# Structured assertions consume `-o json` output (list/versions/whoami)
# rather than scraping the human tables, so jq is required.
command -v jq >/dev/null 2>&1 || { echo "smoke.sh requires jq" >&2; exit 1; }
# Persistent key so repeated smokes reuse one identity instead of minting
# a fresh key each run (which exhausts the server's per-subnet new-key
# gate). Override the path with HOSTTHIS_SMOKE_KEY.
KEY="${HOSTTHIS_SMOKE_KEY:-$HOME/.config/hostthis/smoke_id_ed25519}"
# A deploy fronts ssh on 22; `make run` uses 2222. Without this the suite can
# only ever be run against a deploy, which is the wrong way round for a check
# a contributor should be able to run before pushing.
SSH_PORT="${HOSTTHIS_SSH_PORT:-22}"
SSH="ssh -p $SSH_PORT -i $KEY -o StrictHostKeyChecking=no -o IdentitiesOnly=yes"

# slug_from_url extracts the 8-char slug from a hostthis URL. Handles
# both URL shapes the server can emit:
#   subdomain mode: https://<slug>.apex.tld/      → take chars before first dot
#   path mode:      http(s)://host[:port]/p/<slug> → take chars after last "/"
# Slug is always exactly 8 chars (domain.SlugLength), so we use that as
# the disambiguator: strip scheme, then look at the LAST path segment
# (path mode) OR the FIRST hostname label (subdomain mode), and return
# whichever is 8 chars.
slug_from_url() {
  local url="$1"
  # Strip scheme.
  local rest="${url#http://}"
  rest="${rest#https://}"
  # If the path contains "/p/<slug>", it's path mode: take the last segment.
  if [[ "$rest" == */p/* ]]; then
    printf '%s' "${rest##*/}"
    return
  fi
  # Otherwise subdomain mode: slug is the first DNS label.
  printf '%s' "${rest%%.*}"
}

# A transcript of the whole run, written before the first step so a failure is
# diagnosable after the fact rather than only while someone is watching it.
#
# The FILE gets everything: each step's output, and an xtrace line carrying the
# exact command, its expansion, a timestamp and a source line. The CONSOLE gets
# the same minus the xtrace, which would otherwise bury the result.
#
# The split is done by filtering at the tee rather than with BASH_XTRACEFD,
# which needs bash 4.1 and is SILENTLY IGNORED on the bash 3.2 that macOS
# ships - it sends the trace to stderr instead, burying the console with no
# error to say so.
SMOKE_LOG="${SMOKE_LOG:-${TMPDIR:-/tmp}/hostthis-smoke-$(printf '%s' "$HOST" | tr -c 'A-Za-z0-9._-' '_')-$(date -u +%Y%m%dT%H%M%SZ).log}"
mkdir -p "$(dirname "$SMOKE_LOG")" 2>/dev/null || true
exec > >(tee -a "$SMOKE_LOG" | grep -Ev --line-buffered '^\++\[trace\]') 2>&1
PS4='+[trace] $(date -u +%H:%M:%S) ${BASH_SOURCE##*/}:${LINENO}: '
set -x

PASS=0
FAIL=0
FAILED=()

red()    { printf "\033[31m%s\033[0m" "$*"; }
green()  { printf "\033[32m%s\033[0m" "$*"; }
yellow() { printf "\033[33m%s\033[0m" "$*"; }

step() { printf "[%s] %s\n" "$(yellow "····")" "$*"; }
ok()   { PASS=$((PASS+1)); printf "[%s] %s\n" "$(green "PASS")" "$*"; }
bad()  { FAIL=$((FAIL+1)); FAILED+=("$1"); printf "[%s] %s\n" "$(red "FAIL")" "$1"; [ -n "${2:-}" ] && printf "       %s\n" "$2"; }

# upload pipes stdin to the upload verb; extra args (--name, --type, a slug)
# pass through.
upload() { $SSH -- "$HOST" "$@"; }

# mint uploads stdin as a new paste and sets MINT_URL / MINT_SLUG. Called with
# a redirect, never in a pipeline, so the variables land in this shell. A
# missing URL fails <label> and returns 1.
mint() {
  local label="$1"; shift
  MINT_URL=$(upload "$@" 2>/dev/null | head -1)
  MINT_SLUG=$(slug_from_url "$MINT_URL")
  if [ -z "$MINT_SLUG" ]; then
    bad "$label" "no URL emitted"
    return 1
  fi
  ok "$label → $MINT_URL"
}

# smoke_delete removes one paste, best-effort but never silently: cleanup must
# not fail the run, yet a swallowed delete failure is how a stranded paste sits
# invisible across runs. Any failure prints a loud line with the slug and exit
# code, so the transcript shows what was left behind.
smoke_delete() {
  local slug="$1" rc=0
  $SSH -n "$HOST" delete "$slug" >/dev/null 2>&1 || rc=$?
  if [ "$rc" -ne 0 ]; then
    printf 'WARN: cleanup delete %s failed (exit %d); the paste may be stranded in the smoke account\n' "$slug" "$rc"
  fi
  return 0
}

# delete_all_pastes empties the smoke account. It holds only smoke artifacts,
# so `list -ojson` is the complete inventory: it covers sites (a directory is
# a paste, kind "site") and a site deletes through the same delete verb. `ssh
# -n` (inside smoke_delete) keeps ssh from slurping the loop's stdin; without
# it only the first slug is deleted.
delete_all_pastes() {
  $SSH -n "$HOST" list -ojson 2>/dev/null | jq -r '.[].slug' 2>/dev/null | \
  while IFS= read -r slug; do
    [ -n "$slug" ] && smoke_delete "$slug"
  done
  return 0
}

trap 'cleanup' EXIT
cleanup() {
  # Best-effort: the persistent key ($KEY) is kept for reuse.
  delete_all_pastes
  return 0
}

if [ -f "$KEY" ]; then
  step "setup: reusing persistent ssh key ($KEY)"
else
  step "setup: generating persistent ed25519 key ($KEY)"
  mkdir -p "$(dirname "$KEY")"
  ssh-keygen -t ed25519 -f "$KEY" -q -N "" -C "hostthis-smoke"
fi

# A reused key may still own pastes from a prior run that died before its
# cleanup ran. Delete them so the "active: 0" precondition below holds.
step "setup: clearing any pastes left by a prior run"
delete_all_pastes

# ---- 1. whoami (pre-upload) ------------------------------------------------
step "whoami (expect active_pastes: 0)"
whoami_out=$($SSH "$HOST" whoami -ojson 2>&1)
if echo "$whoami_out" | jq -e '.active_pastes == 0' >/dev/null 2>&1; then
  ok "whoami shows 0 active"
else
  bad "whoami pre-upload" "$whoami_out"
fi

# ---- 2. upload HTML with --name --------------------------------------------
step "upload HTML with --name"
mint "upload HTML" '--name "smoke html"' <<< '<!doctype html><h1>smoke 1</h1>'
URL1="$MINT_URL"; SLUG1="$MINT_SLUG"

# ---- 3. upload Markdown ----------------------------------------------------
step "upload Markdown"
mint "upload Markdown" '--name "smoke md"' < <(printf '# Smoke MD\n\nbody\n')
URL2="$MINT_URL"; SLUG2="$MINT_SLUG"

# ---- 4. HTTP fetch both ----------------------------------------------------
step "HTTP GET both pastes"
code1=$(curl -sS -o /dev/null -w "%{http_code}" "$URL1")
code2=$(curl -sS -o /dev/null -w "%{http_code}" "$URL2")
[ "$code1" = "200" ] && ok "HTML serves 200" || bad "HTML HTTP" "got $code1"
[ "$code2" = "200" ] && ok "Markdown serves 200" || bad "MD HTTP" "got $code2"

# ---- 5. list ---------------------------------------------------------------
step "list"
list_out=$($SSH "$HOST" list -ojson 2>&1)
echo "$list_out" | jq -e --arg a "$SLUG1" --arg b "$SLUG2" \
    '(map(.slug) | contains([$a, $b]))' >/dev/null 2>&1 \
  && ok "list contains both slugs" \
  || bad "list" "$list_out"

# ---- 6. update HTML → v2 ---------------------------------------------------
step "update HTML to v2"
update_out=$(echo '<!doctype html><h1>smoke 1 - v2</h1>' | $SSH "$HOST" "$SLUG1" 2>&1)
echo "$update_out" | grep -q "^v2" && ok "update creates v2" \
  || bad "update" "$update_out"

# ---- 7. versions -----------------------------------------------------------
step "versions"
ver_out=$($SSH "$HOST" versions "$SLUG1" -ojson 2>&1)
echo "$ver_out" | jq -e \
    '(.versions | any(.version == 2 and .current)) and (.versions | any(.version == 1))' \
    >/dev/null 2>&1 \
  && ok "versions lists v1 + v2 (v2 current)" \
  || bad "versions" "$ver_out"

# ---- 8. pin v1 + verify served bytes ---------------------------------------
step "pin v1"
$SSH "$HOST" pin "$SLUG1" 1 >/dev/null 2>&1
body=$(curl -sS "$URL1")
echo "$body" | grep -q "smoke 1" && ! echo "$body" | grep -q "v2" \
  && ok "pin v1 rolls back served bytes" \
  || bad "pin v1" "served: $body"

# ---- 8b. update while pinned holds pin + warns -----------------------------
step "update while pinned (pin should hold)"
upd_pinned=$(echo '<!doctype html><h1>smoke 1 - v3 while pinned</h1>' | $SSH "$HOST" "$SLUG1" 2>&1)
echo "$upd_pinned" | grep -q "pinned to v1" \
  && ok "update warns about active pin" \
  || bad "update while pinned (stderr warning)" "$upd_pinned"
body_after=$(curl -sS "$URL1")
echo "$body_after" | grep -q "smoke 1" && ! echo "$body_after" | grep -q "v3" \
  && ok "URL still serves v1 after pinned update" \
  || bad "pinned URL serves v1" "$body_after"

# ---- 8c. unpin → URL now serves the new latest -----------------------------
step "unpin (URL should jump to v3)"
$SSH "$HOST" unpin "$SLUG1" >/dev/null 2>&1
body_unpinned=$(curl -sS "$URL1")
echo "$body_unpinned" | grep -q "v3" \
  && ok "unpin rolls URL forward to latest" \
  || bad "unpin" "served: $body_unpinned"

# ---- 9. get (over ssh) -----------------------------------------------------
step "get (owner read over ssh)"
get_out=$($SSH "$HOST" get "$SLUG1" 2>&1)
echo "$get_out" | grep -q "smoke 1" \
  && ok "get prints content" \
  || bad "get" "$get_out"

# ---- 10. rename ------------------------------------------------------------
step "rename markdown paste"
$SSH "$HOST" "rename $SLUG2 \"smoke md renamed\"" >/dev/null 2>&1
list_after=$($SSH "$HOST" list -ojson 2>&1)
echo "$list_after" | jq -e --arg s "$SLUG2" \
    'any(.[]; .slug == $s and .name == "smoke md renamed")' >/dev/null 2>&1 \
  && ok "rename reflected in list" \
  || bad "rename" "$list_after"

# ---- 11. whoami (post-upload) ----------------------------------------------
step "whoami (expect active_pastes: 2)"
whoami2=$($SSH "$HOST" whoami -ojson 2>&1)
echo "$whoami2" | jq -e '.active_pastes == 2' >/dev/null 2>&1 \
  && ok "whoami shows 2 active" \
  || bad "whoami post-upload" "$whoami2"

# ---- 11b. every renderable kind -------------------------------------------
# One upload per accepted kind, checking the things that can silently go
# wrong independently of each other: the kind the GATE assigned, the SHELL the
# bare URL serves, the Content-Type of ?raw, and for diff the deep-link
# resolver the shell must also load (line anchors hang off it). A kind can be
# detected right and served by the wrong viewer, or served by the right viewer
# with a Content-Type that makes a browser download it instead.
step "every renderable kind: detect, shell, raw type"

# kind|shell asset the page must load|expected ?raw Content-Type prefix|extra asset
kind_specs='mermaid|/_hostthis/mermaid.js|text/plain|
csv|/_hostthis/data.js|text/plain|
json|/_hostthis/data.js|application/json|
pdf|/_hostthis/pdf.js|application/pdf|
flamegraph|/_hostthis/flame.js|text/plain|
log|/_hostthis/log.js|application/x-ndjson|
text|/_hostthis/text.js|text/plain|
diff|/_hostthis/diff.js|text/plain|/_hostthis/deeplink.js'

kind_body() {
  case "$1" in
    mermaid) printf 'flowchart TD\n  A[start] --> B[end]\n' ;;
    csv)     printf 'region,rep,units\nnorth,ada,4\nsouth,grace,5\neast,alan,6\n' ;;
    json)    printf '{"service":"api","counts":{"info":1,"error":2}}\n' ;;
    # Smallest structurally-valid PDF: the gate keys on the signature, and the
    # viewer is exercised by the browser tests, not here.
    pdf)     printf '%%PDF-1.4\n1 0 obj\n<</Type/Catalog>>\nendobj\ntrailer<</Root 1 0 R>>\n%%%%EOF\n' ;;
    flamegraph) printf 'main;serve;read 41\nmain;serve;write 18\nmain;gc 6\n' ;;
    log)     printf '{"@timestamp":"2026-08-02T03:00:00Z","level":"INFO","message":"a"}\n{"@timestamp":"2026-08-02T03:00:01Z","level":"ERROR","message":"b"}\n' ;;
    # No markdown cue anywhere: the point is that this reaches the fallback.
    text)    printf 'server {\n  listen 443;\n}\n' ;;
    diff)    printf -- '--- a/x\n+++ b/x\n@@ -1,4 +1,4 @@\n ctx\n-old one\n+new one\n ctx2\n' ;;
  esac
}

while IFS='|' read -r kind want_shell want_ct want_extra; do
  [ -z "$kind" ] && continue
  mint "kind $kind: upload" < <(kind_body "$kind") || continue
  url="$MINT_URL"; slug="$MINT_SLUG"

  # -n: without it this ssh swallows the loop's here-string and only the
  # first kind is ever checked.
  got_kind=$($SSH -n "$HOST" list -ojson 2>/dev/null \
    | jq -r --arg s "$slug" '.[] | select(.slug==$s) | .kind')
  [ "$got_kind" = "$kind" ] \
    && ok "kind $kind: detected" \
    || bad "kind $kind: detection" "stored as '${got_kind:-?}'"

  page=$(curl -sS "$url")
  case "$page" in
    *"$want_shell"*) ok "kind $kind: serves its viewer" ;;
    *) bad "kind $kind: viewer" "page does not load $want_shell" ;;
  esac
  if [ -n "$want_extra" ]; then
    case "$page" in
      *"$want_extra"*) ok "kind $kind: loads $want_extra" ;;
      *) bad "kind $kind: extra asset" "page does not load $want_extra" ;;
    esac
  fi

  ct=$(curl -sS -o /dev/null -w '%{content_type}' "$url?raw=1")
  case "$ct" in
    "$want_ct"*) ok "kind $kind: raw is $want_ct" ;;
    *) bad "kind $kind: raw Content-Type" "got '$ct', want '$want_ct'" ;;
  esac
done <<< "$kind_specs"

# ---- 11c. a markdown doc quoting a diff stays markdown ---------------------
# The gate matches a hunk header anywhere in the prefix; a fenced diff inside
# prose must not classify the whole paste as a diff. The fence wins.
step "markdown quoting a diff is markdown, not diff"
if mint "quoted-diff upload" < <(printf '# Review\n\nThe change:\n\n```diff\n--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n-old\n+new\n```\n'); then
  qd_kind=$($SSH "$HOST" list -ojson 2>/dev/null | jq -r --arg s "$MINT_SLUG" '.[] | select(.slug==$s) | .kind')
  [ "$qd_kind" = "markdown" ] \
    && ok "quoted diff stays markdown" \
    || bad "quoted diff kind" "stored as '${qd_kind:-?}', want markdown: its prose would be served as diff noise"
fi

# ---- 11d. an unsupported type is still refused -----------------------------
# The gate accepts many kinds; it must not have become a catch-all.
step "unsupported content is still rejected"
# The xtrace of upload's own body lands in the captured stderr; drop it.
rej=$(printf '\x00\x01\x02binary junk\x00\xff' | upload 2>&1 | grep -Ev '^\++\[trace\]' | head -2)
case "$rej" in
  *"only accepts"*) ok "binary refused with the format message" ;;
  *hostthis.dev*|*"$HOST"*) bad "binary accepted" "the gate became a catch-all: $rej" ;;
  *) ok "binary refused" ;;
esac

# ---- 12. delete + verify 404 -----------------------------------------------
step "delete + verify 404"
$SSH "$HOST" delete "$SLUG1" >/dev/null 2>&1
code_after=$(curl -sS -o /dev/null -w "%{http_code}" "$URL1")
[ "$code_after" = "404" ] \
  && ok "delete makes URL 404" \
  || bad "delete" "URL serves $code_after"

# ---- 13. unknown verb → help -----------------------------------------------
step "unknown verb → help"
unk=$($SSH "$HOST" notarealverb 2>&1; true)
echo "$unk" | grep -q "unknown command" && echo "$unk" | grep -q "Pipe a rendered file" \
  && ok "unknown verb prints help" \
  || bad "unknown verb" "$unk"

# ---- 14. help directly -----------------------------------------------------
step "explicit help"
hlp=$($SSH "$HOST" help 2>&1)
# Assert on a fragment that's identical in every deploy regardless of
# the configured apex domain. The verb table in helpText() lives under
# the "UPDATE & MANAGE" heading and is the same string for every host.
echo "$hlp" | grep -q "UPDATE & MANAGE" && echo "$hlp" | grep -q " list " \
  && ok "help lists verbs" \
  || bad "help" "$hlp"

# ---- 14b. streamed upload: large body, and an aborted one ------------------
# The write path is constant-memory. Incompressible input on purpose: zstd
# must not be able to shrink the work and hide a buffer. The size is
# deliberately larger than any inline path.
#
# This asserts the upload SUCCEEDS and reads back intact. It cannot see the
# server's memory - that is measured by the operator-side deploy check, which
# samples RSS while this runs.
step "streamed upload: ${SMOKE_STREAM_MB:-8} MiB incompressible body"
stream_mb="${SMOKE_STREAM_MB:-8}"
stream_dir="$(mktemp -d)"
mkdir -p "$stream_dir/site"
dd if=/dev/urandom of="$stream_dir/site/big.bin" bs=1048576 count="$stream_mb" 2>/dev/null
printf '<!doctype html><h1>stream</h1>' > "$stream_dir/site/index.html"
stream_url=$(tar czf - -C "$stream_dir" site 2>/dev/null | $SSH "$HOST" 2>/dev/null | grep -oE 'https?://[^ ]+' | head -1)
if [ -n "$stream_url" ]; then
  ok "streamed ${stream_mb} MiB upload accepted"
  got=$(curl -s -o /dev/null -w '%{size_download}' "${stream_url%/}/big.bin" 2>/dev/null)
  want=$((stream_mb * 1048576))
  [ "$got" = "$want" ] \
    && ok "streamed body reads back intact ($got bytes)" \
    || bad "streamed body round-trip" "read back $got bytes, want $want"
else
  bad "streamed upload" "no URL returned for a ${stream_mb} MiB body"
fi

# An upload killed mid-flight must not wedge the service or serve a partial
# site. The bytes it staged are left for the reclamation sweep, which is the
# designed outcome - this asserts the SERVICE survives, not that nothing leaks.
step "aborted upload does not wedge the service"
(tar czf - -C "$stream_dir" site 2>/dev/null; :) | $SSH "$HOST" >/dev/null 2>&1 &
abort_pid=$!
sleep 2
kill -9 $abort_pid 2>/dev/null
wait $abort_pid 2>/dev/null
rm -rf "$stream_dir"
if $SSH "$HOST" whoami >/dev/null 2>&1; then
  ok "service healthy after an aborted upload"
else
  bad "aborted upload" "service did not answer whoami afterwards"
fi

# ---- 15. session without a key is rejected ---------------------------------
step "no-key session is rejected"
nokey=$(ssh -p "$SSH_PORT" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o PreferredAuthentications=password \
        -o PubkeyAuthentication=no -- "$HOST" whoami 2>&1; true)
echo "$nokey" | grep -q "ssh key required" \
  && ok "no-key session refused" \
  || bad "no-key rejection" "$nokey"

# ---- latency gate ----------------------------------------------------------
# Behaviour and LATENCY are separate failure modes: a verb can be perfectly
# correct and operationally broken, so a deploy is not verified until both are
# checked.
#
# Budgets are per-verb wall clock over a fresh SSH connection - what a user
# actually waits for, not server-side processing time. SMOKE_LATENCY_BUDGET_MS
# raises the ceiling for a deliberately slow environment; SMOKE_SKIP_LATENCY=1
# skips the gate entirely, which should be rare and never on a deploy.
LAT_BUDGET_MS="${SMOKE_LATENCY_BUDGET_MS:-6000}"
LAT_WARN_MS="${SMOKE_LATENCY_WARN_MS:-3000}"

if [ "${SMOKE_SKIP_LATENCY:-0}" = "1" ]; then
  step "latency gate SKIPPED (SMOKE_SKIP_LATENCY=1)"
else
  now_ms(){ python3 -c 'import time;print(int(time.time()*1000))'; }
  for v in help whoami list; do
    step "latency: $v"
    t0=$(now_ms)
    ssh -p "$SSH_PORT" -i "$KEY" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o BatchMode=yes \
        -o ConnectTimeout=45 "$HOST" "$v" >/dev/null 2>&1
    t1=$(now_ms); ms=$((t1-t0))
    if [ "$ms" -gt "$LAT_BUDGET_MS" ]; then
      bad "latency: $v over budget" "${ms}ms > ${LAT_BUDGET_MS}ms"
    elif [ "$ms" -gt "$LAT_WARN_MS" ]; then
      ok "latency: $v ${ms}ms (SLOW, over ${LAT_WARN_MS}ms warn)"
    else
      ok "latency: $v ${ms}ms"
    fi
  done
fi

# ---- summary ---------------------------------------------------------------
printf "\n"
printf "%s %d / %s %d\n" "$(green PASS)" "$PASS" "$(red FAIL)" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf "Failed steps:\n"
  for f in "${FAILED[@]}"; do printf "  - %s\n" "$f"; done
  printf "\nFull transcript, with the exact command and output for each step:\n  %s\n" "$SMOKE_LOG"
  printf "Keep it. A transient failure is only diagnosable from the run that saw it.\n"
  exit 1
fi
printf "transcript: %s\n" "$SMOKE_LOG"
exit 0
