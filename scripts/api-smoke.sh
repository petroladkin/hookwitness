#!/bin/bash
# API scenario: the Basic-credentials recipe of docs/api.md end to end —
# create an endpoint, set a password, require it, send JSON events with
# right and wrong credentials, wait for them and read their verdicts. On a
# server it starts itself (--binary, --image) it then turns login on: the
# first admin, a login, a token, the API with and without it (docs/access.md).
#
# Usage:
#   scripts/api-smoke.sh <base-url>            against a running hookwitness
#   scripts/api-smoke.sh --binary <path>       start the binary on a temporary database
#   scripts/api-smoke.sh --image <image>       start the container image
# Needs curl and jq; prints every call it makes; leaves nothing behind.
set -euo pipefail
PORT=${SMOKE_PORT:-18081}
WORK=$(mktemp -d)
PID= C=
cleanup() {
  [ -n "$PID" ] && kill "$PID" 2>/dev/null && wait "$PID" 2>/dev/null
  [ -n "$C" ] && docker rm -f "$C" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }

OWN=
case "${1:?usage: api-smoke.sh <base-url> | --binary <path> | --image <image>}" in
--binary)
  "${2:?binary}" serve -addr 127.0.0.1:$PORT -db "$WORK/hw.db" >"$WORK/log" 2>&1 &
  PID=$! OWN=1
  HW=http://127.0.0.1:$PORT ;;
--image)
  INSIDE=http://127.0.0.1:8080 # the server's own address inside the container
  C=hw-api-smoke-$$
  docker run -d --name $C -p 127.0.0.1:$PORT:8080 "${2:?image}" >/dev/null
  OWN=1
  HW=http://127.0.0.1:$PORT ;;
*) HW=${1%/} ;;
esac
API=$HW/admin/api/v1
for _ in $(seq 60); do curl -fsS -o /dev/null "$HW/healthz" 2>/dev/null && break; sleep 0.25; done
curl -fsS -o /dev/null "$HW/healthz" || { [ -f "$WORK/log" ] && cat "$WORK/log" >&2; fail "hookwitness is not up at $HW"; }

# call METHOD PATH [JSON] [curl args...]: prints the call and the answer;
# the status goes to $code, the body to $body.
call() {
  local method=$1 path=$2 data=${3:-}
  shift 2; [ $# -gt 0 ] && shift
  local args=(-sS -X "$method" -o "$WORK/body" -w '%{http_code}' -D "$WORK/headers")
  [ -n "$data" ] && args+=(-H 'Content-Type: application/json' -d "$data")
  echo "\$ curl -X $method '$API$path'${data:+ -H 'Content-Type: application/json' -d '$data'}$([ $# -gt 0 ] && printf " '%s'" "$@")"
  code=$(curl "${args[@]}" "$@" "$API$path")
  body=$(cat "$WORK/body")
  echo "$code $(jq -c . <<<"$body" 2>/dev/null || echo "$body")"
  echo
}
expect() { [ "$code" = "$1" ] || fail "$2: status $code, want $1: $body"; }

# 1. An endpoint for the sender under test.
call POST /endpoints '{"path":"/orders/events"}'
expect 201 "create endpoint"
ID=$(jq -r .id <<<"$body"); URL=$(jq -r .url <<<"$body")
ok "endpoint $ID at $URL"

# 2. The password the sender must use, then an enforced Basic check.
call PUT /endpoints/$ID/secrets/ORDERS_PASSWORD '{"kind":"token","value":"smoke-test-password"}'
expect 200 "set the password"
[ "$(jq -r .fingerprint <<<"$body")" != null ] && ! grep -q smoke-test-password "$WORK/body" || fail "secret answer"
call GET /endpoints/$ID/behaviour
TAG=$(grep -i '^etag:' "$WORK/headers" | tr -d '\r' | cut -d' ' -f2)
call PUT /endpoints/$ID/behaviour '{"checks":[{"kind":"basic","secret":"ORDERS_PASSWORD","user":"orders","enforce":true}]}' -H "If-Match: $TAG"
expect 200 "require Basic credentials"
ok "Basic credentials required (user orders)"

# 3. Wait for the first event while the sender delivers it.
call GET "/endpoints/$ID/requests?after=0&wait=10s" > "$WORK/wait" &
waiter=$!
sleep 0.5
echo "\$ curl -u orders:*** -H 'Content-Type: application/json' -d '{\"order\":42,\"status\":\"paid\"}' $URL"
sent=$(curl -sS -o /dev/null -w '%{http_code}' -u orders:smoke-test-password -H 'Content-Type: application/json' \
  -d '{"order":42,"status":"paid"}' "$URL")
echo "$sent"; echo
[ "$sent" = 200 ] || fail "right credentials answered $sent"
wait $waiter
cat "$WORK/wait"
got=$(tail -n 2 "$WORK/wait" | head -n 1)
[ "$(cut -d' ' -f1 <<<"$got")" = 200 ] || fail "wait: $got"
json=${got#200 }
[ "$(jq '.requests | length' <<<"$json")" = 1 ] || fail "wait returned $json"
[ "$(jq -r '.requests[0].verdict' <<<"$json")" = pass ] || fail "verdict of the right credentials: $json"
R1=$(jq -r '.requests[0].id' <<<"$json")
ok "waited for request $R1: 200, verdict pass"

# 4. Read it: the JSON body as sent, the credentials header, the verdict.
call GET /requests/$R1
expect 200 "read request"
[ "$(jq -r '.body_text | fromjson | .order' <<<"$body")" = 42 ] || fail "body: $body"
[ "$(jq -r '[.headers[] | select(.[0] | ascii_downcase == "authorization")] | length' <<<"$body")" = 1 ] || fail "headers: $body"
[ "$(jq -r '.verdicts[0].result' <<<"$body")" = pass ] || fail "verdicts: $body"
ok "request $R1: body order 42, Authorization kept, check passed"

# 5. Wrong credentials: refused with 401 and recorded as fail.
echo "\$ curl -u orders:wrong -H 'Content-Type: application/json' -d '{\"order\":43}' $URL"
sent=$(curl -sS -o /dev/null -w '%{http_code}' -u orders:wrong -H 'Content-Type: application/json' -d '{"order":43}' "$URL")
echo "$sent"; echo
[ "$sent" = 401 ] || fail "wrong credentials answered $sent"
call GET "/endpoints/$ID/requests?after=$R1&wait=10s"
expect 200 "wait for the second"
[ "$(jq -r '.requests[0].verdict + " " + (.requests[0].status|tostring)' <<<"$body")" = "fail 401" ] || fail "second: $body"
ok "wrong credentials: 401, verdict fail"

# 6. Guards: a stale version and a non-JSON write are refused.
call PUT /endpoints/$ID/behaviour '{}' -H "If-Match: $TAG"
expect 412 "stale version"
echo "\$ curl -X POST $API/endpoints -d 'path=/form'"
code=$(curl -sS -o /dev/null -w '%{http_code}' -d 'path=/form' "$API/endpoints")
echo "$code"; echo
[ "$code" = 415 ] || fail "form post answered $code"
ok "stale If-Match 412, form post 415"

# 7. Replay: the first request sent again to a path of this server (the
# Unmatched register), byte for byte: its credentials arrive with it.
call POST /requests/$R1/replays "{\"url\":\"${INSIDE:-$HW}/replayed/orders?from=smoke\"}"
expect 201 "replay"
[ "$(jq -r '.status, .line' <<<"$body" | paste -sd' ' -)" = "404 POST /replayed/orders?from=smoke HTTP/1.1" ] || fail "replay: $body"
call GET "/unmatched/requests?limit=1"
U=$(jq -r '.requests[0].id' <<<"$body")
call GET /requests/$U
[ "$(jq -r '.path + " " + .body_text' <<<"$body")" = '/replayed/orders {"order":42,"status":"paid"}' ] || fail "replayed request: $body"
[ "$(jq -r '[.headers[] | select(.[0] | ascii_downcase == "authorization")] | length' <<<"$body")" = 1 ] || fail "replayed headers: $body"
call POST /requests/$R1/replays '{"url":"http://169.254.169.254/latest/meta-data/"}'
expect 422 "a metadata target"
[ "$(jq -r .error.code <<<"$body")" = target_refused ] || fail "metadata target: $body"
ok "replay: the same body and credentials arrived; a metadata address refused"

# 8. Login (only on a server of our own: it cannot be turned off again).
if [ -n "$OWN" ]; then
  call POST /users '{"name":"smoke-admin","role":"admin","password":"smoke-admin-password"}'
  expect 201 "the first admin"
  call GET /endpoints
  expect 401 "the API once login is on"
  grep -qi '^www-authenticate: Bearer' "$WORK/headers" || fail "401 without WWW-Authenticate"
  echo "\$ curl -c cookies -d name=smoke-admin -d password=*** $HW/admin/login"
  code=$(curl -sS -o /dev/null -w '%{http_code}' -c "$WORK/cookies" -d name=smoke-admin -d password=smoke-admin-password "$HW/admin/login")
  echo "$code"; echo
  [ "$code" = 303 ] || fail "login answered $code"
  call POST /me/tokens '{"name":"smoke"}' -b "$WORK/cookies" >/dev/null
  expect 201 "make a token"
  TOKEN=$(jq -r .token <<<"$body"); TID=$(jq -r .id <<<"$body")
  echo "\$ curl -X POST '$API/me/tokens' -b cookies -d '{\"name\":\"smoke\"}'"
  echo "201 $(jq -c '.token = "***"' <<<"$body")"; echo
  [[ "$TOKEN" =~ ^hkw_[a-z2-7]{8}_[a-z2-7]{40}$ ]] || fail "token format"
  call GET "/endpoints/$ID/requests" '' -H "Authorization: Bearer $TOKEN" >/dev/null
  echo "\$ curl '$API/endpoints/$ID/requests' -H 'Authorization: Bearer ***'"; echo "$code"; echo
  expect 200 "the API with the token"
  [ "$(jq '.requests | length' <<<"$body")" = 2 ] || fail "requests with the token: $body"
  call POST /me/tokens '{"name":"more"}' -H "Authorization: Bearer $TOKEN" >/dev/null
  expect 403 "a token making tokens"
  [ "$(jq -r .error.code <<<"$body")" = session_required ] || fail "token making tokens: $body"
  call DELETE /me/tokens/$TID '' -b "$WORK/cookies"
  expect 204 "revoke"
  call GET /endpoints '' -H "Authorization: Bearer $TOKEN" >/dev/null
  expect 401 "a revoked token"
  ! grep -q "$TOKEN" "$WORK/body" || fail "an answer holds the token"
  ok "login on: 401 without credentials, a token works, cannot make tokens, revoked 401"
fi

echo "api smoke: all passed"
