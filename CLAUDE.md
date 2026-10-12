# Hookwitness

Guidance for AI coding agents (and a concise orientation for humans).

Hookwitness is a self-hosted webhook catcher for developers, QA engineers and
AI agents: catch endpoints with a live UI, scriptable responses and fault
scenarios, pass/fail verdicts for request authentication (HMAC signatures in
GitHub/Stripe/Svix formats, bearer, basic) and JSON Schema payloads, Markdown
and HTML report export, a REST API and an MCP server. One Go binary with
sqlite, also shipped as a Docker image.

## Current state

`hookwitness serve` catches webhooks: endpoints are exact paths managed in the
web UI under the admin prefix (default `/admin`); every other path is caught
into the Unmatched register with a 404. The UI shows each endpoint's requests
live (SSE + HTMX) with the recorder strip and the request detail. Each
endpoint's behaviour page scripts its answers: required-header checks, ordered
rules (every Nth, first N, random p%) with reply / timeout / drop, a default
reply, template variables; every request records its outcome. Authorization
checks (GitHub, Stripe, Standard Webhooks/Svix, generic HMAC, bearer, basic)
give each request pass / fail / n/v verdicts, optionally enforced, with
re-check; their secrets are write-only (signing keys sealed, tokens hashed).
The REST API core (`<prefix>/api/v1`: endpoints list/create/read,
behaviour GET/PUT with ETag/If-Match, write-only secrets, request lists with
long-poll `wait`, one request) has the UI's access rules; docs in
`docs/api.md`, an agent skill in `skills/hookwitness`. No JSON Schema
verdicts, reports, the rest of the API (rename/delete, clear, raw body,
actions, Unmatched list, filters, OpenAPI) or MCP yet. `hookwitness version`
prints the build identity; `hookwitness health` probes `/healthz` (the
container's health check). A container image (distroless, non-root, data in
the `/data` volume) is built, smoke-tested and published to GHCR by CI.
This section must describe what is true now — update it in the same change
that makes it false.

## Layout

- `cmd/hookwitness` — the command: subcommands `serve`, `health`, `version`; signal
  handling; logger setup; wiring of the parts below.
- `internal/config` — `serve` settings from flags and `HOOKWITNESS_*` env.
- `internal/store` — sqlite (modernc.org/sqlite, no cgo): pragmas, embedded
  migrations in `internal/store/migrations/NNNN_name.sql`, applied in order and
  recorded in `schema_migrations`; a newer database schema is refused.
  Endpoints and requests (`catch.go`); `endpoint_id` NULL is the Unmatched
  register; request ids are never reused (they are the SSE event ids).
- `internal/wire` — follows the raw HTTP/1.x stream of each connection to keep
  header lines in received order, case and repetition (net/http's map loses
  them). The root handler must `wire.Take` every request.
- `internal/server` — the root router on the raw path (`/healthz`, admin
  prefix, everything else caught; no ServeMux cleaning) and graceful shutdown.
- `internal/emulate` — pure decision logic for an endpoint's answer: config
  (JSON in `endpoints.config`), validation, required-header and
  authorization checks (`checks.go`: kinds, secrets list, `Verify`), rule
  conditions, text/template rendering, the outcome record (JSON in
  `requests.outcome`). Verdicts go to `requests.verdict` / `verdicts`.
- `internal/auth` — pure verifiers of signatures and credentials, one-line
  reasons, constant-time compares; golden vectors in `testdata/auth`
  (published by the providers or computed with openssl — protected).
- `internal/secret` — the secret key (`HOOKWITNESS_SECRET_KEY` or `<db>.key`,
  0600), sealing signing keys (AES-256-GCM), hashing tokens (PBKDF2-SHA256),
  keyed fingerprints. Plain secret values never reach the store, the logs or
  a rendered page.
- `internal/catch` — records and answers caught requests (body limit, 413,
  404 + Unmatched); runs checks, lets the store decide the rule inside the
  counter's transaction (exact under concurrency), then replies, delays,
  holds (timeout) or resets (drop, SO_LINGER 0); holds end on shutdown.
- `internal/live` — wake-ups for live feeds; feeds read what they missed from
  the store, so nothing is lost or repeated.
- `internal/api` — the JSON API under `<prefix>/api/v1`, mounted by
  `internal/web` behind its host check and cross-origin protection; one
  error shape, unknown fields refused, behaviour replaced with If-Match
  (store compare-and-set), secrets sealed by `emulate.SealSecret`, request
  lists wait on `internal/live`. Secret values never reach an answer or log.
- `internal/web` — the UI: templ components (`views.templ`, generated
  `views_templ.go` is committed), handlers, SSE feed, embedded `static/`
  (our `app.css`, `app.js`; vendored htmx, htmx-ext-sse and fonts under
  `static/vendor/<name>/` with `LICENSE` and `SOURCE`). Strict CSP: no
  inline scripts or styles, nothing from other origins. Answers only to IP
  addresses, localhost and `-admin-hosts` names (DNS rebinding); refuses
  cross-origin form posts.
- `internal/version` — version and commit, set via `-ldflags` by `make build`.
- `internal/repoguard` — tests over the repository itself: no private network
  addresses or home-directory paths in tracked files (exempt a line with
  `guard:allow`, a file with `guard:allow-file` in its first line), and
  `THIRD-PARTY-NOTICES.md` matching the modules compiled in and the bundled
  web assets.
- `scripts/hooks` — git hooks; `scripts/notices.sh` — notices generator;
  `scripts/docker-smoke.sh` — the container scenario (README's `docker run`,
  health, persistence of data and sealed secrets, graceful stop, read-only);
  `scripts/api-smoke.sh` — the API scenario (the Basic-credentials recipe
  end to end) against a URL, a binary or an image.
- `docs/api.md` — the API reference and recipes; `skills/hookwitness/SKILL.md`
  — a Claude Code skill for testing webhook senders with hookwitness.
- `Dockerfile`, `.dockerignore` — two-stage image: cross-compiling build
  stage through `make build`, distroless static nonroot final stage; the
  build context is an allowlist.
- `.github/workflows/ci.yml` — `make check`, then the amd64 image and its
  container and API smoke tests on every PR and push; from `dev` (`:edge`) and
  `v*` tags the amd64+arm64 image is pushed to GHCR with SBOM and provenance
  attestations attached as referrers (not in the image index). Actions are
  pinned to commit SHAs; `.github/dependabot.yml` updates them, the Go modules
  and the base images weekly.

## Commands

- `make build` — `bin/hookwitness`.
- `make check` — templ check, build, vet, gofmt check, staticcheck (a
  `go tool` dependency), `go test -race ./...`. Must pass before a merge.
- `make generate` — format the `.templ` files and regenerate `*_templ.go`
  (templ is a `go tool` dependency); commit both.
- `make notices` — regenerate `THIRD-PARTY-NOTICES.md` after a dependency
  or vendored-asset change; only permissive licenses are accepted.
- `make hooks` — enable the git hooks.
- `make image` — the container image `hookwitness:dev` (Docker); `make
  image-smoke` — the container scenario against it (Docker, curl, openssl).
- `make api-smoke` — the API scenario against a freshly built binary (curl,
  jq); `scripts/api-smoke.sh --image <image>` runs it against an image.

## Scope

- In the first release: the features above.
- Out of scope: Kafka / MQTT / gRPC, users and RBAC, a hosted cloud version.

## Stack

Go, sqlite, templ, HTMX, Server-Sent Events, Docker.

## Working rules

- Gitflow: `main` + `dev` + `feature/*` + `release/*` + `fix/*` + `hotfix/*`.
  No direct commits to `dev` or `main`; work reaches `dev` from a branch after
  review. `make hooks` enables the guards.
- Conventional Commits; English everywhere in the repository.
- Build binaries into `bin/`, never at the repository root.
- Never commit secrets, real customer payloads, personal data, local machine
  paths or private network addresses — not in code, tests, fixtures, docs or
  commit messages. Test data is synthetic.
- Every dependency's license must be compatible with Apache-2.0 and
  recorded (`make notices`); vendored assets ship with their license text
  beside them.
- No test, script or make target may depend on files outside the
  repository: a clean clone must pass `make check`.
- Documentation changes in the same change as the code it describes.
