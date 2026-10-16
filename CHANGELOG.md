# Changelog

All notable changes to Hookwitness. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/): until 1.0, a minor version (0.2)
may change the API, the flags or the stored data, and says how here.

## [0.1.0] - 2026-10-17

The first release.

### Catching

- Endpoints are exact paths, managed in the web UI under the admin prefix
  (`/admin`). Any other path is caught into the Unmatched register and
  answered 404, so a typo in the sender's URL shows up.
- Each endpoint's register shows its requests live: method, path, query,
  headers in the order and case they were sent, the body (pretty JSON, text
  or hex) and what was answered. Retries of the same body are marked, and the
  recorder strip shows the last two minutes.
- Bodies are limited (`-max-body`, up to 512 MiB; `-max-inflight` for the
  bytes held at once), and each endpoint keeps its newest `-retention`
  requests.

### Scripted replies

- Required-header checks, ordered rules (every Nth, the first N, a random
  share) and a default reply, per endpoint.
- A reply is a status, headers and a body (a Go template with the request's
  data and the endpoint's variables, or exact bytes), after an optional
  delay. It can also time out (hold the request, never answer) or drop the
  connection (a reset).
- Catch replies carry `Content-Security-Policy: sandbox` and
  `X-Content-Type-Options: nosniff` unless the endpoint sets them.

### Verdicts

- Authorization checks: GitHub, Stripe, Standard Webhooks (Svix), generic
  HMAC, bearer and Basic. Each request gets pass, fail (with the reason) or
  n/v, optionally enforced with the endpoint's own reply, and can be checked
  again after a secret changes.
- Secrets are write-only: signing keys are sealed with the server's key
  (`HOOKWITNESS_SECRET_KEY` or `<db>.key`), tokens are hashed.
- JSON Schema checks: one schema, any of several, or one picked by a body
  pointer or a header. Problems are listed and marked in the body. A schema
  can be drafted from caught bodies.
- Images in payloads (declared with `contentMediaType: image/*`, data URLs,
  image bodies) are decoded, checked, previewed and shown in reports.

### Reports

- A register's requests (all, a time range or picked by number) as
  Markdown, one self-contained HTML file or a PDF, with a summary (answers,
  verdicts per check, retry groups, the strip) and optional header masking.
- Every report names the build that caught its requests.

### API and agents

- A REST API under `<prefix>/api/v1` covering the UI: endpoints, behaviour
  (with ETag and If-Match), secrets, requests with filters and long-poll
  waits, the Unmatched register, raw bodies, images, reports. Described by
  OpenAPI 3.1 at `<prefix>/api/v1/openapi.json`.
- An MCP server at `<prefix>/mcp` (Streamable HTTP) with task-shaped tools,
  among them `wait_for_request` and `expect`; `hookwitness mcp` bridges stdio
  clients to it.
- An agent skill (`skills/hookwitness`) for testing webhook senders with
  Claude Code.

### Running it

- One Go binary with sqlite, no cgo: `hookwitness serve`, `health`, `mcp`,
  `version`.
- A container image for linux/amd64 and linux/arm64
  (`ghcr.io/petroladkin/hookwitness:0.1`): distroless, non-root, data in the
  `/data` volume, works read-only, with a health check, a signed build
  provenance and an SBOM.
- The web UI answers only to IP addresses, localhost and the names in
  `-admin-hosts`, and refuses cross-origin form posts. It has no login yet:
  keep the admin prefix on localhost or behind a proxy that does not publish
  it.
- Documentation for every task in English and Ukrainian (`docs/`, `docs/uk/`).

[0.1.0]: https://github.com/petroladkin/hookwitness/releases/tag/v0.1.0
