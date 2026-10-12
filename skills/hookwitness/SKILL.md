---
name: hookwitness
description: Test code that sends webhooks or HTTP event callbacks (JSON events, signed payloads, Basic or Bearer auth) by catching them in a local hookwitness server run with Docker, then asserting on what arrived through its REST API. Use when developing or verifying a webhook sender, an event publisher, retries on failure, or request signing/authentication.
---

# Testing a webhook sender with hookwitness

hookwitness is a webhook catcher: it records every request sent to it, answers
the way you script it (status, delay, timeout, connection drop), and gives
each request a verdict for its signature or credentials. Its REST API lets you
set it up and read what arrived without a browser.

Full reference: https://github.com/petroladkin/hookwitness/blob/dev/docs/api.md

## 1. Start it

```sh
docker run -d --rm --name hookwitness -p 127.0.0.1:8080:8080 ghcr.io/petroladkin/hookwitness:edge
HW=http://localhost:8080
API=$HW/admin/api/v1
until curl -fs $HW/healthz >/dev/null; do sleep 0.5; done
```

- Already running? `curl -fs $HW/healthz` answers `{"status":"ok",…}` — reuse it.
- `--rm` without a volume: every run starts empty. Stop with `docker stop hookwitness`.
- The UI for a human is at `$HW/admin/` (the same data, live).

## 2. Create an endpoint and point the sender at it

```sh
EP=$(curl -s -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/orders/events"}')
ID=$(echo "$EP" | jq .id); URL=$(echo "$EP" | jq -r .url)    # http://localhost:8080/orders/events
```

- Reuse an existing one: `curl -s "$API/endpoints?path=/orders/events" | jq '.endpoints[0]'`.
- Configure the app under test with the webhook URL:
  - app on the host → `$URL`;
  - app in a container → `http://host.docker.internal:8080/orders/events`
    (Linux: run the app with `--add-host=host.docker.internal:host-gateway`),
    or `http://hookwitness:8080/...` when both are in one compose network.
- If *your* commands run inside a container and call the API by a name such
  as `hookwitness`, start hookwitness with `-e HOOKWITNESS_ADMIN_HOSTS=hookwitness`
  (otherwise the API answers `421 wrong_host`). Catch paths accept any name.

## 3. Require what the sender must send (optional)

Secrets are write-only (`kind`: `token` for passwords and bearer tokens, `key`
for signing keys). Then replace the behaviour with the ETag you read:

```sh
curl -s -X PUT $API/endpoints/$ID/secrets/ORDERS_PASSWORD -H 'Content-Type: application/json' \
  -d '{"kind":"token","value":"test-password"}'
TAG=$(curl -sI $API/endpoints/$ID/behaviour | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -s -X PUT $API/endpoints/$ID/behaviour -H 'Content-Type: application/json' -H "If-Match: $TAG" \
  -d '{"checks":[{"kind":"basic","secret":"ORDERS_PASSWORD","user":"orders","enforce":true}]}'
```

Check kinds: `basic` (+`user`), `bearer` (token secrets); `github`, `stripe`,
`standard` (`whsec_` key), `hmac` (+`header`, `algo`, `encoding`) (key secrets).
`enforce: true` refuses failures with 401; without it the request gets the
normal reply and only its `verdict` says `fail` — use that first to observe a
sender without breaking it.

Fault scripting (same PUT, add `rules`): `{"when":{"kind":"first","n":2},"do":"reply","reply":{"status":503}}`
fails the first two deliveries (retries); `"do":"timeout"` hangs; `"do":"drop"`
resets the connection; `{"kind":"every","n":3}`, `{"kind":"random","percent":10}`.

## 4. Trigger the sender, then wait for what arrived

Keep a cursor: the id of the last request you handled, `0` at the start.

```sh
LAST=0
# ... trigger the app under test ...
R=$(curl -s "$API/endpoints/$ID/requests?after=$LAST&wait=30s" | jq '.requests[0]')
[ "$R" != null ] || echo "FAIL: no webhook within 30 s"
LAST=$(echo "$R" | jq .id)
```

`wait` returns as soon as a request arrives (at most 60 s; an empty list when
the time is up). Several requests come oldest first; none is returned twice
if you always pass the last id you saw.

## 5. Assert

```sh
echo "$R" | jq '{status, verdict}'                      # what hookwitness answered; pass | fail | n/v | ""
FULL=$(curl -s $API/requests/$LAST)
echo "$FULL" | jq -r .body_text | jq -e '.order == 42'  # the JSON payload exactly as sent
echo "$FULL" | jq -r '.headers[] | select(.[0]|ascii_downcase=="content-type") | .[1]'
echo "$FULL" | jq '.verdicts[] | {label, result, reason}'   # why a check failed
```

- `body_text` holds the body when it is UTF-8 (else `body_base64`).
- `headers` are `[name, value]` pairs exactly as received (order, case, repeats).
- `status` `401` with `verdict` `fail` = refused by an enforced check;
  `verdicts[].reason` says why (wrong password, missing header, bad signature).

## Pitfalls

- Writes need `Content-Type: application/json` (`415` otherwise).
- A behaviour PUT needs `If-Match` with the current ETag (`428`/`412`): GET,
  change, PUT; on `412` read again and retry.
- The behaviour PUT replaces everything (checks, rules, default, vars); send
  the parts you want to keep. Secrets are kept and set separately.
- Secret values are never returned; to change one, PUT it again.
- Errors are `{"error":{"code","message","field"}}` — read `message`.
- A caught request shows the credentials the sender used (that is the evidence);
  use test secrets only.

## Install this skill

Copy or symlink this folder into a project as `.claude/skills/hookwitness/`
(or into `~/.claude/skills/hookwitness/` for every project):

```sh
mkdir -p .claude/skills/hookwitness && curl -fsSL -o .claude/skills/hookwitness/SKILL.md \
  https://raw.githubusercontent.com/petroladkin/hookwitness/dev/skills/hookwitness/SKILL.md
```
