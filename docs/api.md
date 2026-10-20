# hookwitness REST API

The API lets tests, scripts and AI agents do what the web UI does: create catch
endpoints, script how they answer, require signatures or credentials, wait for
webhooks and read them with their verdicts. Everything is JSON.

- [Access](#access)
- [Conventions and errors](#conventions-and-errors)
- [Routes](#routes): [endpoints](#endpoints), [behaviour](#behaviour),
  [secrets](#secrets), [requests](#requests), [actions](#actions),
  [reports](#reports), [users and tokens](#users-and-tokens), [OpenAPI](#openapi)
- [Recipes](#recipes): [wait for a webhook](#wait-for-a-webhook-and-read-its-verdict),
  [wait for one task's callback](#wait-for-the-callback-of-one-task),
  [Basic credentials](#require-basic-credentials),
  [GitHub signature](#require-a-github-signature)
- [Testing a sender in Docker](#testing-a-sender-in-docker)

## Access

The API lives under the admin prefix: `http://<host>:8080/admin/api/v1`
(`-admin-prefix` / `HOOKWITNESS_ADMIN_PREFIX` moves it with the UI; with
`-admin-addr` / `HOOKWITNESS_ADMIN_ADDR` it moves to the admin's own port,
`http://<host>:8081/api/v1` when the prefix is `/`). An endpoint's `url` is
built on `-public-url` / `HOOKWITNESS_PUBLIC_URL` when it is set. It has
the UI's access rules:

- **Credentials, once login is on.** While there are no users it is open.
  Once a user exists (or a machine token is set,
  `HOOKWITNESS_API_TOKEN_FILE`), every route needs a token as
  `Authorization: Bearer <token>` (from the account page, or `POST
  /me/tokens`), or the web UI's login cookie; without them `401
  unauthorized`, with a wrong or revoked token `401 invalid_token`, both with
  `WWW-Authenticate: Bearer`. A token acts as its owner, with the owner's
  role as it is now. See [Users, login and tokens](access.md).

- It answers only when called by IP address, `localhost`, or a host name
  listed in `-admin-hosts` / `HOOKWITNESS_ADMIN_HOSTS`; any other name gets
  `421 wrong_host` (this stops a web page from reaching it through a domain
  pointed at your machine). A request with a valid token is answered on any
  name.
- Writes (`POST`, `PUT`) must send `Content-Type: application/json`, else
  `415`; a write sent by a web page from another site is refused (`403
  cross_origin`). curl and HTTP client libraries are not affected.
- Without users, keep it on localhost or a trusted network: whoever reaches
  it can configure endpoints and make the server verify with your test
  secrets.

Catch endpoints themselves (where senders deliver webhooks) answer to any
host name: only the admin prefix is restricted.

The same server answers MCP at `/admin/mcp` (Streamable HTTP), with the same
access rules (with login on, a token: a login cookie is not enough); its
tools are built on these routes and act as the token's owner. At most
`-mcp-max-sessions` sessions (32 by default) are open at once: one more is
refused at `initialize` with `503`, `Retry-After: 30` and a JSON-RPC error
naming the limit. See [MCP](agents.md#mcp).

## Conventions and errors

- Bodies are JSON objects; unknown fields are refused (`400 unknown_field`).
  Bodies are limited to 1 MiB.
- Times are RFC 3339 in UTC with exactly three fraction digits
  (`2026-10-13T18:48:01.380Z`), so they sort and slice as text. Ids are numbers; request ids are never reused.
- Every error has one shape:

```json
{"error": {"code": "invalid", "message": "rule 2: the count N must be at least 1", "field": "rules[1]"}}
```

| Status | `code` | When |
|---|---|---|
| 400 | `bad_json`, `unknown_field` | the body is not valid JSON, has an unknown field or a wrong type (`field` names it) |
| 401 | `unauthorized`, `invalid_token` | login is on and the request has no credentials, or a token that is not valid |
| 403 | `cross_origin` | a write from a web page on another site |
| 403 | `forbidden`, `session_required` | only an admin may do this; managing tokens and users needs a login, not a token |
| 404 | `not_found` | no such endpoint, secret, request or route |
| 409 | `duplicate_path`, `secret_in_use`, `conflict` | the path is taken; the secret is used by a check; too many concurrent changes |
| 409 | `exists`, `last_admin`, `self_demotion`, `login_off` | a user or token name is taken; the change would leave no admin; an admin would make themselves a user; there are no users yet |
| 412 | `stale_version` | the behaviour changed since you read it (`If-Match`) |
| 413 | `too_large` | the body is over 1 MiB |
| 415 | `unsupported_media_type` | a write without `Content-Type: application/json` |
| 421 | `wrong_host` | called by a host name that is not allowed (see [Access](#access)) |
| 422 | `invalid` | the value is not acceptable; `field` is a JSON path such as `checks[0]`, `path`, `value`, `wait` |
| 428 | `version_required` | a behaviour `PUT` without `If-Match` |

## Routes

All paths below are relative to `/admin/api/v1`. The examples use

<!-- docs-check: skip: defines $API; docs-check's setups define it for each example -->
```sh
API=http://localhost:8080/admin/api/v1
```

### Endpoints

An endpoint is an exact path that catches requests. Everything else is caught
into the Unmatched register with a `404`.

| Route | Does |
|---|---|
| `GET /endpoints` | all endpoints, by path; `?path=/x` finds one (an empty list when there is none); `?enabled=true` or `false` only those on or off |
| `POST /endpoints` | create: `{"path": "/x"}`; no path or an empty one picks a random path. `201`, `Location` |
| `GET /endpoints/{id}` | one endpoint |
| `PATCH /endpoints/{id}` | change its path, `{"path": "/y"}` (validated as on create; its requests and behaviour stay), turn it off or on, `{"enabled": false}`, or both at once (both change or, on an error, neither). `200` with the endpoint |
| `DELETE /endpoints/{id}` | delete it and its requests. `204` |

```console
$ curl -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/orders/events"}'
201
{"id":1,"path":"/orders/events","url":"http://localhost:8080/orders/events","requests":0,"counter":0,"enabled":true,"created_at":"2026-10-12T17:28:06.808Z"}
```

- `url` is where senders deliver: the path on the host name you called the
  API by (rewrite the host when the sender reaches hookwitness by another
  name, e.g. from another container).
- `requests` is how many are kept; `counter` how many passed the checks since
  the rule counter was last reset; `last_request_at` appears once one arrived.
- `enabled` is `false` for an endpoint that is off: it keeps its path,
  behaviour, secrets, requests and counter, and requests to its path are
  answered `404` and kept in the Unmatched register (outcome `source` `off`).
  A new endpoint is on.
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

```console
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
    {"when": {"kind": "every", "n": 7},       "do": "reply", "reply": {"status": 200, "body_base64": "//4="}},  // exact bytes
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

- **Bodies:** `body` is a template; `body_base64` gives a reply's exact bytes
  instead (invalid UTF-8, binary), sent as they are and at most 1 MiB. A
  reply with both is refused (`422`). A recorded outcome keeps a reply body
  that is not UTF-8 as `body_base64` too.
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
  the endpoint's `schemas`: `"schema": "OrderPaid"`; any of several,
  `"schemas": ["V1", "V2"]` (the reason names the one matched, or, when none
  did, the closest one, whose `problems` the verdict carries, and the others'
  counts; "closest" is the fewest problems at the top of the body, a wrong
  shape, then the fewest in all, then the first listed); or picked by a
  discriminator, `"select": {"from": "body", "key": "/type", "map": [{"value":
  "order.paid", "schema": "OrderPaid"}], "default": ["V1", "V2"]}` (`from` is
  `body` with a JSON pointer or `header` with a header name; `default` is one
  name or a list, for a value without a mapping or a request without the
  value). Without a default such a request gets the verdict `n/c` (not
  checked) and is never refused; enforced failures are
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

### Setup files

An endpoint's setup (path, behaviour, the names and kinds of its secrets) as
a file to share between instances ([Sharing a setup](sharing.md)). The file
never holds a secret's value, hash or fingerprint.

| Route | Does |
|---|---|
| `GET /setup` | the setup file of `?id=<id>` (repeat it for several) or of every endpoint; `Content-Disposition` names it `<path>.hookwitness.json` |
| `POST /setup` | create the file's endpoints, all or none; `201` when it created one, else `200`, with `{"endpoints": [{"id", "path", "result", "secrets_not_set"}]}`. `?on_conflict=replace` replaces the behaviour of an endpoint already at a path (its requests, counter and on/off state stay; a secret of the same name and kind keeps its value); without it a taken path is `409 duplicate_path` |

```console
$ curl -X POST $API/setup -H 'Content-Type: application/json' --data-binary @payments-webhooks.hookwitness.json
201
{"endpoints":[{"id":3,"path":"/payments/webhooks","result":"created","secrets_not_set":["STRIPE_SECRET"]}]}
```

- `result` is `created`, `replaced` or `unchanged` (the same setup applied
  again); importing the same file twice changes nothing.
- An imported secret has no value (`"set": false` in the behaviour): checks
  that use it give `n/v` "secret NAME not set" and never refuse. Give the
  value with `PUT /endpoints/{id}/secrets/{name}`.
- A file that is not a setup (`"hookwitness": 1` missing), of a newer
  format, with an unknown field, an invalid path or behaviour is `422
  invalid_setup`, its message naming the problem; nothing is imported. Over
  16 MiB is `413`.

### Secrets

Signing keys and passwords that checks use. They are **write-only**: a value
goes in and never comes back; answers show a fingerprint.

| Route | Does |
|---|---|
| `PUT /endpoints/{id}/secrets/{name}` | create or replace: `{"kind": "token" \| "key", "value": "…"}`; replacing keeps the kind (it may be left out) |
| `DELETE /endpoints/{id}/secrets/{name}` | delete; `409 secret_in_use` while a check uses it. `204` |

```console
$ curl -X PUT $API/endpoints/1/secrets/ORDERS_PASSWORD -H 'Content-Type: application/json' \
    -d '{"kind":"token","value":"test-password"}'
200
{"name":"ORDERS_PASSWORD","kind":"token","fingerprint":"3f6d2964","set":true,"used_by":[]}
```

- `set` is `false` for a secret imported from a setup file without its
  value (`fingerprint` is empty then); this call gives it one.

- `kind`: `token` for bearer tokens and Basic passwords (stored as a salted
  hash), `key` for signing keys (stored encrypted).
- Names are letters, digits and `_`, not starting with a digit.
- Use test values only. A caught request keeps its headers, so a password a
  sender sent is visible in that request.

### Requests

| Route | Does |
|---|---|
| `GET /endpoints/{id}/requests` | newest first; `limit` (default 50, at most 500; a larger one is capped), `before=<id>` pages back |
| `GET /endpoints/{id}/requests?after=<id>` | the requests newer than `<id>`, **oldest first** |
| `GET /endpoints/{id}/requests?after=<id>&wait=30s` | the same, but when there are none yet, wait until one arrives (at most `60s`; `wait=30` means seconds); an empty list when the time is up |
| `…/requests?verdict=fail&status=401` | filters, with any of the above: `verdict` is `pass`, `fail`, `n/v` or `n/c`; `status` an answer status (`0`: nothing sent) |
| `…/requests?q=<text>` | only requests whose kept body contains the text: a byte substring, case-sensitive (URL-encode it; `%FF` is the byte 0xFF) |
| `…/requests?json=<pointer>=<value>` | only requests whose JSON body has, at the [JSON pointer](https://www.rfc-editor.org/rfc/rfc6901), a string equal to the value, or a number, `true`, `false` or `null` written exactly as the value: `json=/taskId=42` matches `"taskId":42` and `"taskId":"42"`, not `42.0`; objects and arrays never match. The pointer ends at the first `=`. Repeat it to require several: all must match |
| `…/requests?include=body` | each item carries `body_text` or `body_base64`, as one request does; `limit` is then at most 100 |
| `DELETE /endpoints/{id}/requests` | clear the endpoint's requests (numbering goes on). `204` |
| `GET /unmatched/requests` | the Unmatched register (requests to paths no endpoint has), with the same parameters; read-only |
| `GET /requests/{id}` | one request in full, Unmatched ones too |
| `GET /requests/{id}/body` | the exact body as received, with the sender's `Content-Type`, always as a download (`attachment`, `nosniff`, a `sandbox` CSP): whatever a sender sent never runs as a page; `404 not_kept` when it was over the limit |
| `GET /requests/{id}/image?path=<pointer>` | the decoded bytes of an image in the body (`path=` empty for a body that is an image), typed by what they are; `404 not_found` without a value there, `422 not_an_image` when it is no image |

A list item:

```json
{"id":1,"endpoint_id":1,"no":1,"received_at":"2026-10-12T17:28:07.433Z","method":"POST","path":"/orders/events",
 "query":"","status":200,"verdict":"pass","body_size":28,"body_kept":true,"content_type":"application/json","replays":0}
```

- `status` is what hookwitness answered (`0` while a timeout still holds it).
- `verdict` is the overall verdict of the authorization and schema checks:
  `fail` if any failed, else `n/v` if any could not verify, else `pass` if any
  passed, else `n/c` when none applied; `""` when the endpoint has none.
- `body_kept` is false when the body was over `-max-body` (answered `413`) or
  the server was busy reading other bodies (answered `503`).
- `body_cut` is true when only the start of the body was kept: the Unmatched
  register keeps 64 KiB; `body_size` is the whole.

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
  `label`, `check` with the refusing `check`, `limit`, `busy`, `unmatched`
  for a path without an endpoint, `off` for an endpoint that is off), `action` (`reply`,
  `timeout`, `drop`, `refused`, `error`, `abandoned`), the reply sent
  (`status`, `headers` as `{name, value}`, the first 64 KiB of `body`),
  `delay_ms`, `waited_ms` for a timeout, `counter`. `abandoned`: the sender
  gave up during the reply's delay, so nothing was sent — `status` is `0`,
  `waited_ms` how long the sender stayed, `delay_ms` the delay it did not
  wait out, `would_status` the reply it would have got; reports count it as
  a timeout.
- `verdicts` has one entry per authorization or schema check: `check` (its
  1-based position), `label`, `result`, `reason`, `at`; a failed schema check
  adds `problems` (up to 20 of `{path, keyword, message}`, `path` a JSON
  pointer into the body) and `total`; a schema check that declares images
  adds `images` (see below); a signature check adds `expected` and
  `received`, and `note` when the signature passed and repeats one an
  earlier kept request of the endpoint carried ("same signature as #N: a
  replay, or a redelivery of the same signed request"; the result is not
  changed by it).
- `images` lists the pictures in the body: those a schema declares
  (`source: "schema"`), `data:image/...` strings no schema declares
  (`"data URL"`), and a body that is itself an image (`"body"`, `path` "").
  Each has `path`, `declared` (the schema's type), `type` (what the bytes
  are), `size`, `width`, `height`, `sha256`, `error` when it is not a valid
  image of its declared type, `not_checked` when its pixels were not decoded
  (over 128 MB to decode, or past the request's 256 MB; the header was
  checked), and `url` for its bytes:

  ```json
  "images":[{"path":"/avatar","declared":"image/png","type":"image/png","size":4120,"width":64,"height":64,
             "sha256":"6e67…","source":"schema","url":"/admin/api/v1/requests/7/image?path=%2Favatar"}]
  ```
- In shell scripts pipe JSON from a variable with `printf '%s\n' "$VAR"`, not
  `echo` (zsh's `echo` expands `\n` inside JSON strings).

**Waiting without missing anything:** keep the id of the last request you have
handled (0 at the start) and ask for `after=<that id>&wait=…`. Requests that
arrive between two calls are returned by the next one; none is returned twice.
With `q` or `json` the wait ends at the first request that matches; the others
do not end it. A cursor taken from a filtered answer skips the requests in
between that did not match: to wait for several things, give each its own
cursor (see [the recipe](#wait-for-the-callback-of-one-task)).

### Actions

Writes, so send them as JSON (an empty object is enough):

| Route | Does |
|---|---|
| `POST /endpoints/{id}/reset-counter` | start the rules' count again ("every 3rd" counts from the next request). `200` with the endpoint |
| `POST /endpoints/{id}/recheck` | verify the kept requests again with the current checks, secrets and schemas — after fixing a secret or a schema. Only verdicts change. `200 {"rechecked": N}` |
| `POST /endpoints/{id}/schema-draft` `{"requests":[ids]}` | draft a JSON Schema (2020-12) from the bodies of up to 100 of the endpoint's requests; **nothing is stored**. `200` with `name` (suggested), `text`, `exists` and `changes` (how it differs from a stored schema of that name), `discriminator` (the check that picks by a value, the value the requests share, and what it maps to now). `422` (field `requests`) for a body not kept or not JSON, or a request of another endpoint |

<!-- docs-check: setup: api -->
```sh
curl -s -X POST $API/endpoints/1/recheck -H 'Content-Type: application/json' -d '{}' | jq -c .
# {"rechecked":0}
```

A draft holds the types seen, objects with their properties (required when
every body has the key; other keys allowed), array items merged from all
elements, `integer` when every number is whole, `null` in the type list, and
a `format` (date-time, date, uuid, email, uri) only when every string there
has it. It never guesses enums, patterns, ranges or lengths: review it, then
save it with the behaviour (and map it, if a check picks by a value):

<!-- docs-check: fresh; setup: api_draft -->
```sh
D=$(curl -s -X POST $API/endpoints/$ID/schema-draft -H 'Content-Type: application/json' -d '{"requests":[7,8]}')
printf '%s\n' "$D" | jq -r .text > OrderShipped.json     # review and edit
printf '%s\n' "$D" | jq -c '{name, exists}'   # the name is suggested from the path
# {"name":"Events","exists":false}
```

### Replays

A caught request sent again to a URL, byte for byte, and what that target
answered ([Replaying a request](replay.md)).

| Route | Does |
|---|---|
| `POST /requests/{id}/replays` `{"url": …}` | send it to `url` and wait for the answer (or the error). `201` with the replay. Optional: `"host": "received"` sends the caught `Host` line (default `"target"`: the URL's host and port), `"insecure_tls": true` accepts any https certificate, `"timeout_ms"` (default 30000, at most `-max-wait`). A target that refuses, times out or resets is still a replay, with `status` 0 and `error` set |
| `GET /requests/{id}/replays` | its replays, newest first (the newest 50 are kept); `include=body` adds the answers' bodies |
| `GET /replays/{id}` | one replay with its answer body |
| `GET /replays/{id}/body` | the answer body as the target sent it (never decompressed, at most 1 MiB), with its `Content-Type`, always as a download |

Refusals: `409 body_not_kept` when the body was not kept whole (over the
limit, or cut in Unmatched); `422 invalid` (field `url`, `host` or
`timeout_ms`): not http or https, a user name or password in the URL, a
timeout out of range; `422 target_refused` (field `url`): a link-local or
cloud-metadata address, also through a name resolving to one, or a target
outside `-replay-allow`, with the rule in the message; `503 busy` when 4
replays are running or `-max-inflight` is spent (`Retry-After`). A request
carries `"replays"`: how many it has.

```json
{"id":3,"request_id":12,"created_at":"2026-10-19T09:14:02.118Z","by":"ann","url":"http://localhost:3000/webhooks/github",
 "host":"target","insecure_tls":false,"timeout_ms":30000,"line":"POST /webhooks/github HTTP/1.1",
 "sent":[["Host","localhost:3000"],["X-GitHub-Event","pull_request"],["X-Hub-Signature-256","sha256=5c1f…"],["Content-Length","73"]],
 "notes":[],"proto":"HTTP/1.1","status":401,"reason":"Unauthorized",
 "headers":[["Content-Type","application/json"],["Content-Length","30"]],"body_size":30,"body_cut":false,
 "duration_ms":4,"ttfb_ms":3,"error":null,"body_text":"{\"error\":\"signature mismatch\"}"}
```

### Reports

| Route | Does |
|---|---|
| `GET /endpoints/{id}/report` | the report the UI exports, as a download |
| `GET /unmatched/report` | the same for the Unmatched register |

Parameters as in the UI's dialog: `format` (`md`, `html`, `pdf`; required),
`scope` (`all`, `range` with `from` / `to`, `picked` with `pick=3,5-9`),
`mask` (a header name; repeat it) and `mask_other` (comma-separated),
`images` (`shown` or `listed`), `setup` (`true` adds the endpoint's setup:
checks, schemas, rules, replies, variables, and secrets by name, kind and
whether they are set, never a value; the Unmatched register has none, so
it is ignored there). Times are RFC 3339 (`2026-10-13T09:00:00Z`) or
a date-time without a zone, read as UTC. A wrong parameter is `422 invalid`
naming it.

<!-- docs-check: setup: api -->
```sh
curl -s -o report.pdf -w '%{http_code}\n' "$API/endpoints/1/report?format=pdf&scope=picked&pick=3-5&mask=Authorization"
# 200
```

### Users and tokens

The caller, their tokens, and (for admins) users and everyone's tokens.
Everything but `GET /me` needs a login (the web UI's cookie, or the
session of a script that logged in at `<prefix>/login`): a token gets `403
session_required`, so a leaked token cannot make others. While there are no
users the user routes are open, to add the first one, which must be an
admin.

| Route | Does |
|---|---|
| `GET /me` | who the caller is: `{"login":true,"name","role","via"}` (`via`: `session`, `token`, `machine token`), or `{"login":false}` while login is off |
| `PUT /me/password` `{"current","new"}` | changes your password (at least 12 characters); your other logins end. `204`; `422` (field `current` or `new`) |
| `GET /me/tokens` | your tokens: `id`, `name`, `shown` (`hkw_<id>_…`), `created_at`, `last_used_at`; never the value |
| `POST /me/tokens` `{"name"}` | makes a token; the answer's `token` is the value, **shown this once**. `201`; `409 exists` for a name you use |
| `DELETE /me/tokens/{id}` | revokes one of yours: refused from its next request. `204` |
| `GET /users` | users with `name`, `role`, `created_at`, `tokens` (admins) |
| `POST /users` `{"name","role","password"}` | adds a user (`role` `user` by default). `201`; `409 exists`; `422` naming the field |
| `PATCH /users/{name}` `{"role"?, "password"?}` | changes a role or sets a password; either ends that user's logins. `409 last_admin` for the last admin, `409 self_demotion` when you would make yourself a user |
| `DELETE /users/{name}` | removes a user with their tokens. `204`; `409 last_admin` |
| `GET /tokens`, `DELETE /tokens/{id}` | everyone's tokens, with their `owner`; revoke any (admins) |

### OpenAPI

`GET /openapi.json` describes every route, parameter and answer (OpenAPI
3.1), for clients and code generators. The repository's tests check that it
is valid, lists every route, and that real answers match its schemas.

## Recipes

### Wait for a webhook and read its verdict

<!-- docs-check: setup: api_sender -->
```sh
EP=$(curl -s -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/hooks"}')
ID=$(printf '%s\n' "$EP" | jq .id); URL=$(printf '%s\n' "$EP" | jq -r .url)
LAST=0

# ... make the app under test send its webhook to $URL ...

R=$(curl -s "$API/endpoints/$ID/requests?after=$LAST&wait=30s" | jq '.requests[0]')
[ "$R" != null ] || { echo "no webhook within 30 s"; exit 1; }
printf '%s\n' "$R" | jq -c '{status, verdict}'
# {"status":200,"verdict":""}
LAST=$(printf '%s\n' "$R" | jq .id)
curl -s $API/requests/$LAST | jq -r .body_text | jq -c .   # the payload as sent
# {"order":42}
```

### Wait for the callback of one task

The sender calls back once per task and the endpoint is busy with others: take
the cursor before starting the task, then wait for the request whose body
names it, with its body in the same answer.

<!-- docs-check: setup: api_tasks -->
```sh
LAST=$(curl -s "$API/endpoints/$ID/requests?limit=1" | jq '.requests[0].id // 0')

# ... start task 42 in the app under test ...

R=$(curl -s "$API/endpoints/$ID/requests?after=$LAST&wait=30s&json=/taskId=42&include=body" | jq '.requests[0]')
[ "$R" != null ] || { echo "no callback for task 42 within 30 s"; exit 1; }
printf '%s\n' "$R" | jq -c '{status, verdict}'
# {"status":200,"verdict":""}
printf '%s\n' "$R" | jq -r .body_text | jq .state   # the payload, no second call
# "done"

# every callback of the task so far, bodies included (at most 100 a page)
curl -s "$API/endpoints/$ID/requests?after=$LAST&json=/taskId=42&include=body" | jq -r '.requests[].body_text'
# {"taskId":42,"state":"done"}
```

`q=` finds text in any body, JSON or not: `q=batch-42` instead of
`json=/taskId=42`.

### Require Basic credentials

<!-- docs-check: fresh; setup: api -->
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

<!-- docs-check: setup: api -->
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

### Replay a request to your handler

Your handler refuses a provider's delivery. Send the caught original to it
unchanged and read its answer: if it refuses that too, its verification is
wrong, not the bytes. Here a second endpoint that checks the same GitHub
signature stands in for the handler:

<!-- docs-check: setup: api handler -->
```sh
BODY='{"action":"opened"}'
SIG=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac gh_test_secret | awk '{print $2}')
curl -s -o /dev/null -d "$BODY" -H "X-Hub-Signature-256: sha256=$SIG" $URL   # 200
REQ=$(curl -s "$API/endpoints/$ID/requests?limit=1" | jq '.requests[0].id')
HANDLER=${URL%/orders/events}/handler   # yours: http://localhost:3000/webhooks/github
curl -s -X POST $API/requests/$REQ/replays -H 'Content-Type: application/json' \
  -d "{\"url\":\"$HANDLER\"}" | jq -c '{status, error}'
# {"status":200,"error":null}
```

## Testing a sender in Docker

<!-- docs-check: skip: starts a container; scripts/docker-smoke.sh runs it in CI -->
```sh
docker run -d --name hookwitness -p 127.0.0.1:8080:8080 ghcr.io/petroladkin/hookwitness:0.1
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
    image: ghcr.io/petroladkin/hookwitness:0.1
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
