#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
COPYFILE_DISABLE=1 tar --no-xattrs -cf - deploy/inference | limactl shell relayna-cell sudo tar -xf - -C /opt/relayna-cell
limactl shell relayna-cell sudo docker compose -f /opt/relayna-cell/deploy/inference/compose.yaml up -d --wait --wait-timeout 120
limactl shell relayna-cell sudo docker compose -f /opt/relayna-cell/deploy/inference/compose.yaml exec -T ollama ollama pull qwen3:4b-instruct
limactl shell relayna-cell sudo docker compose -f /opt/relayna-cell/deploy/inference/compose.yaml exec -T ollama ollama list
