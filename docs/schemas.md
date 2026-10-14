# JSON Schema checks

Does the event keep its contract? A JSON Schema check validates each
request's body and records a verdict next to the authorization ones:

- **pass**;
- **fail**, with up to 20 problems, each a JSON pointer into the body, the
  failing keyword and a message;
- **n/v** when the body was not kept (over the size limit);
- **n/c**, not checked, when no schema applies to the event.

## Add a schema and a check, step by step

1. Open the endpoint's **Behaviour** page, tab **Schemas**, and click **New
   schema**. Name it (`OrderPaid`) and paste the schema or upload a file. An
   invalid schema is refused when you save it, with the reason.
2. Open the **Checks** tab, click **New check** and choose the kind **JSON
   Schema**. Pick the schema, and save.

![The Schemas tab of /shop/events with two schemas](images/schemas.webp)

The schema of the example below:

```json
{"type":"object","required":["order_id","amount"],
 "properties":{"order_id":{"type":"integer"},"amount":{"type":"integer","minimum":1},
               "paid_at":{"type":"string","format":"date-time"}}}
```

The request's page stamps the verdict (`Schema: fail, 3 errors`), lists the
problems and marks the offending places in the body.

![A request that failed its schema: the offending keys marked in the body, three problems listed](images/schema-fail.webp)

## What the schemas can do

- **Drafts** follow `$schema`: draft-04 to 2020-12, and 2020-12 when it is
  absent.
- **`format` is asserted**: a `date-time` of `"yesterday"` fails.
- **References by name**: schemas of one endpoint refer to each other with
  `{"$ref": "Address"}`. Nothing is ever fetched: a reference to any other URL
  or a file is refused when you save.
- **A body that is not JSON** fails with the parse error and its byte offset;
  an empty body fails as such.

## Which schema applies

A check uses:

- **one schema**: every body must match it;
- **several**, when the sender may send either shape (v1 or v2): the body must
  match one of them, and the verdict names which. When none matches it gives
  the problems of the closest one and how far the others were:
  `matches none of V1, V2; closest V2 with 1 error (V1: another shape)`;
- **a schema picked by a value**, a JSON pointer into the body (`/type`) or a
  header (`X-GitHub-Event`), with a mapping `order.paid = OrderPaid`, one per
  line. A value without a mapping, or a request without the value, is checked
  against the default schemas when there are any; without a default it gets
  **n/c** and is never refused.

![The check dialog: JSON Schema picked by /type, two values mapped](images/schema-check.webp)

An endpoint `/orders/events` with the schema `OrderPaid` above and a check
that picks it by `/type`, mapping `order.paid = OrderPaid`:

<!-- docs-check: setup: orders -->
```sh
H=http://127.0.0.1:8080/orders/events
curl -s -d '{"type":"order.paid","order_id":42,"amount":1999,"paid_at":"2026-10-12T17:28:07Z"}' $H   # pass
curl -s -d '{"type":"order.paid","order_id":"42","paid_at":"yesterday"}' $H                        # fail: 3 errors against OrderPaid
curl -s -d '{"type":"order.created","order_id":43}' $H                                              # n/c: no schema for
```

## Enforce

With **Enforce**, a body that fails is refused with `400` listing the problems
(or your own reply) before the rules, so it is not counted. A body that could
not be checked (n/v) is refused too; n/c never is.

## Draft a schema from what was caught

Writing a schema for every event type by hand is the slow part. Let the
caught bodies draft it:

1. Click **Draft schema** on a request, or on the register for several:
   picked by number (`3, 5-9`) or all the requests with the same value of the
   check's discriminator (`/type = order.shipped`).

   ![The Draft schema dialog: all requests with /type order.shipped](images/draft-dialog.webp)

2. The schema editor opens on a draft: the types seen, the keys (required when
   every body has them), array items, and string formats (`date-time`, `date`,
   `uuid`, `email`, `uri`) when every value has them. The name is suggested
   from the value (`order.shipped` → `OrderShipped`).

   ![The schema editor on a draft, with the offer to map order.shipped to it](images/draft-editor.webp)

3. Review it: a draft knows only what those bodies had. Enums, patterns,
   ranges and lengths are never guessed. Nothing is stored until you click
   **Save schema**.

With a discriminator check, the editor offers to map the value to the new
schema in that check. When a schema of that name exists, it lists what the
draft would change (keys added, keys now optional, types widened), and
saving replaces it. Requests caught before stay **n/c** until you click
**Re-check**.

## Images in the payload

A string declared as an image must really be one:

```json
{"type":"string","contentEncoding":"base64","contentMediaType":"image/png"}
```

`image/jpeg`, `image/gif`, `image/webp`, or `image/*` for any of them, work
too, and a `data:image/...;base64,` URL is accepted. The image is decoded
completely, so a truncated or mislabelled picture, text, or an SVG fails at
its path. Up to 40 megapixels. HEIC and AVIF are recognised but cannot be
decoded here, so their verdict is n/v.

The request's page shows the images under the body: those a schema declares,
plain `data:image` strings, and a body that is itself an image, with a
full-size viewer and a download. Reports carry them too.

![A request with an image declared by its schema: the thumbnail above the body, the base64 shortened in it](images/payload-images.webp)

## Re-check

After editing a schema, **Re-check** on the register validates every kept
request again. Only the verdicts change.

## With the API

Schemas and schema checks are part of the behaviour document; `POST
/endpoints/{id}/schema-draft` drafts a schema without storing it. See the
[API reference](api.md#behaviour).
