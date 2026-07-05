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
# Persistent key so repeated smokes reuse one identity instead of minting
# a fresh key each run (which exhausts the server's per-subnet new-key
# gate). Override the path with HOSTTHIS_SMOKE_KEY.
KEY="${HOSTTHIS_SMOKE_KEY:-$HOME/.config/hostthis/smoke_id_ed25519}"
SSH="ssh -i $KEY -o StrictHostKeyChecking=no -o IdentitiesOnly=yes"

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

PASS=0
FAIL=0
FAILED=()

red()    { printf "\033[31m%s\033[0m" "$*"; }
green()  { printf "\033[32m%s\033[0m" "$*"; }
yellow() { printf "\033[33m%s\033[0m" "$*"; }

step() { printf "[%s] %s\n" "$(yellow "····")" "$*"; }
ok()   { PASS=$((PASS+1)); printf "[%s] %s\n" "$(green "PASS")" "$*"; }
bad()  { FAIL=$((FAIL+1)); FAILED+=("$1"); printf "[%s] %s\n" "$(red "FAIL")" "$1"; [ -n "${2:-}" ] && printf "       %s\n" "$2"; }

trap 'cleanup' EXIT
cleanup() {
  # Best-effort delete of any pastes created. `ssh -n` keeps ssh from
  # slurping the loop's stdin (the slug list) - without it only the
  # first slug is deleted.
  [ -f /tmp/hostthis-smoke.slugs ] || return 0
  while IFS= read -r slug; do
    $SSH -n "$HOST" delete "$slug" >/dev/null 2>&1 || true
  done < /tmp/hostthis-smoke.slugs
  # Keep the persistent key ($KEY) for reuse; only drop the slug list.
  rm -f /tmp/hostthis-smoke.slugs
}

if [ -f "$KEY" ]; then
  step "setup: reusing persistent ssh key ($KEY)"
else
  step "setup: generating persistent ed25519 key ($KEY)"
  mkdir -p "$(dirname "$KEY")"
  ssh-keygen -t ed25519 -f "$KEY" -q -N "" -C "hostthis-smoke"
fi
> /tmp/hostthis-smoke.slugs

# A reused key may still own pastes from a prior run that died before its
# cleanup ran. Delete them so the "active: 0" precondition below holds.
# `list` output is space-padded columns (header on line 1); field 1 is the
# slug, so extract with awk rather than assuming a tab delimiter.
step "setup: clearing any pastes left by a prior run"
$SSH "$HOST" list 2>/dev/null | tail -n +2 | awk '{print $1}' | while IFS= read -r s; do
  [ -n "$s" ] && $SSH -n "$HOST" delete "$s" >/dev/null 2>&1 || true
done

# ---- 1. whoami (pre-upload) ------------------------------------------------
step "whoami (expect active: 0)"
whoami_out=$($SSH "$HOST" whoami 2>&1)
if echo "$whoami_out" | grep -q "^active:  0 paste"; then
  ok "whoami shows 0 active"
else
  bad "whoami pre-upload" "$whoami_out"
fi

# ---- 2. upload HTML with --name --------------------------------------------
step "upload HTML with --name"
URL1=$(echo '<!doctype html><h1>smoke 1</h1>' | \
  ssh -i "$KEY" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -- \
    "$HOST" '--name "smoke html"' 2>/dev/null | head -1)
SLUG1=$(slug_from_url "$URL1")
if [ -z "$URL1" ]; then
  bad "upload HTML (--name)" "no URL emitted"
else
  echo "$SLUG1" >> /tmp/hostthis-smoke.slugs
  ok "upload HTML → $URL1"
fi

# ---- 3. upload Markdown ----------------------------------------------------
step "upload Markdown"
URL2=$(printf '# Smoke MD\n\nbody\n' | \
  ssh -i "$KEY" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -- \
    "$HOST" '--name "smoke md"' 2>/dev/null | head -1)
SLUG2=$(slug_from_url "$URL2")
if [ -z "$URL2" ]; then
  bad "upload Markdown" "no URL emitted"
else
  echo "$SLUG2" >> /tmp/hostthis-smoke.slugs
  ok "upload Markdown → $URL2"
fi

# ---- 4. HTTP fetch both ----------------------------------------------------
step "HTTP GET both pastes"
code1=$(curl -sS -o /dev/null -w "%{http_code}" "$URL1")
code2=$(curl -sS -o /dev/null -w "%{http_code}" "$URL2")
[ "$code1" = "200" ] && ok "HTML serves 200" || bad "HTML HTTP" "got $code1"
[ "$code2" = "200" ] && ok "Markdown serves 200" || bad "MD HTTP" "got $code2"

# ---- 5. list ---------------------------------------------------------------
step "list"
list_out=$($SSH "$HOST" list 2>&1)
echo "$list_out" | grep -q "$SLUG1" && echo "$list_out" | grep -q "$SLUG2" \
  && ok "list contains both slugs" \
  || bad "list" "$list_out"

# ---- 6. update HTML → v2 ---------------------------------------------------
step "update HTML to v2"
update_out=$(echo '<!doctype html><h1>smoke 1 - v2</h1>' | $SSH "$HOST" "$SLUG1" 2>&1)
echo "$update_out" | grep -q "^v2" && ok "update creates v2" \
  || bad "update" "$update_out"

# ---- 7. versions -----------------------------------------------------------
step "versions"
ver_out=$($SSH "$HOST" versions "$SLUG1" 2>&1)
echo "$ver_out" | grep -q "^v2.*current" && echo "$ver_out" | grep -q "^v1" \
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
list_after=$($SSH "$HOST" list 2>&1)
echo "$list_after" | grep -q "smoke md renamed" \
  && ok "rename reflected in list" \
  || bad "rename" "$list_after"

# ---- 11. whoami (post-upload) ----------------------------------------------
step "whoami (expect active: 2)"
whoami2=$($SSH "$HOST" whoami 2>&1)
echo "$whoami2" | grep -q "^active:  2 paste" \
  && ok "whoami shows 2 active" \
  || bad "whoami post-upload" "$whoami2"

# ---- 12. delete + verify 404 -----------------------------------------------
step "delete + verify 404"
$SSH "$HOST" delete "$SLUG1" >/dev/null 2>&1
code_after=$(curl -sS -o /dev/null -w "%{http_code}" "$URL1")
[ "$code_after" = "404" ] \
  && ok "delete makes URL 404" \
  || bad "delete" "URL serves $code_after"
# strip SLUG1 from cleanup list since we already deleted it
grep -v "^$SLUG1$" /tmp/hostthis-smoke.slugs > /tmp/hostthis-smoke.slugs.new
mv /tmp/hostthis-smoke.slugs.new /tmp/hostthis-smoke.slugs

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

# ---- 14a. per-verb help: help get ------------------------------------------
# `help <verb>` emits the verb's descriptor (signature + description +
# examples) instead of the global banner. The descriptor carries a
# "Usage:" line the global banner lacks, so checking for the verb name
# plus "Usage:" reliably distinguishes verb help from the global help.
step "help get (per-verb help)"
help_get=$($SSH "$HOST" help get 2>&1)
help_get_rc=$?
echo "$help_get" | grep -q "get" && echo "$help_get" | grep -q "Usage:" \
  && [ "$help_get_rc" -eq 0 ] \
  && ok "help get emits verb-specific help" \
  || bad "help get" "rc=$help_get_rc out=$help_get"

# ---- 14b. per-verb help: get --help byte-matches help get ------------------
# `<verb> --help` and `<verb> -h` are routed through the same descriptor
# lookup as `help <verb>`, so all three forms should produce identical
# bytes on stderr.
step "get --help matches help get"
get_dashdash=$($SSH "$HOST" get --help 2>&1)
[ "$get_dashdash" = "$help_get" ] \
  && ok "get --help byte-matches help get" \
  || bad "get --help" "got: $get_dashdash"

# ---- 14c. per-verb help: get -h byte-matches help get ----------------------
step "get -h matches help get"
get_h=$($SSH "$HOST" get -h 2>&1)
[ "$get_h" = "$help_get" ] \
  && ok "get -h byte-matches help get" \
  || bad "get -h" "got: $get_h"

# ---- 14d. help <unknown> → unknown-verb message + global banner ------------
# `help <unknown>` prefixes an `unknown verb` line and then emits the
# global banner, exiting 0 (the user asked for help, so they get help).
step "help unknown → unknown-verb + global banner"
help_unk=$($SSH "$HOST" help notarealverb 2>&1)
help_unk_rc=$?
echo "$help_unk" | grep -q "unknown verb" \
  && echo "$help_unk" | grep -q "UPDATE & MANAGE" \
  && [ "$help_unk_rc" -eq 0 ] \
  && ok "help unknown shows banner with prefix, exit 0" \
  || bad "help unknown" "rc=$help_unk_rc out=$help_unk"

# ---- 15. session without a key is rejected ---------------------------------
step "no-key session is rejected"
nokey=$(ssh -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o PreferredAuthentications=password \
        -o PubkeyAuthentication=no -- "$HOST" whoami 2>&1; true)
echo "$nokey" | grep -q "ssh key required" \
  && ok "no-key session refused" \
  || bad "no-key rejection" "$nokey"

# ---- 16. hardening: direct-tcpip channel refused (-W) ---------------------
# Phase C4: the server's LocalPortForwardingCallback returns false, so
# the client's direct-tcpip channel request is refused.
#
# Why -W not -L: with -L the ssh client only opens the direct-tcpip
# channel WHEN TRAFFIC FLOWS through the local listener; a session that
# doesn't push bytes never triggers the server-side check. -W asks ssh
# to use stdio as a direct-tcpip channel IMMEDIATELY at session start,
# which forces the server to accept-or-reject before any command runs.
step "ssh -W (direct-tcpip channel) refused"
fwd_l=$(ssh -i "$KEY" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes \
        -W localhost:80 "$HOST" 2>&1 </dev/null)
fwd_l_rc=$?
if [ "$fwd_l_rc" -ne 0 ] && \
   echo "$fwd_l" | grep -qiE "refused|open failed|administratively prohibited|forward"; then
  ok "direct-tcpip refused (rc=$fwd_l_rc)"
else
  bad "ssh -W not refused" "rc=$fwd_l_rc out=$fwd_l"
fi

# ---- 17. hardening: reverse port-forward refused (-R) ---------------------
# ReversePortForwardingCallback returns false, so the `tcpip-forward`
# global request is rejected at session start. ExitOnForwardFailure=yes
# guarantees ssh exits non-zero in that case.
step "ssh -R (reverse forward) refused"
fwd_r=$(ssh -i "$KEY" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes \
        -o ExitOnForwardFailure=yes \
        -R 19998:localhost:80 -- "$HOST" whoami 2>&1)
fwd_r_rc=$?
if [ "$fwd_r_rc" -ne 0 ] && \
   echo "$fwd_r" | grep -qiE "refused|open failed|administratively prohibited|forward"; then
  ok "reverse forward refused (rc=$fwd_r_rc)"
else
  bad "ssh -R not refused" "rc=$fwd_r_rc out=$fwd_r"
fi

# ---- 18. hardening: subsystem (sftp) refused ------------------------------
# SessionRequestCallback returns false for "subsystem", so sftp's
# subsystem handshake fails. BatchMode=yes prevents sftp from hanging
# on a password prompt if auth somehow fell through.
step "sftp subsystem refused"
sftp_out=$(sftp -i "$KEY" -o StrictHostKeyChecking=no -o IdentitiesOnly=yes \
           -o BatchMode=yes -b /dev/null "$HOST" 2>&1)
sftp_rc=$?
if [ "$sftp_rc" -ne 0 ] && \
   echo "$sftp_out" | grep -qiE "subsystem|refused|received remote disconnect|connection closed"; then
  ok "sftp subsystem refused (rc=$sftp_rc)"
else
  bad "sftp not refused" "rc=$sftp_rc out=$sftp_out"
fi

# ---- summary ---------------------------------------------------------------
printf "\n"
printf "%s %d / %s %d\n" "$(green PASS)" "$PASS" "$(red FAIL)" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf "Failed steps:\n"
  for f in "${FAILED[@]}"; do printf "  - %s\n" "$f"; done
  exit 1
fi
exit 0
