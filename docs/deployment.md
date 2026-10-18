# Configuration and deployment

How to run Hookwitness for longer than an afternoon: in Docker with its data
in a volume, with its settings, its secret key, and behind a reverse proxy.

## Docker

<!-- docs-check: skip: starts a container; scripts/docker-smoke.sh runs this command in CI -->
```sh
docker run -d --name hookwitness -p 127.0.0.1:8080:8080 -v hookwitness:/data \
  ghcr.io/petroladkin/hookwitness:0.1
```

The UI is at <http://127.0.0.1:8080/admin/>; webhooks go to any other path of
port 8080. Images are built for linux/amd64 and linux/arm64, with these tags:

- `:0.1` (used in these pages) follows the newest 0.1.x release: fixes, no
  breaking changes;
- `:X.Y.Z`, such as `:0.1.0`, is one release and never changes;
- `:latest` is the newest release, whatever its version;
- `:edge` follows the development branch, with no promises.

Release candidates (`:0.1.0-rc.1`) have only their own tag.

### Keep the data in a volume

Everything Hookwitness keeps (endpoints, their behaviour, secrets, caught
requests) is in `/data`: the database and, unless `HOOKWITNESS_SECRET_KEY` is
set, the key that seals the secrets (`/data/hookwitness.db.key`).

**Always mount `/data`.** Without `-v hookwitness:/data`, the data lives in
the container's own layer: `docker rm` followed by a new `docker run` (an
upgrade, a changed flag) starts empty, with every endpoint gone. A named
volume, as above, outlives the container.

With a host directory instead of a named volume, give it to the image's user
(`chown 65532:65532 <dir>`) or run with `--user "$(id -u):$(id -g)"`.

Back up the database and the key together: a database restored without its
key keeps the requests but loses the secrets.

### Compose

```yaml
services:
  hookwitness:
    image: ghcr.io/petroladkin/hookwitness:0.1
    ports: ["127.0.0.1:8080:8080"]
    volumes: ["hookwitness:/data"]
    restart: unless-stopped
volumes:
  hookwitness:
```

### The image

- Runs as a non-root user without a shell, and works with
  `--read-only --tmpfs /tmp`.
- Has a health check (`hookwitness health`, which asks `/healthz`); `docker
  ps` shows it.
- Carries a signed build provenance and an SBOM:
  `gh attestation verify oci://ghcr.io/petroladkin/hookwitness:0.1 --owner petroladkin`.
- `make image` builds it locally; `make image-smoke` runs the scenario CI runs
  against it.

## Settings

Settings come from flags or environment variables; a flag wins over the
variable. In Docker, pass the variables: `-e HOOKWITNESS_RETENTION=5000`.

| Flag | Variable | Default | Meaning |
|---|---|---|---|
| `-addr` | `HOOKWITNESS_ADDR` | `127.0.0.1:8080` (the image: `:8080`) | the address to listen on |
| `-db` | `HOOKWITNESS_DB` | `hookwitness.db` (the image: `/data/hookwitness.db`) | the database file |
| `-log-format` | `HOOKWITNESS_LOG_FORMAT` | `text` | `text` or `json` |
| `-max-body` | `HOOKWITNESS_MAX_BODY` | `10MiB` | the largest body kept, in bytes or with `KiB`, `MiB`, `GiB`, at most `512MiB`; a larger one is answered `413` and not kept |
| `-max-inflight` | `HOOKWITNESS_MAX_INFLIGHT` | the larger of `256MiB` and twice `-max-body` | bytes of request bodies read at once; a body that does not fit is answered `503` with `Retry-After: 5` and kept without its body; at least `-max-body` |
| `-retention` | `HOOKWITNESS_RETENTION` | `1000` | requests kept per endpoint, and as many unmatched |
| `-admin-addr` | `HOOKWITNESS_ADMIN_ADDR` | none | the admin's own address (host:port) for the UI, the API and MCP; none: they live under `-admin-prefix` on `-addr` ([below](#the-admin-on-its-own-port)) |
| `-admin-prefix` | `HOOKWITNESS_ADMIN_PREFIX` | `/admin` | where the UI and the API live; `/` (the root) only with `-admin-addr` |
| `-public-url` | `HOOKWITNESS_PUBLIC_URL` | none | the URL senders reach the catch paths at, such as `https://hooks.example.com`: the catch URLs the UI and the API show are built on it (behind a proxy, a tunnel or remapped ports) |
| `-admin-hosts` | `HOOKWITNESS_ADMIN_HOSTS` | none | host names the UI and the API answer to, comma-separated |
| `-max-wait` | `HOOKWITNESS_MAX_WAIT` | `5m` | the longest reply delay or timeout hold, from 1s to 1h |
| — | `HOOKWITNESS_SECRET_KEY` | none | the secret key (below); without it, the key file `<db>.key` |

The database grows to at most about (endpoints × `-retention` × `-max-body`)
plus `-retention` × 64 KiB for the Unmatched register, which keeps only the
first 64 KiB of a body.

Ctrl-C or SIGTERM stops the server gracefully: requests in flight finish,
held timeouts end.

### The secret key

The secrets of the endpoints' checks depend on a 32-byte key: signing keys
are encrypted with it, and tokens and passwords are hashed with it. The key
is:

- `HOOKWITNESS_SECRET_KEY`, 32 bytes in base64 (`openssl rand -base64 32`).
  It is read only from the environment, so it does not show in the process
  list;
- or, when it is not set, a key file next to the database (`<db>.key`,
  created with mode 0600 on the first run).

Keep the key with the database when you move it. A database opened with
another key keeps everything except the secrets: signing keys cannot be
decrypted (their checks give **n/v**) and stored tokens never match. The
server warns about it at startup; see [Troubleshooting](troubleshooting.md).

## Reaching it from other machines

The UI and the API have **no login yet**. Keep them on localhost, or behind a
reverse proxy that publishes only the catch paths.

- `-p 127.0.0.1:8080:8080` keeps it on this host. `-p 8080:8080` publishes it
  to the network.
- The UI and the API answer only when opened by an IP address or `localhost`,
  so a hostile web page cannot reach them by pointing its own domain at your
  server (DNS rebinding). Behind a proxy with a name, allow that name:
  `-e HOOKWITNESS_ADMIN_HOSTS=hooks.example.com`. Catch paths answer to any
  name.
- Where strangers can reach the server, publish everything except the admin
  prefix (`/admin`), or give the admin its own port and publish only the
  catch port (next section).

![The answer to a host name that is not allowed](images/host-refused.webp)

### The admin on its own port

By default one port does everything: the catch paths and, under `/admin`,
the UI, the API and MCP. Set `HOOKWITNESS_ADMIN_ADDR` to give the admin its
own port; the catch port then keeps only the catch paths and `/healthz`, and
every other path there is caught, `/admin/…` too. Publish the catch port and
keep the admin port on localhost:

<!-- docs-check: skip: starts a container; scripts/docker-smoke.sh runs this layout in CI -->
```sh
docker run -d --name hookwitness -p 8080:8080 -p 127.0.0.1:8081:8081 \
  -e HOOKWITNESS_ADMIN_ADDR=:8081 -e HOOKWITNESS_ADMIN_PREFIX=/ \
  -e HOOKWITNESS_PUBLIC_URL=http://hooks.example.com:8080 \
  -v hookwitness:/data ghcr.io/petroladkin/hookwitness:0.1
```

The UI is then at <http://127.0.0.1:8081/>, the API at
`http://127.0.0.1:8081/api/v1` and MCP at `http://127.0.0.1:8081/mcp`
(`hookwitness mcp -url http://127.0.0.1:8081/mcp`). With the default prefix
they stay under `/admin` on the admin port. `/healthz` answers on both ports;
the image's health check asks the catch port.

The catch URLs the UI shows need to know where senders reach the catch port.
Without `HOOKWITNESS_PUBLIC_URL` they use the catch port's own host when
`-addr` names one, else the host you opened the UI by, with the catch port
(`8080`). Behind a proxy, a tunnel or a remapped port (`-p 9000:8080`) that
guess is wrong: set `HOOKWITNESS_PUBLIC_URL`.

An endpoint may now use a path under `/admin`. If the admin later moves back
under the prefix on the catch port, it covers such an endpoint, which then
catches nothing; the server names those endpoints in a warning at start.

## Build from source

Requires Go 1.27 or newer. Without a clone, `go install
github.com/petroladkin/hookwitness/cmd/hookwitness@latest` puts the newest
release in `$(go env GOPATH)/bin` (`@v0.1.0` for a given one). In a clone:

<!-- docs-check: skip: builds and starts the server this check already runs -->
```sh
make build                    # writes bin/hookwitness, a single static binary
bin/hookwitness version
bin/hookwitness serve         # listens on 127.0.0.1:8080, database ./hookwitness.db
```

`make check` also needs a C compiler for the race detector; the binary itself
is built without cgo.

`bin/hookwitness health` asks a running server's `/healthz` and exits 0 when
it is up.

## Which build is running

Every place that names Hookwitness names the build as `<version> (<commit>)`,
for example `v0.1.0 (5a45bb4)`: the line at the bottom of the Endpoints page
(and the logo's tooltip on every page), `hookwitness version`, and the
"Generated … by Hookwitness" line of every report. So a report attached to a
ticket says which build caught its requests. Released images say their tag
(`v1.2.0 (…)`), the `:edge` image says `edge`, `make build` says what
`git describe` finds, and a build without it (`go install`, a plain `go
build`) says the module version Go recorded: the tag for a release, a
pseudo-version such as `v0.1.1-0.20261016120000-abcdef123456` between
releases, or `dev` when there is none.

`/healthz` gives the same in two fields:

```sh
curl -s http://127.0.0.1:8080/healthz | jq -r 'keys | join(" ")'
# commit status version
```
