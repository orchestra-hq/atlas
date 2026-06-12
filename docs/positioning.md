# Positioning & marketing tracker

Living doc. Every differentiator we plan to market, with the proof it requires — a claim without a demo/benchmark/doc behind it doesn't ship. Add angles here as they surface; promote them to website/launch copy when their proof exists.

## Core narrative

**"Point your agents at your own hardware."** Everyone else markets "serve models"; we market what you *do* with the endpoint. The hero demo is always an agent (Claude Code / Agent SDK) completing real work against the user's own machines.

## Differentiator inventory

| # | Angle | The claim | Proof required | Status |
|---|---|---|---|---|
| 1 | **Agent-first compatibility** | Change `ANTHROPIC_BASE_URL`, your Claude Agent SDK app runs on your hardware. Drop-in is a release gate, not a feature. | Published conformance matrix run with real Anthropic/OpenAI SDKs + Claude Code smoke test in CI | Planned (M0) |
| 2 | **Spot-friendly GPU fleets** | Workers are disposable: spot interruption = heartbeat timeout = automatic re-placement. Run your inference fleet at spot prices (often 60–90% off on-demand GPU). Nobody else leads with this. | Demo: kill a spot worker mid-stream, fleet recovers; cost comparison table on-demand vs spot | Planned (M1 drain/heartbeat) |
| 3 | **Zero-inbound-ports security story** | GPU workers need no inbound rules, no public IPs, no SSH to operate. Security review takes minutes. | deployment-aws.md topology; security one-pager | Drafted ([deployment-aws.md](deployment-aws.md)) |
| 4 | **Hybrid for free** | Control plane in one cloud, workers anywhere — on-prem DGX + AWS + a Mac Studio behind home NAT, one endpoint. Consequence of outbound-dial design. | Demo video: three-network fleet | Planned (M1) |
| 5 | **Minutes-to-first-token** | `curl \| sh` → working agent endpoint in <10 minutes. One static Go binary, no Python on the host. | Timed, scripted install demo; compare against GPUStack/DIY vLLM setup time | Planned (M0) |
| 6 | **~100-line Terraform** | Your whole AWS deployment is ~100 lines, because the product absorbs the hard parts. The reference module *is* the ad. | `examples/aws-terraform/` kept under the line-count bar | Planned (M2) |
| 7 | **Curated agent-capable model catalog** | We test which open models actually do tool-calling well and ship working configs. "Works for agents" badge earned by suite, not vibes. | Published per-model test results; catalog CI | Planned (M0 seed, ongoing) |
| 8 | **Data sovereignty** | Prompts, outputs, and weights never leave hardware you control. The compliance unlock for regulated teams. | Architecture doc + data-flow diagram (no third-party calls in request path) | Drafted (architecture.md) |
| 9 | **Honest API scope** | We document exactly what we don't emulate (server-side tools, batches, thinking semantics) instead of half-faking. Trust as a feature. | api-surface.md out-of-scope table, mirrored in public docs | Drafted ([api-surface.md](api-surface.md)) |
| 10 | **No Kubernetes mandate** | Great on bare metal and plain VMs; K8s is packaging, not architecture. Positions against llm-d/KubeAI complexity. | Bare-metal + VM quickstarts | Planned (M2) |

## Channels & assets (build as milestones land)

- Each milestone ships a **demo video** + polished guide (roadmap standing track).
- **Recipes** as SEO surface: "Run Claude Code on \<model\> with Atlas", "Claude Agent SDK on your own GPU", "Spot GPU inference fleet on AWS" — one page each, kept current.
- **Compat matrix page** — refreshed by CI, linkable proof for angle #1.
- Launch sequencing, name, and license land together (see open-questions).

## Competitive one-liners (internal, keep honest)

- vs **Ollama**: "Ollama for your whole fleet — and your agents' API."
- vs **GPUStack**: same architecture, but single-binary install, Anthropic-native API, agent-first catalog.
- vs **LiteLLM**: they route to other people's clouds; we run the models on yours.
- vs **llm-d/KubeAI**: no Kubernetes required, minutes not sprints.
