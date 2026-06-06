#!/usr/bin/env bash
# Smoke-test every spec'd ssh verb against a deployed hostthis instance.
# Designed to be run after `make deploy` against a live URL.
#
#   ./scripts/smoke.sh                      # uses HOSTTHIS_HOST=hostthis.dev
#   HOSTTHIS_HOST=staging.example.com ./scripts/smoke.sh
#
# Generates a throwaway ed25519 key under /tmp, uploads two pastes,
# runs every verb against them, asserts the expected output / http
# status at each step, and cleans up everything it created.
#
# Exit codes:
#   0  every verb passed
#   1  any verb failed (script logs which one)

set -u  # don't set -e — we want to keep going on individual failures
        # and report a summary at the end

HOST="${HOSTTHIS_HOST:-hostthis.dev}"
KEY="$(mktemp -u /tmp/hostthis-smoke-XXXXXX)"
SSH="ssh -i $KEY -o StrictHostKeyChecking=no -o IdentitiesOnly=yes"

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
  # Best-effort delete of any pastes created.
  [ -f /tmp/hostthis-smoke.slugs ] || return 0
  while IFS= read -r slug; do
    $SSH "$HOST" delete "$slug" >/dev/null 2>&1 || true
  done < /tmp/hostthis-smoke.slugs
  rm -f /tmp/hostthis-smoke.slugs "$KEY" "$KEY.pub"
}

step "setup: generating throwaway ed25519 key"
ssh-keygen -t ed25519 -f "$KEY" -q -N "" -C "smoke-$$"
> /tmp/hostthis-smoke.slugs

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
SLUG1=$(echo "$URL1" | sed -E 's|https://([^.]+)\.[^/]+|\1|')
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
SLUG2=$(echo "$URL2" | sed -E 's|https://([^.]+)\.[^/]+|\1|')
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
update_out=$(echo '<!doctype html><h1>smoke 1 — v2</h1>' | $SSH "$HOST" "$SLUG1" 2>&1)
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
upd_pinned=$(echo '<!doctype html><h1>smoke 1 — v3 while pinned</h1>' | $SSH "$HOST" "$SLUG1" 2>&1)
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

# ---- 9. show (over ssh) ----------------------------------------------------
step "show (owner read over ssh)"
show_out=$($SSH "$HOST" show "$SLUG1" 2>&1)
echo "$show_out" | grep -q "smoke 1" \
  && ok "show prints content" \
  || bad "show" "$show_out"

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
echo "$unk" | grep -q "unknown command" && echo "$unk" | grep -q "hostthis — pipe" \
  && ok "unknown verb prints help" \
  || bad "unknown verb" "$unk"

# ---- 14. help directly -----------------------------------------------------
step "explicit help"
hlp=$($SSH "$HOST" help 2>&1)
echo "$hlp" | grep -q "ssh hostthis.dev list" \
  && ok "help lists verbs" \
  || bad "help" "$hlp"

# ---- 15. session without a key is rejected ---------------------------------
step "no-key session is rejected"
nokey=$(ssh -o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o PreferredAuthentications=password \
        -o PubkeyAuthentication=no -- "$HOST" whoami 2>&1; true)
echo "$nokey" | grep -q "ssh key required" \
  && ok "no-key session refused" \
  || bad "no-key rejection" "$nokey"

# ---- summary ---------------------------------------------------------------
printf "\n"
printf "%s %d / %s %d\n" "$(green PASS)" "$PASS" "$(red FAIL)" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  printf "Failed steps:\n"
  for f in "${FAILED[@]}"; do printf "  - %s\n" "$f"; done
  exit 1
fi
exit 0
