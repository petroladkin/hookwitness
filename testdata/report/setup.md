# Hookwitness report · /orders

| | |
|---|---|
| Endpoint | `/orders` |
| Time range | 2026-10-11T12:00:01.250Z – 2026-10-11T12:00:01.250Z |
| Selection | whole register |
| Requests | 1 |
| Generated | 2026-10-12T09:30:00.000Z by Hookwitness v0.0.0-test |
| Masked headers | `Authorization`, `cookie`, `X-Hub-Signature-256` |

## Summary

**Answers**

| 2xx | 4xx | 5xx | timeout | drop | refused by a check |
|---:|---:|---:|---:|---:|---:|
| 1 | 0 | 0 | 0 | 0 | 0 |

**Verdicts**

| Check | pass | fail | not checked | not verifiable |
|---|---:|---:|---:|---:|
| 1 · GitHub signature | 1 | 0 | 0 | 0 |
| 2 · JSON Schema by /type | 1 | 0 | 0 | 0 |

**Retries** (the same body delivered more than once)

None.

## Setup

The endpoint's setup when this report was generated, 2026-10-12T09:30:00.000Z.

**Checks** (in order)

| # | Check | What it does |
|---:|---|---|
| 1 | header X-Shop-Domain | equals "test-shop.example" · else 400 naming the header |
| 2 | GitHub signature | secret GITHUB\_KEY · enforced · else 401 |
| 3 | Bearer token | secret API\_TOKEN · record only |
| 4 | JSON Schema by /type | "order.created" → order · other values → event · enforced · else 400 listing the problems |

**Schemas**

| Name | Used by |
|---|---|
| order | check 4 (JSON Schema by /type) |
| event | check 4 (JSON Schema by /type) |
| draft | not used |

**Rules** (in order; the first that holds answers)

| # | When | Then |
|---:|---|---|
| 1 | every 3rd | 503 after 2.0 s+ |
| 2 | first 1 | time out |
| 3 | 10% | drop the connection |
| 4 | every 7th | 202 |

*Rule 1 · every 3rd → 503*

| | |
|---|---|
| Status | 503 |
| Delay | 2.0 s + up to 500 ms at random |
| `Retry-After` | `2` |

Body (a template)

```
busy | try `later`
```

*Rule 4 · every 7th → 202*

| | |
|---|---|
| Status | 202 |
| Delay | none |
| Body | 4 B given as exact bytes |

**Default reply**

| | |
|---|---|
| Status | 200 |
| Delay | none |
| `Content-Type` | `application/json` |

Body (a template)

```
{"ok":true,"shop":"{{.Var "shop"}}"}
```

**Variables**

| Name | Value |
|---|---|
| `shop` | `test-shop` |

**Secrets** (named, never shown)

| Name | Kind | State | Used by |
|---|---|---|---|
| GITHUB\_KEY | signing key | set | check 2 (GitHub signature) |
| API\_TOKEN | token or password | not set | check 3 (Bearer token) |
| SPARE\_TOKEN | token or password | set | not used |

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
