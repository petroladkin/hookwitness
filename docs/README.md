# Hookwitness documentation

Catch webhooks, get a verdict, attach the evidence. These pages teach it task
by task: what to click and what happens. [Українською](uk/README.md).

1. [Getting started](getting-started.md): run it, create an endpoint, send a
   webhook, read it.
2. [Endpoints and the register](endpoints.md): exact paths, the Unmatched
   register, turning an endpoint off, the live register and its strip, a
   request in full, retries, re-checking.
3. [Scripted replies](replies.md): rules (every Nth, the first N, at random),
   delays, timeouts, dropped connections, templates, required headers.
4. [Authorization checks and secrets](authorization.md): GitHub, Stripe,
   Standard Webhooks, generic HMAC, bearer, basic; enforce; write-only secrets.
5. [JSON Schema checks](schemas.md): one schema, any of several, or picked by
   a value; drafts from caught bodies; images in the payload.
6. [Reports](reports.md): Markdown, HTML and PDF evidence for a ticket;
   masking.
7. [Configuration and deployment](deployment.md): Docker and its volume,
   settings, the secret key, the admin on its own port, upgrading,
   binaries.
8. [Troubleshooting](troubleshooting.md): a secret but no verdict, a refused
   host name, unreadable secrets, 413, lost data.
9. [Testing a sender with an AI agent](agents.md): the Claude Code skill and
   the MCP server.
10. [Users, login and tokens](access.md): turning login on, roles, tokens for
    scripts and MCP, a lost password.
11. [Sharing an endpoint's setup](sharing.md): export to a file, import it
    on another instance, secrets without values, endpoints at start.
12. [Replaying a request to your handler](replay.md): send a caught request
    byte for byte to your own handler and keep its answer; which targets.
13. [Setting up a shared server](server.md): from nothing to a team
    server: HTTPS for senders, the first admin, the admin out of reach,
    teammates and tokens, backups.

Reference: the [REST API](api.md) (English only), and its OpenAPI 3.1
description served at `/admin/api/v1/openapi.json`.
