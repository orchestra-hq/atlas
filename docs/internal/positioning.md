# Positioning & marketing tracker

Living doc. Every differentiator we plan to market, with the proof it requires — a claim without a demo/benchmark/doc behind it doesn't ship. Add angles here as they surface; promote them to website/launch copy when their proof exists.

## Core narrative

**"Point your agents at your own hardware."** Everyone else markets "serve models"; we market what you _do_ with the endpoint. The hero demo is always an agent (Claude Code / Agent SDK) completing real work against the user's own machines.

## Differentiator inventory

| #   | Angle                                   | The claim                                                                                                                                                                                                                                             | Proof required                                                                                  | Status                                                             |
| --- | --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- | ------------------------------------------------------------------ |
| 1   | **Agent-first compatibility**           | Change `ANTHROPIC_BASE_URL`, your Claude Agent SDK app runs on your hardware. Drop-in is a release gate, not a feature.                                                                                                                               | Published conformance matrix run with real Anthropic/OpenAI SDKs + Claude Code smoke test in CI | Shipped — G1–G22 green per-PR + nightly (m0–m3 acceptance)         |
| 2   | **Spot-friendly GPU fleets**            | Workers are disposable: spot interruption = heartbeat timeout = automatic re-placement. Run your inference fleet at spot prices (often 60–90% off on-demand GPU). Nobody else leads with this.                                                        | Demo: kill a spot worker mid-stream, fleet recovers; cost comparison table on-demand vs spot    | Mechanism shipped (M1); spot demo + cost table pending             |
| 3   | **Zero-inbound-ports security story**   | GPU workers need no inbound rules, no public IPs, no SSH to operate. Security review takes minutes.                                                                                                                                                   | deployment-aws.md topology; security one-pager                                                  | Drafted ([deployment-aws.md](deployment-aws.md))                   |
| 4   | **Hybrid for free**                     | Control plane in one cloud, workers anywhere — on-prem DGX + AWS + a Mac Studio behind home NAT, one endpoint. Consequence of outbound-dial design.                                                                                                   | Demo video: three-network fleet                                                                 | Mechanism shipped (M1 cross-network fleet); demo video pending     |
| 5   | **Minutes-to-first-token**              | `curl \| sh` → working agent endpoint in <10 minutes. One static Go binary, no Python on the host.                                                                                                                                                    | Timed, scripted install demo; compare against GPUStack/DIY vLLM setup time                      | Shipped (M4 install.sh + Homebrew tap); timed demo pending         |
| 6   | **~100-line Terraform**                 | Your whole AWS deployment is ~100 lines, because the product absorbs the hard parts. The reference module _is_ the ad.                                                                                                                                | `examples/aws-terraform/` kept under the line-count bar                                         | Deferred to M7 (reference IaC not yet built)                       |
| 7   | **Curated agent-capable model catalog** | We test which open models actually do tool-calling well and ship working configs. "Works for agents" badge earned by suite, not vibes.                                                                                                                | Published per-model test results; catalog CI                                                    | Seeded (M0 starter catalog); ongoing                               |
| 8   | **Data sovereignty**                    | Prompts, outputs, and weights never leave hardware you control. The compliance unlock for regulated teams.                                                                                                                                            | Architecture doc + data-flow diagram (no third-party calls in request path)                     | Drafted (architecture.md)                                          |
| 9   | **Honest API scope**                    | We document exactly what we don't emulate (server-side tools, batches, prompt caching) instead of half-faking — and where we do support a hard feature (thinking blocks via real reasoning models, ADR-0005), we say exactly how. Trust as a feature. | api-surface.md out-of-scope table, mirrored in public docs                                      | Shipped — published on the docs site (reference/api-compatibility) |
| 10  | **No Kubernetes mandate**               | Great on bare metal and plain VMs; K8s is packaging, not architecture. Positions against llm-d/KubeAI complexity.                                                                                                                                     | Bare-metal + VM quickstarts                                                                     | Deferred to M7 (bare-metal/VM quickstarts); no-K8s stance holds    |

## Channels & assets (build as milestones land)

- Each milestone ships a **demo video** + polished guide (roadmap standing track).
- **Recipes** as SEO surface: "Run Claude Code on \<model\> with Atlas", "Claude Agent SDK on your own GPU", "Spot GPU inference fleet on AWS" — one page each, kept current.
- **Compat matrix page** — refreshed by CI, linkable proof for angle #1.
- Name (**Atlas**, decided 2026-06-12) and license (**Apache 2.0**, see `LICENSE`) are settled; launch sequencing gets decided as M0 nears completion.

## Competitive one-liners (internal, keep honest)

- vs **Ollama**: "Ollama for your whole fleet — and your agents' API."
- vs **GPUStack**: same architecture, but single-binary install, Anthropic-native API, agent-first catalog.
- vs **LiteLLM**: they route to other people's clouds; we run the models on yours.
- vs **llm-d/KubeAI**: no Kubernetes required, minutes not sprints.
