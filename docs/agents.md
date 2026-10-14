# Testing a sender with an AI agent

An agent that writes a webhook sender can also prove it works: start
Hookwitness, point the sender at it, trigger the sender and read what
arrived, with the verdicts, from the REST API. No browser, no polling loop.

## The skill

Hookwitness ships a [Claude Code](https://claude.com/claude-code) skill,
[`skills/hookwitness`](../skills/hookwitness/SKILL.md), that teaches an agent
this workflow:

1. run Hookwitness in Docker, or reuse a running one;
2. create an endpoint and give the sender its URL (from the host, or from
   another container);
3. require what the sender must send: a Basic password, a bearer token, a
   signature;
4. trigger the sender and wait for the request (`wait=30s` holds the call
   until it arrives);
5. assert on the status, the verdict, the headers and the body;
6. script failures (`503` for the first two deliveries, a timeout) to test
   the sender's retries.

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

An MCP server, so agents can drive Hookwitness as tools without the shell, is
planned and not there yet.
