# hookwitness REST API

The API lets tests, scripts and AI agents do what the web UI does: create catch
endpoints, script how they answer, require signatures or credentials, wait for
webhooks and read them with their verdicts. Everything is JSON.

- [Access](#access)
- [Conventions and errors](#conventions-and-errors)
- [Routes](#routes): [endpoints](#endpoints), [behaviour](#behaviour),
  [secrets](#secrets), [requests](#requests)
- [Recipes](#recipes): [wait for a webhook](#wait-for-a-webhook-and-read-its-verdict),
  [Basic credentials](#require-basic-credentials),
  [GitHub signature](#require-a-github-signature)
- [Testing a sender in Docker](#testing-a-sender-in-docker)

## Access

The API lives under the admin prefix: `http://<host>:8080/admin/api/v1`
(`-admin-prefix` / `HOOKWITNESS_ADMIN_PREFIX` moves it with the UI). It has
the UI's access rules and no login of its own yet:

- It answers only when called by IP address, `localhost`, or a host name
  listed in `-admin-hosts` / `HOOKWITNESS_ADMIN_HOSTS`; any other name gets
  `421 wrong_host` (this stops a web page from reaching it through a domain
  pointed at your machine).
- Writes (`POST`, `PUT`) must send `Content-Type: application/json`, else
  `415`; a write sent by a web page from another site is refused (`403
  cross_origin`). curl and HTTP client libraries are not affected.
- Keep it on localhost or a trusted network: whoever reaches it can configure
  endpoints and make the server verify with your test secrets.

Catch endpoints themselves (where senders deliver webhooks) answer to any
host name: only the admin prefix is restricted.

## Conventions and errors

- Bodies are JSON objects; unknown fields are refused (`400 unknown_field`).
  Bodies are limited to 1 MiB.
- Times are RFC 3339 in UTC. Ids are numbers; request ids are never reused.
- Every error has one shape:

```json
{"error": {"code": "invalid", "message": "rule 2: the count N must be at least 1", "field": "rules[1]"}}
```

| Status | `code` | When |
|---|---|---|
| 400 | `bad_json`, `unknown_field` | the body is not valid JSON, has an unknown field or a wrong type (`field` names it) |
| 403 | `cross_origin` | a write from a web page on another site |
| 404 | `not_found` | no such endpoint, secret, request or route |
| 409 | `duplicate_path`, `secret_in_use`, `conflict` | the path is taken; the secret is used by a check; too many concurrent changes |
| 412 | `stale_version` | the behaviour changed since you read it (`If-Match`) |
| 413 | `too_large` | the body is over 1 MiB |
| 415 | `unsupported_media_type` | a write without `Content-Type: application/json` |
| 421 | `wrong_host` | called by a host name that is not allowed (see [Access](#access)) |
| 422 | `invalid` | the value is not acceptable; `field` is a JSON path such as `checks[0]`, `path`, `value`, `wait` |
| 428 | `version_required` | a behaviour `PUT` without `If-Match` |

## Routes

All paths below are relative to `/admin/api/v1`. The examples use

```sh
API=http://localhost:8080/admin/api/v1
```

### Endpoints

An endpoint is an exact path that catches requests. Everything else is caught
into the Unmatched register with a `404`.

| Route | Does |
|---|---|
| `GET /endpoints` | all endpoints, by path; `?path=/x` finds one (an empty list when there is none) |
| `POST /endpoints` | create: `{"path": "/x"}`; no path or an empty one picks a random path. `201`, `Location` |
| `GET /endpoints/{id}` | one endpoint |

```sh
$ curl -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/orders/events"}'
201
{"id":1,"path":"/orders/events","url":"http://localhost:8080/orders/events","requests":0,"counter":0,"created_at":"2026-10-12T17:28:06.808Z"}
```

- `url` is where senders deliver: the path on the host name you called the
  API by (rewrite the host when the sender reaches hookwitness by another
  name, e.g. from another container).
- `requests` is how many are kept; `counter` how many passed the checks since
  the rule counter was last reset; `last_request_at` appears once one arrived.
- Paths are letters, digits and `. _ ~ -` segments; `/healthz` and the admin
  prefix are reserved (`422`, field `path`). A taken path is `409
  duplicate_path`.

### Behaviour

How an endpoint answers: checks, then the first matching rule, else the
default reply. Read it, change it, and `PUT` it back whole with the `ETag`
you read as `If-Match`; a change made meanwhile (by the UI or another client)
makes your `PUT` fail with `412` instead of being overwritten.

| Route | Does |
|---|---|
| `GET /endpoints/{id}/behaviour` | the behaviour, `ETag` header |
| `PUT /endpoints/{id}/behaviour` | replace it; `If-Match: <ETag>` required. `200` with the new behaviour and `ETag` |

```sh
$ curl -i $API/endpoints/1/behaviour
ETag: "e87050872d7e"
{"default":{"status":200},"rules":[],"checks":[],"vars":[],"secrets":[]}

$ curl -X PUT $API/endpoints/1/behaviour -H 'Content-Type: application/json' -H 'If-Match: "e87050872d7e"' \
    -d '{"rules":[{"when":{"kind":"first","n":2},"do":"reply","reply":{"status":503,"headers":[{"name":"Retry-After","value":"2"}]}}]}'
200
```

The document (fields left out take their zero value; `default.status` 0 means 200):

```jsonc
{
  "default": {                      // the reply when no rule matches
    "status": 200,                  // 200-599
    "headers": [{"name": "Content-Type", "value": "application/json"}],  // values are templates
    "body": "{\"ok\":true,\"n\":{{.N}}}",                               // a template
    "delay_ms": 0, "jitter_ms": 0   // wait before replying: delay + random 0..jitter
  },
  "rules": [                        // in order; the first whose condition holds decides
    {"when": {"kind": "every", "n": 3},       "do": "reply", "reply": {"status": 500}},
    {"when": {"kind": "first", "n": 2},       "do": "timeout"},   // hold, never answer
    {"when": {"kind": "random", "percent": 10}, "do": "drop"}     // reset the connection
  ],
  "checks": [                       // run first, in order
    {"header": "X-Api-Key", "match": "equals", "value": "key_test"},           // required header: refuses with 400
    {"kind": "basic",  "secret": "PASS", "user": "orders", "enforce": true},    // see below
    {"kind": "github", "secret": "GH_KEY"}                                      // enforce off: verdict only
  ],
  "vars": [{"name": "TEAM", "value": "qa"}],  // template values {{.Var.TEAM}}, plain text
  "schemas": [                      // JSON Schemas for schema checks, by name
    {"name": "OrderPaid", "text": "{\"type\":\"object\",\"required\":[\"order_id\"]}"}
  ],
  "secrets": []                     // read-only here: set them on the secrets route
}
```

- **Conditions:** `every` N (N, 2N, …), `first` N since the counter reset,
  `random` with `percent` (0 < p ≤ 100). **Actions:** `reply` (with `reply`),
  `timeout`, `drop`. Delays and timeouts are capped by `-max-wait`.
- **Required header** checks: `match` is `present`, `equals` (`value`) or
  `regex` (`value`); an optional `error` reply replaces the standard `400`.
- **Authorization** checks give every request a verdict — `pass`, `fail` or
  `n/v` — and refuse (401, or their `error` reply) only with `enforce: true`:

| `kind` | Verifies | Secret kind | Extra fields |
|---|---|---|---|
| `github` | `X-Hub-Signature-256` | key | — |
| `stripe` | `Stripe-Signature` | key | `tolerance_s`, `ignore_timestamp` |
| `standard` | Standard Webhooks / Svix | key (`whsec_…`) | `tolerance_s`, `ignore_timestamp` |
| `hmac` | any signature header | key | `header`, `algo` (sha1/sha256/sha512), `encoding` (hex/base64), `prefix`, `content` (template; raw body when empty) |
| `bearer` | `Authorization: Bearer` | token | — |
| `basic` | `Authorization: Basic` | token (the password) | `user` |

- **JSON Schema** checks (`"kind": "schema"`) validate the body against one of
  the endpoint's `schemas`: `"schema": "OrderPaid"`, or picked by a
  discriminator, `"select": {"from": "body", "key": "/type", "map": [{"value":
  "order.paid", "schema": "OrderPaid"}]}` (`from` is `body` with a JSON
  pointer or `header` with a header name). A value without a mapping gets the
  verdict `n/c` (not checked) and is never refused; enforced failures are
  refused with `400` listing the problems. Schemas follow `$schema` (2020-12
  when absent), refer to each other by name (`{"$ref": "Address"}`), are never
  fetched from anywhere, and assert `format`. An invalid schema is a `422`
  with field `schemas`.

- Templates: `{{.Header "X-Id"}}`, `{{.Query "x"}}`, `{{.JSON "a.b.0"}}`,
  `{{.Var.NAME}}`, `{{.N}}`, `{{.Method}}`, `{{.Path}}`, `{{.Body}}`.
- A `PUT` keeps the endpoint's secrets; a `secrets` list in the body (e.g.
  sent back as read) is ignored.
- Validation errors are `422` with `field` pointing at the part:
  `rules[1]`, `checks[0]`, `default`, `vars`.

### Secrets

Signing keys and passwords that checks use. They are **write-only**: a value
goes in and never comes back; answers show a fingerprint.

| Route | Does |
|---|---|
| `PUT /endpoints/{id}/secrets/{name}` | create or replace: `{"kind": "token" \| "key", "value": "…"}`; replacing keeps the kind (it may be left out) |
| `DELETE /endpoints/{id}/secrets/{name}` | delete; `409 secret_in_use` while a check uses it. `204` |

```sh
$ curl -X PUT $API/endpoints/1/secrets/ORDERS_PASSWORD -H 'Content-Type: application/json' \
    -d '{"kind":"token","value":"test-password"}'
200
{"name":"ORDERS_PASSWORD","kind":"token","fingerprint":"3f6d2964","used_by":[]}
```

- `kind`: `token` for bearer tokens and Basic passwords (stored as a salted
  hash), `key` for signing keys (stored encrypted).
- Names are letters, digits and `_`, not starting with a digit.
- Use test values only. A caught request keeps its headers, so a password a
  sender sent is visible in that request.

### Requests

| Route | Does |
|---|---|
| `GET /endpoints/{id}/requests` | newest first; `limit` (default 50, at most 500), `before=<id>` pages back |
| `GET /endpoints/{id}/requests?after=<id>` | the requests newer than `<id>`, **oldest first** |
| `GET /endpoints/{id}/requests?after=<id>&wait=30s` | the same, but when there are none yet, wait until one arrives (at most `60s`; `wait=30` means seconds); an empty list when the time is up |
| `GET /requests/{id}` | one request in full, Unmatched ones too |

A list item:

```json
{"id":1,"endpoint_id":1,"no":1,"received_at":"2026-10-12T17:28:07.433Z","method":"POST","path":"/orders/events",
 "query":"","status":200,"verdict":"pass","body_size":28,"body_kept":true,"content_type":"application/json"}
```

- `status` is what hookwitness answered (`0` while a timeout still holds it).
- `verdict` is the overall verdict of the authorization and schema checks:
  `fail` if any failed, else `n/v` if any could not verify, else `pass` if any
  passed, else `n/c` when none applied; `""` when the endpoint has none.
- `body_kept` is false when the body was over `-max-body` (answered `413`).

One request adds:

```json
{"proto":"HTTP/1.1","remote_addr":"127.0.0.1:55702",
 "headers":[["Host","localhost:8080"],["Authorization","Basic b3JkZXJzOnRlc3QtcGFzc3dvcmQ="],["Content-Type","application/json"]],
 "body_text":"{\"order\":42,\"status\":\"paid\"}",
 "outcome":{"source":"default","action":"reply","status":200,"counter":1},
 "verdicts":[{"check":1,"label":"Basic credentials","result":"pass","reason":"user and password match","at":"2026-10-12T17:28:07.433Z"}]}
```

- `headers` are as received: in order, with their case, repeated names kept.
- `body_text` when the body is valid UTF-8, else `body_base64`; neither when
  the body was not kept.
- `outcome` says what was done: `source` (`default`, `rule` with `rule` and
  `label`, `check` with the refusing `check`, `limit`), `action` (`reply`,
  `timeout`, `drop`, `refused`, `error`), the reply sent (`status`, `headers`
  as `{name, value}`, the first 64 KiB of `body`), `delay_ms`, `waited_ms`
  for a timeout, `counter`.
- `verdicts` has one entry per authorization or schema check: `check` (its
  1-based position), `label`, `result`, `reason`, `at`; a failed schema check
  adds `problems` (up to 20 of `{path, keyword, message}`, `path` a JSON
  pointer into the body) and `total`.
- In shell scripts pipe JSON from a variable with `printf '%s\n' "$VAR"`, not
  `echo` (zsh's `echo` expands `\n` inside JSON strings).

**Waiting without missing anything:** keep the id of the last request you have
handled (0 at the start) and ask for `after=<that id>&wait=…`. Requests that
arrive between two calls are returned by the next one; none is returned twice.

## Recipes

### Wait for a webhook and read its verdict

```sh
EP=$(curl -s -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/hooks"}')
ID=$(printf '%s\n' "$EP" | jq .id); URL=$(printf '%s\n' "$EP" | jq -r .url)
LAST=0

# ... make the app under test send its webhook to $URL ...

R=$(curl -s "$API/endpoints/$ID/requests?after=$LAST&wait=30s" | jq '.requests[0]')
[ "$R" != null ] || { echo "no webhook within 30 s"; exit 1; }
printf '%s\n' "$R" | jq '{status, verdict}'
LAST=$(printf '%s\n' "$R" | jq .id)
curl -s $API/requests/$LAST | jq -r .body_text | jq .   # the payload as sent
```

### Require Basic credentials

```sh
curl -s -X PUT $API/endpoints/$ID/secrets/ORDERS_PASSWORD -H 'Content-Type: application/json' \
  -d '{"kind":"token","value":"test-password"}'
TAG=$(curl -sI $API/endpoints/$ID/behaviour | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -s -X PUT $API/endpoints/$ID/behaviour -H 'Content-Type: application/json' -H "If-Match: $TAG" \
  -d '{"checks":[{"kind":"basic","secret":"ORDERS_PASSWORD","user":"orders","enforce":true}]}'

curl -s -o /dev/null -w '%{http_code}\n' -u orders:test-password -H 'Content-Type: application/json' -d '{"order":42}' $URL  # 200
curl -s -o /dev/null -w '%{http_code}\n' -u orders:wrong         -H 'Content-Type: application/json' -d '{"order":43}' $URL  # 401
curl -s "$API/endpoints/$ID/requests?after=0" | jq -c '.requests[] | {id, status, verdict}'
# {"id":1,"status":200,"verdict":"pass"}
# {"id":2,"status":401,"verdict":"fail"}
```

Without `"enforce": true` both requests get the default reply and only the
verdicts differ — use that to see what a sender does without breaking it.
The failing request's `verdicts[0].reason` says why (wrong password, wrong
user, no `Authorization` header, another scheme).

### Require a GitHub signature

```sh
curl -s -X PUT $API/endpoints/$ID/secrets/GH_KEY -H 'Content-Type: application/json' \
  -d '{"kind":"key","value":"gh_test_secret"}'
TAG=$(curl -sI $API/endpoints/$ID/behaviour | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -s -X PUT $API/endpoints/$ID/behaviour -H 'Content-Type: application/json' -H "If-Match: $TAG" \
  -d '{"checks":[{"kind":"github","secret":"GH_KEY","enforce":true}]}'

BODY='{"action":"opened"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac gh_test_secret | awk '{print $2}')
curl -s -o /dev/null -w '%{http_code}\n' -d "$BODY" -H "X-Hub-Signature-256: sha256=$SIG" $URL   # 200
```

The same pattern works for `stripe`, `standard` (a `whsec_` key), `hmac` and
`bearer`; see the table under [Behaviour](#behaviour).

## Testing a sender in Docker

```sh
docker run -d --name hookwitness -p 127.0.0.1:8080:8080 ghcr.io/petroladkin/hookwitness:edge
```

- Without a volume every run starts empty, which suits tests. Add
  `-v hookwitness:/data` to keep endpoints and secrets between runs.
- **The sender runs on the host:** deliver to the `url` the API returns
  (`http://localhost:8080/...`).
- **The sender runs in a container:** deliver to
  `http://host.docker.internal:8080/<path>` (on Linux add
  `--add-host=host.docker.internal:host-gateway` to the sender), or put both
  in one compose network and use `http://hookwitness:8080/<path>`. Catch
  paths answer to any host name.
- **The test itself calls the API from a container:** allow the name it uses,
  e.g. `-e HOOKWITNESS_ADMIN_HOSTS=hookwitness,host.docker.internal`.

Compose, for a sender under test:

```yaml
services:
  hookwitness:
    image: ghcr.io/petroladkin/hookwitness:edge
    ports: ["127.0.0.1:8080:8080"]           # the API and UI for the test on the host
    environment:
      HOOKWITNESS_ADMIN_HOSTS: hookwitness   # the API for tests inside the network
  app:
    build: .
    environment:
      WEBHOOK_URL: http://hookwitness:8080/orders/events
```

A Claude Code skill that teaches an agent this workflow is in
[`skills/hookwitness`](../skills/hookwitness/SKILL.md).
