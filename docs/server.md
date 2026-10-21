# Setting up a shared server

From nothing to a Hookwitness your team uses: senders reach the catch paths
over HTTPS, the admin asks for a login, and the data survives upgrades. Each
step links to the page that covers it in depth.

## 1. Decide who reaches what

Hookwitness has two faces on one server:

- **the catch paths**, where senders deliver webhooks: they must be reachable
  by the sender, often from the internet;
- **the admin** (the web UI, the REST API and MCP under `/admin`): it shows
  every caught request and changes every endpoint, so it needs a login and
  should be reachable only by your team.

Pick the layout before you start:

| Who uses it | Catch paths | Admin |
|---|---|---|
| Only you, on your machine | `127.0.0.1:8080` | the same port, no login needed: [Getting started](getting-started.md) is enough |
| You and senders on the internet | `https://hooks.example.com` through a proxy | its own port on the server's localhost, reached through an SSH tunnel |
| A team in the browser | `https://hooks.example.com` through a proxy | `https://admin.example.com`, its own site on the proxy |

The rest of this page builds the last two with Docker Compose and
[Caddy](https://caddyserver.com), which gets the TLS certificates by itself.
Point the DNS names at the server first, and open ports 80 and 443.

## 2. The password of the first admin

The first user turns login on and is always an admin. Here it comes from
the environment at the first start, with the password in a file that only
the image's user (uid 65532) can read:

<!-- docs-check: skip: changes file ownership with sudo -->
```sh
mkdir hookwitness && cd hookwitness
openssl rand -base64 18 > admin_password.txt   # or write your own: one line, at least 12 characters
sudo chown 65532 admin_password.txt && sudo chmod 400 admin_password.txt
```

The server reads that file at every start while
`HOOKWITNESS_ADMIN_PASSWORD_FILE` is set, and stops if it cannot: a file
owned by you with mode 600 is not readable by the image's user. It creates
the admin only on a database without users; later starts log that they
skipped it, and editing the file changes nothing. Other ways to add the
first admin are in [Users, login and tokens](access.md#login-is-off-until-the-first-user).

## 3. Compose and Caddy

`compose.yaml`, for the layout with an SSH tunnel to the admin:

```yaml
services:
  hookwitness:
    image: ghcr.io/petroladkin/hookwitness:0.1
    restart: unless-stopped
    ports: ["127.0.0.1:8081:8081"]   # the admin, on the server's localhost only
    volumes: ["hookwitness:/data"]
    environment:
      HOOKWITNESS_ADMIN_ADDR: ":8081"
      HOOKWITNESS_ADMIN_PREFIX: /
      HOOKWITNESS_PUBLIC_URL: https://hooks.example.com
      HOOKWITNESS_ADMIN_USER: ann
      HOOKWITNESS_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
    secrets: [admin_password]
  caddy:
    image: caddy:2
    restart: unless-stopped
    ports: ["80:80", "443:443"]
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy:/data
secrets:
  admin_password:
    file: ./admin_password.txt
volumes:
  hookwitness:
  caddy:
```

`Caddyfile`:

```
hooks.example.com {
	reverse_proxy hookwitness:8080
}
```

What each setting does:

- `HOOKWITNESS_ADMIN_ADDR` gives the admin its own port, 8081. Port 8080
  then serves only the catch paths and `/healthz`, so the proxy can pass
  everything on it to senders, and `/admin/…` there is just another catch
  path. Nothing publishes 8080 on the server: only Caddy reaches it.
- `HOOKWITNESS_ADMIN_PREFIX: /` puts the UI at the root of the admin port.
- `HOOKWITNESS_PUBLIC_URL` is the address senders use: the catch URLs the UI
  shows are built on it, and an `https://` address marks the login cookie
  *Secure*.
- The `hookwitness` volume holds the database and the key that seals the
  secrets; see [Keep the data in a volume](deployment.md#keep-the-data-in-a-volume).

More settings are in [Configuration and deployment](deployment.md#settings).

### For a team in the browser

Give the admin its own site instead of the tunnel. In `compose.yaml`, drop
the `ports` line of `hookwitness` and add the name it answers to:

```yaml
      HOOKWITNESS_ADMIN_HOSTS: admin.example.com
```

and add the site to the `Caddyfile`:

```
admin.example.com {
	reverse_proxy hookwitness:8081
}
```

The admin answers only to the names in `HOOKWITNESS_ADMIN_HOSTS` (and to IP
addresses and `localhost`), which keeps a hostile web page from reaching it
through a name of its own; see [Reaching it from other
machines](deployment.md#reaching-it-from-other-machines).

Logins from the internet are limited per address: ten failures in 15
minutes pause that address. Behind Caddy every login comes from Caddy's
address, so one guesser would pause everyone. Name Caddy as a trusted proxy
with the subnet of the compose network, which `docker network inspect`
prints once the stack is up:

<!-- docs-check: skip: needs the compose stack running -->
```sh
docker network inspect hookwitness_default -f '{{(index .IPAM.Config 0).Subnet}}'
```

Set it as `HOOKWITNESS_TRUSTED_PROXIES` and run `docker compose up -d`
again; see [Behind a reverse proxy](access.md#behind-a-reverse-proxy).

## 4. Start it and log in

<!-- docs-check: skip: starts a compose stack with public names -->
```sh
docker compose up -d
docker compose logs hookwitness | grep -E 'admin user created|bootstrap|level=ERROR'
curl -s https://hooks.example.com/healthz
```

The log says `admin user created from HOOKWITNESS_ADMIN_USER: login is on`.
`level=ERROR` with `HOOKWITNESS_ADMIN_PASSWORD_FILE: … permission denied`
means the image's user cannot read the file: go back to step 2.

Open the admin. With the tunnel, run on your own machine:

<!-- docs-check: skip: opens an SSH tunnel to your server -->
```sh
ssh -N -L 8081:127.0.0.1:8081 you@your-server
```

and open <http://127.0.0.1:8081/>. For a team, open
<https://admin.example.com/>. Log in as `ann` with the password from
`admin_password.txt`, then change it on your account page (your name at the
top right, *Account and tokens*, *Change password*). The file has done its
job: you may delete it after removing the two `HOOKWITNESS_ADMIN_…`
variables and the secret from `compose.yaml`, or keep all three; the server
stops at start if the variables name a file that is gone.

## 5. Bring in the team

- **People:** an admin adds them on the **Users** page with a first
  password; each changes it on their account page. A *user* manages
  endpoints, an *admin* also manages people. Nobody can demote or remove
  themselves, so an admin always remains: [Roles](access.md#roles).
- **Scripts, CI and agents:** each person makes personal tokens on their
  account page and sends them as `Authorization: Bearer …`: [Tokens for
  scripts and agents](access.md#tokens-for-scripts-and-agents). For Claude
  Code over MCP, see [MCP with a token](access.md#mcp-with-a-token).
- **Senders:** create the endpoints and give each sender its catch URL, now
  on `https://hooks.example.com`. A known set of endpoints can come from a
  file at every start: [Sharing an endpoint's setup](sharing.md).

## 6. Back up

Everything is in the `hookwitness` volume: the database and its key. Back
them up together; a database restored without its key keeps the requests
and loses the secrets ([The secret key](deployment.md#the-secret-key)). With
the server stopped for a moment:

<!-- docs-check: skip: stops your server and writes a backup file -->
```sh
docker compose stop hookwitness
docker run --rm -v hookwitness_hookwitness:/data:ro -v "$PWD":/backup alpine \
  tar -czf /backup/hookwitness-$(date +%F).tar.gz -C /data .
docker compose start hookwitness
```

To restore, stop the server, unpack the archive into an empty volume and
start it again. The volume's name has the compose project in front
(`hookwitness_hookwitness` for a directory named `hookwitness`); `docker
volume ls` shows it.

## Ready

- [ ] `https://hooks.example.com/healthz` answers `"status":"ok"`.
- [ ] The admin asks for a login, and the dashed "No login" line is gone.
- [ ] From the internet, the admin is not reachable on port 8080 or 8081:
      `https://hooks.example.com/admin/` lands in the Unmatched register.
- [ ] The first admin's password is changed, and a second admin exists.
- [ ] A backup of the volume exists and has been restored once.

For upgrades, see [Upgrading](deployment.md#upgrading); for the usual
surprises, [Troubleshooting](troubleshooting.md).
