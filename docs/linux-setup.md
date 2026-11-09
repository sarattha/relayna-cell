# Linux VM installation

Run the Bash installer **on the target Linux machine**, from a checkout of this repository:

```bash
sudo bash scripts/setup-linux.sh
```

It asks, in order:

1. **Docker or Podman.** Install and configure the chosen engine yourself first.
2. **Ollama, llama.cpp, vLLM, SGLang, or skip local inference.** No engine is silently selected.
3. **Model.** Press Enter for a small Qwen3 0.6B smoke-test model. For llama.cpp, supply a local GGUF path or accept the checksum-verified default download.

The setup script checks prerequisites before pulling images or installing Cell. It prints every missing prerequisite and stops with a nonzero status. It never runs a package manager, installs a container engine, or installs GPU drivers/toolkits. Kubernetes is outside this installer's scope.

## Supported profiles and prerequisites

Use a dedicated systemd Linux VM, kernel 5.6 or newer, x86_64 or aarch64, and cgroup v2. Rootful operation is required. The initial supported host profile is Ubuntu/Debian without SELinux enforcement. The script rejects unsupported profiles; it does not disable security controls.

| Choice | Required infrastructure | Initial model/profile |
| --- | --- | --- |
| Docker | Running local Docker Engine, `dockerd`, iptables/ip6tables, `DOCKER-USER` chain | Unix socket `/var/run/docker.sock`; remote Docker contexts ignored |
| Podman | Rootful local Podman, Netavark, nftables | Native CLI; no Docker compatibility service required |
| No inference | 3 GiB RAM, 6 GiB free disk | Cell and complete observability stack |
| Ollama | 3.5 GiB RAM, 8 GiB free disk | CPU; `qwen3:0.6b`, 4K context, one loaded model |
| llama.cpp | 3.5 GiB RAM, 8 GiB free disk | CPU; Qwen3 0.6B Q8_0 GGUF, 4K context |
| vLLM / SGLang | 15 GiB RAM, 50 GiB free disk, NVIDIA GPU with at least 8 GB VRAM, driver 580+, configured NVIDIA Container Toolkit | CUDA 13 GPU image; `Qwen/Qwen3-0.6B`, 4K context |

These are minimum checks for the small default model, not sizing guarantees for arbitrary models. A **32 GiB VM disk** is recommended for the CPU profiles: the Ollama image and Go builder consume substantially more disk than the small model itself, and reruns also need free staging space. CPU inference containers are capped at 2 CPUs/2 GiB; GPU inference containers at 2 CPUs/12 GiB host memory. Larger models require an explicit deployment change. GPU profiles are NVIDIA-only in this installer; CPU variants of vLLM/SGLang, ROCm, distributed serving, and gated-model credentials are not configured.

Common utilities: Bash, Python 3.10 or newer with its SQLite module, curl, tar with zstd support, zstd, systemctl, and flock. Go, Compose, and a separate SQLite server are **not** host prerequisites. Without prebuilt binaries, a pinned Go builder container compiles Cell from the checkout.

Install infrastructure using the upstream instructions: [Docker](https://docs.docker.com/engine/install/), [Podman](https://podman.io/docs/installation), and [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html). Configure Docker's NVIDIA runtime or Podman's NVIDIA CDI devices before selecting a GPU profile.

## Automation and preflight

```bash
# Read-only prerequisite checks: no image pulls or installation.
sudo bash scripts/setup-linux.sh --engine docker --inference ollama --check

# Full setup, including model download, inference, logs, and dashboards.
sudo bash scripts/setup-linux.sh --engine docker --inference ollama
sudo bash scripts/setup-linux.sh --engine podman --inference llama.cpp

# Existing local GGUF; no model download.
sudo bash scripts/setup-linux.sh --engine podman --inference llama.cpp \
  --model /srv/models/model.gguf

# Bring your own native Linux binaries to skip the builder container.
sudo bash scripts/setup-linux.sh --engine docker --inference none \
  --bin-dir /srv/cell-binaries
```

Noninteractive use requires both engine flags. The binary directory must contain native `cell`, `cell-init`, and `cell-server` ELF binaries. `--model` is an Ollama model tag, a local GGUF path for llama.cpp, or a public Hugging Face model repository for vLLM/SGLang. Models are downloaded by trusted infrastructure; Cells themselves remain subject to the network policy.

## What setup installs

- Cell CLI, init, and server under `/opt/relayna-cell/bin`, with `/usr/local/bin/cell` as a launcher.
- SHA-512-verified gVisor release `20261102.0`, including adjacent runtime sidecars; a dedicated `runsc-cell` wrapper selects systrap and the SecCheck configuration.
- Docker runtime registration, preserving unrelated daemon settings and backing up changed configuration, or a native Podman runtime path.
- Host network guard: only the Cell egress proxy is reachable from Cell bridges. Direct forwarding and IPv6 egress are blocked.
- Root-only management socket, systemd services, and persistent SQLite journal at `/var/lib/relayna-cell/cell.db`.
- Loki, Grafana Alloy, Grafana, persistent volumes, the Loki datasource, **overview and audit dashboards**, and the selected inference profile.
- A restricted inference gateway. Cells can call Chat Completions; model pull/delete and other management routes are blocked. Infrastructure uses ordinary containers; every Cell uses gVisor.

The image catalog is [`deploy/linux/images.json`](../deploy/linux/images.json). Installation records resolved image digests and choices in `/etc/relayna-cell/setup.json`. Runtime and model downloads use HTTPS; gVisor and the default GGUF also have checked hashes. No `curl | sh` installer is used.

## Completion checks and access

Setup is successful only when the verification script passes: a real gVisor execution runs as UID 65532, root filesystem writes and direct internet access fail, workspace writes succeed, SecCheck process evidence appears, SQLite passes `quick_check`, and an exact output event is found through Alloy in Loki. Grafana must expose both dashboards and a healthy Loki datasource. If inference is selected, a Cell must get a real model response while model-management requests and proxied/direct internet requests are denied.

```bash
sudo cell health
sudo python3 /opt/relayna-cell/scripts/verify-linux-setup.py
systemctl status cell-server relayna-infrastructure
sudo journalctl -u cell-server -u relayna-infrastructure -n 100
```

Grafana listens on **127.0.0.1:3000**. For a remote VM:

```bash
ssh -L 3000:127.0.0.1:3000 USER@LINUX_HOST
```

Open [overview](http://127.0.0.1:3000/d/relayna-cell) or [audit](http://127.0.0.1:3000/d/relayna-cell-audit). Grafana has anonymous **Viewer** access on loopback. Loki is on loopback port 3100. Do not expose either unauthenticated service directly to the internet.

Inference settings are in `/etc/relayna-cell/inference.json`. The host-only endpoint is `http://127.0.0.1:18080/v1`. Create a Cell with the generated exact gateway grant:

```bash
sudo cell create -f /etc/relayna-cell/model-cell.json
```

For OpenAI Agents SDK, use the recorded model/base URL with its Chat Completions adapter and disable OpenAI tracing, as in [`examples/agents-sdk`](../examples/agents-sdk). The Linux smoke test validates the selected backend's Chat Completions response, not every framework/tool-calling combination. Arbitrary model choices must support the intended agent tool schema.

## Reruns and lifecycle

Rerunning setup with the same container engine preserves the journal, Loki data, Grafana data, Alloy offsets, and model cache. It stops active Cells and replaces managed infrastructure containers. Audit history survives. Changed configuration gets a `.before-setup-*` backup. Engine migration and adoption of the legacy Lima installation are rejected; use a fresh VM for those cases.

Names beginning `relayna-linux-` are managed using the `dev.relayna.installation=linux-v1` ownership label; foreign resources with those names are not removed. Never remove the named volumes to restart services. `systemctl restart relayna-infrastructure` recreates the owned containers, regenerates the gateway address and example grant, and reuses stored data. Recreate model-enabled Cells after an infrastructure restart because the gateway IP may change.

If setup fails, it exits nonzero and leaves logs and persistent data for diagnosis; it does not claim success or erase the journal. Fix the reported error and rerun. Services are enabled at boot. Stop both with `sudo systemctl stop cell-server relayna-infrastructure`.

## Validation limits

On 2026-11-09, separate Ubuntu 24.04 ARM64 VMs passed real installation checks with Docker/Ollama and Podman/llama.cpp, including inference from a Cell, model-management denial, direct/proxied internet denial, audit delivery, and both dashboards. Reinstallation and checks after a VM reboot also passed. The Docker path built the binaries inside its builder container; the Podman path also exercised prebuilt binaries. Missing-engine and missing-GPU preflights stopped before image pulls. Eight host-safe regression checks cover selection, hash rejection, resource ownership, config backups, and disabling previously configured inference. x86_64 and actual NVIDIA GPU execution remain unverified in this environment.

The security model is still the development MVP's: trusted host administrator and container engine, asynchronous runtime audit, and the documented silent trace-session removal gap. Installer smoke checks do not prove resistance to every sandbox escape. GPU runtime validation requires real NVIDIA hardware; CPU-only VM tests cannot establish vLLM/SGLang GPU behavior.
