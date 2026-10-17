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
| 0.1.x, the newest patch release | yes: a patch release (0.1.1, 0.1.2…) and the `:0.1` image |
| older 0.1.x releases | upgrade to the newest 0.1.x |
| `:edge` and `dev` builds | fixed on `dev`; no promises in between |

Before 1.0, only the newest minor version gets fixes: once 0.2.0 is out, 0.1
does not.

## In scope

- An authorization check that passes a request it should fail (or fails one
  it should pass) for a correctly configured secret.
- A secret, token or credential value reaching the database in plain text,
  the logs, a rendered page or a response.
- Cross-site scripting, request forgery or DNS rebinding against the web UI.
- Anything a sender of webhooks can do to the server beyond being caught:
  reading other endpoints' requests, changing configuration, running code,
  exhausting memory or disk past the configured limits.

## Not a vulnerability

- **The web UI has no login yet.** Anyone who can reach the admin prefix can
  read every caught request and change every endpoint. Keep it on localhost
  or behind a reverse proxy that does not publish it (see
  [Reaching it from other machines](docs/deployment.md#reaching-it-from-other-machines)).
- Caught requests are stored as received, including the senders' own
  `Authorization` headers and signatures: they are the evidence.
- Secrets used with the checks are meant to be test values; protect the
  secret key (`HOOKWITNESS_SECRET_KEY` or `<db>.key`) like the database.
