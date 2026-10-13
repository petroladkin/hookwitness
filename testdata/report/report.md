# Hookwitness report · /orders

| | |
|---|---|
| Endpoint | `/orders` |
| Time range | 2026-10-11T12:00:01.250Z – 2026-10-11T12:01:05.500Z |
| Selection | whole register |
| Requests | 8 |
| Generated | 2026-10-12T09:30:00.000Z by Hookwitness v0.0.0-test |
| Masked headers | `Authorization`, `cookie`, `X-Hub-Signature-256` |

## Summary

**Answers**

| 2xx | 4xx | 5xx | timeout | drop | refused by a check |
|---:|---:|---:|---:|---:|---:|
| 3 | 1 | 1 | 1 | 1 | 1 |

**Verdicts**

| Check | pass | fail | not checked | not verifiable |
|---|---:|---:|---:|---:|
| 1 · GitHub signature | 2 | 1 | 0 | 0 |
| 2 · JSON Schema by /type | 3 | 1 | 1 | 1 |

**Retries** (the same body delivered more than once)

| Deliveries | Gaps | Body SHA-256 |
|---|---|---|
| #1, #2 | +1.5 s | `297faa831cf94fb8…` |

## Requests

### #1 · POST `/orders?source=test&retry`

| | |
|---|---|
| Received | 2026-10-11T12:00:01.250Z from `203.0.113.5:40100` · HTTP/1.1 |
| Answer | 200 sent · default reply |

**Verdicts**

- GitHub signature: pass — signature matches
- JSON Schema by /type: pass — matches OrderPaid

**Query**

| Name | Value |
|---|---|
| `source` | `test` |
| `retry` | — |

**Headers**

| Name | Value |
|---|---|
| `Host` | `hooks.example.com` |
| `Content-Type` | `application/json` |
| `X-GitHub-Event` | `order` |
| `X-Hub-Signature-256` | •••• 71 chars |
| `User-Agent` | `Shop-Hookshot/2.1` |

**Body** · 156 B

```json
{
  "type": "order.paid",
  "order_id": 1042,
  "amount": {
    "value": 4200,
    "currency": "UAH"
  },
  "customer": {
    "name": "Олена Петренко",
    "email": "olena@example.com"
  }
}
```

### #2 · POST `/orders`

| | |
|---|---|
| Received | 2026-10-11T12:00:02.750Z from `203.0.113.5:40102` · HTTP/1.1 |
| Answer | 200 sent · rule 1 · every 2nd · after 1.5 s |
| Delivery | 2nd of this body (a retry) |

**Verdicts**

- GitHub signature: pass — signature matches
- JSON Schema by /type: pass — matches OrderPaid

**Headers**

| Name | Value |
|---|---|
| `Content-Type` | `application/json` |
| `X-Hub-Signature-256` | •••• 71 chars |

**Body** · 156 B

```json
{
  "type": "order.paid",
  "order_id": 1042,
  "amount": {
    "value": 4200,
    "currency": "UAH"
  },
  "customer": {
    "name": "Олена Петренко",
    "email": "olena@example.com"
  }
}
```

### #3 · POST `/orders`

| | |
|---|---|
| Received | 2026-10-11T12:00:09.000Z from `198.51.100.7:51000` · HTTP/1.1 |
| Answer | 400 refused · check 2: JSON Schema by /type: 3 errors against OrderPaid |

**Verdicts**

- GitHub signature: fail — signature does not match
  - received `•••• 9 chars`, expected `•••• 12 chars`
- JSON Schema by /type: fail — 3 errors against OrderPaid
  - `/order_id` `type` got string, want integer
  - `/amount/value` `minimum` must be \>= 1 but found -5
  - and 1 more

**Headers**

| Name | Value |
|---|---|
| `Content-Type` | `application/json` |
| `X-Hub-Signature-256` | •••• 9 chars |

**Body** · 108 B

````json
{
  "type": "order.paid",
  "order_id": "1043",
  "amount": {
    "value": -5
  },
  "note": "use ``` fences and `code` and | pipes"
}
````

### #4 · PUT `/orders?a=%7C&b=x%60y`

| | |
|---|---|
| Received | 2026-10-11T12:00:15.500Z from `198.51.100.7:51002` · HTTP/1.1 |
| Answer | timeout · rule 2 · first 1 · held 30.0 s |

**Verdicts**

- JSON Schema by /type: n/c — no /type in the request

**Query**

| Name | Value |
|---|---|
| `a` | `\|` |
| `b` | ``x`y`` |

**Headers**

| Name | Value |
|---|---|
| `Content-Type` | `text/plain` |

**Body** · 54 B

`````
line one
```
not a fence end
````
line | with | pipes
`````

### #5 · POST `/orders`

| | |
|---|---|
| Received | 2026-10-11T12:00:31.000Z from `198.51.100.7:51004` · HTTP/1.1 |
| Answer | dropped · rule 3 · random 10% |

**Headers**

| Name | Value |
|---|---|
| `Content-Type` | `image/png` |

**Images**

| Where | Image | SHA-256 |
|---|---|---|
| `the body` | PNG · 70 B · 1×1 px · not valid: a truncated or corrupt PNG: png: invalid format: zlib: invalid checksum | `1390c4e2d05b99f4…` |

**Body** · 70 B · sha256 1390c4e2d05b99f4f8e1e4e9ff9c5e4548f1fd2ae03c292081976c358949e749 · binary, not shown

### #6 · POST `/orders`

| | |
|---|---|
| Received | 2026-10-11T12:00:44.125Z from `198.51.100.7:51006` · HTTP/1.1 |
| Answer | 413 sent · body over the limit |

**Verdicts**

- JSON Schema by /type: n/v — body not kept (over the size limit)

**Headers**

| Name | Value |
|---|---|
| `Content-Type` | `application/json` |
| `Content-Length` | `2000000` |

**Body** · 1.9 MiB declared · not kept: over the body limit

### #7 · POST `/orders`

| | |
|---|---|
| Received | 2026-10-11T12:01:00.000Z from `198.51.100.7:51008` · HTTP/1.1 |
| Answer | 500 sent · default reply · template: reply:1: function "nope" not defined |

**Headers**

| Name | Value |
|---|---|
| `Authorization` | •••• 34 chars |
| `Cookie` | •••• 23 chars |
| `Content-Type` | `text/plain` |

**Body** · 300 B · sha256 0d4e2ca9e9cbced7a7a5380eb29e1a3783b9b6d0db72de36a1051038e1c1fbc7 · first 256 B shown

```
xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
```

*Cut at 256 B.*

### #8 · POST `/orders`

| | |
|---|---|
| Received | 2026-10-11T12:01:05.500Z from `198.51.100.7:51010` · HTTP/1.1 |
| Answer | 200 sent · default reply |

**Verdicts**

- JSON Schema by /type: pass — matches Avatar

**Headers**

| Name | Value |
|---|---|
| `Content-Type` | `application/json` |

**Images**

| Where | Image | SHA-256 |
|---|---|---|
| `/avatar` | PNG · 91 B · 24×16 px · declared image/png | `6e67d5560e2bf3b3…` |
| `/badge` | GIF · 54 B · 8×8 px · a data URL, no schema | `19b2ac9b993d694c…` |

**Body** · 279 B · sha256 1e03aca15365834447cbe88a8bf7ad82a2fea7d3a0930e251609766146be52b5 · first 256 B shown

```
{"type":"avatar.updated","user_id":77,"avatar":"iVBORw0KGgoAAAANSUhEUgAAABgAAAAQCAIAAACDRijCAAAAIklEQVR4nGNUMehhoAZgooopDKMGEQNGDSIMRg0iDAafQQBJpAEAFvi3hAAAAABJRU5ErkJggg==","badge":"data:image/gif;base64,R0lGODdhCAAIAIEAAMIzHwAAAAAAAAAAACwAAAAACAAIAAAIDwA
```

*Cut at 256 B.*
