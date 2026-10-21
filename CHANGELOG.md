# Changelog

All notable changes to Hookwitness. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/): until 1.0, a minor version (0.2)
may change the API, the flags or the stored data, and says how here.

## [0.2.0] - 2026-10-21

Users and login, replay, setup files, the admin on its own address, and
release binaries.

### Changed

- Catch replies always carry `Content-Security-Policy: sandbox`, also when
  the endpoint sets a policy of its own (sent next to it), and a reply never
  sets the login cookie: catch paths share the admin's origin, so a reply
  must not act as a page of the admin.
- The database is upgraded at start (migrations 5 to 9), one way: 0.1 refuses
  it afterwards. Back up the volume before upgrading.

### Users and login

- Login turns on with the first user, who is always an admin; until then the
  admin stays open, with a "No login" line on every page and a warning at
  start when it listens beyond loopback. The first admin comes from the
  Users page, from `HOOKWITNESS_ADMIN_USER` and
  `HOOKWITNESS_ADMIN_PASSWORD_FILE` at start, from `hookwitness user add`
  or from the API.
- Two roles: a user manages endpoints, an admin also manages people. Nobody
  demotes or removes themselves, so an admin always remains.
- Browser sessions (idle 24 h, at most 7 days), personal tokens for scripts
  and agents (`Authorization: Bearer hkw_…`, shown once), and a machine
  token (`HOOKWITNESS_API_TOKEN_FILE`) that closes the API and MCP even
  without users. `hookwitness mcp -token-file` sends a token to MCP.
- Failed logins are limited per client address; `-trusted-proxies` names
  the reverse proxies whose `X-Forwarded-For` is believed.
- `hookwitness user` (add, passwd, rm, list) works on the database
  directly.

### The admin on its own address

- `-admin-addr` gives the UI, the API and MCP their own listener (with
  `-admin-prefix /` at its root); the catch listener then keeps only
  endpoints and `/healthz`. `-public-url` sets the catch URLs shown.

### Endpoints and requests

- An endpoint can be turned off: it keeps its setup and requests, and its
  path is answered as unmatched until it is on again.
- Replay: a request whose body was kept whole is sent again, byte for byte,
  to a URL of your choice, from the UI, the API or MCP; the target's answer
  is kept with the request. Once login is on, only an admin replays.
  Link-local and cloud-metadata addresses are always refused;
  `-replay-allow` narrows the targets or turns replay off.
- The strip shows 2 min, 10 min, 1 h or 6 h, and where the kept requests
  begin.
- A signature that an earlier kept request already carried gets a note
  (the verdict is unchanged).
- The live feed keeps outcome updates across a reconnect.

### Setup files

- An endpoint's setup (path, behaviour, secret names and kinds, never their
  values) exports from the Behaviour page, the API and MCP, and imports all
  or none: through a dialog with a review, the API, MCP, `serve -import` at
  every start, or `hookwitness apply`. A secret without its value is "not
  set" and its checks give n/v.
- Reports can include the endpoint's setup at generation time.

### Running it

- Release archives for Linux and macOS on amd64 and arm64, with
  `SHA256SUMS` and a provenance attestation; built reproducibly.
- The database gives space back after clearing and pruning;
  `hookwitness db compact` shrinks a 0.1 database once (server stopped).
- MCP sessions open at once are capped (`-mcp-max-sessions`, 32).
- The UI has section tabs, an account menu and a theme switch (Auto, Light,
  Dark).
- A guide for setting up a shared server, and notes on upgrading.

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
  (tag `:0.1` on GHCR): distroless, non-root, data in the
  `/data` volume, works read-only, with a health check, a signed build
  provenance and an SBOM.
- The web UI answers only to IP addresses, localhost and the names in
  `-admin-hosts`, and refuses cross-origin form posts. It has no login yet:
  keep the admin prefix on localhost or behind a proxy that does not publish
  it.
- Documentation for every task in English and Ukrainian (`docs/`, `docs/uk/`).

[0.2.0]: https://github.com/petroladkin/hookwitness/releases/tag/v0.2.0
[0.1.0]: https://github.com/petroladkin/hookwitness/releases/tag/v0.1.0
