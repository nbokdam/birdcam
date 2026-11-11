#!/usr/bin/env bash
# Start the local OIDC test environment (see README.md). On the first run it
# generates the secrets into dev/.secrets/ (git-ignored); the configs
# themselves are plain files in dev/.
set -euo pipefail

AUTHELIA_IMAGE=authelia/authelia:4.39.28
DEV=$(cd "$(dirname "$0")" && pwd)
SEC=$DEV/.secrets
mkdir -p "$SEC/authelia-data" "$SEC/caddy-data" "$SEC/state"
chmod 700 "$SEC"

# new_secret FILE CMD...: write CMD's output to FILE unless it exists.
new_secret() {
	local f=$SEC/$1
	shift
	[ -s "$f" ] || { "$@" >"$f"; chmod 600 "$f"; echo "generated $f"; }
}
hex() { openssl rand -hex "$1"; }

new_secret jwt-secret hex 32
new_secret session-secret hex 32
new_secret storage-key hex 32
new_secret hmac-secret hex 32
new_secret client-secret hex 32
new_secret birdcam-session.key hex 32
new_secret device-key hex 24
new_secret oidc.key openssl genrsa 2048
new_secret client-secret.hash sh -c "docker run --rm $AUTHELIA_IMAGE authelia crypto hash generate pbkdf2 --variant sha512 --password \"\$(cat '$SEC/client-secret')\" | sed -n 's/^Digest: //p'"

docker compose -f "$DEV/compose.yaml" up -d

# Caddy creates its local CA on first start; the server needs it to reach Authelia.
for _ in $(seq 1 30); do
	if docker compose -f "$DEV/compose.yaml" exec -T caddy cat /data/caddy/pki/authorities/local/root.crt >"$SEC/caddy-root.crt" 2>/dev/null && [ -s "$SEC/caddy-root.crt" ]; then
		break
	fi
	sleep 1
done

cat <<MSG

Up. Next:
  1. Server:  SSL_CERT_FILE=dev/.secrets/caddy-root.crt bin/birdcam-server -config dev/server.yaml
  2. Agent on the Pi:  device_id: test-pi   key: $(cat "$SEC/device-key")
  3. Browser: https://birdcam.localhost:8443  (alice/alice in group family, bob/bob)
     Trust dev/.secrets/caddy-root.crt as a CA, or accept the warning once per host.
MSG
