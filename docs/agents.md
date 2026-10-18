# Testing a sender with an AI agent

An agent that writes a webhook sender can also prove it works: start
Hookwitness, point the sender at it, trigger the sender and read what
arrived, with the verdicts, from the REST API. No browser, no polling loop.

## The skill

Hookwitness ships a [Claude Code](https://claude.com/claude-code) skill,
[`skills/hookwitness`](../skills/hookwitness/SKILL.md), that teaches an agent
this workflow:

1. run Hookwitness in Docker, or reuse a running one;
2. create an endpoint and give the sender its URL (from the host or from
   another container);
3. require what the sender must send (a Basic password, a bearer token, a
   signature), and script failures such as `503` for the first two
   deliveries or a timeout, to test the sender's retries;
4. trigger the sender and wait for the request (`wait=30s` holds the call
   until it arrives);
5. assert on the status, the verdict, the headers and the body.

Install it in a project, or in `~/.claude/skills/hookwitness/` for every
project:

<!-- docs-check: skip: downloads from GitHub -->
```sh
mkdir -p .claude/skills/hookwitness && curl -fsSL -o .claude/skills/hookwitness/SKILL.md \
  https://raw.githubusercontent.com/petroladkin/hookwitness/dev/skills/hookwitness/SKILL.md
```

Then ask for it in your own words, e.g. *"Check that the order service signs
its webhooks with the GitHub scheme and retries after a 503"*. The agent picks
the skill up by its description.

## What the agent does, by hand

The same session in shell, the way a test would run it:

```sh
API=http://127.0.0.1:8080/admin/api/v1
ID=$(curl -s -X POST $API/endpoints -H 'Content-Type: application/json' -d '{"path":"/orders/events"}' | jq .id)
curl -s -o /dev/null -X PUT $API/endpoints/$ID/secrets/ORDERS_PASSWORD -H 'Content-Type: application/json' \
  -d '{"kind":"token","value":"test-password"}'
TAG=$(curl -sI $API/endpoints/$ID/behaviour | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -s -o /dev/null -X PUT $API/endpoints/$ID/behaviour -H 'Content-Type: application/json' -H "If-Match: $TAG" \
  -d '{"checks":[{"kind":"basic","secret":"ORDERS_PASSWORD","user":"orders","enforce":true}]}'

curl -s -u orders:test-password -H 'Content-Type: application/json' -d '{"order":42}' \
  http://127.0.0.1:8080/orders/events   # 200: the sender under test

curl -s "$API/endpoints/$ID/requests?after=0&wait=30s" | jq -c '.requests[] | {id, status, verdict}'
# {"id":1,"status":200,"verdict":"pass"}
```

Every route, the behaviour document and more recipes are in the
[API reference](api.md); for a sender running in a container see
[Testing a sender in Docker](api.md#testing-a-sender-in-docker).

## MCP

Hookwitness is also an MCP server, so an agent can drive it with tools
instead of the shell. It answers MCP over Streamable HTTP at
`http://127.0.0.1:8080/admin/mcp`, with the same access rules as the REST API.
`hookwitness mcp` bridges it to clients that speak only stdio.

Add it to Claude Code:

<!-- docs-check: skip: adds the server to your Claude Code configuration -->
```sh
claude mcp add --transport http hookwitness http://127.0.0.1:8080/admin/mcp
```

For a client that speaks only stdio, run the bridge. It can be the binary
or the one inside the container:

<!-- docs-check: skip: adds the server to your Claude Code configuration -->
```sh
claude mcp add hookwitness -- hookwitness mcp -url http://127.0.0.1:8080
claude mcp add hookwitness -- docker exec -i hookwitness /hookwitness mcp -url http://127.0.0.1:8080
```

Other clients take the same in their configuration file:

```json
{
  "mcpServers": {
    "hookwitness": { "type": "http", "url": "http://127.0.0.1:8080/admin/mcp" }
  }
}
```

```json
{
  "mcpServers": {
    "hookwitness": { "command": "hookwitness", "args": ["mcp", "-url", "http://127.0.0.1:8080"] }
  }
}
```

With the admin on its own port ([Configuration and
deployment](deployment.md#the-admin-on-its-own-port)), MCP is on that port:
`http://127.0.0.1:8081/mcp` when the admin is at its root. Give the bridge the
full URL then: `hookwitness mcp -url http://127.0.0.1:8081/mcp`.

With login on ([Users, login and tokens](access.md)), MCP needs a token:
`--header "Authorization: Bearer <token>"` for a client that speaks HTTP,
`-token-file <file>` for the bridge (see [MCP with a
token](access.md#mcp-with-a-token)). The agent then acts as the token's
owner.

Check that the server answers MCP:

```sh
curl -s -X POST http://127.0.0.1:8080/admin/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"curl","version":"1"}}}' \
  | grep -o '"name":"hookwitness"'
# "name":"hookwitness"
```

The tools are task-shaped. Each result is a summary line, then compact JSON.

| Tool | Does |
|---|---|
| `create_endpoint` | an endpoint and its catch URL, the address for the sender |
| `list_endpoints`, `get_endpoint` | endpoints; one with its behaviour and secrets (names and fingerprints only) |
| `configure_endpoint` | sets parts of the behaviour (`default`, `rules`, `checks`, `vars`, `schemas`); the parts not given stay |
| `set_secret` | a signing key or a token for the checks; write-only, the answer is a fingerprint |
| `wait_for_request` | waits for the next request that matches (verdict, status, text, a JSON value), up to 300 s, and returns it in full |
| `list_requests`, `get_request` | requests with the same filters; one in full, its body cut at 16 KiB with its size |
| `expect` | asserts that N matching requests arrive within a time: pass, or fail with what arrived and the reasons |
| `switch_endpoint` | turns an endpoint off (its path goes to Unmatched, everything else kept) or on again |
| `reset_endpoint`, `recheck`, `delete_endpoint` | reset the counter or clear requests, verify again, delete |
| `export_endpoint`, `import_endpoints` | an endpoint's setup as a file (secret names and kinds, never values); endpoints created, or replaced, from one ([Sharing a setup](sharing.md)) |
| `replay_request` | sends a caught request byte for byte to a URL (your handler) and returns its answer: status, headers, body, timings or the error ([Replaying a request](replay.md)); `get_request` lists a request's replays |

`delete_endpoint`, `reset_endpoint` and `import_endpoints` (it can replace an endpoint's behaviour) are marked destructive, and the
reading tools read-only, so a client can ask you before the destructive ones.
`replay_request` is the one tool that reaches outside the server (open world).
A session looks like this:

1. `create_endpoint` with `/github/events`, then give its URL to the sender;
2. `set_secret` `GH` as a key, and `configure_endpoint` with the check
   `{"kind": "github", "secret": "GH"}`;
3. trigger the sender, then `expect` with `verdict: pass`: you get `pass`,
   or `fail: … #2 fail GitHub signature: signature does not match`.

The headers of a caught request are evidence, so a token or password that
the sender sent is visible in `get_request`, as in the API. The values of
the secrets you set are never shown.
