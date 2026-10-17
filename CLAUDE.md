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
reply, template variables; a reply body is a template or exact bytes
(`body_base64`); every request records its outcome (a reply body that is not
UTF-8 as base64). Authorization
checks (GitHub, Stripe, Standard Webhooks/Svix, generic HMAC, bearer, basic)
give each request pass / fail / n/v verdicts, optionally enforced, with
re-check; their secrets are write-only (signing keys sealed, tokens hashed).
JSON Schema checks validate bodies against the endpoint's schemas (one, any of
several, or picked by a body pointer or header with an optional default; verdicts pass / fail with problems / n/v /
n/c; offending paths marked in the body view); images in payloads declared
with contentMediaType image/* are decoded and checked, recorded in the
verdict, previewed under the body with a viewer, served by the API and shown
in reports. A schema can be drafted from caught bodies (`internal/schema`
`Draft`, offered on a request and the register, and by the API) and is
stored only when saved. The REST API core
(`<prefix>/api/v1`: endpoints list/create/read, behaviour GET/PUT with
ETag/If-Match, write-only secrets, request lists with long-poll `wait`, one
request) has the UI's access rules; reference in `docs/api.md`, an agent skill in
`skills/hookwitness`. User documentation in `docs/` (task by task, with
screenshots) and its Ukrainian twin in `docs/uk/`; the README keeps the pitch,
the quick start and links. Reports: a register's requests (all, a time range or
picked by number) download from the UI as Markdown, self-contained HTML or a
native PDF, with a summary (answers by class, verdicts per check, retry
groups, the strip of the range) and optional header masking. The REST API
covers the UI: endpoints (create, change path, delete), behaviour, secrets,
requests with filters (verdict, status, text in the body, a value at a
JSON pointer), bodies in lists and long-poll, the Unmatched register, the raw body,
images, clear, reset counter, re-check, reports, and its OpenAPI 3.1
description (`internal/api/openapi.json`, checked by tests); every time
in an answer has exactly three fraction digits. An MCP server
(`<prefix>/mcp`, Streamable HTTP, the API's access rules) gives agents
task-shaped tools over the REST API (`internal/mcp`), and `hookwitness mcp`
bridges stdio clients to it. `hookwitness
version`, reports, the UI and `/healthz` name the build as `<version>
(<commit>)` (the image from `dev` is `edge`); `hookwitness health` probes
`/healthz` (the container's health check). A container image (distroless, non-root, data
in the `/data` volume) is built, smoke-tested and published to GHCR by CI.
Released: 0.1.0 (`CHANGELOG.md`); the documentation tells users to run the
image `:0.1` (`TestDocsImageTag`), and `main` holds the tagged releases.
This section must describe what is true now — update it in the same change
that makes it false.

## Layout

- `cmd/hookwitness` — the command: subcommands `serve`, `health`, `mcp` (the
  stdio bridge), `version`; signal
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
  conditions, text/template rendering (`guard.go`: the string builtins
  replaced with bounded ones, a byte budget per render, a render past its
  deadline stopped), the outcome record (JSON in
  `requests.outcome`). Verdicts go to `requests.verdict` / `verdicts`.
- `internal/auth` — pure verifiers of signatures and credentials, one-line
  reasons (never quoting a credential), constant-time compares; golden vectors in `testdata/auth`
  (published by the providers or computed with openssl — protected).
- `internal/schema` — pure, offline JSON Schema validation
  (santhosh-tekuri/jsonschema): an endpoint's schemas compiled together and
  referable by name, a loader that refuses every URL (the library would read
  local files), `format` asserted, problems as JSON pointers; compiled sets
  cached by content; `contentMediaType` image/* asserted through
  `internal/picture` and every declared image recorded (`images.go`, a walk
  over the compiled schema and the body). Scenario fixtures in
  `testdata/schema` (protected).
- `internal/picture` — images a request carries: base64 / data URL
  decoding, signature sniffing, full decode of PNG, JPEG, GIF, WebP
  (golang.org/x/image), HEIC/AVIF recognised only, SVG refused, 40 MP limit;
  decoding bounded by memory (`budget.go`: the decoded size estimated from
  the header, 128 MB per image, 256 MB per validation, each image decoded
  once, at most two decodes at once in the process, images shown remembered
  by digest);
  finding them in a request (schema verdicts, data:image strings, an image
  body) and reading one by JSON pointer.
- `internal/payload` — a body and a query prepared for people: JSON
  pretty-printed with tokens tagged (class, JSON pointer; indentation capped
  at 32 levels, no pointer past 128, so deep bodies stay small), the text test, the
  hex dump, query pairs; shared by the UI and the reports.
- `internal/report` — reports of caught requests: Markdown, one HTML file
  (inline CSS, the UI's fonts as data URIs, a CSP that forbids requests, an
  SVG strip) and PDF (signintech/gopdf; Martian Mono and Atkinson
  Hyperlegible TTFs vendored in `internal/report/fonts/<name>/` with LICENSE
  and SOURCE). Requests come through a Source read twice (summary, then
  streamed with bodies); Markdown and HTML are byte-identical for the same
  input apart from the generation time — golden files in `testdata/report`
  (protected; `go test ./internal/report -update` after an intended change).
- `internal/secret` — the secret key (`HOOKWITNESS_SECRET_KEY` or `<db>.key`,
  0600, created atomically: a synced temporary file hard-linked into place), sealing signing keys (AES-256-GCM), hashing tokens (PBKDF2-SHA256),
  keyed fingerprints. Plain secret values never reach the store, the logs or
  a rendered page.
- `internal/catch` — records and answers caught requests (body limit, 413,
  404 + Unmatched keeping a body's first 64 KiB; bodies held at once bounded
  by `-max-inflight`, over it 503 "busy" without the body); runs checks, lets the store decide the rule inside the
  counter's transaction (exact under concurrency), then replies (with
  `Content-Security-Policy: sandbox` and `nosniff` unless the endpoint sets
  them: catch paths share the UI's origin), delays, holds (timeout) or
  resets (drop, SO_LINGER 0); holds end on shutdown.
- `internal/live` — wake-ups for live feeds; feeds read what they missed from
  the store, so nothing is lost or repeated.
- `internal/api` — the JSON API under `<prefix>/api/v1`, mounted by
  `internal/web` behind its host check and cross-origin protection; one
  error shape, unknown fields refused, behaviour replaced with If-Match
  (store compare-and-set), secrets sealed by `emulate.SealSecret`, request
  lists wait on `internal/live`. Secret values never reach an answer or log.
  Routes are one table (`routes()`); `openapi.json` must document each — a
  test validates it against the official schema (`testdata/openapi`,
  third-party) and real answers against its schemas.
- `internal/mcp` — the MCP server (github.com/modelcontextprotocol/go-sdk):
  task-shaped tools that call the REST API's handler in-process (same
  validation, errors and access), mounted by `internal/web` at
  `<prefix>/mcp`; `wait_for_request` and `expect` chain the API's long-polls
  and end on cancellation and at shutdown (a tool error); open sessions are
  tracked and closed on shutdown (`Streamable.CloseSessions`, called by
  `StopStreams`); results are a summary line and compact JSON,
  bodies cut at 16 KiB; `Bridge` pipes stdio to Streamable HTTP.
- `internal/web` — the UI (and its report dialog and download, `report.go`): templ components (`views.templ`, generated
  `views_templ.go` is committed), handlers, SSE feed, embedded `static/`
  (our `app.css`, `app.js`; vendored htmx, htmx-ext-sse and fonts under
  `static/vendor/<name>/` with `LICENSE` and `SOURCE`). Strict CSP: no
  inline scripts or styles, nothing from other origins. Answers only to IP
  addresses, localhost and `-admin-hosts` names (DNS rebinding); refuses
  cross-origin form posts.
- `internal/version` — version and commit, set via `-ldflags` by `make build`;
  without them (`go install`) taken from the build info Go records.
- `internal/repoguard` — tests over the repository itself: no private network
  addresses or home-directory paths in tracked files (exempt a line with
  `guard:allow`, a file with `guard:allow-file` in its first line), and
  `THIRD-PARTY-NOTICES.md` matching the modules compiled in and the bundled
  assets (web vendor files and report fonts); the documentation: every page
  of `docs/` has its twin in `docs/uk/` (except `api.md`) with the same `sh`
  blocks, relative links and anchors resolve, every image is used.
- `scripts/hooks` — git hooks; `scripts/notices.sh` — notices generator;
  `scripts/docker-smoke.sh` — the container scenario (README's `docker run`,
  health, persistence of data and sealed secrets, graceful stop, read-only);
  `scripts/api-smoke.sh` — the API scenario (the Basic-credentials recipe
  end to end) against a URL, a binary or an image.
- `docs/` — user documentation, one page per task (`docs/README.md` is the
  index), screenshots in `docs/images/` (WebP, from `make media`, and the
  README's `demo.gif`), the social preview in `docs/media/`; `docs/uk/`
  — the Ukrainian twins; `docs/api.md` — the API reference and recipes
  (English only); `skills/hookwitness/SKILL.md` — a Claude Code skill for
  testing webhook senders with hookwitness.
- `scripts/docscheck` — `make docs-check`: runs every `sh` block of the README
  and `docs/` against a fresh server and checks the verdicts, statuses and
  outputs the text promises (`setup.sh` does through the API what a page
  says to click); `scripts/media/media.mjs` — `make media`: does through the
  API what each page's text does (same paths, values, requests) and shoots
  the documentation's screenshots over the Chrome DevTools Protocol (Node, no
  npm packages); records the README's demo GIF from a screencast, encoded by
  ffmpeg in a container pinned by digest. A shot that shows something else
  than its text is a bug.
- `Dockerfile`, `.dockerignore` — two-stage image: cross-compiling build
  stage through `make build`, distroless static nonroot final stage; the
  build context is an allowlist.
- `.github/workflows/ci.yml` — `make check`, then the amd64 image and its
  container and API smoke tests (job `image`, read-only) on every PR and push
  to `dev` or `main`; on a push to `dev` (`:edge`) or a release tag
  (`vX.Y.Z`, `vX.Y.Z-rc.N`; other tags start nothing) the job `publish`,
  the only one with write permissions, pushes the amd64+arm64 image to GHCR
  with SBOM and provenance attestations attached as referrers (not in the
  image index); a release candidate gets only its own tag. Actions are
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
- `make docs-check` — every shell example of the documentation against a
  fresh build (curl, jq, openssl). Run it after changing a page or what a
  page describes.
- `make media` — regenerate `docs/images/` and `docs/media/` (Node 22+,
  Chrome, Docker for the GIF; `CHROME=` to choose the browser).

## Scope

- In the first release: the features above.
- Out of scope: Kafka / MQTT / gRPC, users and RBAC, a hosted cloud version.

## Stack

Go, sqlite, templ, HTMX, Server-Sent Events, Docker.

## Working rules

- Gitflow: `main` + `dev` + `feature/*` + `release/*` + `fix/*` + `hotfix/*`.
  No direct commits to `dev` or `main`; work reaches `dev` from a branch after
  review. `make hooks` enables the guards.
- Conventional Commits; English everywhere in the repository, except the
  Ukrainian documentation in `docs/uk/`.
- Build binaries into `bin/`, never at the repository root.
- Never commit secrets, real customer payloads, personal data, local machine
  paths or private network addresses — not in code, tests, fixtures, docs or
  commit messages. Test data is synthetic.
- Every dependency's license must be compatible with Apache-2.0 and
  recorded (`make notices`); vendored assets ship with their license text
  beside them.
- No test, script or make target may depend on files outside the
  repository: a clean clone must pass `make check`.
- Documentation changes in the same change as the code it describes: the
  page in `docs/` and its twin in `docs/uk/` (translated in the same change;
  the shell blocks stay identical), and `make docs-check` passes.
- Releases: a `release/X.Y` branch from `dev` (the docs' image tag moves to
  `:X.Y` there, never on `dev` before the image exists; `CHANGELOG.md` gets
  the version's entry, curated from `feat`/`fix`/`perf` commits), a
  `vX.Y.Z-rc.N` tag checked first, then the merge into `main`, the tag
  `vX.Y.Z` on it, and the merge back into `dev`. A fix to a release is a
  `hotfix/*` branch from `main`, released as the next patch. A pushed tag is
  never moved or deleted: fix forward.
- Tests that measure wall-clock time assert only what runner load cannot
  fake: never early, and ended well before the alternative (a delay applied
  twice, a hold past its cap); exact durations go through the catcher's timer
  seam (`Handler.after`). Wait for a recorded outcome by polling, not by a
  fixed sleep.
