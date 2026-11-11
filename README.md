# BirdCam

Live video and audio from nest box cameras on Raspberry Pis, watched in the browser.

- **Only streams while someone watches.** Each Pi keeps a small control connection to the
  server; the camera pipeline starts when the first viewer arrives and stops 30 s after
  the last one leaves. That keeps a slow home uplink free.
- **No transcoding on the server.** The Pi sends H.264 + Opus (RTP over a TLS-pinned TCP
  connection) and the server forwards the packets unchanged to browsers via WebRTC.
- **Hardware encoding on the Pi.** Cameras with built-in H.264 are passed through (with
  their bitrate controlled on Sonix-based cameras); MJPEG cameras are decoded and encoded
  by the Pi's hardware codec within a bandwidth budget.
- **Viewer page:** fits any screen, mute toggle, rotation, recording to a local file,
  automatic grayscale for IR night images, login via OIDC (e.g. Authelia) with per-camera
  group access.

```
 Pi: birdcam-agent ──WebSocket (control)──► birdcam-server ──WebRTC──► browsers
     GStreamer      ──TLS TCP (RTP)───────►  (Go, one binary)
```

## Components

| | |
|---|---|
| `cmd/birdcam-server` | server: agent control, ingest, WebRTC, viewer page, OIDC |
| `cmd/birdcam-agent` | Pi agent: camera discovery, mode selection, GStreamer supervisor |
| `cmd/birdcam-fakeagent`, `cmd/birdcam-probe` | test tools: fake Pi, headless viewer |
| `deploy/` | example configs, systemd units, agent install script, Caddyfile |
| `dev/` | local test environment with Authelia + Caddy |
| `docs/` | [architecture](docs/ARCHITECTURE.md), [verified Pi pipelines](docs/PIPELINES.md) |

## Install

**Server** (container image, amd64/arm64):

```sh
docker run -d --name birdcam \
  -v /etc/birdcam:/etc/birdcam:ro -v birdcam-state:/var/lib/birdcam \
  -p 8080:8080 -p 9001-9010:9001-9010 -p 8189:8189/udp \
  ghcr.io/nbokdam/birdcam-server:main
```

Config: see [`deploy/server.example.yaml`](deploy/server.example.yaml). Put a TLS reverse
proxy (e.g. [Caddy](deploy/Caddyfile.example)) in front of port 8080; agents should connect
with `wss://`. Binaries are attached to [releases](../../releases) and CI runs.

**Agent** (Raspberry Pi OS 64-bit, Pi 3/4; Pi 5 only with H.264 cameras since it has no
hardware encoder): download `birdcam-agent-linux-arm64`, then on the Pi run
`deploy/install-agent.sh` (installs GStreamer, a service user and the systemd unit) and
edit `/etc/birdcam/agent.yaml` (see [`deploy/agent.example.yaml`](deploy/agent.example.yaml)).
`birdcam-agent -list-cameras` shows what the agent detects.

## Develop

```sh
make test          # Go + web tests
make build         # bin/: agent (arm64, armv7) and server
make image         # local server image
dev/up.sh          # Authelia + Caddy for testing login, see dev/README.md
```

## License

[AGPL-3.0](LICENSE). If you run a modified birdcam-server for others, you must offer them
the source of your changes.
