# Architecture image source prompt

Generated with the built-in image generation tool. The reference informed the grouped layout and icon style; the component and flow definitions come from this repository.

The final image also received a routing pass to keep the inference connections outside component cards and the audit connector clear of the section heading. See the refinement prompt below.

```text
Use case: infographic-diagram.
Asset type: a high-resolution PNG architecture illustration for the first page of the Relayna Cell GitHub README.

Create a precise, polished engineering architecture diagram, inspired by the readability of professional cloud solution diagrams: white canvas, clearly labeled nested pastel zones, white component tiles with recognizable flat service icons, thin orthogonal connectors, arrowheads, a compact flow-color legend, generous spacing, and sharp dark sans-serif text. It must look like a carefully arranged cloud architecture drawing, not a Mermaid flowchart, not a marketing poster, not a UI mockup. No gradients, 3D, glow, photography, decorative waves, or background patterns. Use a wide approximately 3:2 canvas. All labels should be large enough for a full-width GitHub README; do not add tiny explanatory paragraphs. Keep all text inside its assigned card and route every connector through empty gutters, never through text.

Title at upper left: "Relayna Cell".
Subtitle: "Logical architecture · Platform-neutral access".
Legend: blue "Execution", teal "Network / inference", purple dashed "Audit events".

STRUCTURE:
Left narrow column, pale gray group headed "Clients & integrations". A clear shared platform line reads "Linux · macOS · Windows" with equal visual weight for all three. Two stacked white cards:
- "Operators" with user/terminal icon; second line "CLI · SDK · SSH access".
- "External agent" with agent icon; second line "OpenAI Agents SDK"; small third line "Delegate tool execution".

Center broad pale blue group headed "Relayna Cell". Within it, two side-by-side panels:
1. Pale blue-white panel headed "Trusted control plane", second line "cell-server". Contains three clearly separated white tiles:
  - "Cell API & lifecycle" / "Create · exec · files · stop" with API/controller icon.
  - "Egress policy & proxy" / "Exact destination grants" with gateway/shield icon.
  - "SecCheck observer" / "Process & connection evidence" with eye/radar icon.
2. Pale warm ivory panel headed "Isolated execution". A slim top tile "Docker / Podman" / "Runtime adapter" with container icon, connected downward to a larger mint-tinted boundary labeled "gVisor sandbox". Within this sandbox two white mode cards side by side:
  - "Tool session" / "Shell & code execution" with terminal icon.
  - "Contained agent" / "Agent loop + tools" with agent icon.
  Under those cards inside the sandbox, a white storage tile "Bounded workspace" / "Shared within one Cell" with folder icon.
  A compact readable restrictions strip inside sandbox bottom: "Read-only root · CPU / memory / PID limits".
  A small red blocked badge under the sandbox: "Direct internet egress blocked".
  The Docker / Podman runtime tile is OUTSIDE the gVisor boundary. Both execution modes and workspace are INSIDE that boundary. The sandbox is untrusted; the control plane is trusted.

Right column, pale teal group headed "Shared inference", second line "Outside the sandbox". Contains:
- white "Inference gateway" card / "Chat Completions only" with gateway icon.
- below it a white "Local model engine" card with brain/server icon, and four clear equal-sized engine names in a 2-by-2 arrangement: "Ollama", "llama.cpp", "vLLM", "SGLang".
- small note "Model management blocked".
The model engine is never inside the gVisor sandbox.

Bottom wide pale lavender group headed "Audit & observability", spanning the central and right columns. Inside it a clean LEFT-TO-RIGHT chain of five white component cards, each with its own simple recognizable icon:
"SQLite journal" / "Source of truth"
"JSONL export" / "Trusted event envelopes"
"Grafana Alloy" / "Collect & forward"
"Loki" / "Searchable logs"
"Grafana" / "Charts & investigations".
Connect these five cards sequentially with purple arrows.
No other database, no Kubernetes, no cloud-vendor products.

REQUIRED CONNECTIONS:
Blue arrows: Operators -> Cell API & lifecycle; External agent -> Cell API & lifecycle; Cell API & lifecycle -> Docker / Podman -> gVisor sandbox. Label the main execution arrow "create / execute".
Teal arrows: gVisor sandbox -> Egress policy & proxy, label "network requests"; Egress policy & proxy -> Inference gateway, label "approved destinations"; Inference gateway -> Local model engine. Route the longer proxy-to-gateway connector below the central panels, above the audit group, with room for its label. A separate thin teal connection from External agent to Inference gateway may travel along an outside top gutter, labeled "model requests". The sandbox must NEVER connect directly to the model engine.
Purple dashed arrow: gVisor sandbox -> SecCheck observer, label "runtime evidence".
Purple arrow: trusted control plane -> SQLite journal, label "events + captured output".
Maintain obvious arrow direction and visible endpoints. Prefer fewer clean connections over decorative extra lines; include no unlabeled dangling lines.

Footer in readable small type, outside all groups:
"Deployment detail: gVisor execution currently requires a Linux host or Linux VM."
This is the ONLY platform-specific deployment footnote. Do not title the architecture as Mac, Apple Silicon, Lima, Linux VM, Windows runtime, or a particular cloud. Do not imply that gVisor runs natively on macOS or Windows. Do not claim Windows has a tested installer. Do not show remote TCP management as implemented.

Accuracy: spell "Relayna", "gVisor", "SecCheck", "llama.cpp", "SGLang", "SQLite", "Grafana Alloy", and "Loki" exactly. Preserve these component roles and trust boundaries. No repeated section titles, no duplicate component cards, no invented services. White background, no transparency, no watermark.
```

## Refinement prompt

```text
Refine this Relayna Cell architecture image. Keep its exact components, spelling, icons, relative panel layout, platform-neutral framing, footer, and all architectural relationships. Make only the following routing and fill refinements.

1. Reroute the teal "model requests" connection so it visibly originates at the LEFT EDGE of the "External agent" card, travels up the empty far-left margin beside (not through) the Operators card and Clients heading, then crosses the empty top gutter. At the right, take it down the empty right margin of Shared inference and turn toward the TOP EDGE of the Inference gateway card in the gap beneath the section subtitle. It must not pass through the "Shared inference" heading or through any component card. Preserve the label "model requests" on the top horizontal segment and the arrow pointing into the gateway. Remove the old vertical teal line behind the Operators card.

2. The teal "approved destinations" route must start at the RIGHT EDGE of "Egress policy & proxy", slightly below the incoming "network requests" arrow. Route it down the empty gutter immediately to the right of the control-plane cards, then across the clear gutter above Audit & observability, then up into the LEFT EDGE of Inference gateway. Remove the old teal segment that runs behind the SecCheck observer. This edge belongs to the Egress policy & proxy, NEVER to the observer. Keep the purple runtime-evidence connector separate. When differently colored lines must cross in a gutter, do not add a junction dot.

3. Move the "Audit & observability" group heading horizontally to the center of its lavender band so the vertical purple arrow into SQLite no longer crosses its heading. Keep the five audit cards and their arrows exactly in their current order.

4. Replace the visible background gradients with very light FLAT pastel fills (pale blue control area, pale ivory execution area, pale mint gVisor area, pale teal inference area, pale lavender observability, pale gray clients). White component cards and white outer background. This is an engineering diagram, not a glossy illustration.

Do not add or remove nodes. Keep all text, including Linux · macOS · Windows and the Linux-runtime footnote, unchanged. Maintain the crisp legibility of the current type. Output the whole finished diagram, not a cropped region.
```
