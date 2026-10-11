# Hookwitness

Catch webhooks, get a verdict, attach the evidence. Self-hosted, single binary,
MCP-ready.

**Status: early development.** Catch endpoints with a live view and scripted
responses work; verdicts, reports, the API and MCP come next.

## What it is for

You send webhooks to someone else's endpoint and need to prove they actually
left your side, correctly signed and shaped. Hookwitness gives you catch
endpoints with a live view, scriptable responses (status, delay, timeout,
dropped connection, every Nth request failing), pass/fail verdicts for
signatures and JSON Schema, and a report you can attach to a ticket. A REST
API and an MCP server let tests and AI agents drive it.

## Quick start

```sh
make build
bin/hookwitness serve
```

1. Open <http://127.0.0.1:8080/admin/> and create an endpoint, say
   `/stripe/webhooks` (leave the path empty for a random one).
2. Send it a request, or point the real sender at it:

   ```sh
   curl -X POST http://127.0.0.1:8080/stripe/webhooks \
     -H 'Content-Type: application/json' \
     -d '{"type":"invoice.paid","id":"evt_1"}'
   ```

3. Watch it arrive on the endpoint's page without a reload: method, path,
   query, headers in the order they were sent, and the body (pretty JSON, raw
   text, or a hex view for binary), with the raw body to download.

An endpoint is an exact path: `/a` and `/a/` are different endpoints, the
query string is ignored. Any other path is answered `404` and kept in the
**Unmatched** register, so a typo in a sender's URL shows up at once.
Endpoints answer `200` with an empty body until you script them (below); a
body over the limit gets `413` and is recorded without its body. Each endpoint keeps its newest requests up
to the retention limit. The same body delivered again is marked as a retry.

## Response emulation

Senders have to cope with receivers that are slow, time out, crash or fail now
and then. Each endpoint's **Behaviour** page sets how it answers, one tab per
part (checks, rules, default reply, variables):

- **Checks** run first. A required header (present, equal to a value, or
  matching a regular expression) refuses a request that lacks it, with a `400`
  naming the header or with your own reply.
- **Rules** come next, in order; the first whose condition holds decides.
  Conditions: every Nth request, the first N requests since the counter was
  reset, or at random with a probability. Actions: a reply (status, headers,
  body, delay with jitter), a timeout (hold the connection and never answer,
  until the sender gives up or `-max-wait` passes), or a drop (reset the
  connection, as a crashed receiver would).
- The **default reply** answers the rest. It starts as `200` with an empty body.
- **Variables** (`NAME=value`) are test values for templates, shown in plain
  text: never put real secrets there.

Requests refused by a check do not count towards the rules. Every request
records what was done with it: the rule or the default, the status and headers
sent, the measured delay, how long a timed-out sender waited.

Header values and bodies are Go templates over the request:
`{{.Header "X-Request-Id"}}`, `{{.Query "name"}}`, `{{.JSON "event.items.0.id"}}`
(a field of a JSON body), `{{.Var.TOKEN}}`, `{{.N}}` (the request's number
since the counter reset), `{{.Method}}`, `{{.Path}}`, `{{.Body}}`, with `if`,
`with` and the standard functions; `range` and sub-templates are not
supported, and rendering is capped at 1 MiB and one second.

Examples:

| To test | Set up |
|---|---|
| Fail every 3rd delivery | rule: every 3rd → reply `500` |
| Fail the first two, then recover (retries with backoff) | rule: first 2 → reply `503`, header `Retry-After: 2`; reset the counter to run it again |
| A slow receiver | default reply: delay `4000` ms, jitter `2000` ms |
| A receiver that hangs | rule: at random 10% → time out |
| A crashing receiver | rule: every 5th → drop |
| Echo a URL-verification challenge (Slack-style) | default reply: header `Content-Type: application/json`, body `{"challenge":"{{.JSON "challenge"}}"}` |
| Demand a header like the real client | check: header `X-Api-Key` equals `key_test`, own reply `401` with a JSON error body |

Response headers go out as net/http writes them (sorted by name; repeated
headers keep their order). `Content-Length`, `Transfer-Encoding` and
`Connection` are the server's and cannot be set.

Everything except `/healthz` and the admin prefix is caught, so senders can be
pointed at their real paths. To expose the catch endpoints but not the UI,
publish everything except `/admin` through your reverse proxy. The UI itself
has no login: keep it on localhost or behind the proxy. It answers only when
opened by IP address or `localhost` (so a hostile web page cannot reach it by
pointing its own domain at your server); allow other host names with
`-admin-hosts`.

## Build from source

Requires Go 1.27 or newer. `make check` also needs a C compiler for the
race detector (`build-essential` on Debian/Ubuntu, Xcode command line tools
on macOS); the binary itself is built without cgo.

```sh
make build                    # writes bin/hookwitness, a single static binary
bin/hookwitness version
bin/hookwitness serve         # listens on 127.0.0.1:8080, database ./hookwitness.db
curl http://127.0.0.1:8080/healthz
# {"status":"ok","version":"dev"}
```

Settings come from flags or environment variables; a flag wins over the
variable.

| Flag | Variable | Default |
|---|---|---|
| `-addr` | `HOOKWITNESS_ADDR` | `127.0.0.1:8080` |
| `-db` | `HOOKWITNESS_DB` | `hookwitness.db` |
| `-log-format` | `HOOKWITNESS_LOG_FORMAT` | `text` (or `json`) |
| `-max-body` | `HOOKWITNESS_MAX_BODY` | `10MiB` (bytes, or with `KiB`, `MiB`, `GiB`) |
| `-retention` | `HOOKWITNESS_RETENTION` | `1000` requests per endpoint |
| `-admin-prefix` | `HOOKWITNESS_ADMIN_PREFIX` | `/admin` |
| `-admin-hosts` | `HOOKWITNESS_ADMIN_HOSTS` | none: host names, comma-separated |
| `-max-wait` | `HOOKWITNESS_MAX_WAIT` | `5m`: longest reply delay or timeout hold (1s to 1h) |

Ctrl-C or SIGTERM stops the server gracefully. `make check` runs the templ
check, the build, `go vet`, the gofmt check, staticcheck and the tests. The
HTML components are [templ](https://templ.guide) files; after editing one, run
`make generate` and commit the generated `*_templ.go` with it.

## License

[Apache License 2.0](LICENSE). Third-party licenses of the modules compiled
into the binary and of the bundled web assets (htmx, fonts) are in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
