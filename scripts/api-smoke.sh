#!/bin/bash
# API scenario: the Basic-credentials recipe of docs/api.md end to end —
# create an endpoint, set a password, require it, send JSON events with
# right and wrong credentials, wait for them and read their verdicts; a
# sender's retry and duplicate judged by event id; a request passed on to a
# receiver (a second endpoint) and both sides kept. On a server it starts
# itself (--binary, --image) it then turns login on: the first admin, a
# login, a token, the API with and without it (docs/access.md).
#
# Usage:
#   scripts/api-smoke.sh <base-url>            against a running hookwitness
#   scripts/api-smoke.sh --binary <path>       start the binary on a temporary database
#   scripts/api-smoke.sh --image <image>       start the container image
# Needs curl and jq; prints every call it makes; leaves nothing behind.
set -euo pipefail
PORT=${SMOKE_PORT:-18081}
WORK=$(mktemp -d)
PID= C= PID2= MOCK= BIN=
cleanup() {
  [ -n "$PID2" ] && kill "$PID2" 2>/dev/null && wait "$PID2" 2>/dev/null
  [ -n "$MOCK" ] && kill "$MOCK" 2>/dev/null && wait "$MOCK" 2>/dev/null
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
  BIN=${2:?binary}
  "$BIN" serve -addr 127.0.0.1:$PORT -db "$WORK/hw.db" >"$WORK/log" 2>&1 &
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

# 8. Delivery: an endpoint names GitHub's delivery header as its event id,
# answers the first request 503 and checks duplicates. The delivery, its
# retry and the same delivery again after the 200: pass, pass, fail, and
# ?event= lists the three attempts.
call POST /endpoints '{"path":"/github/push"}'
expect 201 "create the delivery endpoint"
D=$(jq -r .id <<<"$body"); DURL=$(jq -r .url <<<"$body")
call GET /endpoints/$D/behaviour
TAG=$(grep -i '^etag:' "$WORK/headers" | tr -d '\r' | cut -d' ' -f2)
call PUT /endpoints/$D/behaviour '{"event":{"from":"header","key":"X-GitHub-Delivery"},"rules":[{"when":{"kind":"first","n":1},"do":"reply","reply":{"status":503}}],"checks":[{"kind":"duplicates"}]}' -H "If-Match: $TAG"
expect 200 "event id and duplicates check"
for want in 503 200 200; do
  echo "\$ curl -H 'X-GitHub-Delivery: smoke-delivery-1' -d '{\"ref\":\"main\"}' $DURL"
  sent=$(curl -sS -o /dev/null -w '%{http_code}' -H 'X-GitHub-Delivery: smoke-delivery-1' -d '{"ref":"main"}' "$DURL")
  echo "$sent"; echo
  [ "$sent" = "$want" ] || fail "delivery answered $sent, want $want"
done
call GET "/endpoints/$D/requests?event=smoke-delivery-1"
expect 200 "list by event"
[ "$(jq -c '[.requests[] | [.attempt, .status, .verdict, .event]]' <<<"$body")" = \
  '[[3,200,"fail","smoke-delivery-1"],[2,200,"pass","smoke-delivery-1"],[1,503,"pass","smoke-delivery-1"]]' ] || fail "attempts: $body"
call GET "/requests/$(jq -r '.requests[0].id' <<<"$body")"
[ "$(jq -r '.verdicts[0].reason' <<<"$body")" = "duplicate of #2, which was answered 200" ] || fail "duplicate verdict: $body"
call GET "/endpoints/$D/requests?event="
expect 422 "an empty event"
ok "delivery: 503, retry, duplicate after the 200 failed; event= lists 3 attempts"

# 9. Recorder: /smoke/live forwards to /smoke/mock, a second endpoint of
# this server standing in for the receiver; the sender gets the receiver's
# 201, both sides are kept, and a receiver with a query is refused.
call POST /endpoints '{"path":"/smoke/mock"}'
expect 201 "create the receiver stand-in"
M=$(jq -r .id <<<"$body")
call GET /endpoints/$M/behaviour
TAG=$(grep -i '^etag:' "$WORK/headers" | tr -d '\r' | cut -d' ' -f2)
call PUT /endpoints/$M/behaviour '{"default":{"status":201,"body":"{\"accepted\":true}"}}' -H "If-Match: $TAG"
expect 200 "the stand-in answers 201"
call POST /endpoints '{"path":"/smoke/live"}'
expect 201 "create the recording endpoint"
L=$(jq -r .id <<<"$body"); LURL=$(jq -r .url <<<"$body")
call GET /endpoints/$L/behaviour
TAG=$(grep -i '^etag:' "$WORK/headers" | tr -d '\r' | cut -d' ' -f2)
call PUT /endpoints/$L/behaviour '{"forward":{"url":"'"${INSIDE:-$HW}"'/smoke/mock?token=1"}}' -H "If-Match: $TAG"
expect 422 "a receiver with a query"
call PUT /endpoints/$L/behaviour '{"forward":{"url":"'"${INSIDE:-$HW}"'/smoke/mock"}}' -H "If-Match: $TAG"
expect 200 "set the receiver"
[ "$(jq -r '.warnings[0]' <<<"$body")" != null ] || fail "no open-relay warning: $body"
echo "\$ curl -d '{\"order\":42}' $LURL?src=smoke"
sent=$(curl -sS -o "$WORK/answer" -w '%{http_code}' -d '{"order":42}' "$LURL?src=smoke")
echo "$sent $(cat "$WORK/answer")"; echo
[ "$sent" = 201 ] && [ "$(cat "$WORK/answer")" = '{"accepted":true}' ] || fail "the sender got $sent $(cat "$WORK/answer")"
call GET "/endpoints/$L/requests?forward=ok"
expect 200 "forwarded and answered 2xx"
FR=$(jq -r '.requests[0].id' <<<"$body")
[ "$(jq -c '.requests[0].forward | {status}' <<<"$body")" = '{"status":201}' ] || fail "the receiver's side: $body"
call GET "/requests/$FR/forward"
expect 200 "the exchange"
[ "$(jq -r .line <<<"$body")" = "POST /smoke/mock?src=smoke HTTP/1.1" ] && [ "$(jq -r .body_text <<<"$body")" = '{"accepted":true}' ] ||
  fail "the exchange: $body"
call GET "/endpoints/$M/requests?limit=1&include=body"
[ "$(jq -r '.requests[0].body_text' <<<"$body")" = '{"order":42}' ] || fail "the stand-in did not get the body: $body"
ok "recorder: the receiver's 201 passed back, the exchange kept, the body and query forwarded, a query in the receiver refused"

# 10. Login (only on a server of our own: it cannot be turned off again).
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

# 10. Sign-in through an OpenID Connect provider (with --binary): a second
# server of the same binary, configured for the test provider
# scripts/oidcmock, signed in to with curl walking the redirects.
if [ -n "$BIN" ]; then
  go build -o "$WORK/oidcmock" ./scripts/oidcmock || fail "cannot build scripts/oidcmock"
  printf 'smoke-client-secret-0123456789' > "$WORK/oidc-secret"
  printf 'smoke-oidc-admin-password' > "$WORK/oidc-admin-pw"
  "$WORK/oidcmock" -client-id smoke -client-secret-file "$WORK/oidc-secret" -sub smoke-sub-1 -email ann@example.test >"$WORK/mock.out" &
  MOCK=$!
  for _ in $(seq 40); do [ -s "$WORK/mock.out" ] && break; sleep 0.1; done
  ISSUER=$(head -1 "$WORK/mock.out")
  P2=$((PORT + 1)) H2=http://127.0.0.1:$((PORT + 1))
  HOOKWITNESS_ADMIN_USER=ann HOOKWITNESS_ADMIN_PASSWORD_FILE="$WORK/oidc-admin-pw" HOOKWITNESS_ADMIN_EMAIL=ann@example.test \
    HOOKWITNESS_ADMIN_URL=$H2 HOOKWITNESS_OIDC_PROVIDERS=mock HOOKWITNESS_OIDC_MOCK_ISSUER="$ISSUER" \
    HOOKWITNESS_OIDC_MOCK_CLIENT_ID=smoke HOOKWITNESS_OIDC_MOCK_CLIENT_SECRET_FILE="$WORK/oidc-secret" \
    "$BIN" serve -addr 127.0.0.1:$P2 -db "$WORK/oidc.db" >"$WORK/oidc.log" 2>&1 &
  PID2=$!
  for _ in $(seq 60); do curl -fsS -o /dev/null "$H2/healthz" 2>/dev/null && break; sleep 0.25; done
  grep -q "redirect_uri=$H2/admin/oidc/mock/callback" "$WORK/oidc.log" || fail "no redirect URI in the start log: $(cat "$WORK/oidc.log")"
  echo "\$ curl $H2/admin/api/v1/login/providers"
  providers=$(curl -sS "$H2/admin/api/v1/login/providers")
  echo "$(jq -c . <<<"$providers")"; echo
  [ "$(jq -c 'map({name, label})' <<<"$providers")" = '[{"name":"mock","label":"Mock"}]' ] || fail "providers: $providers"
  echo "\$ curl -c jar -d next=/admin/ $H2/admin/oidc/mock/start"
  to_idp=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -c "$WORK/jar" -b "$WORK/jar" -d next=/admin/ "$H2/admin/oidc/mock/start")
  echo "${to_idp%%\?*}?…"; echo
  [ "${to_idp%% *}" = 303 ] || fail "start: $to_idp"
  echo "\$ curl <the provider's authorization endpoint>"
  back=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' "${to_idp#* }")
  echo "${back%%\?*}?code=…&state=…"; echo
  [ "${back%% *}" = 302 ] || fail "the provider: $back"
  CODE=$(sed -E 's/.*[?&]code=([^&]*).*/\1/' <<<"${back#* }")
  echo "\$ curl -b jar <the callback>"
  done_=$(curl -sS -o /dev/null -w '%{http_code} %{redirect_url}' -c "$WORK/jar" -b "$WORK/jar" "${back#* }")
  echo "$done_"; echo
  [ "$done_" = "303 $H2/admin/" ] || fail "callback: $done_"
  echo "\$ curl -b jar $H2/admin/api/v1/me"
  me=$(curl -sS -b "$WORK/jar" "$H2/admin/api/v1/me")
  echo "$(jq -c '.identities |= map({provider, email})' <<<"$me")"; echo
  [ "$(jq -r '.name + " " + .via + " " + .identities[0].provider' <<<"$me")" = "ann session mock" ] || fail "me: $me"
  for s in smoke-client-secret-0123456789 "$CODE"; do
    ! grep -q -- "$s" "$WORK/oidc.log" || fail "the server log holds the client secret or the code"
  done
  grep -q 'msg="oidc identity linked" user=ann provider=mock email=ann@example.test' "$WORK/oidc.log" || fail "no link in the log"
  ok "provider sign-in: start, the provider, the callback, a session as ann linked to mock; no secret or code in the log"
fi

echo "api smoke: all passed"
