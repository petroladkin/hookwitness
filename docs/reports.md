# Reports

Attach the evidence to a ticket: a report says which requests arrived, what
Hookwitness answered and every verdict, with a summary on top.

## Download one

1. Click **Report** on an endpoint's register (or on one request, for that
   request alone).
2. Choose:
   - **Format**: **HTML, one file**, **Markdown**, or **PDF, A4**;
   - **Requests**: all kept, those received in a time range (UTC), or picked
     by number (`3, 5-9`);
   - **Images in the requests**: shown, or listed only for a smaller file;
   - **Mask header values**: which headers to hide (below).
3. Click **Download report**.

![The Report dialog: HTML, all 6 kept, images shown, header masking](images/report-dialog.webp)

## What is in it

- **On top**: the endpoint, the range, when and by which Hookwitness version
  the report was written.
- **Summary**: answers by class (2xx, 4xx, 5xx, timeout, drop, refused by a
  check); verdicts per check (pass, fail, not checked, not verifiable); the
  bodies delivered more than once with the gaps between deliveries (a
  sender's backoff); the recorder strip of the range.
- **Each request**: number, time, method, path and query, sender address,
  headers, the body (pretty JSON; binary as its size and SHA-256; cut at 64 KiB
  with the full size and SHA-256), what Hookwitness answered, every verdict
  with its reason and schema problems, and its images.

Times are UTC with milliseconds. The same requests and options always give
the same Markdown and HTML, apart from the generation time.

![An HTML report: the header, the strip of the range, the summary and the first request](images/report-html.webp)

## Formats

- **HTML** is one self-contained file: fonts embedded, no network requests,
  light or dark following the reader's setting, and it prints cleanly.
- **Markdown** renders in GitHub and GitLab issues: paste it into a comment.
- **PDF** is drawn by the server (no browser), A4, with a running header and
  page numbers; a request never splits before the end of its headers.

An excerpt of a Markdown report:

````markdown
## Summary

**Answers**

| 2xx | 4xx | 5xx | timeout | drop | refused by a check |
|---:|---:|---:|---:|---:|---:|
| 5 | 1 | 0 | 2 | 1 | 1 |

**Verdicts**

| Check | pass | fail | not checked | not verifiable |
|---|---:|---:|---:|---:|
| 1 · GitHub signature | 6 | 3 | 0 | 1 |
| 2 · JSON Schema by header X-GitHub-Event | 5 | 1 | 3 | 1 |
````

## Masking

Masking is off by default: the values are test values. When a report leaves
your team, tick `Authorization`, `Cookie`, `Set-Cookie`, the signature headers
of the endpoint's checks, or name other headers. Their values become their
length (`•••• 71 chars`), the signatures taken from them in verdicts too, and
the report says which headers were masked.

## With the API

`GET /endpoints/{id}/report?format=html|md|pdf` with the same choices as the
dialog; see the [API reference](api.md#reports).

<!-- docs-check: setup: stripe_endpoint -->
```sh
API=http://127.0.0.1:8080/admin/api/v1
curl -s -d '{"type":"invoice.paid"}' http://127.0.0.1:8080/stripe/webhooks   # 200
curl -s "$API/endpoints/1/report?format=md" | head -1
# # Hookwitness report · /stripe/webhooks
```
