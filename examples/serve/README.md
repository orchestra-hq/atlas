# Serving Atlas on a cloud GPU

Two recipes for running Atlas on a GPU you rent and pointing your tools at it. Both end the same way — an `ANTHROPIC_BASE_URL` your Claude Code / SDKs talk to — they differ only in how the box gets created and reached.

| Recipe                                             | Extra tooling        | Best when                                                              |
| -------------------------------------------------- | -------------------- | ---------------------------------------------------------------------- |
| [SkyPilot one-command](#skypilot-one-command)      | `skypilot` + a cloud | you want the cheapest GPU across clouds picked for you, in one command |
| [Single box + SSH tunnel](#single-box--ssh-tunnel) | none                 | you already have a GPU box (any cloud, or your own) and want zero deps |

> Security: the gateway mints an API key on first start and requires it on every request — the endpoint is never open, even with the port exposed. The SkyPilot recipe opens port 8080 on the host, so still lock the security group; read the minted key from `sky logs` (or mint more with `atlas keys create`). The single-box recipe binds to localhost and reaches it over an SSH tunnel — nothing is exposed to the internet. For a public endpoint, terminate TLS with `atlas server --tls-acme-domain <name>` (Let's Encrypt); for a private fleet, `--tls-self-signed` prints a pin workers join with over `wss://` ([ADR-0009](../../docs/internal/decisions/0009-transport-security-tls-and-pinning.md)).

## SkyPilot one-command

[`atlas-serve.sky.yaml`](atlas-serve.sky.yaml) brings up a ~24GB spot/on-demand GPU, builds Atlas, and serves a capable model.

```bash
pip install 'skypilot[aws]'   # or [gcp], [azure], [kubernetes], …
sky check

sky launch -c atlas examples/serve/atlas-serve.sky.yaml -y

# Atlas mints and prints an API key on first start; read it from the logs:
sky logs atlas | grep 'ATLAS API KEY'

# Find your endpoint, then point a client at it:
sky status --endpoint 8080 atlas        # -> http://<ip>:8080
ANTHROPIC_BASE_URL=http://<ip>:8080 ANTHROPIC_API_KEY=<key from logs> claude

sky down atlas                          # tear it down when done
```

Override the model or its parser flags per launch, e.g. `--env ATLAS_MODEL=Qwen/Qwen3-14B`. SkyPilot is a removable convenience here — the only Atlas-specific line in the recipe is `atlas up`.

## Single box + SSH tunnel

No extra tooling — works on any GPU host (EC2, GCE, a box under your desk).

1. **Get a GPU box** and SSH in. Install Atlas one of three ways:
   - the static binary from [GitHub Releases](https://github.com/orchestra-hq/atlas/releases), or
   - the container image: `docker run --gpus all -p 127.0.0.1:8080:8080 -v atlas:/var/lib/atlas ghcr.io/orchestra-hq/atlas:cuda up --engine vllm --model Qwen/Qwen3-8B --addr 0.0.0.0:8080` (see [docs/docker.md](../../docs/docker.md)), or
   - build from source.

2. **Serve, bound to localhost** (so nothing is exposed):

   ```bash
   atlas up --engine vllm --model Qwen/Qwen3-8B \
     --alias claude-sonnet-4-6=Qwen/Qwen3-8B \
     --engine-arg --tool-call-parser --engine-arg hermes \
     --engine-arg --reasoning-parser --engine-arg qwen3 \
     --addr 127.0.0.1:8080
   ```

   Atlas prints a default API key on first start (or mint one with `atlas keys create`); save it for the next step.

3. **Tunnel from your laptop** and point Claude Code at it:

   ```bash
   ssh -N -L 8080:localhost:8080 user@your-gpu-box &
   ANTHROPIC_BASE_URL=http://localhost:8080 ANTHROPIC_API_KEY="$KEY" claude
   ```

The tunnel makes the remote gateway look local, encrypted over SSH, with no inbound port open on the box.

## See also

- [docs/usage-scenarios.md](../../docs/usage-scenarios.md) — which path fits which situation (laptop / single cloud GPU / fleet)
- [docs/docker.md](../../docs/docker.md) — the container images
- [examples/acceptance/](../acceptance/README.md) — the GPU acceptance run (proving, not serving)
