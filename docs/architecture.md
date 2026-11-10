# Relayna Cell architecture decisions

The [interactive HTML architecture guide](architecture.html) illustrates these decisions with component, flow, and sequence diagrams. It describes the implemented MVP and labels future work separately.

## Accepted product scope

The MVP supports external agents delegating shell/code operations and complete agents running inside a Cell. It audits process execution while enforcing network, filesystem, and resource restrictions. Local inference is shared and remains outside gVisor. The first deployment is an ARM64 Linux VM on macOS with Loki and Grafana Alloy included from the beginning.

A Cell owns the isolated environment and its lifetime. An API execution owns a submitted command, output stream, and exit status. Multiple executions share a Cell's workspace. Runtime process events carry container and process identities; an execution ID is attached only where the API actually knows it. We do not claim perfect causal attribution between arbitrary agent child processes and API calls.

## Implementation boundaries

| Component | Responsibility | Authority |
| --- | --- | --- |
| Mac CLI and SDK | Submit requests and read results through Lima SSH | Trusted local operator |
| Cell server | Admission, lifecycle, SQLite journal, workspace transfer | Root in the dedicated VM; owns Docker access |
| Docker adapter | Translate the Cell spec into a restricted runtime profile | Container and network administration |
| gVisor observer | Decode bounded SecCheck packets and report execution evidence | Root-only Unix socket outside workloads |
| Egress proxy | Enforce exact approved destinations and record decisions | Outbound connections from the VM |
| Workload and cell-init | Execute tools and access the bounded workspace | UID 65532 inside gVisor |
| Alloy | Tail trusted event envelopes and ship them to Loki | Read-only log mount; no Docker socket |
| Loki and Grafana | Store, query, and display log projections | Dedicated management network and persistent volumes |

The first deployment combines the API, node operations, observer, and proxy in one Go server process. Internal interfaces separate these responsibilities. This is a single-operator development deployment; separating privileged node operations from a remotely accessible control plane is required before adding remote tenants.

## Enforcement and failure semantics

Admission rejects unsupported configuration instead of passing arbitrary Docker options through the API. Workload specifications cannot select a different runtime, add host mounts, grant capabilities, expose ports, or set privileged mode. Image-declared volumes are rejected.

Offline Cells use `network=none`. Network-enabled Cells use a dedicated internal Docker bridge whose host interface begins with `rc`. Host INPUT and forwarding rules restrict this interface to the egress proxy. IPv6 forwarding and host access are denied. Proxy environment variables support ordinary HTTP clients, while kernel rules enforce the boundary even when a workload ignores those variables.

Destination grants are immutable for a Cell lifetime. A private inference address must be explicitly approved. CONNECT authorizes a host/port connection; it does not inspect TLS payloads or restrict methods within that tunnel. Applications needing protocols other than HTTP or CONNECT are currently unsupported.

CPU and memory limits are applied through Docker/cgroups. The `pids` setting becomes a workload `RLIMIT_NPROC`; the host cgroup gets that limit plus 128 for gVisor runtime threads and helpers. Guest tasks and host threads are different resources, and applying a small guest limit directly to the host can prevent the sandbox from starting. Workspace/tmpfs, file-size, output, and lifetime limits constrain additional resource use. Limit behavior is checked against the pinned runtime. See [gVisor's resource model](https://gvisor.dev/docs/architecture_guide/resources/).

Tracing starts before the initial workload. The remote SecCheck stream is asynchronous. Dropped events and disconnected observers make auditing incomplete and trigger workload shutdown; they are not synchronous execution-denial decisions. Environment arrays are stripped at the receiver. Unknown trace message types are ignored for protocol compatibility; the configured trace profile is explicitly pinned and tested.

At process exit or deadline, the controller drains available output, records the result, revokes network access, and removes runtime resources. Cancellation of an API execution stops the whole Cell. Before accepting work after a server restart, reconciliation removes all resources with the Cell ownership label. Durable audit records survive cleanup.

## Audit and observability

An event has a stable event ID, monotonic journal sequence, timestamp, Cell ID, optional execution ID, source, kind, and payload. The source is assigned by the trusted producer, never extracted from workload text. SQLite commits precede JSONL export, so export failure leaves a recoverable journal entry and stops further admission.

Alloy reads exported segments, parses only the outer envelope, preserves event IDs as structured metadata, removes filename labels, and writes to Loki. Seven-day Loki retention is independent of Cell lifetime. Local journal exports have a fixed capacity; archival is operator-managed for the MVP. File positions and retries improve recovery but do not establish exactly-once ingestion. The replay utility exports the original records when Cell is stopped.

Grafana has an overview with summary counts, activity trends, outcome distributions, binary/destination rankings, and a Cell outcome table. Its Cell links open an investigation dashboard with readable lifecycle, API command, process, network, and output records plus expandable raw JSON. Both dashboards share a Cell ID regex filter and time range. Whole-window totals deduplicate event IDs (Cell IDs for requested-Cell counts); interval charts count received records. Loki coverage, replay, and retention can differ from the authoritative SQLite journal. The dashboards do not infer live state or CPU/memory utilization from audit events. Service lifecycle and failure records use the same journal. Infrastructure diagnostics remain in their bounded Docker/systemd logs.

## Follow-up work

- Split privileged host operations from the control plane before remote or multi-tenant use.
- Add capability discovery, per-principal authorization, and a production secret-broker design.
- Add automatic audit archival, retention, ingestion-lag/drop alerting, and recovery tooling.
- Extend workload compatibility and benchmark monitoring overhead against representative agents.
- Add persistent workspace options and artifact export on exit with explicit quotas.
- Implement Kubernetes as a backend, evaluating SIG Agent Sandbox for provisioning.

The deterministic integration fixture verifies enforcement and protocol routing without a model download. A separate `make model-smoke` uses a local Ollama container, Qwen3 4B Instruct, and OpenAI Agents SDK for Python to test both execution modes with generated code and independent assertions. The framework integration remains outside the Go core: Cell does not depend on any agent framework.

Ollama is shared infrastructure outside gVisor, with a persistent model volume and bounded CPU/memory. A separate gateway exposes only Chat Completions, so granting inference does not also grant Ollama's model-management APIs. The deployment publishes no host ports. The SDK's external runner is trusted and receives the local management socket; contained SDK agents have only the inference endpoint grant. OpenAI trace export and Ollama cloud features are disabled for this local test. Framework events are workload claims; SecCheck events remain the independent process evidence.

## References

- [Latest shared architecture](https://chatgpt.com/s/t_6aeefd2ca92c8191aaccc474c88ae45b)
- [Earlier inference and sandbox decision](https://chatgpt.com/s/t_6aef3be37bf88191941398ca56522766)
- [gVisor runtime monitoring](https://gvisor.dev/docs/user_guide/runtimemonitor/)
- [Pinned SecCheck protocol](https://github.com/google/gvisor/tree/release-20261102.0/pkg/sentry/seccheck/sinks/remote)
- [Grafana Alloy file collection](https://grafana.com/docs/alloy/latest/reference/components/loki/loki.source.file/)
- [Loki structured metadata](https://grafana.com/docs/loki/latest/get-started/labels/structured-metadata/)
- [OpenAI Agents SDK model providers](https://developers.openai.com/api/docs/guides/agents/models)
- [Ollama OpenAI-compatible API](https://docs.ollama.com/api/openai-compatibility)
- [Ollama CPU container deployment](https://docs.ollama.com/docker)
