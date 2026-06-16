"""Conformance harness runner.

Starts the target (built-in stub gateway, or an external Atlas via
--base-url), runs the pytest and vitest suites against it, and merges
per-test results into a single structured matrix.json — the artifact that
becomes the published compat matrix (docs/conformance-suite.md).

Exit codes:
  0  harness ran; all --require groups (if any) are green
  1  harness ran; a required group is not green
  2  harness error (suite did not run to completion, coverage gap, etc.)

Test failures in non-required groups do NOT fail the run — they are the
point: the suite is written before the product, and matrix.json records
exactly what is not conformant yet. The --require gate widens as build
phases land (phase 2 gates G1 against the real gateway, and so on).
"""

import argparse
import datetime
import json
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

CONF_DIR = Path(__file__).resolve().parent
sys.path.insert(0, str(CONF_DIR))

from stubgw import DEFAULT_API_KEY, DEFAULT_MODEL, StubGateway  # noqa: E402

GROUPS = [f"G{i}" for i in range(1, 11)]
# Vitest tests carry their matrix coordinates in the title: "[G2][c2][anthropic-ts] ..."
TS_TAG_RE = re.compile(r"^\[(G\d+)\]\[c(\d+)\]\[([^\]]+)\]\s*(.*)$")


class HarnessError(Exception):
    pass


def run_pytest(env: dict, results_path: Path) -> list[dict]:
    cmd = [
        sys.executable,
        "-m",
        "pytest",
        str(CONF_DIR / "py"),
        "-q",
        "--results-json",
        str(results_path),
    ]
    proc = subprocess.run(cmd, cwd=CONF_DIR, env=env)
    # 0 = all passed, 1 = some tests failed; both mean the suite ran.
    if proc.returncode not in (0, 1):
        raise HarnessError(f"pytest did not run to completion (exit {proc.returncode})")
    if not results_path.exists():
        raise HarnessError("pytest produced no results file")
    return json.loads(results_path.read_text())


def run_vitest(env: dict, results_path: Path) -> list[dict]:
    ts_dir = CONF_DIR / "ts"
    npm = shutil.which("npm")
    if npm is None:
        raise HarnessError("npm not found — install Node 22+ (or pass --skip-ts)")
    if not (ts_dir / "node_modules").exists():
        raise HarnessError(f"{ts_dir}/node_modules missing — run `npm install` there (or pass --skip-ts)")

    cmd = [npm, "run", "--silent", "test", "--", "--reporter=json", f"--outputFile={results_path}"]
    proc = subprocess.run(cmd, cwd=ts_dir, env=env)
    if proc.returncode not in (0, 1):
        raise HarnessError(f"vitest did not run to completion (exit {proc.returncode})")
    if not results_path.exists():
        raise HarnessError("vitest produced no results file")

    status_map = {"passed": "pass", "failed": "fail", "skipped": "skip", "pending": "skip", "todo": "skip"}
    cells = []
    for suite in json.loads(results_path.read_text()).get("testResults", []):
        for result in suite.get("assertionResults", []):
            title = result.get("title", "")
            tag = TS_TAG_RE.match(title)
            if tag is None:
                raise HarnessError(f"vitest test missing [Gx][cN][client] title tag: {title!r}")
            status = status_map.get(result.get("status"), "fail")
            failure_messages = result.get("failureMessages") or []
            cells.append(
                {
                    "id": f"ts::{title}",
                    "suite": "vitest",
                    "group": tag.group(1),
                    "criterion": int(tag.group(2)),
                    "client": tag.group(3),
                    "status": status,
                    "duration_s": round((result.get("duration") or 0) / 1000, 3),
                    "failure": {"phase": "call", "message": "\n".join(failure_messages)[:4000]}
                    if status == "fail"
                    else None,
                    "skip_reason": None,
                }
            )
    return cells


def summarize(cells: list[dict]) -> dict:
    groups = {}
    for g in GROUPS:
        in_group = [c for c in cells if c["group"] == g]
        counts = {s: sum(1 for c in in_group if c["status"] == s) for s in ("pass", "fail", "skip")}
        if counts["fail"]:
            status = "fail"
        elif counts["pass"]:
            status = "pass"
        elif counts["skip"]:
            status = "skip"
        else:
            status = "missing"
        groups[g] = {**counts, "status": status}
    return groups


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--base-url", help="run against an external Atlas instead of the built-in stub gateway")
    parser.add_argument("--api-key", default=os.environ.get("ATLAS_API_KEY", DEFAULT_API_KEY))
    parser.add_argument("--engine", help="engine label for matrix.json (default: stub or external)")
    parser.add_argument("--model", default=os.environ.get("ATLAS_MODEL", DEFAULT_MODEL))
    parser.add_argument(
        "--reasoning-model",
        default=os.environ.get("ATLAS_REASONING_MODEL", ""),
        help="reasoning-capable model name for G4 (omit to skip reasoning cases)",
    )
    parser.add_argument("--require", default="", metavar="G1[,G2...]", help="groups that must be green (exit 1 otherwise)")
    parser.add_argument("--skip-ts", action="store_true", help="skip the vitest (anthropic-ts) suite")
    parser.add_argument("--output", type=Path, default=CONF_DIR / "results" / "matrix.json")
    args = parser.parse_args()

    required = [g for g in args.require.split(",") if g]
    if unknown := [g for g in required if g not in GROUPS]:
        print(f"harness error: unknown group(s) in --require: {', '.join(unknown)}", file=sys.stderr)
        return 2

    results_dir = args.output.parent
    results_dir.mkdir(parents=True, exist_ok=True)

    stub = None
    if args.base_url:
        kind, base_url, engine = "external", args.base_url.rstrip("/"), args.engine or "external"
    else:
        stub = StubGateway(api_key=args.api_key, model=args.model)
        kind, base_url, engine = "stub", stub.start(), args.engine or "stub"

    env = os.environ.copy()
    env.update({"ATLAS_BASE_URL": base_url, "ATLAS_API_KEY": args.api_key, "ATLAS_MODEL": args.model})
    if args.reasoning_model:
        env["ATLAS_REASONING_MODEL"] = args.reasoning_model

    try:
        cells = run_pytest(env, results_dir / "pytest-results.json")
        if args.skip_ts:
            print("note: vitest suite skipped (--skip-ts); anthropic-ts cells absent from matrix.json")
        else:
            cells += run_vitest(env, results_dir / "vitest-results.json")
    except HarnessError as err:
        print(f"harness error: {err}", file=sys.stderr)
        return 2
    finally:
        if stub is not None:
            stub.stop()

    for cell in cells:
        cell.update(engine=engine, model=args.model, retries=0)

    groups = summarize(cells)
    # Suite principle 5: every group has at least one test, even if skipped.
    # The TS suite only covers a subset, so the check applies to the merged matrix.
    if missing := [g for g, s in groups.items() if s["status"] == "missing"]:
        print(
            f"harness error: no tests found for group(s) {', '.join(missing)} — "
            "bidirectional criterion mapping is broken (docs/conformance-suite.md principle 5)",
            file=sys.stderr,
        )
        return 2

    matrix = {
        "schema_version": 1,
        "generated_at": datetime.datetime.now(datetime.UTC).isoformat(timespec="seconds"),
        "target": {"kind": kind, "engine": engine, "model": args.model},
        "summary": {
            "total": len(cells),
            "passed": sum(c["status"] == "pass" for c in cells),
            "failed": sum(c["status"] == "fail" for c in cells),
            "skipped": sum(c["status"] == "skip" for c in cells),
            "flakes": 0,  # retries land with real engines; field reserved
            "groups": groups,
        },
        "cells": cells,
    }
    args.output.write_text(json.dumps(matrix, indent=2) + "\n")

    print(f"\nconformance matrix — target={kind} engine={engine} model={args.model}")
    print(f"{'group':<7}{'pass':>6}{'fail':>6}{'skip':>6}  status")
    for g in GROUPS:
        s = groups[g]
        print(f"{g:<7}{s['pass']:>6}{s['fail']:>6}{s['skip']:>6}  {s['status'].upper()}")
    print(f"\nwrote {args.output}")

    if not_green := [g for g in required if groups[g]["status"] != "pass"]:
        print(f"\ngate FAILED: required group(s) not green: {', '.join(not_green)}", file=sys.stderr)
        return 1
    if required:
        print(f"gate passed: {', '.join(required)} green")
    return 0


if __name__ == "__main__":
    sys.exit(main())
