#!/bin/bash
# Container scenario: run the image the way the README tells users to and
# check what they rely on. Usage: scripts/docker-smoke.sh <image>
# Needs docker, curl and openssl; leaves nothing behind.
set -euo pipefail
IMG=${1:?usage: docker-smoke.sh <image>}
PORT=${SMOKE_PORT:-18080}
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

# AC-1 / AC-2: identity, user, shell, size, labels
docker run --rm "$IMG" version | grep -q '^hookwitness ' || fail "version"
ok "version: $(docker run --rm "$IMG" version)"
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
docker cp $C:/data/hookwitness.db.key - | tar -tvf - | grep -q '^-rw------- .*65532' || fail "key file mode/owner"
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
echo "smoke: all passed"
