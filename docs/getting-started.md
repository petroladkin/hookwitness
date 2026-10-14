# Getting started

In ten minutes: run Hookwitness, create an endpoint, send it a webhook and
read what arrived.

## 1. Run it

With Docker:

<!-- docs-check: skip: starts a long-running container; scripts/docker-smoke.sh runs this command in CI -->
```sh
docker run -d --name hookwitness -p 127.0.0.1:8080:8080 -v hookwitness:/data \
  ghcr.io/petroladkin/hookwitness:edge
```

Or from source (Go 1.27 or newer):

<!-- docs-check: skip: builds and starts the server this check already runs -->
```sh
make build
bin/hookwitness serve
```

Either way it listens on port 8080 of this machine only. Check that it is up:

```sh
curl -s http://127.0.0.1:8080/healthz
# {"status":"ok","version":…
```

The `-v hookwitness:/data` part matters: it keeps your endpoints, requests and
secrets in a named volume, so they survive the container being removed and
created again. More in [Configuration and deployment](deployment.md).

## 2. Create an endpoint

Open <http://127.0.0.1:8080/admin/>. The **Endpoints** page lists the paths
Hookwitness catches; it starts empty.

1. Click **New endpoint**.
2. Type the path the sender should call, say `/stripe/webhooks`, or leave it
   empty for a random one.
3. Click **Create endpoint**.

![The New endpoint dialog with the path /stripe/webhooks](images/new-endpoint.webp)

The list now shows the endpoint with its **catch URL**: the address to give
the sender.

![The Endpoints page with /stripe/webhooks and its catch URL](images/endpoints.webp)

## 3. Send it a webhook

Point the real sender at the catch URL, or send one by hand:

<!-- docs-check: setup: stripe_endpoint -->
```sh
curl -s -X POST http://127.0.0.1:8080/stripe/webhooks \
  -H 'Content-Type: application/json' \
  -d '{"type":"invoice.paid","id":"evt_1"}'   # 200
```

Hookwitness answers `200` with an empty body. That is the default; the
[scripted replies](replies.md) change it.

## 4. Read what arrived

Click the endpoint's path. Its page is the **register**: every request,
newest on top, arriving live without a reload. Click one to see it in full:
method, path and query, the headers in the order the sender wrote them, the
body (pretty JSON, raw text, or a hex view for binary) and what Hookwitness
answered.

![The register of /stripe/webhooks with the invoice.paid request open: its body, the 200 answer and the headers](images/first-request.webp)

## Next

- [Endpoints and the register](endpoints.md): the register, the strip, the
  Unmatched register, retries.
- [Scripted replies](replies.md): fail every 3rd delivery, answer slowly, hang,
  drop the connection.
- [Authorization checks and secrets](authorization.md): is it correctly signed?
- [JSON Schema checks](schemas.md): does the event keep its contract?
- [Reports](reports.md): attach the evidence to a ticket.
