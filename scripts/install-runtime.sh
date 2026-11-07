#!/usr/bin/env bash
# Run as root inside the dedicated Linux VM.
set -euo pipefail
test "$(uname -s)" = Linux
test "$(uname -m)" = aarch64
task_dir=$(mktemp -d)
trap 'rm -rf "$task_dir"' EXIT
if ! /usr/local/bin/runsc --version 2>/dev/null | grep -q '20261102.0'; then
  curl -fsSL https://storage.googleapis.com/gvisor/releases/release/20261102.0/aarch64/gvisor.tar.zstd -o "$task_dir/gvisor.tar.zstd"
  echo 'e829ca32eef36f4e960460a6dcb094d7945ab507e9bfe0275f09467ab9bfeb846528fddfd42349ad64bb4f5b02148e1065c06e573feb8f6a8b3f47efa30a7d2e  gvisor.tar.zstd' > "$task_dir/checksum"
  (cd "$task_dir" && sha512sum -c checksum)
  tar --zstd -xf "$task_dir/gvisor.tar.zstd" -C /usr/local/bin
fi
install -d -m 0755 /etc/relayna-cell
install -m 0644 /opt/relayna-cell/deploy/runtime/pod-init.json /etc/relayna-cell/pod-init.json
python3 - <<'PY'
import pathlib,json
p=pathlib.Path('/etc/docker/daemon.json')
d=json.loads(p.read_text()) if p.exists() else {}
d.setdefault('runtimes',{})['runsc-cell']={'path':'/usr/local/bin/runsc','runtimeArgs':['--platform=systrap','--pod-init-config=/etc/relayna-cell/pod-init.json']}
p.write_text(json.dumps(d,indent=2)+'\n')
PY
systemctl restart docker
/usr/local/bin/runsc --version
