# Hookwitness

Catch webhooks, get a verdict, attach the evidence. Self-hosted, single binary,
MCP-ready.

**Status: early development.** The server starts and reports its health; catching webhooks comes next.

## What it is for

You send webhooks to someone else's endpoint and need to prove they actually
left your side, correctly signed and shaped. Hookwitness gives you catch
endpoints with a live view, scriptable responses (status, delay, timeout,
dropped connection, every Nth request failing), pass/fail verdicts for
signatures and JSON Schema, and a report you can attach to a ticket. A REST
API and an MCP server let tests and AI agents drive it.

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

Ctrl-C or SIGTERM stops the server gracefully. `make check` runs the build,
`go vet`, the gofmt check, staticcheck and the tests.

## License

[Apache License 2.0](LICENSE). Third-party licenses of the modules compiled
into the binary are in [THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
