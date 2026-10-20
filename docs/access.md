# Users, login and tokens

Out of the box Hookwitness has no users and no login: whoever can open the
admin (the web UI, the REST API, MCP) can read every caught request and
change every endpoint. That is fine on your own machine. On a team server,
in Docker on a network, or anywhere strangers might reach the admin, turn
login on by adding a user. Catch endpoints and `/healthz` stay open to
senders either way.

## Login is off until the first user

While there are no users, every admin page says so in a dashed line under
the header, and the server warns at start when the admin listens on more
than loopback.

![The endpoints page with login off](images/login-off.webp)

The first user turns login on for the web UI, the REST API and MCP. It is
always an admin (otherwise nobody could manage users). There are three ways
to add it:

- **The Users page.** Click *Add a user* in that dashed line, give a name
  and a password of at least 12 characters, and log in with them.
- **The environment, for Docker and compose:** `HOOKWITNESS_ADMIN_USER` and
  `HOOKWITNESS_ADMIN_PASSWORD_FILE` create the first admin at start, but
  only on a database without users. A restart or a redeploy creates nothing
  and logs that it skipped; editing the password file later changes nothing
  (change the password on the account page or with the command below).

  ```yaml
  services:
    hookwitness:
      image: ghcr.io/petroladkin/hookwitness:0.1
      ports: ["8080:8080"]
      volumes: ["hookwitness:/data"]
      environment:
        HOOKWITNESS_ADMIN_USER: ann
        HOOKWITNESS_ADMIN_PASSWORD_FILE: /run/secrets/admin_password
      secrets: [admin_password]
  secrets:
    admin_password:
      file: ./admin_password.txt   # one line, at least 12 characters
  volumes:
    hookwitness:
  ```

- **The command line,** on the server's database (the server may be
  running):

  <!-- docs-check: skip: works on the server's own database -->
  ```sh
  hookwitness user add ann --admin            # asks for the password twice
  docker exec -it hookwitness /hookwitness user add ann --admin
  ```

Or through the API, while login is still off:

```sh
curl -s -X POST http://127.0.0.1:8080/admin/api/v1/users -H 'Content-Type: application/json' \
  -d '{"name":"ann","role":"admin","password":"correct-horse-battery"}' | jq -r '.name + " " + .role'
# ann admin
```

## Logging in

From then on the admin asks for a name and a password. A page you opened
before logging in comes back after it.

![The login page](images/login.webp)

- A login lasts until you log out (*Log out* in the menu under your name), or a day
  without activity, or a week at most. An open register counts as activity.
- Ten failed logins from one address in 15 minutes pause logins from that
  address for the rest of the 15 minutes.
- Changing your password ends your other logins; an admin's reset of
  someone's password, a change of their role, and removing them end all of
  theirs at once.

## Roles

There are two, and no other permissions:

- **user**: does everything with endpoints (create, behaviour, secrets,
  reports, clear, delete), changes their own password and manages their own
  tokens;
- **admin**: also adds, changes and removes users, and sees and revokes
  everyone's tokens on the **Users** page.

![The users page](images/users.webp)

The last admin can be neither removed nor demoted: make another user an
admin first. An admin cannot make themselves a user either: another admin
can.

## Tokens for scripts and agents

Scripts, CI jobs and AI agents use the REST API and MCP with a personal
token instead of a password. Make one on your account page (click your name
in the header, *Account and tokens*, then *New token*). It is shown once: copy it then.

![A new token, shown once](images/token-new.webp)

A token acts as you, with your role as it is now: demote or remove a user and
their tokens follow at once. Revoke one on the account page (admins also on
the Users page); whatever uses it is refused from its next request. Tokens do
not expire; each shows when it was last used. A token cannot make tokens or
manage users: that needs a login.

Send it as `Authorization: Bearer <token>`. The whole flow with curl, from a
login to a call with the token:

```sh
curl -s -c hw.cookies -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/admin/login \
  --data-urlencode name=ann --data-urlencode password=correct-horse-battery
# 303
TOKEN=$(curl -s -b hw.cookies -X POST http://127.0.0.1:8080/admin/api/v1/me/tokens \
  -H 'Content-Type: application/json' -d '{"name":"ci"}' | jq -r .token)
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/admin/api/v1/endpoints
# 401
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/admin/api/v1/me | jq -c .
# {"login":true,"name":"ann","role":"admin","via":"token"}
```

Tokens look like `hkw_` followed by 8 and 40 characters
(`hkw_[a-z2-7]{8}_[a-z2-7]{40}`). Add that pattern to your secret scanner
(for GitHub: a custom pattern in secret scanning) to catch one committed by
mistake.

A request with a valid token is answered whatever host name it used: the
host-name rule ([deployment](deployment.md#reaching-it-from-other-machines))
guards browsers, and a browser does not send tokens.

### MCP with a token

With login on, `/admin/mcp` needs a token too. Give it to Claude Code as a
header, or to the stdio bridge as a file:

<!-- docs-check: skip: adds the server to your Claude Code configuration -->
```sh
claude mcp add --transport http hookwitness http://127.0.0.1:8080/admin/mcp \
  --header "Authorization: Bearer $(cat ~/.config/hookwitness/token)"
claude mcp add hookwitness -- hookwitness mcp -url http://127.0.0.1:8080 -token-file ~/.config/hookwitness/token
```

### A machine token

For a CI job that starts Hookwitness itself, `HOOKWITNESS_API_TOKEN_FILE`
names a file with a token of your choosing (one line, at least 32
characters, for example `hkw_machine_$(openssl rand -hex 24)`). It is read at
start and has the user role. Setting it closes the API and MCP even while
there are no users (only the token gets in); the web UI then stays open,
with the "no login" line, until the first user exists.

## Behind a reverse proxy

- The login cookie is marked *Secure* when the request came over TLS, or when
  `-public-url` / `HOOKWITNESS_PUBLIC_URL` is `https://…`: behind a TLS
  proxy, set it.
- The login limit counts failures per client address. Behind a proxy every
  login comes from the proxy's address, so they would share one limit.
  `-trusted-proxies` / `HOOKWITNESS_TRUSTED_PROXIES` (addresses or CIDRs,
  comma-separated) names your proxies; from them, the client is the
  rightmost address in `X-Forwarded-For` that is not a proxy. List only your
  own proxy: anyone else could write that header.

## Lost the admin password

On the server, set a new one; every login of that user ends:

<!-- docs-check: skip: works on the server's own database -->
```sh
hookwitness user passwd ann
hookwitness user list
```

`hookwitness user` reads the database and the secret key the way `serve`
does (`-db` / `HOOKWITNESS_DB`, `HOOKWITNESS_SECRET_KEY` or the `.key` file
next to the database). Its exit codes, for scripts: `0` done, `1` failed
(database, key), `2` usage (a bad name, a short password), `3` no such user,
`4` refused (the name is taken, or the last admin). `user add` without
`--admin` cannot make the first user.

## What is never shown

Passwords and tokens are kept only as salted hashes made with the server's
secret key; a login keeps only a hash of its id. None of them appear in logs,
API answers or pages; the logs name who made each change (`user=ann`, or
`machine` for the machine token).

Cookies ignore ports, so a browser logged in to an admin on another port of
the same host sends its login cookie to catch paths too. Hookwitness never
keeps it: in a caught request's `Cookie` header the value of
`hookwitness_session` reads `[hookwitness session]`, and nothing else is
changed.

## Turning login off again

Once on, login stays on: the last admin cannot be removed. To open the admin
again, stop the server and delete the users from its database with their
logins and tokens (`sqlite3 hookwitness.db 'PRAGMA foreign_keys=ON; DELETE
FROM users'`), or start with a new database.
