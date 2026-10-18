# Troubleshooting

The surprises people run into, each with what causes it and what to do.

## A secret is added, but requests still get 200 and no verdict

A secret alone does nothing; it is only the value that a check uses. Add the
check: **Behaviour → Checks → New check**, then choose the kind (Bearer
token, Basic credentials, GitHub signature…) and the secret. The Secrets tab
lists the checks that use each secret, and "not used: add a check" means
none does.

<!-- docs-check: setup: secret_only -->
```sh
curl -s -o /dev/null -d '{}' -H 'Authorization: Bearer wrong' http://127.0.0.1:8080/ping   # 200: no check, no verdict
```

Even with the check, a request that fails gets the usual `200` unless the
check's **Enforce** is on: without it, the check only records a verdict. See
[Authorization checks and secrets](authorization.md#enforce).

## Everything is gone after I recreated the container

The container ran without a volume for `/data`, so the database lived in the
container itself and went with it. Run it with a named volume
(`-v hookwitness:/data`) from now on; see
[Configuration and deployment](deployment.md#keep-the-data-in-a-volume). What
was in the removed container cannot be recovered.

## The UI says it does not answer to the host name

```text
The web UI does not answer to the host name "hooks.example.com:8080". Open it by IP address or localhost, or allow the name with -admin-hosts.
```

The UI and the API answer only to IP addresses and `localhost`, so that a web
page on another domain cannot reach them by pointing its own name at your
server (see [Reaching it from other machines](deployment.md#reaching-it-from-other-machines)). Open it by IP address, or
allow the name: `-admin-hosts hooks.example.com` (in Docker:
`-e HOOKWITNESS_ADMIN_HOSTS=hooks.example.com`). The API answers the same
refusal as `421 wrong_host`. Catch paths answer to any name.

```sh
curl -s -H 'Host: hooks.example.com' http://127.0.0.1:8080/admin/api/v1/endpoints | jq -r .error.code
# wrong_host
```

## Every signature check says n/v: "cannot be decrypted (wrong secret key?)"

The server runs with another secret key than the one that sealed the
secrets: `HOOKWITNESS_SECRET_KEY` changed, or the database was moved without
its `.key` file. At startup the log says so:

```text
this secret key is not the one this database was used with: sealed signing keys cannot be opened and stored tokens never match
```

Start it with the original key (the variable, or the `.key` file next to the
database), or set the secrets again with **Replace** on the Secrets tab, then
re-check the kept requests (see
[Verdicts and re-checking](endpoints.md#verdicts-and-re-checking)). Stored tokens and passwords cannot be
recovered either: with the wrong key they simply never match.

## A request got 413 and its checks say n/v

Its body was larger than `-max-body` (10 MiB by default). It was answered
`413` and kept without its body, so nothing that needs the body (a signature
or a schema) can be verified, and the verdict is **n/v**. Raise the limit
(`HOOKWITNESS_MAX_BODY=50MiB`) if such bodies are expected.

With the GitHub check from [Authorization checks](authorization.md#an-example-for-each)
on `/hooks`, an 11 MB body gets `413` and this verdict:

<!-- docs-check: setup: github -->
```sh
head -c 11000000 /dev/zero | curl -s -o /dev/null --data-binary @- http://127.0.0.1:8080/hooks   # n/v: body not kept (over the size limit)
```

## A request got 503 "busy"

Hookwitness was already reading as many bytes of request bodies as
`-max-inflight` allows (by default the larger of 256 MiB and twice
`-max-body`). The request was answered `503` with `Retry-After: 5`, kept
without its body, and the register says **Busy**; a sender that retries gets
through. If this happens under normal load, raise the limit
(`HOOKWITNESS_MAX_INFLIGHT=1GiB`).

## My requests land in Unmatched with 404

The path does not match an endpoint exactly. Compare them in the Unmatched
register: a trailing slash (`/hooks/` is not `/hooks`), a missing segment, a
different case. Fix the sender's URL, or create the endpoint at that path.

## The sender says it timed out, but Hookwitness has the request

The reply's delay (or a timeout rule) was longer than the sender's timeout.
The request is recorded as **sender left**, with how long the sender waited,
and counted as a timeout. Shorten the delay, or keep it to test exactly that.

## A Stripe or Standard Webhooks check fails with "timestamp … old"

The signed timestamp is older than the tolerance (300 s), typically when you
replay a captured request. Tick **Accept any timestamp (replayed captures)** on
the check, or raise its tolerance.

## The register does not update by itself

The live register is a Server-Sent Events stream at `/admin/e/<id>/events`. A
reverse proxy that buffers responses holds it back; turn buffering off for
the admin prefix (nginx: `proxy_buffering off;`; Hookwitness also sends
`X-Accel-Buffering: no`). A reload always shows the current state.

## The API answers 401 since a user was added

The first user turns login on for the REST API and MCP too: scripts and
agents now need a token (`Authorization: Bearer <token>`, from your account
page), see [Users, login and tokens](access.md#tokens-for-scripts-and-agents).
`401 invalid_token` means the token is wrong or was revoked, or its owner
was removed.

## Nobody can log in any more

On the server, `hookwitness user passwd <name>` sets a new password and
`hookwitness user add <name> --admin` adds another admin; see [Lost the admin
password](access.md#lost-the-admin-password). Ten failed logins from one
address pause its logins for 15 minutes; behind a reverse proxy, set
`-trusted-proxies` so that the pause hits the client, not the proxy.
