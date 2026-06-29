---
title: CLI
description: The atlas command-line interface — serving, the fleet, models, keys, and usage.
---

`atlas` is one binary; the role is chosen by subcommand. Run `atlas <command> --help` for full flags.

## Serving (single node)

| Command                      | What it does                                                              |
| ---------------------------- | ------------------------------------------------------------------------ |
| `atlas up`                   | Resolve a model, provision the engine, and serve the gateway locally     |
| `atlas run <model> [prompt]` | Run a single prompt one-shot (no long-running daemon)                     |
| `atlas ps`                   | Show what is running locally                                              |
| `atlas version`              | Print the version                                                        |

## Fleet (control plane + workers)

| Command                          | What it does                                                       |
| -------------------------------- | ----------------------------------------------------------------- |
| `atlas server`                              | Run the control plane only (workers dial out to it)               |
| `atlas worker --join <url> --token <token>` | Run a worker that dials out to a server and serves models         |
| `atlas workers list`                        | List workers connected to the server                              |
| `atlas workers remove <worker-id>`          | Gracefully drain and disconnect a worker                          |

## Models

| Command                              | What it does                                              |
| ------------------------------------ | -------------------------------------------------------- |
| `atlas inspect <model>`              | Preview a model's derived serving plan + verdict (no download) |
| `atlas pull [model...]`              | Download model weights into the store                    |
| `atlas deploy <model>`               | Deploy a model instance onto the fleet                   |
| `atlas scale <model> --replicas N`   | Scale a deployed model to N replicas                     |
| `atlas stop <model>`                 | Stop a deployed model                                    |
| `atlas deployments`                  | List deployments                                         |

## Keys, usage & audit

| Command                          | What it does                                                       |
| -------------------------------- | ----------------------------------------------------------------- |
| `atlas keys create`              | Mint an API key (model-scoped with `--allow`, admin-scoped with `--admin`) |
| `atlas keys list`                | List keys                                                         |
| `atlas keys revoke <key-id>`     | Revoke a key                                                      |
| `atlas usage`                    | Summarize the usage ledger (`--json` for machine-readable)        |
| `atlas audit`                    | List control-plane mutations                                      |

## Observability

| Command         | What it does                                       |
| --------------- | ------------------------------------------------- |
| `atlas status`  | Snapshot of the fleet                             |
| `atlas top`     | Live view of the fleet (run over SSH on the gateway) |

## Runtime

| Command                     | What it does                                              |
| --------------------------- | -------------------------------------------------------- |
| `atlas runtime provision`   | Provision an engine runtime                              |
| `atlas runtime upgrade`     | Upgrade/pin an engine runtime version                    |
| `atlas runtime list`        | Show each engine's pinned and provisioned versions      |

See [Operate](/atlas/operate/) for keys, usage, observability, and TLS in depth.
