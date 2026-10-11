# Hookwitness

Guidance for AI coding agents (and a concise orientation for humans).

Hookwitness is a self-hosted webhook catcher for developers, QA engineers and
AI agents: catch endpoints with a live UI, scriptable responses and fault
scenarios, pass/fail verdicts for request authentication (HMAC signatures in
GitHub/Stripe/Svix formats, bearer, basic) and JSON Schema payloads, Markdown
and HTML report export, a REST API and an MCP server. One Go binary with
sqlite, also shipped as a Docker image.

## Current state

Early development: no code yet. This section must describe what is true now —
update it in the same change that makes it false.

## Scope

- In the first release: the features above.
- Out of scope: Kafka / MQTT / gRPC, users and RBAC, a hosted cloud version.

## Stack

Go, sqlite, HTMX, Server-Sent Events, Docker.

## Working rules

- Gitflow: `main` + `dev` + `feature/*` + `release/*` + `fix/*` + `hotfix/*`.
  No direct commits to `dev` or `main`; work reaches `dev` from a branch after
  review. `git config core.hooksPath scripts/hooks` enables the guards.
- Conventional Commits; English everywhere in the repository.
- Build binaries into `bin/`, never at the repository root.
- Never commit secrets, real customer payloads, personal data, local machine
  paths or private network addresses — not in code, tests, fixtures, docs or
  commit messages. Test data is synthetic.
- Every dependency's license must be compatible with Apache-2.0 and
  recorded; vendored assets ship with their license text beside them.
- Documentation changes in the same change as the code it describes.
