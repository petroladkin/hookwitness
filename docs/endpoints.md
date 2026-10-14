# Endpoints and the live view

## Endpoints

An endpoint is an **exact path** on the server, such as `/github/webhooks`.
Every request to it is kept as evidence and answered the way the endpoint is
scripted (`200` with an empty body until you script it).

- `/a` and `/a/` are different endpoints. The query string is not part of
  the path: `/a?x=1` reaches `/a`.
- Paths are made of letters, digits and `. _ ~ -` between slashes. Leave the
  path empty in **New endpoint** for a random one, handy when the address
  should not be guessed.
- `/healthz` and the admin prefix (`/admin` by default) are Hookwitness's
  own; any other path can be an endpoint, so a sender can keep its real
  paths.

The **Endpoints** page lists them with their catch URL, how many requests
are kept and when the last one arrived. **Behaviour** opens an endpoint's
[scripted replies](replies.md) and checks, **Clear** deletes its requests,
**Delete** deletes the endpoint with its requests.

## Unmatched requests

A request to a path that is not an endpoint is answered `404` and kept in
the **Unmatched** register, linked at the top of the Endpoints page. A typo
in the sender's URL, a missing or extra trailing slash, a forgotten
`/webhooks` suffix: it shows up there at once instead of disappearing.

<!-- docs-check: setup: stripe_endpoint -->
```sh
curl -s -X POST http://127.0.0.1:8080/stripe/webhook -d '{}'   # 404: kept under Unmatched
```

![The Unmatched register with the request to /stripe/webhook, one letter short](images/unmatched.webp)

## The register

Click an endpoint to open its register: the endpoint's requests, newest on
top. It is live: new requests appear without a reload, and **RECORDING** in
the header says the feed is connected.

![The register of /github/webhooks: the strip on top, the requests with their verdicts on the left](images/register.webp)

- Each row shows the request's number, method, body size, content type and
  time (UTC, to the millisecond). A row is red when the answer was not 2xx;
  a note says how it was answered when it was not a plain default reply
  (`503 · rule 1`, `401 refused`, `timeout`, `dropped`, `sender left`).
- With checks configured, each row carries its verdict (`CHECKS PASS`,
  `CHECKS FAIL`) and the register counts them; **Re-check** verifies every
  kept request again after you change a secret or a schema.
- A body delivered again is marked `RETRY 1`, `RETRY 2`… on its row.
- **Show older** loads more rows; **Clear** deletes the endpoint's requests.
- The number is the request's place since the endpoint was created; it is
  never reused, so "request 7" stays request 7 in a report.

### The strip

Above the register, the strip draws the last two minutes like a recorder's
paper: one mark per request, its height the body size. A red mark was not
answered 2xx, a dashed one timed out, a cross was dropped, a red bar under
the line means a check failed, and the same body delivered more than once is
bracketed. A sender's retry pattern (backoff, bursts, duplicates) is visible
at a glance.

### Retries

The same body delivered again is a retry: the request's page says
`delivery 2 of this body`, the strip brackets the deliveries, and reports
group them with the gaps between deliveries, so you can check a sender's
backoff.

## A request in full

Click a row to open it on the right:

- **The stamp** (top right): the answer and what decided it: `200 sent ·
  default reply`, `503 sent · rule 1 · every 4th`, `401 refused · check 1`,
  `timeout`, `dropped`, or `sender left` (the sender gave up during a reply's
  delay, so it never got the reply).
- **Body**: JSON pretty-printed, other text as is, binary as a hex dump; **Raw
  view** shows it byte for byte and **Download body** saves it. A body over the
  size limit (10 MiB by default) is answered `413` and kept without its body.
- **Images** a body carries are shown above it, with a full-size viewer
  ([JSON Schema checks](schemas.md#images-in-the-payload)).
- **Verdicts** of the endpoint's checks, with their reasons.
- **Answer**: what Hookwitness sent, which rule or the default decided, the
  measured delay.
- **Headers**: in the order and case the sender wrote them, repeated ones
  kept.

**Report** (on the request or on the register) exports the evidence:
see [Reports](reports.md). **Draft schema** drafts a JSON Schema from the
body: see [JSON Schema checks](schemas.md#draft-a-schema-from-what-was-caught).

## How many requests are kept

Each endpoint keeps its newest 1000 requests (`-retention`), the Unmatched
register too; older ones are deleted as new ones arrive. See
[Configuration and deployment](deployment.md).
