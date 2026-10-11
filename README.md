# Hookwitness

Catch webhooks, get a verdict, attach the evidence. Self-hosted, single binary,
MCP-ready.

**Status: early development.** Catch endpoints with a live view work; scripted
responses, verdicts, reports, the API and MCP come next.

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
Endpoints answer `200` with an empty body; a body over the limit gets `413`
and is recorded without its body. Each endpoint keeps its newest requests up
to the retention limit. The same body delivered again is marked as a retry.

Everything except `/healthz` and the admin prefix is caught, so senders can be
pointed at their real paths. To expose the catch endpoints but not the UI,
publish everything except `/admin` through your reverse proxy. The UI itself
has no login: keep it on localhost or behind the proxy.

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

Ctrl-C or SIGTERM stops the server gracefully. `make check` runs the templ
check, the build, `go vet`, the gofmt check, staticcheck and the tests. The
HTML components are [templ](https://templ.guide) files; after editing one, run
`make generate` and commit the generated `*_templ.go` with it.

## License

[Apache License 2.0](LICENSE). Third-party licenses of the modules compiled
into the binary and of the bundled web assets (htmx, fonts) are in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
