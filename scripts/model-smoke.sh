#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
COPYFILE_DISABLE=1 tar --no-xattrs -cf - scripts/model-smoke.py | limactl shell relayna-cell sudo tar -xf - -C /opt/relayna-cell
limactl shell relayna-cell sudo python3 -u /opt/relayna-cell/scripts/model-smoke.py "$@"
