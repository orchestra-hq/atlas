#!/usr/bin/env python3
"""capability_matrix.py — aggregate per-run conformance matrices into the
published "works for agents" capability matrix (M2 phase 4c, G18).

run.py emits one matrix.json per (engine, model) run: the structural conformance
result for that single target. A fleet's worth of those — every catalog model on
every engine that can serve it — is the raw material for the capability matrix
this tool produces: one row per (model, engine) with a single agent-readiness
verdict plus the per-group detail behind it. That verdict is the badge promised
in roadmap.md — earned by the suite, not by vibes.

The verdict turns on the *agent-critical* groups (tool use and the streamed
multi-call agent loop): a model an SDK agent can actually drive. The other
compat groups (basic messages, streaming wire, models/aliases, count_tokens,
errors, the OpenAI surface, ops) refine "ready" vs "partial" but do not by
themselves sink agent-readiness.

Input is one or more matrix.json files (run.py's --output). Output is a
machine-readable capability-matrix.json and an optional human-readable Markdown
table. Stdlib only, so it runs with no conformance deps installed.

Exit codes:
  0  aggregated; if --require-ready is set, every cell is ready
  1  --require-ready set and at least one cell is not ready
  2  usage / input error
"""

from __future__ import annotations

import argparse
import datetime
import json
import sys
from pathlib import Path

SCHEMA_VERSION = 1

# Groups whose pass is necessary for an agent to drive the model: G3 is the
# tool-use loop (request -> tool_use -> tool_result -> answer), G9 the streamed
# >=3-call agent-SDK loop. A failure in either means "an agent cannot rely on
# this model×engine", regardless of how the basic-compat groups did.
AGENT_CRITICAL = ("G3", "G9")

# The full structural-compat set run.py reports. A non-critical failure here
# downgrades a cell to "partial" rather than sinking it.
CORE_GROUPS = tuple(f"G{i}" for i in range(1, 11))

# Verdicts, worst to best. ready: agent-critical pass and no core group failed.
# partial: agent-critical pass but some non-critical group failed. incomplete:
# an agent-critical group was not exercised (skipped/absent) — the run cannot
# vouch for it. unsupported: an agent-critical group failed.
VERDICT_READY = "ready"
VERDICT_PARTIAL = "partial"
VERDICT_INCOMPLETE = "incomplete"
VERDICT_UNSUPPORTED = "unsupported"


class MatrixError(Exception):
    """A bad or unreadable input matrix."""


def load_matrix(path: Path) -> dict:
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as err:
        raise MatrixError(f"{path}: {err}") from err
    target = data.get("target")
    groups = data.get("summary", {}).get("groups")
    if not isinstance(target, dict) or not isinstance(groups, dict):
        raise MatrixError(f"{path}: not a run.py matrix (missing target/summary.groups)")
    if not target.get("engine") or not target.get("model"):
        raise MatrixError(f"{path}: target is missing engine/model")
    return data


def group_status(groups: dict, name: str) -> str:
    """The status run.py recorded for a group, or 'absent' if it has no entry."""
    cell = groups.get(name)
    if not isinstance(cell, dict):
        return "absent"
    return cell.get("status", "absent")


def verdict_for(groups: dict) -> tuple[str, list[str]]:
    """Classify one run's groups into an agent-readiness verdict, plus the list
    of groups responsible for anything short of ready."""
    critical = {g: group_status(groups, g) for g in AGENT_CRITICAL}
    if any(s == "fail" for s in critical.values()):
        return VERDICT_UNSUPPORTED, sorted(g for g, s in critical.items() if s == "fail")
    if any(s not in ("pass",) for s in critical.values()):
        # A critical group that neither passed nor failed (skipped/absent): the
        # run did not exercise it, so it cannot be vouched for.
        return VERDICT_INCOMPLETE, sorted(g for g, s in critical.items() if s != "pass")
    failed_core = sorted(g for g in CORE_GROUPS if group_status(groups, g) == "fail")
    if failed_core:
        return VERDICT_PARTIAL, failed_core
    return VERDICT_READY, []


def aggregate(paths: list[Path]) -> dict:
    cells = []
    seen: dict[tuple[str, str], Path] = {}
    for path in paths:
        data = load_matrix(path)
        target = data["target"]
        key = (target["model"], target["engine"])
        if key in seen:
            raise MatrixError(
                f"{path}: duplicate (model={key[0]}, engine={key[1]}) — already from {seen[key]}"
            )
        seen[key] = path
        groups = data["summary"]["groups"]
        verdict, responsible = verdict_for(groups)
        cells.append(
            {
                "model": target["model"],
                "engine": target["engine"],
                "verdict": verdict,
                "responsible_groups": responsible,
                "groups": {g: group_status(groups, g) for g in CORE_GROUPS},
                "source_generated_at": data.get("generated_at"),
                "source": str(path),
            }
        )
    cells.sort(key=lambda c: (c["model"], c["engine"]))
    return {
        "schema_version": SCHEMA_VERSION,
        "generated_at": datetime.datetime.now(datetime.UTC).isoformat(timespec="seconds"),
        "agent_critical": list(AGENT_CRITICAL),
        "core_groups": list(CORE_GROUPS),
        "summary": {
            "cells": len(cells),
            "ready": sum(c["verdict"] == VERDICT_READY for c in cells),
            "partial": sum(c["verdict"] == VERDICT_PARTIAL for c in cells),
            "incomplete": sum(c["verdict"] == VERDICT_INCOMPLETE for c in cells),
            "unsupported": sum(c["verdict"] == VERDICT_UNSUPPORTED for c in cells),
        },
        "cells": cells,
    }


_STATUS_GLYPH = {"pass": "✅", "fail": "❌", "skip": "➖", "absent": "·", "missing": "❌"}
_VERDICT_GLYPH = {
    VERDICT_READY: "✅ ready",
    VERDICT_PARTIAL: "🟡 partial",
    VERDICT_INCOMPLETE: "❔ incomplete",
    VERDICT_UNSUPPORTED: "❌ unsupported",
}


def to_markdown(matrix: dict) -> str:
    """Render the capability matrix as a Markdown table: one row per
    (model, engine), the agent-readiness verdict, then each core group."""
    cols = ["Model", "Engine", "Agent-ready", *matrix["core_groups"]]
    lines = [
        "# Agent-capability matrix",
        "",
        "Generated by `capability_matrix.py` from per-run conformance matrices "
        f"(M2 phase 4c, G18). Agent-critical groups: {', '.join(matrix['agent_critical'])}.",
        "",
        "| " + " | ".join(cols) + " |",
        "| " + " | ".join("---" for _ in cols) + " |",
    ]
    for c in matrix["cells"]:
        row = [c["model"], c["engine"], _VERDICT_GLYPH.get(c["verdict"], c["verdict"])]
        row += [_STATUS_GLYPH.get(c["groups"][g], c["groups"][g]) for g in matrix["core_groups"]]
        lines.append("| " + " | ".join(row) + " |")
    lines.append("")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("matrices", nargs="+", type=Path, help="run.py matrix.json file(s) to aggregate")
    parser.add_argument("--output", type=Path, help="write capability-matrix.json here (default: stdout)")
    parser.add_argument("--markdown", type=Path, help="also write a Markdown table here")
    parser.add_argument(
        "--require-ready",
        action="store_true",
        help="exit 1 unless every (model, engine) cell is agent-ready",
    )
    args = parser.parse_args(argv)

    try:
        matrix = aggregate(args.matrices)
    except MatrixError as err:
        print(f"capability_matrix: {err}", file=sys.stderr)
        return 2

    rendered = json.dumps(matrix, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(rendered)
    else:
        sys.stdout.write(rendered)
    if args.markdown:
        args.markdown.parent.mkdir(parents=True, exist_ok=True)
        args.markdown.write_text(to_markdown(matrix))

    s = matrix["summary"]
    print(
        f"capability matrix: {s['cells']} cell(s) — "
        f"{s['ready']} ready, {s['partial']} partial, "
        f"{s['incomplete']} incomplete, {s['unsupported']} unsupported",
        file=sys.stderr,
    )
    if args.require_ready and (not_ready := [c for c in matrix["cells"] if c["verdict"] != VERDICT_READY]):
        for c in not_ready:
            print(
                f"  not ready: {c['model']} on {c['engine']} = {c['verdict']} "
                f"(groups: {', '.join(c['responsible_groups']) or '—'})",
                file=sys.stderr,
            )
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
