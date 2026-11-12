#!/bin/sh
# Install birdcam-agent on a Raspberry Pi. Run with sudo from the directory that
# contains the birdcam-agent binary, birdcam-agent.service and agent.example.yaml.
set -eu

DIR=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-$DIR/birdcam-agent}
[ "$(id -u)" -eq 0 ] || { echo "run as root (sudo)" >&2; exit 1; }
[ -f "$BIN" ] || { echo "binary not found: $BIN (set BIN=...)" >&2; exit 1; }

apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y \
	gstreamer1.0-tools gstreamer1.0-plugins-base gstreamer1.0-plugins-good \
	gstreamer1.0-plugins-bad gstreamer1.0-alsa alsa-utils

if ! id birdcam-agent >/dev/null 2>&1; then
	useradd --system --home-dir /var/lib/birdcam-agent --create-home \
		--shell /usr/sbin/nologin birdcam-agent
fi
usermod -aG video,audio birdcam-agent

install -m 0755 "$BIN" /usr/local/bin/birdcam-agent
# A manual install wins over a build the server pushed earlier (see auto_update).
rm -rf /var/lib/birdcam-agent/update

install -d -m 0755 /etc/birdcam
if [ ! -f /etc/birdcam/agent.yaml ]; then
	install -m 0640 -o root -g birdcam-agent "$DIR/agent.example.yaml" /etc/birdcam/agent.yaml
	echo "Installed /etc/birdcam/agent.yaml - edit server_url, device_id and key!"
fi

install -m 0644 "$DIR/birdcam-agent.service" /etc/systemd/system/birdcam-agent.service
systemctl daemon-reload
systemctl enable birdcam-agent.service
systemctl restart birdcam-agent.service
echo "birdcam-agent installed and started. Logs: journalctl -u birdcam-agent -f"
