# OpenAI Agents SDK example

This Python 3.13 [uv project](https://docs.astral.sh/uv/guides/projects/) demonstrates two execution modes with a local model:

- **Delegated:** the agent runs in a trusted runner and submits shell tools through Cell's API. Generated code executes inside a Cell.
- **Contained:** the complete agent, including its shell tools, runs inside a Cell. Only the approved inference endpoint is reachable.

`pyproject.toml` declares the direct dependencies, `uv.lock` pins the complete dependency graph, and `.python-version` selects Python 3.13. The example is a script project; it does not install a Python package.

## Local development (Linux or macOS)

[Install uv](https://docs.astral.sh/uv/getting-started/installation/) first. On macOS, you can use `brew install uv`. From the repository root:

```bash
cd examples/agents-sdk
uv sync --locked
uv run --locked agent.py --help
```

uv creates a local `.venv` and downloads Python 3.13 if needed. No manual environment activation or `pip install` is required. `.venv` is excluded from Git, container builds, and Lima uploads.

The help command checks that the SDK imports successfully; it does not run a model. Agent execution requires a running Cell service and a compatible inference endpoint. `--mode contained` must run inside Cell; the script checks for the Cell init file and sandbox UID. The delegated runner requires authorized access to Cell's Linux Unix socket. Its actions outside Cell remain trusted host activity. See the [Linux setup guide](../../docs/linux-setup.md) or the [macOS walkthrough](../../README.md#start-on-macos) to prepare the services.

After changing dependencies, commit both `pyproject.toml` and `uv.lock`:

```bash
uv add PACKAGE
uv lock --check
uv run --locked agent.py --help
```

## Container build

From the repository root on a Linux host with Docker:

```bash
docker build -t relayna-cell/agents-sdk:dev examples/agents-sdk
docker run --rm --network none --read-only --user 65532:65532 \
  relayna-cell/agents-sdk:dev
```

Use `podman` in place of `docker` when that is your configured engine. This command runs the CLI help as an offline, unprivileged startup check. Use the Cell API to execute an agent with the sandbox's network, filesystem, resource, and audit controls.

The multi-stage Dockerfile uses a pinned uv image to install locked dependencies into `/opt/venv`. The final image runs `python /app/agent.py` directly with the virtual environment on `PATH`; it does not include uv or resolve dependencies at runtime. Host uv is not required to build the container. See [uv's Docker integration](https://docs.astral.sh/uv/guides/integration/docker/) for the underlying build pattern.

## Test both modes with the downloaded local model

From the repository root on **macOS**, with the existing `relayna-cell` Lima development VM:

```bash
make inference-up   # start Ollama/gateway; reuse the downloaded Qwen3 model
make agents-build   # build the uv-managed image inside Linux
make model-smoke    # run delegated and contained SDK agents
```

The test requires real model tool calls, independently checks the generated code, verifies gVisor process events and Loki delivery, and cleans up its Cells. The contained run also checks an allowed inference connection. OpenAI tracing is disabled and no OpenAI API key is needed. These Make targets use the Lima/Ollama development stack; the Linux installer documents its engine and inference choices separately.
