# BirdCam v2 architecture

Pis ("agents") with USB cameras push live video+audio to a central server, **only while
someone is watching**. Browsers watch via WebRTC. The server never transcodes; it forwards
RTP packets unchanged.

```
 Pi: birdcam-agent (Go, arm64)            Server: birdcam-server (Go, single binary)
 ┌───────────────────────────┐  WS /agent   ┌───────────────────────────────────────┐
 │ control loop              │◄────────────►│ device registry + auth (devices.yaml)  │
 │ camera discovery          │ hello/start/ │ viewer counting per camera             │
 │ pipeline supervisor       │ stop/ping    │ ingest port pool 9001-9010             │
 │  gst-launch-1.0 → stdout  │              │ ingest: RFC4571 RTP → WebRTC tracks    │
 │  agent copies stdout ─────┼─TCP:900x────►│ HTTP :8080 web page + WHEP-ish signalling│
 └───────────────────────────┘              │ WebRTC media on one UDP port (:8189)   │
                                            └───────────────────────────────────────┘
```

The wire contract lives in `internal/protocol/protocol.go` and is authoritative.

## Ports (defaults, configurable)
| Port | Proto | Use |
|---|---|---|
| 8080 | TCP | HTTP: web page, `/agent` WebSocket, `/api/...` signalling |
| 9001–9010 | TCP (TLS 1.3 by default) | ingest pool, shared by all agents/cameras |
| 8189 | UDP | WebRTC media (pion UDP mux, single port) |

## Control channel
- Agent connects to `ws(s)://SERVER/agent`, sends `hello` with `device_id` + `key`
  (pre-shared, in server `config.yaml`). Bad key → server sends `error` and closes.
- Server replies `welcome` (incl. `ingest_cert_sha256` when ingest TLS is on). Agent sends `cameras` whenever its camera set changes.
- Both sides send `ping` every 10 s and answer `pong`. Any received message resets a 30 s
  deadline; on expiry the connection is closed. Agent reconnects with backoff
  1,2,5,10,15,15… s (reset after a connection survives 60 s).
- On agent disconnect the server drops the device's sessions and frees ports. On
  reconnect, if viewers are still waiting, the server issues fresh `start`s.
- On control disconnect the agent stops all pipelines.

## Streaming lifecycle (per camera)
1. First viewer for camera C → server allocates a free port P from the pool, a random
   `session_id` and `token`, sends `start{camera_id, session_id, port, token, profile}`.
2. Agent builds a pipeline (see modes), connects TCP to SERVER:P (TLS if `welcome` carried
   `ingest_cert_sha256`), writes `BIRDCAM1 <session_id> <token>\n`, then copies gst-launch
   stdout (RFC 4571 framed RTP) into the socket. Reports `stream_status` starting/running/failed.
3. Server ingest listener on P accepts only a connection whose preamble matches the
   pending session (else closes immediately), then reads frames and routes by payload type:
   96 = H.264 (90 kHz) → video track, 111 = Opus (48 kHz) → audio track.
4. Last viewer leaves → after 30 s linger, server sends `stop`, frees the port.

## Ingest TLS
`ingest.tls` (default true): every ingest connection must complete a TLS 1.3 handshake
(within the 5 s preamble deadline) before the preamble is read; plain TCP is dropped.
The certificate is `ingest.cert_file`/`key_file`, or a self-signed ECDSA P-256 certificate
generated once in `state_dir` (default `/var/lib/birdcam`) so its fingerprint survives
restarts. `welcome.ingest_cert_sha256` carries the lowercase hex SHA-256 of the leaf DER;
the agent pins exactly that (no CA/hostname/date checks). Trust is anchored in the control
channel, so agents must use `wss://`. With `ingest.tls: false` no fingerprint is sent and
plain TCP is accepted.

## Pipeline modes (agent)
- `h264-passthrough`: camera produces H.264 itself. No encoding on the Pi.
  `v4l2src device=… ! video/x-h264,width,height,framerate ! h264parse config-interval=-1 ! rtph264pay pt=96 mtu=1200 config-interval=-1 aggregate-mode=zero-latency`
- `mjpeg-hwenc`: MJPEG camera → HW JPEG decode (bcm2835-codec-decode) → HW H.264 encode
  (bcm2835-codec-encode) at a chosen bitrate. Used for MJPEG-only cameras and to honor a
  bandwidth budget the camera's own encoder can't meet.
- Audio (if camera has a mic and profile.audio): `alsasrc … ! audioconvert ! audioresample ! opusenc ! rtpopuspay pt=111`
- Both branches → `funnel ! rtpstreampay ! fdsink fd=1`.
- Exact, verified pipeline strings: see `docs/PIPELINES.md` (written by the pipeline spike).

Never hard-code `/dev/videoN`: cameras are found via `/dev/v4l/by-id`, codec devices by
their V4L2 card name (`bcm2835-codec-encode` / `-decode`). Must work on Pi 3 and Pi 4
(64-bit Raspberry Pi OS). Pi 5 has no HW encoder → only passthrough.

## Bandwidth
Server config has `max_kbps` per device and optionally per camera; the result is sent in
`start.profile.max_kbps`. When a session starts, the device budget is divided evenly over
the device's cameras that currently have a session (streaming or starting), including the
one being started — a single streaming camera gets the whole budget. Sessions already
running keep the bitrate they were started with (no renegotiation). A per-camera
`max_kbps` caps the share (and is used as-is if the device is unlimited). Agent: if passthrough camera's bitrate can
be set to fit, use it; otherwise fall back to `mjpeg-hwenc` with that bitrate.

## Watchdogs
- Agent: restart gst-launch on exit/error (backoff); report `failed` after repeated failures.
- Server: per session, if no RTP for 10 s while running → `stop` + new `start`.
  Track A/V drift: for each stream `drift = (rtp_ts - first_ts)/clock - (arrival - first_arrival)`;
  if |video_drift - audio_drift| > 1.0 s → restart session.
- systemd `Restart=always` for both binaries.

## Repo layout
```
cmd/birdcam-server/   main for server
cmd/birdcam-agent/    main for agent
internal/protocol/    shared wire contract (do not change without updating both sides)
internal/server/...   server packages
internal/agent/...    agent packages
web/                  static viewer page (embedded via go:embed)
deploy/               systemd units, example configs, install scripts
docs/                 ARCHITECTURE.md, PIPELINES.md
```

## Viewer authentication and access control (server)
- Login is enabled iff `auth.oidc.issuer` is set. OIDC authorization code flow with PKCE
  (S256), state and nonce; confidential client (`client_secret_basic` default, or
  `client_secret_post`). Discovery runs in the background and is retried; until it succeeds
  `/auth/login` answers 503. Groups come from `auth.oidc.groups_claim` (default `groups`);
  if it (or `preferred_username`) is missing from the ID token, the userinfo endpoint is
  queried (Authelia's default claims policy).
- No server-side session store: the session is an AES-256-GCM sealed cookie
  (`{username, name, email, groups, expiry, id_token?}`; the ID token is dropped if the
  cookie would exceed ~3.8 KB), HttpOnly, SameSite=Lax, Path=/, Secure (+ `__Host-` name)
  when `external_url` is https. Key: `auth.session_key[_file]`, else random per start.
  The login state (state, nonce, PKCE verifier, return path, redirect URI) lives in a
  separate 10-minute sealed cookie on `/auth/`.
- The redirect URI is `auth.oidc.redirect_url` if set, otherwise
  `<external_url scheme>://<request host>/auth/callback` when the request host is
  `external_url`'s host or listed in `hosts`, else `external_url + /auth/callback`.
  Register every such callback URL at the provider.
- Access: a camera's groups are `cameras[].groups` if set, else `devices[].groups`;
  `cameras[].public: true` forces public; no groups = public. A private camera is visible
  and watchable iff the user is in at least one of its groups. With login disabled, private
  cameras are hidden (startup warning). `hidden: true` hides a camera from everyone.
  Already-connected viewers are not re-checked (config is not hot-reloaded).
- `DELETE /api/viewers/{id}` is not access-checked: the id is a random 128-bit value only
  returned to the viewer that created it.

## Viewer-facing HTTP API (contract between server and web/)

All JSON. Errors: `{"error": "message"}` with a proper status code.

### `GET /api/session`
```json
{
  "auth_enabled": true,
  "user": {"name": "Cat", "username": "cat", "email": "cat@example.com", "groups": ["family"]},
  "login_url": "/auth/login",
  "logout_url": "/auth/logout",
  "default_device_id": "maci-pi"
}
```
- `user` is `null` when not logged in. `auth_enabled` false → no login UI at all (and
  `login_url`/`logout_url` are `""`; the `/auth/*` endpoints do not exist).
- `default_device_id`: from the server's `hosts` config, matched on the request Host
  (port stripped, case-insensitive; `X-Forwarded-Host` instead if `trust_proxy_headers`);
  `""` if no match.
- Login: page navigates to `login_url + "?return=" + encodeURIComponent(location.pathname + location.search)`.
  `return` must be a local path (`/…`, not `//…`); anything else falls back to `/`. After
  login the browser is redirected (303) to it. Login answers 503 (plain text) while the
  OIDC provider is unreachable.
- Logout: `POST logout_url`. A form post (`application/x-www-form-urlencoded` or
  `multipart/form-data`) gets a 303 redirect; any other request (e.g. `fetch` with no body)
  gets JSON `{"redirect": "<url>"}`. The URL is the provider's `end_session_endpoint`
  (with `client_id`, `post_logout_redirect_uri` and `id_token_hint` when available) if the
  provider advertises one, else `"/"`. The session cookie is cleared either way.

### `GET /api/cameras`
Array, **filtered by access**: private cameras the viewer may not see are omitted entirely.
Each entry (existing fields kept):
```json
{
  "device_id": "maci-pi", "device_name": "Maci", "camera_id": "usb-...", "name": "Nest box",
  "online": true, "streaming": false, "viewers": 0, "state": "idle", "error": "",
  "audio": true, "rotation": 90, "public": false, "stats": {...}
}
```
- `rotation`: effective clockwise display rotation (server override > agent-reported > 0).
- `public`: false if the camera requires login/group membership.

### WHEP `POST /api/cameras/{device}/{camera}/whep`
Unchanged, plus access control: 404 if the camera doesn't exist or is not visible to the viewer
(don't leak private camera existence to anonymous users: anonymous → 401 only if the camera
exists and is private; logged-in user not in a permitted group → 404). "Exists" means the
camera has been reported by its agent at least once since the server started (it may be
offline now). Error bodies are JSON `{"error": "..."}`.

### Page URL parameters
`/?device=<id>&camera=<id>` selects a camera (overrides `default_device_id`). The page keeps the
URL in sync with the selection (history.replaceState) so it can be bookmarked/shared.
