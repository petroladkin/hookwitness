# Hookwitness

Catch webhooks, get a verdict, attach the evidence. Self-hosted, single binary,
MCP-ready.

**Status: early development.** Nothing to run yet.

## What it is for

You send webhooks to someone else's endpoint and need to prove they actually
left your side, correctly signed and shaped. Hookwitness gives you catch
endpoints with a live view, scriptable responses (status, delay, timeout,
dropped connection, every Nth request failing), pass/fail verdicts for
signatures and JSON Schema, and a report you can attach to a ticket. A REST
API and an MCP server let tests and AI agents drive it.

## License

[Apache License 2.0](LICENSE).
