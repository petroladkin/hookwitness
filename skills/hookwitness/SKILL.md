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

If your MCP client has the `hookwitness` server (`claude mcp add --transport
http hookwitness http://localhost:8080/admin/mcp`), its tools do the same
steps: `create_endpoint`, `set_secret`, `configure_endpoint`,
`wait_for_request`, `expect`. The REST calls below work without it.

## 1. Start it

```sh
docker run -d --rm --name hookwitness -p 127.0.0.1:8080:8080 ghcr.io/petroladkin/hookwitness:0.1
HW=http://localhost:8080
API=$HW/admin/api/v1
until curl -fs $HW/healthz >/dev/null; do sleep 0.5; done
```

- Already running? `curl -fs $HW/healthz` answers `{"status":"ok",…}` — reuse it.
- `--rm` without a volume: every run starts empty. Stop with `docker stop hookwitness`.
- The UI for a human is at `$HW/admin/` (the same data, live).
- A shared server may have login on: the API then answers `401`. Ask the user
  for a token (from their account page in the UI) and send it with every
  call: `AUTH="Authorization: Bearer $(cat token-file)"`, `curl -H "$AUTH" …`
  (MCP: `--header "Authorization: Bearer …"`). Never print the token. A fresh
  container you start yourself has no users, so no token is needed there.

## 2. Create an endpoint and point the sender at it

```sh
EP=$(curl -s -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/orders/events"}')
ID=$(printf '%s\n' "$EP" | jq .id); URL=$(printf '%s\n' "$EP" | jq -r .url)    # http://localhost:8080/orders/events
```

- Create the endpoint before triggering the sender: an unknown path is
  answered `404` (and kept in the Unmatched register).
- Reuse an existing one: `curl -s "$API/endpoints?path=/orders/events" | jq '.endpoints[0]'`.
  If its `enabled` is `false` it is off (its requests go to Unmatched with a
  `404`); turn it on first:
  `curl -s -X PATCH $API/endpoints/$ID -H 'Content-Type: application/json' -d '{"enabled":true}'`.
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
To check the payload's shape, add JSON Schemas and a `schema` check in the same
PUT: `"schemas":[{"name":"OrderPaid","text":"<schema JSON as a string>"}]`,
`"checks":[{"kind":"schema","schema":"OrderPaid"}]`, any of several shapes with
`"schemas":["V1","V2"]`, or pick the schema by the event type with
`"select":{"from":"body","key":"/type","map":[{"value":"order.paid","schema":"OrderPaid"}],"default":"V1"}`
(`default`, one name or a list, covers unmapped or missing values; without it they are `n/c`).
A failed schema verdict lists `problems` (`path`, `keyword`, `message`).
No schema yet? Catch one real delivery, then
`POST $API/endpoints/$ID/schema-draft` with `{"requests":[<id>]}` returns a draft
(`name`, `text`; nothing stored) to review and PUT into `schemas`.

Between test cases: `DELETE $API/endpoints/<id>/requests` clears an endpoint,
`POST $API/endpoints/<id>/reset-counter` (body `{}`, JSON) restarts "every Nth"
rules, `POST $API/endpoints/<id>/recheck` re-verifies kept requests after you
fix a secret or schema. `?verdict=fail` (or `status=401`) on a request list
finds the failures; `GET $API/unmatched/requests` shows requests that went to
a wrong path (a typo in the sender's URL). `GET $API/endpoints/<id>/report?format=md`
is evidence to attach; `$API/openapi.json` describes every route.
A string the schema marks `"contentEncoding":"base64","contentMediaType":"image/png"`
(or `image/jpeg`, `image/gif`, `image/webp`, `image/*`; a `data:image/...` URL
works too) is checked to really be that image; the request's `images` list it
with its size and pixels, and `GET $API/requests/<id>/image?path=/avatar`
returns the picture itself — save it and look at it when the test is about
what the sender put in the image.
`enforce: true` refuses failures with 401; without it the request gets the
normal reply and only its `verdict` says `fail` — use that first to observe a
sender without breaking it.

Given a setup file (`*.hookwitness.json`, e.g. from a teammate or a ticket)?
Skip the PUTs: `curl -s -X POST "$API/setup?on_conflict=replace" -H 'Content-Type: application/json' --data-binary @file.hookwitness.json`
creates (or updates) its endpoints at once (MCP: `import_endpoints`). It holds
no secret values: each name in `secrets_not_set` is a check that gives `n/v`
until you PUT that secret's test value. `GET "$API/setup?id=$ID"` (MCP:
`export_endpoint`) saves your own setup to hand back with the results.

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
LAST=$(printf '%s\n' "$R" | jq .id)
```

`wait` returns as soon as a request arrives (at most 60 s; an empty list when
the time is up). With `after` the list is **oldest first**, and none is
returned twice if you always pass the last id you saw; without `after` it is
newest first (`limit`, `before=<id>` to page back).

On a busy endpoint, pick the one you want instead of fetching each request:
`json=/taskId=42` keeps JSON bodies with that value at the pointer (string, or
number/true/false/null as written; repeat for AND), `q=<text>` keeps bodies
containing the text (bytes, case-sensitive), and `include=body` puts
`body_text`/`body_base64` into the list (limit then at most 100). The wait ends
at the first match. Take the cursor before triggering, one per thing you wait
for: a filtered answer's id skips the non-matching requests in between.

```sh
LAST=$(curl -s "$API/endpoints/$ID/requests?limit=1" | jq '.requests[0].id // 0')
# ... start task 42 ...
R=$(curl -s "$API/endpoints/$ID/requests?after=$LAST&wait=30s&json=/taskId=42&include=body" | jq '.requests[0]')
```

Times in answers always have three fraction digits (`…T18:48:01.380Z`).

## 5. Assert

```sh
printf '%s\n' "$R" | jq '{status, verdict}'          # what hookwitness answered; pass | fail | n/v | ""
curl -s $API/requests/$LAST > req.json                # the request in full
jq -r .body_text req.json | jq -e '.type == "order.paid" and .order_id == 42'   # the JSON payload as sent
jq -r '.headers[] | select(.[0]|ascii_downcase=="content-type") | .[1]' req.json
jq -r '.headers[] | select(.[0]|ascii_downcase=="authorization") | .[1]' req.json \
  | sed 's/^Basic //' | base64 -d; echo                # the exact user:password sent
jq '.verdicts[] | {label, result, reason}' req.json  # why a check failed
jq '.outcome | {source, action, status, check}' req.json   # what was done: default/rule/check, reply/timeout/drop/refused
```

- `body_text` holds the body when it is UTF-8 (else `body_base64`).
- `headers` are `[name, value]` pairs exactly as received (order, case, repeats);
  the reply hookwitness sent is in `outcome` (`status`, `headers` as
  `{name, value}`, `body`).
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
- Pipe JSON with `printf '%s\n' "$VAR"` or straight from `curl`, never
  `echo "$VAR"`: zsh's `echo` turns `\n` inside JSON strings into newlines
  and `jq` then fails.
- A caught request shows the credentials the sender used (that is the evidence);
  use test secrets only.

## Install this skill

Copy or symlink this folder into a project as `.claude/skills/hookwitness/`
(or into `~/.claude/skills/hookwitness/` for every project):

```sh
mkdir -p .claude/skills/hookwitness && curl -fsSL -o .claude/skills/hookwitness/SKILL.md \
  https://raw.githubusercontent.com/petroladkin/hookwitness/dev/skills/hookwitness/SKILL.md
```
