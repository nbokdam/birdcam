# Local OIDC test environment

Authelia 4.39 (file-based users instead of lldap) + Caddy (local TLS CA) in Docker, in
front of a `birdcam-server` running on this host on `:8080`.

```sh
dev/up.sh     # generates missing secrets into dev/.secrets/ (git-ignored), starts containers
SSL_CERT_FILE=dev/.secrets/caddy-root.crt bin/birdcam-server -config dev/server.yaml
dev/down.sh
```

## Files

| File | What to change there |
|---|---|
| `server.yaml` | birdcam-server config: devices, cameras (names, groups, rotation, night mode, budgets) |
| `authelia/configuration.yml` | Authelia: OIDC client (redirect URIs, scopes), session, policies |
| `authelia/users.yml` | test users and their groups |
| `Caddyfile` | hostnames / ports |
| `compose.yaml` | container images, mounts, secret environment variables |
| `up.sh` | only generates secrets and starts things; no config in here |

Secrets are never in these files. `up.sh` creates them once in `.secrets/` (random
secrets, the OIDC signing key, the client secret + its hash, the device key, the birdcam
session key). `server.yaml` refers to them with relative paths (relative to the config
file); Authelia reads them via `*_FILE` environment variables and its template filter
(`{{ secret "/secrets/..." }}`; note that Authelia evaluates template syntax even inside
YAML comments). Delete `.secrets/` for a completely fresh start (the Pi then needs the new
device key).

## Use

- https://birdcam.localhost:8443 — birdcam (default device `test-pi` via the `hosts` mapping)
- https://auth.birdcam.localhost:8443 — Authelia
- Users: `alice`/`alice` (group `family`), `bob`/`bob` (no groups). `server.yaml` makes the
  H.264 test camera private to `family`.
- Browsers: import `.secrets/caddy-root.crt` as a trusted CA, or accept the certificate
  warning once for each of the two hosts.
- Agent: `device_id: test-pi`, key from `.secrets/device-key`.
- `SSL_CERT_FILE` lets the server trust Caddy's local CA for OIDC discovery/token calls.
- Caddy runs with host networking (reaches the server on 127.0.0.1:8080 regardless of
  firewall rules for Docker bridges); its HTTP port is 8480 to avoid needing :80.
- Night mode tuning: add `?debugNight=1` to the page URL.

## Authelia 4.39 notes (also relevant for production)

- `groups` (and other profile claims) are only in the userinfo response, not in the ID
  token; the server falls back to userinfo automatically.
- No OIDC logout (end_session) support yet: "Log out" ends the birdcam session only; the
  Authelia SSO session stays, so "Log in" again signs in without a password prompt.
- The Authelia session cookie domain must cover the birdcam host(s).
