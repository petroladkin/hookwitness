# Hookwitness

Guidance for AI coding agents (and a concise orientation for humans).

Hookwitness is a self-hosted webhook catcher for developers, QA engineers and
AI agents: catch endpoints with a live UI, scriptable responses and fault
scenarios, pass/fail verdicts for request authentication (HMAC signatures in
GitHub/Stripe/Svix formats, bearer, basic) and JSON Schema payloads, Markdown
and HTML report export, a REST API and an MCP server. One Go binary with
sqlite, also shipped as a Docker image.

## Current state

Skeleton only: `hookwitness serve` opens and migrates the sqlite database and
answers `GET /healthz`; `hookwitness version` prints the build identity. No
webhook catching, UI or API yet. This section must describe what is true now —
update it in the same change that makes it false.

## Layout

- `cmd/hookwitness` — the command: subcommands `serve`, `version`; signal
  handling; logger setup.
- `internal/config` — `serve` settings from flags and `HOOKWITNESS_*` env.
- `internal/store` — sqlite (modernc.org/sqlite, no cgo): pragmas, embedded
  migrations in `internal/store/migrations/NNNN_name.sql`, applied in order and
  recorded in `schema_migrations`; a newer database schema is refused.
- `internal/server` — HTTP routes and graceful shutdown.
- `internal/version` — version and commit, set via `-ldflags` by `make build`.
- `internal/repoguard` — tests over the repository itself: no private network
  addresses or home-directory paths in tracked files (exempt a line with
  `guard:allow`, a file with `guard:allow-file` in its first line), and
  `THIRD-PARTY-NOTICES.md` matching the modules compiled in.
- `scripts/hooks` — git hooks; `scripts/notices.sh` — notices generator.

## Commands

- `make build` — `bin/hookwitness`.
- `make check` — build, vet, gofmt check, staticcheck (a `go tool`
  dependency), `go test -race ./...`. Must pass before a merge.
- `make notices` — regenerate `THIRD-PARTY-NOTICES.md` after a dependency
  change; only permissive licenses are accepted.
- `make hooks` — enable the git hooks.

## Scope

- In the first release: the features above.
- Out of scope: Kafka / MQTT / gRPC, users and RBAC, a hosted cloud version.

## Stack

Go, sqlite, HTMX, Server-Sent Events, Docker.

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
