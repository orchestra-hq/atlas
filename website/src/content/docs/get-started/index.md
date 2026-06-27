---
title: Get started
description: Install Atlas, serve an open model, and point Claude Code or an OpenAI client at it.
---

Atlas is one binary that scales from a laptop to a fleet. Every path ends the same way: an
`ANTHROPIC_BASE_URL` (and an OpenAI-compatible base URL) you point existing tools at.

## Two steps

1. **[Install Atlas](/atlas/get-started/installation/)** — Homebrew, a one-line script, or the
   container image.
2. **[Quickstart](/atlas/get-started/quickstart/)** — serve a model with `atlas up` and drive Claude
   Code against it.

## Which path fits you

| You have…                      | Engine    | Path                                                       |
| ------------------------------ | --------- | ---------------------------------------------------------- |
| A laptop / dev box (no GPU)    | llama.cpp | [Quickstart](/atlas/get-started/quickstart/)              |
| One rented cloud GPU           | vLLM      | [Deploy → single GPU box](/atlas/deploy/)                 |
| Several machines, one endpoint | either    | [Deploy → cloud fleet](/atlas/deploy/)                   |

A small model on a laptop is great for development, evals, and offline work, but drives Claude Code
only intermittently. For reliable agentic use, serve a capable model on a GPU.
