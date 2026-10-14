# Hookwitness documentation

Catch webhooks, get a verdict, attach the evidence. These pages teach it task
by task: what to click and what happens. [Українською](uk/README.md).

1. [Getting started](getting-started.md): run it, create an endpoint, send a
   webhook, read it.
2. [Endpoints and the live view](endpoints.md): exact paths, the Unmatched
   register, the register and its strip, a request in full, retries.
3. [Scripted replies](replies.md): rules (every Nth, the first N, at random),
   delays, timeouts, dropped connections, templates, required headers.
4. [Authorization checks and secrets](authorization.md): GitHub, Stripe,
   Standard Webhooks, generic HMAC, bearer, basic; enforce; write-only secrets.
5. [JSON Schema checks](schemas.md): one schema, any of several, or picked by
   a value; drafts from caught bodies; images in the payload.
6. [Reports](reports.md): Markdown, HTML and PDF evidence for a ticket;
   masking.
7. [Configuration and deployment](deployment.md): Docker and its volume,
   settings, the secret key, reverse proxies.
8. [Troubleshooting](troubleshooting.md): a secret but no verdict, a refused
   host name, unreadable secrets, 413, lost data.
9. [Testing a sender with an AI agent](agents.md): the Claude Code skill.

Reference: the [REST API](api.md) (English only), and its OpenAPI 3.1
description served at `/admin/api/v1/openapi.json`.
