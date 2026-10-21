# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it
privately through GitHub: the repository's **Security** tab →
**Report a vulnerability**. Say what is affected, how to reproduce it and
what an attacker gains.

The aim is to answer within a week. A confirmed problem is fixed in a patch
release of the supported version (below) and on `dev`, and the advisory is
published with the fix; you are credited unless you ask not to be.

## Supported versions

| Version | Fixes |
|---|---|
| 0.2.x, the newest patch release | yes: a patch release (0.2.1, 0.2.2…) and the `:0.2` image |
| older 0.2.x releases | upgrade to the newest 0.2.x |
| 0.1.x | no: upgrade to 0.2 |
| `:edge` and `dev` builds | fixed on `dev`; no promises in between |

Before 1.0, only the newest minor version gets fixes: once 0.3.0 is out, 0.2
does not.

## In scope

- An authorization check that passes a request it should fail (or fails one
  it should pass) for a correctly configured secret.
- A secret, password, token or credential value reaching the database in plain text,
  the logs, a rendered page or a response.
- Cross-site scripting, request forgery or DNS rebinding against the web UI.
- With login on: reaching the admin (UI, API, MCP) without a valid login or
  token, a user doing what only an admin may, a session or token that outlives
  its logout, revocation or its owner, or a way around the login limit.
- Anything a sender of webhooks can do to the server beyond being caught:
  reading other endpoints' requests, changing configuration, running code,
  exhausting memory or disk past the configured limits.

## Not a vulnerability

- **An admin without users has no login.** Until the first user is added,
  anyone who can reach the admin prefix can read every caught request and
  change every endpoint; every page and the start-up log say so. Add a user
  before publishing it (see [Users, login and tokens](docs/access.md)).
- Caught requests are stored as received, including the senders' own
  `Authorization` headers and signatures: they are the evidence.
- Secrets used with the checks are meant to be test values; protect the
  secret key (`HOOKWITNESS_SECRET_KEY` or `<db>.key`) like the database.
