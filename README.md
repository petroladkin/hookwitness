# Hookwitness

Catch webhooks, get a verdict, attach the evidence. Self-hosted, single binary,
MCP-ready.

**Status: early development.** Catch endpoints with a live view, scripted
responses, authorization and JSON Schema verdicts, reports (Markdown, HTML,
PDF) and the REST API work; the MCP server comes next.

## What it is for

You send webhooks to someone else's endpoint and need to prove they actually
left your side, correctly signed and shaped. Hookwitness gives you catch
endpoints with a live view, scriptable responses (status, delay, timeout,
dropped connection, every Nth request failing), pass/fail verdicts for
signatures and JSON Schema, and a report you can attach to a ticket. A REST
API and an MCP server let tests and AI agents drive it.

## Quick start

<!-- docs-check: skip: starts a long-running container; scripts/docker-smoke.sh runs this command in CI -->
```sh
docker run -d --name hookwitness -p 127.0.0.1:8080:8080 -v hookwitness:/data \
  ghcr.io/petroladkin/hookwitness:edge
```

1. Open <http://127.0.0.1:8080/admin/>, click **New endpoint** and create
   `/stripe/webhooks`.
2. Send it a request, or point the real sender at it:

   <!-- docs-check: setup: stripe_endpoint -->
   ```sh
   curl -s -X POST http://127.0.0.1:8080/stripe/webhooks \
     -H 'Content-Type: application/json' \
     -d '{"type":"invoice.paid","id":"evt_1"}'   # 200
   ```

3. Watch it arrive on the endpoint's page without a reload: method, path,
   query, headers in the order they were sent, the body, and what Hookwitness
   answered.

Keep `-v hookwitness:/data`: it holds your endpoints, secrets and requests
across container upgrades. Built from source instead: `make build && bin/hookwitness
serve` (Go 1.27+). [Getting started](docs/getting-started.md) walks through it.

## What it does

- **[Endpoints and the live view](docs/endpoints.md)**: exact paths, a live
  register with a recorder strip, an Unmatched register for typos, retries
  marked.
- **[Scripted replies](docs/replies.md)**: fail every Nth delivery, the first
  N, or at random; delay, hang, drop the connection; templated bodies.
- **[Authorization checks](docs/authorization.md)**: GitHub, Stripe, Standard
  Webhooks (Svix), generic HMAC, bearer, basic; a verdict per request, enforced
  if you want; write-only secrets.
- **[JSON Schema checks](docs/schemas.md)**: the body against one schema, any
  of several, or one picked by the event's type; problems marked in the body;
  schemas drafted from caught bodies; images in the payload decoded.
- **[Reports](docs/reports.md)**: the evidence as Markdown, one HTML file or a
  PDF, with a summary and optional header masking.
- **[REST API](docs/api.md)**: everything the UI does, plus long-poll waits;
  an [agent skill](docs/agents.md) for testing senders with Claude Code.

## Documentation

- English: [docs/](docs/README.md)
- Українською: [docs/uk/](docs/uk/README.md)

[Configuration and deployment](docs/deployment.md) covers Docker, the
settings, the secret key and reverse proxies;
[Troubleshooting](docs/troubleshooting.md) the usual surprises.

## Security

Report vulnerabilities privately, as [SECURITY.md](SECURITY.md) describes.

## License

[Apache License 2.0](LICENSE). Third-party licenses of the modules compiled
into the binary and of the bundled assets (htmx, fonts) are in
[THIRD-PARTY-NOTICES.md](THIRD-PARTY-NOTICES.md).
