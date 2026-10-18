#!/bin/bash
# Container scenario: run the image the way the README tells users to and
# check what they rely on. Usage: scripts/docker-smoke.sh <image>
# Needs docker, curl and openssl; leaves nothing behind.
set -euo pipefail
IMG=${1:?usage: docker-smoke.sh <image>}
PORT=${SMOKE_PORT:-18080}
APORT=${SMOKE_ADMIN_PORT:-18081}
B=http://127.0.0.1:$PORT
A=$B/admin
VOL=hw-smoke-$$
C=hw-smoke-$$
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }
cleanup() { docker rm -f $C >/dev/null 2>&1 || true; docker volume rm $VOL >/dev/null 2>&1 || true; }
trap cleanup EXIT

start() { # start [extra docker run flags]
  docker run -d --name $C -p 127.0.0.1:$PORT:8080 -v $VOL:/data "$@" "$IMG" >/dev/null
  for _ in $(seq 60); do
    [ "$(docker inspect -f '{{.State.Health.Status}}' $C)" = healthy ] && return
    sleep 0.5
  done
  docker logs $C >&2
  fail "container did not become healthy"
}
stop() {
  docker stop $C >/dev/null
  code=$(docker inspect -f '{{.State.ExitCode}}' $C)
  docker rm $C >/dev/null
}
# edit posts a behaviour form the way the page does, with the config version.
edit() {
  v=$(curl -fsS "$A/e/1/behaviour?edit=default" | grep -o 'name="v" value="[0-9a-f]*"' | head -1 | sed 's/.*value="//; s/"//')
  curl -fsS -o /dev/null -d "v=$v" "$@" "$A/e/1/behaviour"
}
sig() { printf '%s' "$1" | openssl dgst -sha256 -hmac "$2" | sed 's/^.* //'; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
# in_log PATTERN-ARGS: grep the container's whole log. Never `docker logs | grep -q`:
# under pipefail grep -q ends the pipe early, docker logs dies of SIGPIPE and
# the pipeline fails whether or not the line was there.
in_log() { local log; log=$(docker logs $C 2>&1); grep -q "$@" <<<"$log"; }

# AC-1 / AC-2: identity, user, shell, size, labels
v=$(docker run --rm "$IMG" version)
grep -q '^hookwitness ' <<<"$v" || fail "version"
ok "version: $v"
[ "$(docker image inspect -f '{{.Config.User}}' "$IMG")" = 65532:65532 ] || fail "user is not 65532"
ok "runs as 65532:65532"
if docker run --rm --entrypoint sh "$IMG" -c true >/dev/null 2>&1; then fail "image has a shell"; fi
ok "no shell"
size=$(docker image inspect -f '{{.Size}}' "$IMG")
[ "$size" -lt 26214400 ] || fail "size $size"
ok "size $((size / 1024 / 1024)) MiB"
[ "$(docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.licenses"}}' "$IMG")" = Apache-2.0 ] || fail "license label"
ok "labels"

# AC-3 / AC-5: serves on the README command, healthy, UI on localhost
start
ok "healthy"
[ "$(status $B/healthz)" = 200 ] || fail "healthz"
curl -fsS -o /dev/null -d path=/hooks "$A/endpoints" || fail "create endpoint"
[ "$(status "$A/")" = 200 ] || fail "UI"
ok "catch endpoint /hooks created through the UI"

# AC-4 setup: a sealed signing key and an enforced GitHub check
edit -d op=secret-save -d i=new -d name=GH -d secret_kind=key -d value=smoke_test_secret
edit -d op=check-save -d i=new -d kind=github -d secret=GH -d enforce=on
BODY='{"smoke":1}'
GOOD="X-Hub-Signature-256: sha256=$(sig "$BODY" smoke_test_secret)"
[ "$(status -d "$BODY" -H "$GOOD" $B/hooks)" = 200 ] || fail "signed request"
[ "$(status -d "$BODY" -H "X-Hub-Signature-256: sha256=$(sig "$BODY" wrong)" $B/hooks)" = 401 ] || fail "badly signed request"
ok "signed 200, badly signed 401"
key=$(docker cp $C:/data/hookwitness.db.key - | tar -tvf -)
grep -q '^-rw------- .*65532' <<<"$key" || fail "key file mode/owner"
ok "key file 0600, owned by 65532"

# AC-6: graceful stop within Docker's 10 s, while a request is held
edit -d op=rule-save -d i=new -d when=every -d n=1 -d do=timeout -d status=500
curl -s -m 30 -o /dev/null -d "$BODY" -H "$GOOD" $B/hooks &
held=$!
sleep 1
t0=$(date +%s)
stop
t1=$(date +%s)
wait $held || true
[ "$code" = 0 ] || fail "exit code $code after docker stop (137 = killed)"
[ $((t1 - t0)) -lt 10 ] || fail "stop took $((t1 - t0)) s"
ok "docker stop: exit 0 in $((t1 - t0)) s with a held request"

# AC-4 / AC-7: recreate on the same volume, read-only root; data and secret survive
start --read-only --tmpfs /tmp
edit -d op=rule-del -d i=0
[ "$(status -d "$BODY" -H "$GOOD" $B/hooks)" = 200 ] || fail "sealed secret lost after recreate"
[ "$(status -d "$BODY" $B/hooks)" = 401 ] || fail "check lost after recreate"
n=$(curl -fsS "$A/e/1" | grep -o 'id="req-' | wc -l | tr -d ' ')
[ "$n" -ge 4 ] || fail "requests lost after recreate ($n rows)"
ok "recreated read-only on the same volume: endpoint, $n requests and the sealed secret are there"
stop

# The admin on its own port, at its root: the catch port catches every path
# but /healthz, the admin answers only on its port.
X=http://127.0.0.1:$APORT
start -p 127.0.0.1:$APORT:8081 -e HOOKWITNESS_ADMIN_ADDR=:8081 -e HOOKWITNESS_ADMIN_PREFIX=/ -e HOOKWITNESS_PUBLIC_URL=$B
ok "healthy with the admin on :8081 (the health check probes the catch port)"
[ "$(status $B/healthz)" = 200 ] && [ "$(status $X/healthz)" = 200 ] || fail "healthz on both ports"
[ "$(status "$X/")" = 200 ] || fail "UI at the root of the admin port"
[ "$(status -d '{}' $B/admin/x)" = 404 ] || fail "/admin/x on the catch port is not caught"
[ "$(status $B/api/v1/endpoints)" = 404 ] || fail "the API answered on the catch port"
got=$(curl -fsS "$X/api/v1/unmatched/requests") && grep -q '"path": "/admin/x"' <<<"$got" || fail "/admin/x not in Unmatched"
got=$(curl -fsS "$X/api/v1/endpoints") && grep -q "\"url\": \"$B/hooks\"" <<<"$got" || fail "catch URL from HOOKWITNESS_PUBLIC_URL"
ok "catch port: /admin/x and /api/v1/endpoints caught into Unmatched; admin and API on :8081; catch URL $B/hooks"
stop
[ "$code" = 0 ] || fail "exit code $code after docker stop"
ok "docker stop: exit 0 with two listeners"

# Login: the first admin from a mounted password file (docs/access.md), on
# the same volume, with the admin on its own port.
PW=$(mktemp); trap 'cleanup; rm -f "$PW"' EXIT
printf 'smoke-admin-password\n' > "$PW"; chmod 644 "$PW"   # the image's user must read it
LOGIN=(-p 127.0.0.1:$APORT:8081 -e HOOKWITNESS_ADMIN_ADDR=:8081 -e HOOKWITNESS_ADMIN_PREFIX=/ -e HOOKWITNESS_PUBLIC_URL=$B
  -e HOOKWITNESS_ADMIN_USER=root -e HOOKWITNESS_ADMIN_PASSWORD_FILE=/run/secrets/admin_password -v "$PW:/run/secrets/admin_password:ro")
start "${LOGIN[@]}"
in_log 'admin user created' || fail "no bootstrap line"
[ "$(status "$X/")" = 303 ] && [ "$(status "$X/api/v1/endpoints")" = 401 ] || fail "the admin is open with a user"
[ "$(status $B/healthz)" = 200 ] || fail "healthz with login on"
J=$(mktemp)
[ "$(status -c "$J" -d name=root -d password=smoke-admin-password "$X/login")" = 303 ] || fail "login"
SID=$(awk '$6=="hookwitness_session"{print $7}' "$J")
[ -n "$SID" ] || fail "no session cookie"
TOKEN=$(curl -fsS -b "$J" -X POST -H 'Content-Type: application/json' -d '{"name":"smoke"}' "$X/api/v1/me/tokens" | sed -n 's/.*"token": "\([^"]*\)".*/\1/p')
[ "$(status -H "Authorization: Bearer $TOKEN" "$X/api/v1/endpoints")" = 200 ] || fail "the API with a token"
ok "first admin from the password file; UI 303 and API 401 without a login; login and a token work"
# A browser logged in on the admin port sends its cookie to the catch port.
status -H "Cookie: theme=dark; hookwitness_session=$SID" -d '{}' $B/hooks >/dev/null
last=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$X/api/v1/endpoints/1/requests?limit=1" | sed -n 's/.*"id": \([0-9]*\),.*/\1/p' | head -1)
got=$(curl -fsS -H "Authorization: Bearer $TOKEN" "$X/api/v1/requests/$last")
grep -q 'hookwitness_session=\[hookwitness session\]' <<<"$got" && ! grep -q "$SID" <<<"$got" || fail "the session cookie was recorded"
ok "the session cookie's value is never kept in a caught request"
stop
start "${LOGIN[@]}"
in_log 'admin bootstrap skipped' || fail "no skipped line on the second start"
[ "$(status -d name=root -d password=smoke-admin-password "$X/login")" = 303 ] || fail "login after a restart"
! in_log -e smoke-admin-password -e "$TOKEN" -e "$SID" || fail "a secret value in the log"
ok "restart: bootstrap skipped, the same password works, no secret value in the log"
rm -f "$J"
stop

# A setup file applied at every start (docs/sharing.md): its endpoint is
# there without clicks, and a restart with the same file changes nothing.
SETUP=$(mktemp); trap 'cleanup; rm -f "$PW" "$SETUP"' EXIT
cat > "$SETUP" <<'JSON'
{"hookwitness": 1, "endpoints": [{"path": "/payments/webhooks", "default": {"status": 200},
  "rules": [{"when": {"kind": "first", "n": 2}, "do": "reply", "reply": {"status": 503}}],
  "checks": [{"kind": "stripe", "enforce": true, "secret": "STRIPE_SECRET"}],
  "secrets": [{"name": "STRIPE_SECRET", "kind": "key"}]}]}
JSON
chmod 644 "$SETUP"
start "${LOGIN[@]}" -e HOOKWITNESS_IMPORT=/setup.json -v "$SETUP:/setup.json:ro"
LOG=$(docker logs $C 2>&1)   # grep -q on a pipe would end docker logs early (pipefail)
grep -q 'msg="endpoint created from a setup file"' <<<"$LOG" || { echo "$LOG" >&2; fail "no import line"; }
[ "$(status -d '{}' $B/payments/webhooks)" = 503 ] || fail "the imported rule"
stop
start "${LOGIN[@]}" -e HOOKWITNESS_IMPORT=/setup.json -v "$SETUP:/setup.json:ro"
LOG=$(docker logs $C 2>&1)
grep -q 'created=0 replaced=0 unchanged=1' <<<"$LOG" || { echo "$LOG" >&2; fail "a restart changed the imported endpoint"; }
[ "$(status -d '{}' $B/payments/webhooks)" = 503 ] || fail "the counter after a restart"
[ "$(status -d '{}' $B/payments/webhooks)" = 200 ] || fail "the counter after a restart (third delivery)"
ok "HOOKWITNESS_IMPORT: the endpoint at start, unchanged on a restart, its counter kept"
stop
echo "smoke: all passed"
