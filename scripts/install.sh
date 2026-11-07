#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
task_bundle=$(mktemp -d)
trap 'rm -rf "$task_bundle"' EXIT
mkdir -p "$task_bundle/bin"
cp bin/linux-arm64/cell bin/linux-arm64/cell-server bin/linux-arm64/cell-init "$task_bundle/bin/"
cp -R deploy scripts examples "$task_bundle/"
limactl shell relayna-cell sudo bash -c 'if systemctl cat cell-server.service >/dev/null 2>&1; then systemctl stop cell-server.service; fi'
COPYFILE_DISABLE=1 tar -C "$task_bundle" --no-xattrs -cf - . | limactl shell relayna-cell sudo tar -xf - -C /opt/relayna-cell
limactl shell relayna-cell sudo bash /opt/relayna-cell/scripts/install-runtime.sh
limactl shell relayna-cell sudo install -m 0644 /opt/relayna-cell/deploy/runtime/cell-server.service /etc/systemd/system/cell-server.service
limactl shell relayna-cell sudo systemctl daemon-reload
limactl shell relayna-cell sudo systemctl enable --now cell-server
limactl shell relayna-cell sudo systemctl restart cell-server
limactl shell relayna-cell sudo docker compose -f /opt/relayna-cell/deploy/observability/compose.yaml up -d
limactl shell relayna-cell sudo /opt/relayna-cell/bin/cell health
