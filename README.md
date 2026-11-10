# Relayna Cell

Relayna Cell runs agent workloads and delegated tools in gVisor sandboxes. It audits process execution and enforces network, filesystem, and resource restrictions. The Go implementation supports both an external agent submitting commands and a complete agent running inside a Cell.

This development MVP supports one trusted operator. The control plane manages sandboxed execution, while inference and observability remain outside the workload sandbox.

## Architecture at a glance

Cell separates the **workload sandbox**, the **trusted control plane**, and **shared inference and observability**. Every workload uses gVisor; a local model runs outside the sandbox and returns tool calls for the agent to execute inside it.

[![Relayna Cell logical architecture: Linux, macOS, and Windows clients connect to a trusted Cell service; Docker or Podman runs gVisor tool sessions or contained agents; approved inference passes through a policy proxy and gateway; SQLite audit events flow through JSONL, Alloy, Loki, and Grafana.](docs/assets/relayna-cell-architecture.png)](docs/assets/relayna-cell-architecture.png)

[Open the full-size diagram](docs/assets/relayna-cell-architecture.png).

The API, lifecycle controller, egress proxy, SecCheck receiver, and audit store are modules in one Go `cell-server` process. The host, container engine, and control plane are trusted; agent workloads run inside the gVisor boundary. External agents can delegate tools to a Cell, or the complete agent loop can run inside one.

This is a logical architecture, independent of the operator’s operating system. The execution backend currently requires a Linux host or Linux VM; platform-specific setup and validation details are in the deployment guides below.

### Components

| Component | Role | Implementation |
| --- | --- | --- |
| CLI and Go SDK | Create Cells, execute commands, transfer workspace files, and read retained events through the management API. | [`cmd/cell`](cmd/cell), [`pkg/client`](pkg/client) |
| Cell API and lifecycle controller | Validate requests, manage deadlines and execution, revoke access, and clean up runtime resources. | [`internal/api`](internal/api), [`internal/engine`](internal/engine) |
| Docker / Podman adapters and gVisor | Require `runsc-cell`, isolate the workload, and apply filesystem, identity, CPU, memory, and process restrictions. | [`internal/runtime`](internal/runtime), [`deploy/runtime`](deploy/runtime) |
| Workload and `cell-init` | Run delegated commands or the complete agent; keep files in a bounded, per-Cell workspace. | [`cmd/cell-init`](cmd/cell-init), [`examples/agents-sdk`](examples/agents-sdk) |
| Egress proxy and host firewall | Block direct egress and authorize exact HTTP/CONNECT destinations. An offline Cell has no external network. | [`internal/network`](internal/network), [`scripts/network-guard.sh`](scripts/network-guard.sh) |
| Inference gateway and selected engine | Share the local model while exposing Chat Completions and restricting model-management routes. | [`deploy/inference`](deploy/inference), [`Linux setup`](docs/linux-setup.md) |
| SecCheck receiver | Receive gVisor process/connection observations outside the workload and attach trusted runtime provenance. | [`internal/observer`](internal/observer) |
| Audit journal and export | Commit events to SQLite, then export JSONL with stable event IDs; retain evidence after Cell deletion. | [`internal/audit`](internal/audit) |
| Grafana Alloy and Loki | Tail the exported records, forward them with retry, and store the searchable log projection. | [`deploy/observability`](deploy/observability) |
| Grafana | Show activity charts, outcomes, destinations, and per-Cell investigations with readable and raw logs. | [`deploy/observability/grafana`](deploy/observability/grafana) |

### Audit flow

**SQLite journal → JSONL export → Grafana Alloy → Loki → Grafana**, as shown in the diagram.

Control-plane lifecycle events, proxy decisions, captured workload output, and gVisor SecCheck observations enter the journal. SQLite is authoritative; Loki is a downstream projection. Workload output retains its own source label and is not treated as trusted runtime evidence.

### Two execution modes

| | Agent delegates tools | Entire agent inside a Cell |
| --- | --- | --- |
| Agent loop | A trusted external agent calls the model and Cell API. | The agent and its tool subprocesses run inside gVisor. |
| Execution flow | Agent → Cell API → sandboxed command → result returned to agent. | Agent → approved inference endpoint → local tool subprocess → next agent turn. |
| Network boundary | Tool Cell can remain offline; the external agent handles inference. | Cell receives an explicit grant for the inference gateway. Other destinations remain denied. |
| Workspace | Shared between commands until the Cell ends. | Shared by the agent and its tools until the Cell ends. |

**Enforcement and auditing have different roles.** Filesystem, network, and resource restrictions enforce the sandbox boundary. Runtime audit events are asynchronous evidence, not synchronous approval of every operation. The current MVP does not guarantee detection of every escape attempt before it succeeds; silent removal of a trace session remains a known monitoring gap.

For deeper detail, see the [architecture decisions](docs/architecture.md). Download and open the [interactive architecture guide](docs/architecture.html) for component details, execution flows, and sequence diagrams; the HTML guide works offline.

## Setup

Choose the path for the machine where you are starting:

- [Linux](#start-on-linux): install directly on a dedicated Linux host or VM, choosing Docker/Podman and an optional inference engine.
- [macOS](#start-on-macos): create a dedicated Lima Linux VM from an Apple Silicon Mac, then install Cell and its dashboards into that VM.

The checkout commands below use `main`. To install a particular release, check out its tag before running setup. If you already have this checkout, skip cloning and enter its root directory. Run subsequent project commands from that directory.

| Task | Linux host or VM | macOS terminal → Lima VM |
| --- | --- | --- |
| Install Cell, logging, and dashboards | `sudo bash scripts/setup-linux.sh` | `make vm-up`, then `make install` |
| Container/inference selection | Installer prompts for Docker/Podman and inference engine | Supplied Lima workflow uses Docker; `make inference-up` adds Ollama |
| Verify sandbox and audit delivery | `sudo python3 /opt/relayna-cell/scripts/verify-linux-setup.py` | `make smoke` |
| Manage Cells | `sudo cell ...` | `./scripts/cell ...` |
| Grafana overview | `http://127.0.0.1:3000/d/relayna-cell` | `http://localhost:3300/d/relayna-cell` |
| Build the optional SDK image | Rootful Docker/Podman build; see the [SDK guide](examples/agents-sdk/README.md#container-build) | `make agents-build` |
| Test SDK agents with a real model | Installer verifies model connectivity; framework/tool-calling tests require separate integration | `make model-smoke` after inference and image setup |

The setup scripts have platform-specific entry points. `make install`, `make smoke`, `make inference-up`, `make agents-build`, `make model-smoke`, and the `./scripts/cell` wrapper invoke Lima and run from **macOS**. On Linux, use the installer and verification commands in the Linux column. Local uv development works on both platforms.

## Start on Linux

### 1. Prepare the Linux machine

Use a dedicated Ubuntu/Debian systemd host or VM with kernel 5.6+, cgroup v2, and root/sudo access. The installer accepts x86_64 and ARM64; real installation tests currently cover ARM64. For the small CPU model, allow at least 3.5 GiB RAM and 8 GiB free disk; a 32 GiB VM disk leaves room for images, builds, and updates.

Install the prerequisites yourself before running Cell setup:

- **One rootful container engine:** [Docker Engine](https://docs.docker.com/engine/install/) with its daemon running and the iptables `DOCKER-USER` chain, or [Podman](https://podman.io/docs/installation) with Netavark and nftables.
- **Host tools:** Git, Bash, Python 3.10+ with SQLite support, curl, tar with zstd support, zstd, systemctl, and flock. Docker also requires `dockerd`, iptables, and ip6tables; Podman requires `nft`.
- **For vLLM/SGLang:** a supported NVIDIA GPU, compatible drivers, and configured NVIDIA Container Toolkit. See the [GPU prerequisites and validation limits](docs/linux-setup.md#supported-profiles-and-prerequisites).

The installer reports missing infrastructure and stops. It does not install OS packages, container engines, or GPU drivers. Cell embeds SQLite, and the default build runs in a Go container. See the [Linux setup guide](docs/linux-setup.md) for the full host profile, sizing, and unsupported configurations.

### 2. Check out the code and run setup

Run these commands **on the Linux machine**:

```bash
git clone https://github.com/sarattha/relayna-cell.git
cd relayna-cell
sudo bash scripts/setup-linux.sh
```

The interactive installer asks, in order:

1. **Container engine:** Docker or Podman.
2. **Inference engine:** Ollama, llama.cpp, vLLM, SGLang, or skip local inference.
3. **Model:** accept the small Qwen3 0.6B default or supply a model name; llama.cpp accepts a local GGUF path.

Ollama and llama.cpp use CPU in these profiles. vLLM and SGLang use NVIDIA GPU profiles; actual GPU execution remains unverified in this environment. Skipping inference still installs Cell and the complete observability stack.

Once prerequisites pass, setup pulls images, builds or installs Cell, installs checksum-verified gVisor, configures the sandbox runtime and network guard, initializes SQLite storage, and starts the systemd services. It also configures **Grafana Alloy, Loki, Grafana's Loki datasource, and both dashboards**, plus the selected inference engine and restricted gateway. Installation succeeds only after the sandbox, audit delivery, and dashboard checks pass.

Expect `[OK]` prerequisite checks followed by installation and verification output, ending with **`Relayna Cell is ready.`** A check-only run ends with `All prerequisites green. Check only: nothing installed.` and still requires a subsequent full setup run.

For a check-only run, or unattended installation, use explicit choices:

```bash
# Check prerequisites without downloading or installing anything.
sudo bash scripts/setup-linux.sh --engine docker --inference ollama --model qwen3:0.6b --check

# Example unattended installation with Docker and CPU inference.
sudo bash scripts/setup-linux.sh --engine docker --inference ollama --model qwen3:0.6b
```

### 3. Verify Cell and open Grafana

```bash
sudo cell health
sudo systemctl status cell-server relayna-infrastructure

# Repeat the sandbox, audit, dashboard, and selected-model checks.
sudo python3 /opt/relayna-cell/scripts/verify-linux-setup.py
```

Open the [overview](http://127.0.0.1:3000/d/relayna-cell) or [audit dashboard](http://127.0.0.1:3000/d/relayna-cell-audit) on the Linux machine. Grafana listens on loopback port **3000** with anonymous Viewer access. For a remote machine, open a tunnel **from your own computer**, keep it running, and use the same browser links:

```bash
ssh -L 3000:127.0.0.1:3000 USER@LINUX_HOST
```

Settings are saved in `/etc/relayna-cell/setup.json`. If inference was selected, `/etc/relayna-cell/inference.json` records the model and endpoint, and `/etc/relayna-cell/model-cell.json` contains an example Cell with the exact gateway grant:

```bash
sudo cell create -f /etc/relayna-cell/model-cell.json
```

### 4. Stop, start, or update

```bash
# Stop workloads and infrastructure; retain stored data.
sudo systemctl stop cell-server relayna-infrastructure

# Start the installed services again.
sudo systemctl start relayna-infrastructure cell-server

# Diagnose a startup or verification failure.
sudo journalctl -u cell-server -u relayna-infrastructure -n 100 --no-pager
```

To update, check out the intended version and rerun `scripts/setup-linux.sh` with the **same container engine**. Rerunning stops active Cells and replaces managed infrastructure containers while preserving the journal, observability volumes, and model cache. Engine migration and adoption of the macOS/Lima installation are not supported by this script. See [reruns and lifecycle](docs/linux-setup.md#reruns-and-lifecycle), including when to recreate model-enabled Cells after a gateway restart.

## Start on macOS

### 1. Prepare the Mac

The supplied VM configuration targets **Apple Silicon** and allocates **4 CPUs, 8 GiB RAM, and a 40 GiB sparse disk**. Have that memory and disk capacity available. Install Xcode Command Line Tools for Git and Make (`xcode-select --install` if needed), then [Homebrew](https://docs.brew.sh/Installation) and the host tools:

```bash
brew install go lima python
go version          # Go 1.26 or newer
limactl --version   # Lima 2 or newer
python3 --version
```

[Lima's installation guide](https://lima-vm.io/docs/installation/) covers other installation methods. This workflow uses Docker inside its own Linux VM. Docker Desktop can remain installed, but Cell does not use its daemon. The VM does not mount your Mac home directory or project files.

### 2. Create the VM and install Cell

Run these commands **in a macOS terminal**. If you already have the checkout, enter it and start at `make vm-up`:

```bash
git clone https://github.com/sarattha/relayna-cell.git
cd relayna-cell
make vm-up
make install
```

`make vm-up` creates and starts the `relayna-cell` Lima VM. Its provisioning script downloads Ubuntu and installs Docker Engine, Compose, and Linux utilities **inside that dedicated VM**. `make install` builds the native Mac CLI and Linux ARM64 binaries, copies deployment files into the VM, installs gVisor, configures the network guard, starts Cell, and provisions **Alloy, Loki, Grafana, the datasource, and both dashboards**.

This macOS shortcut uses a fixed Docker deployment; it does **not** present the Linux installer's engine-selection menu. To choose Podman or a different inference profile from a Mac, prepare a separate Linux VM and follow [Start on Linux](#start-on-linux) inside it. Keep that installation separate from the `make install` workflow.

### 3. Verify the VM and open Grafana

From **macOS**:

```bash
limactl list
./scripts/cell health
make smoke
```

Expect `relayna-cell` to show `Running`. The health command connects over Lima SSH to the root-only Unix API in the VM. `make smoke` runs real sandbox, policy, audit, and Loki recovery checks and cleans up its test Cells.

Wait for `ALL INTEGRATION CHECKS PASSED` before continuing. In Grafana, both **Relayna Cell** (overview) and **Relayna Cell · Investigation** dashboards should be available; the smoke test supplies initial activity to inspect.

Open the [overview](http://localhost:3300/d/relayna-cell) or [audit dashboard](http://localhost:3300/d/relayna-cell-audit) on the Mac. Lima forwards Grafana's guest port 3000 to **localhost:3300** on macOS, with anonymous Viewer access. Grafana is the only application port forwarded to the Mac; management uses Lima's SSH transport to reach the root-only Unix socket.

To inspect Linux directly:

```bash
# Run on macOS to enter the guest.
limactl shell relayna-cell

# Now inside Linux.
uname -s
sudo systemctl status cell-server
exit
```

After `exit`, you are back on macOS. Run `limactl`, project `make` targets, and `./scripts/cell` on the **Mac**, not inside the guest. A `limactl: command not found` message inside the VM means you are already in Linux.

### 4. Optionally add Ollama and test the agent framework

From the project directory **on macOS**, after the base checks pass:

```bash
make inference-up   # start Ollama + gateway and download Qwen3 4B Instruct
make agents-build   # build the OpenAI Agents SDK example image
make model-smoke    # test delegated tools and a complete contained agent
```

This path uses the VM's CPU and stores the model in a persistent Docker volume. It does not require an OpenAI API key. See [Local Ollama and OpenAI Agents SDK](#local-ollama-and-openai-agents-sdk) for model details, expected results, and inspection commands.

### 5. Stop, resume, or update

Run these commands **on macOS**:

```bash
limactl stop relayna-cell    # stop the VM without deleting its disk
limactl start relayna-cell   # resume the existing VM
./scripts/cell health

# Inspect the Cell service's logs inside the VM.
limactl shell relayna-cell sudo journalctl -u cell-server -n 100 --no-pager
```

To update Cell, check out the intended version and rerun `make install` on the Mac. It stops active Cells and restarts Docker in the dedicated VM; stored audit records and named data volumes remain. Use `make dashboards-up` when only the dashboard definitions need updating.

## Setup troubleshooting

| Symptom | Next step |
| --- | --- |
| Linux preflight prints `[FAIL]` | Install or configure the reported prerequisite yourself, then repeat `--check` and full setup with the same choices. |
| `limactl: command not found` | At a `jobz@lima-relayna-cell` prompt, run `exit` to return to macOS. In a Mac terminal, check that Lima is installed and on `PATH`. |
| The existing Lima VM is stopped | Run `limactl start relayna-cell` on macOS, then `./scripts/cell health`. |
| Cell health or Linux verification fails | On Linux, run `sudo journalctl -u cell-server -u relayna-infrastructure -n 100 --no-pager`. For the Lima workflow, run `limactl shell relayna-cell sudo journalctl -u cell-server -n 100 --no-pager` on macOS. |
| Grafana is unreachable | Use port **3000** on the Linux host or through its SSH tunnel, and **3300** on macOS. Start the VM or installed services before opening it. |
| The SDK image exists but Linux Cell cannot find it | Build with `sudo docker` or `sudo podman`, matching the rootful engine selected during installation. Rootless engines and Docker Desktop use separate image stores. |

The first setup downloads container images and, when selected, model files. Wait for the final verification result. If setup exits unsuccessfully, inspect the reported error, fix it, and rerun the same installation path; [Linux rerun details](docs/linux-setup.md#reruns-and-lifecycle) explain persistence and gateway changes.

## Delegate commands from an external agent

The CLI examples below use the **macOS** wrapper `./scripts/cell`. After the **Linux** installation, replace that prefix with `sudo cell` and run from the Linux checkout directory.

```bash
./scripts/cell create -f examples/session.json
# Copy the returned id:
./scripts/cell exec CELL_ID -- sh -c 'printf hello > /workspace/result.txt'
./scripts/cell exec CELL_ID -- cat /workspace/result.txt
./scripts/cell put CELL_ID ./README.md input/README.md
./scripts/cell get CELL_ID result.txt > result.txt
./scripts/cell events --follow CELL_ID
./scripts/cell delete CELL_ID
```

The workspace survives tool calls within the same Cell. It is a bounded tmpfs and is destroyed with the runtime. Download artifacts **before** deleting the Cell or letting its lifetime expire. Uploads, downloads, stdin, and execution output are limited to 4 MiB per operation.

The macOS/Lima Python integration example is:

```bash
python3 examples/external-agent.py
```

The Go SDK lives in `pkg/client`. Use `client.NewLima("relayna-cell")` from macOS, or `client.New("/run/relayna-cell/api.sock")` from an authorized process on the Linux host or inside the VM. The external agent's actions outside the Cell are outside Cell's isolation boundary.

## Run a complete agent inside a Cell

```bash
./scripts/cell create -f examples/agent.json
./scripts/cell events CELL_ID
./scripts/cell inspect CELL_ID
```

`agent` mode starts the supplied command as the workload. Its child processes remain inside gVisor. Runtime resources are removed when the workload finishes; events remain queryable. `session` mode starts a small idle init process and accepts repeated API executions.

`examples/contained-agent.py` is the minimal protocol example used by the deterministic integration fixture. For a real local model and agent framework, use the OpenAI Agents SDK setup below. The inference service never executes the generated tool itself.

## Local Ollama and OpenAI Agents SDK

Run these commands **from the project directory on your Mac**, after `make install`. If your prompt is `jobz@lima-relayna-cell:~$`, run `exit` first to return to macOS.

```bash
make inference-up   # start Ollama + inference gateway; download Qwen3 4B Instruct
make agents-build   # build the uv-managed OpenAI Agents SDK example image
make model-smoke    # exercise both modes with real model-generated code
```

The model is approximately 2.5 GB. Inference runs on the VM's CPU in a shared Ollama container outside gVisor, capped at 2 CPUs and 4 GiB memory. Model files persist in a Docker volume. The official Ollama image includes additional runtime libraries, so its image download is larger than the model. No OpenAI API key is needed: the SDK uses its Chat Completions adapter with an explicit local endpoint, and OpenAI tracing and Ollama cloud features are disabled.

The first agent framework is **OpenAI Agents SDK for Python**, pinned to `0.24.0`. Its `shell` function either submits an execution through Cell's API or starts a child process when the entire agent is already inside Cell. The real-model test asks the model to implement `add(a, b)` in `/workspace/add.py`, run a test, and finish. Separate assertions check the generated function with several inputs. A response without an actual tool call fails the test.

The [SDK example](examples/agents-sdk/README.md) is a **uv-managed Python 3.13 project** with `pyproject.toml` and a committed `uv.lock`. For local development on Linux or macOS, [install uv](https://docs.astral.sh/uv/getting-started/installation/) and run:

```bash
cd examples/agents-sdk
uv sync --locked
uv run --locked agent.py --help
```

This checks the local SDK installation. Use `make model-smoke` from the repository root on macOS for the sandboxed agent test above. Container builds install the locked environment with uv; agents use that preinstalled environment without downloading dependencies at runtime. Building the image does not require uv on the host.

Only `/v1/chat/completions` is exposed by the inference gateway; model-management endpoints are blocked. No inference ports are published to macOS or the LAN. The test discovers the gateway's current private address and grants that exact endpoint to a contained agent. Delegated tool Cells remain offline. A small trusted external SDK runner has access to the local Cell API socket; the sandboxed workload never receives it.

The test cleans up its Cells and external SDK runner, retaining process/output audit history. A summary is stored inside the VM at `/var/lib/relayna-cell/model-smoke-latest.json`. In Grafana, filter by a Cell ID printed by the test. Contained SDK events appear as workload output; runtime process and network records retain their separate trusted provenance. Delegated SDK orchestration prints to the test console, while its executed commands and output are journaled by Cell.

Inspect the model from **macOS**:

```bash
limactl shell relayna-cell sudo docker compose -f /opt/relayna-cell/deploy/inference/compose.yaml ps
limactl shell relayna-cell sudo docker compose -f /opt/relayna-cell/deploy/inference/compose.yaml exec -T ollama ollama list
```

Inside the **Linux VM**, use the same `sudo docker compose ...` commands without the `limactl shell relayna-cell` prefix. To stop inference while keeping its downloaded model:

```bash
limactl shell relayna-cell sudo docker compose -f /opt/relayna-cell/deploy/inference/compose.yaml stop
```

See [the SDK agent](examples/agents-sdk/agent.py) and [inference deployment](deploy/inference/compose.yaml). This small model is intended for integration and simple coding experiments; passing this test does not establish general coding reliability.

## Security behavior

- gVisor is required. There is no fallback to `runc` for Cells. Trusted observability services use ordinary containers outside the sandbox.
- Workloads run as UID/GID 65532 with all Linux capabilities dropped and `no-new-privileges`. Their root filesystem is read-only; `/workspace` and `/tmp` have explicit size limits. Images declaring extra Docker volumes are rejected.
- A Cell without an allowlist has no external network. A Cell with approved destinations gets its own internal bridge. Host firewall rules block direct connections, other networks, and host services; only the egress proxy is reachable.
- The HTTP/CONNECT proxy matches exact host and port pairs, resolves and validates destinations, and dials the validated IP. Private destinations require `allow_private: true`; loopback, link-local, and multicast destinations remain blocked. HTTP redirects do not bypass destination checks.
- CPU, memory, process-count, file-size, workspace-size, output, and lifetime limits apply. The default lifetime is 300 seconds, including preparation. The maximum is one hour. At most eight Cells run concurrently.
- API executions are serialized within each Cell. Cancelling an execution, disconnecting its output stream, or exceeding its timeout stops the **whole Cell**, so detached descendants cannot continue unaudited.
- Streaming execution clients must keep reading responses. Each response write is limited to five seconds or the remaining execution deadline, whichever is shorter; cancellation and server shutdown interrupt blocked writes. Failure to deliver the final exit frame also stops the Cell.
- Process execution is audited, not synchronously approved. Shell built-ins and in-process language operations are not each represented as separate process executions. Network auditing covers proxy decisions and configured connection trace points; it is not a complete packet capture or HTTPS content inspection.
- Tracing is configured before startup. Detected trace drops, unexpected audit disconnects, workload log-stream failures, or journal write failures stop affected workloads. Output collection and process completion are monitored concurrently; an unexpected end of logs while a workload is active records an audit gap and stops that Cell. Detection is asynchronous and does not promise that no operation can occur before the stop.
- A server restart removes owned runtime resources and marks interrupted sessions. It does not resume workloads through a monitoring gap. Host administrators, the container engine, the VM kernel, and Cell services remain trusted.

## Loki and Alloy logging

The collector and control plane store structured events in SQLite and fsync JSONL export segments outside the sandbox. Alloy tails those segments and sends them to Loki. Grafana provides execution, process, network, and workload-output views.

- **Overview** ([Linux](http://127.0.0.1:3000/d/relayna-cell), [macOS](http://localhost:3300/d/relayna-cell)): summary cards, activity trends, execution and network outcomes, binary/destination rankings, workload modes, and recent Cell outcomes. Click a Cell in the outcome table to investigate it.
- **Investigation** ([Linux](http://127.0.0.1:3000/d/relayna-cell-audit), [macOS](http://localhost:3300/d/relayna-cell-audit)): a shared Cell ID and time filter, readable lifecycle/command/process/network logs, workload output, and an expandable raw-JSON section. Dashboard links preserve the filters and time range.

Totals count events received by Loki in the selected window. Headline and category totals deduplicate stable event IDs (requested Cells use Cell IDs); activity charts count received records per interval and can include replay duplicates. These are historical observations, not live-state or service-health gauges. Expected restriction-test failures are included. API execution counts include file-transfer helpers; process exec counts exclude initial container starts and clones. No CPU/memory utilization is collected yet.

Dashboard definitions are generated by `scripts/build-dashboards.py` and checked into `deploy/observability/grafana/dashboards/`. On macOS, run `make dashboards-up` to regenerate and deploy only the dashboards without restarting Cell or the logging stack. Grafana's file provider applies them automatically; refresh an already-open dashboard tab afterward.

- Workload text is nested inside a trusted envelope; it cannot supply audit provenance or indexed labels.
- Indexed labels are bounded: `service_name`, `environment`, and `source`. Cell, execution, and event IDs are structured metadata. Export filenames are removed from labels.
- Loki retains logs for seven days. SQLite is the authoritative journal; deleting a Cell never deletes its events.
- Export segments rotate at 8 MiB, with a 256 MiB journal-export budget. Filling it stops auditing and workload execution until an operator archives exports and restarts the service. SQLite also occupies disk space. This MVP has no automatic audit archive policy.
- Alloy persists file positions and retries Loki delivery. This is not exactly-once delivery, and an Alloy crash can require replay. Event IDs remain stable during replay. The experimental Alloy Loki WAL is not required.
- Environment arrays are excluded from process events. Command arguments and workload output can still contain secrets; applications should avoid putting credentials there.

To replay committed events after an ingestion failure, stop Cell, export, and restart it. This ends active workloads, but leaves stored audit records intact. These commands run on macOS; on the Linux installation, run the `sudo ...` portions directly on the Linux machine:

```bash
limactl shell relayna-cell sudo systemctl stop cell-server
limactl shell relayna-cell sudo python3 /opt/relayna-cell/scripts/replay-audit.py --after 0
limactl shell relayna-cell sudo systemctl start cell-server
```

Replay requires enough room within the export budget and may produce duplicate Loki entries. Do not delete the SQLite journal when rotating or archiving export files.

## Validate

For the Linux installation, use `sudo python3 /opt/relayna-cell/scripts/verify-linux-setup.py` on the Linux machine. The commands below are development checks from the macOS checkout; `make test` itself can run wherever the required Go toolchain is installed.

```bash
make test       # Go tests, race detector, and vet
make smoke      # real Linux VM, Docker, gVisor, Alloy, and Loki checks
make model-smoke # real Ollama + OpenAI Agents SDK checks (after model/image setup)
make dashboards-check # live panel queries and totals against raw Loki events
```

The integration test checks both execution modes, workspace transfer, read-only root filesystem, approved inference access, denied destinations, direct network bypass, resource limits, real SecCheck process events, retained history, and recovery after restarting Loki. It uses a deterministic local inference fixture, not a downloaded model. It cleans up the Cells it creates.

The runtime and protobuf schema are pinned to gVisor `release-20261102.0`. Runtime archives and the Ubuntu base image have checked digests. Grafana, Alloy, and Loki have explicit version tags in `deploy/observability/compose.yaml`; dependency versions are recorded in `go.mod` and `go.sum`.

Stop and resume commands for each platform are included in the [Linux](#4-stop-start-or-update) and [macOS](#5-stop-resume-or-update) setup sections above. Both preserve stored audit data.

## Project layout

`cmd/cell` is the CLI; `cmd/cell-server` hosts the local API, lifecycle controller, observer, and egress proxy; `cmd/cell-init` is the unprivileged workspace/session helper. Internal packages separate policy validation, runtime operations, auditing, observation, and networking. `pkg/cell` and `pkg/client` define the public API and Go client.

See [the architecture decisions](docs/architecture.md) for component boundaries and follow-up work. Kubernetes, multi-tenant authorization, distributed storage, long-lived reconnectable sessions, GPU passthrough, and program allowlisting are outside this release.
