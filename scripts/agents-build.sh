#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
COPYFILE_DISABLE=1 tar --no-xattrs -cf - examples/agents-sdk | limactl shell relayna-cell sudo tar -xf - -C /opt/relayna-cell
limactl shell relayna-cell sudo docker build -t relayna-cell/agents-sdk:dev /opt/relayna-cell/examples/agents-sdk
