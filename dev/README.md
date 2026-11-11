# Local OIDC test environment

Authelia 4.39 (file-based users instead of lldap) + Caddy (local TLS CA) in Docker, in
front of a `birdcam-server` running on this host on `:8080`.

```sh
dev/up.sh     # first run generates secrets + configs into dev/.secrets/ (git-ignored)
SSL_CERT_FILE=dev/.secrets/caddy-root.crt bin/birdcam-server -config dev/.secrets/server.yaml
dev/down.sh
```

- https://birdcam.localhost:8443 — birdcam (default device `test-pi` via the `hosts` mapping)
- https://auth.birdcam.localhost:8443 — Authelia
- Users: `alice`/`alice` (group `family`), `bob`/`bob` (no groups).
  `dev/.secrets/server.yaml` makes the H.264 test camera private to `family`.
- Browsers: import `dev/.secrets/caddy-root.crt` as a trusted CA, or accept the
  certificate warning once for each of the two hosts.
- Agent: `device_id: test-pi`, key from `dev/.secrets/device-key`.
- `SSL_CERT_FILE` lets the server trust Caddy's local CA for OIDC discovery/token calls.
- Caddy runs with host networking (reaches the server on 127.0.0.1:8080 regardless of
  firewall rules for Docker bridges); its HTTP port is 8480 to avoid needing :80.

Authelia 4.39 notes (also relevant for production):
- `groups` (and other profile claims) are only in the userinfo response, not in the ID
  token; the server falls back to userinfo automatically.
- No OIDC logout (end_session) support yet: "Log out" ends the birdcam session only,
  the Authelia SSO session stays, so "Log in" again signs in without a password prompt.
- The Authelia SSO cookie domain must cover the birdcam host(s).
