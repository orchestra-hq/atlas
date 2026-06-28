---
title: API keys
description: Mint, scope, and revoke the API keys clients present to Atlas.
sidebar:
  order: 1
---

Atlas authenticates every client request against keys stored in the state-dir database. On first
start it mints a default full-access admin key and prints it once. There is no shared-secret flag.

```sh
# a model-scoped client key, printed once
atlas keys create --allow qwen3-0.6b

# an admin-scoped key (for /admin/*, fleet commands, and the /metrics scrape)
atlas keys create --admin

# scripting: print only the secret
atlas keys create --quiet

atlas keys list
atlas keys revoke <key-id>
```

- Keys persist in the state-dir database, so they survive restarts (mount the volume when running in
  [Docker](/atlas/deploy/docker/)).
- A key can be scoped to specific models with `--allow`; requests for other models are rejected.
- Revoking a key takes effect on its next request.
- The `/admin/*` control surface requires a key carrying the **`admin`** scope — admin auth reuses the
  same key system rather than a separate token.

Clients present the key as `x-api-key` or `Authorization: Bearer` — see
[Use with Claude Code](/atlas/guides/claude-code/) and
[API compatibility](/atlas/reference/api-compatibility/).
