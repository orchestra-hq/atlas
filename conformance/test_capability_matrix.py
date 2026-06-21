"""Unit tests for capability_matrix.py — the G18 aggregation logic (M2 phase 4c).

These exercise the verdict classification and aggregation with synthetic run.py
matrices, so they run on any CPU with no engine, model, or conformance deps.
This file lives at the conformance root (not under py/) so run.py's own
`pytest py/` suite never collects it — it is harness tooling, not a conformance
group. Run it with `uv run python -m pytest test_capability_matrix.py`.
"""

from __future__ import annotations

import json
from pathlib import Path

import capability_matrix as cm


def write_matrix(path: Path, *, engine: str, model: str, statuses: dict[str, str]) -> Path:
    """Write a minimal run.py-shaped matrix.json with the given per-group statuses."""
    groups = {g: {"status": statuses.get(g, "pass")} for g in cm.CORE_GROUPS}
    path.write_text(
        json.dumps(
            {
                "schema_version": 1,
                "generated_at": "2026-06-21T00:00:00+00:00",
                "target": {"kind": "external", "engine": engine, "model": model},
                "summary": {"groups": groups},
            }
        )
    )
    return path


def test_verdict_ready_when_all_pass():
    groups = {g: {"status": "pass"} for g in cm.CORE_GROUPS}
    verdict, responsible = cm.verdict_for(groups)
    assert verdict == cm.VERDICT_READY
    assert responsible == []


def test_verdict_unsupported_when_critical_fails():
    groups = {g: {"status": "pass"} for g in cm.CORE_GROUPS}
    groups["G9"] = {"status": "fail"}  # agent loop broken
    verdict, responsible = cm.verdict_for(groups)
    assert verdict == cm.VERDICT_UNSUPPORTED
    assert responsible == ["G9"]


def test_verdict_partial_when_noncritical_fails():
    groups = {g: {"status": "pass"} for g in cm.CORE_GROUPS}
    groups["G7"] = {"status": "fail"}  # errors group — not agent-critical
    verdict, responsible = cm.verdict_for(groups)
    assert verdict == cm.VERDICT_PARTIAL
    assert responsible == ["G7"]


def test_verdict_incomplete_when_critical_skipped():
    groups = {g: {"status": "pass"} for g in cm.CORE_GROUPS}
    groups["G3"] = {"status": "skip"}  # tools not exercised
    verdict, responsible = cm.verdict_for(groups)
    assert verdict == cm.VERDICT_INCOMPLETE
    assert responsible == ["G3"]


def test_critical_fail_outranks_noncritical_fail():
    groups = {g: {"status": "pass"} for g in cm.CORE_GROUPS}
    groups["G3"] = {"status": "fail"}  # critical
    groups["G7"] = {"status": "fail"}  # non-critical
    verdict, responsible = cm.verdict_for(groups)
    assert verdict == cm.VERDICT_UNSUPPORTED
    assert responsible == ["G3"]  # only the critical group is blamed


def test_aggregate_sorts_and_counts(tmp_path: Path):
    a = write_matrix(tmp_path / "vllm.json", engine="vllm", model="qwen3-8b", statuses={})
    b = write_matrix(
        tmp_path / "llamacpp.json", engine="llamacpp", model="qwen2.5-1.5b", statuses={"G9": "fail"}
    )
    matrix = cm.aggregate([a, b])
    assert matrix["summary"] == {
        "cells": 2,
        "ready": 1,
        "partial": 0,
        "incomplete": 0,
        "unsupported": 1,
    }
    # Sorted by (model, engine): qwen2.5 before qwen3.
    assert [c["model"] for c in matrix["cells"]] == ["qwen2.5-1.5b", "qwen3-8b"]
    assert matrix["cells"][0]["verdict"] == cm.VERDICT_UNSUPPORTED
    assert matrix["cells"][1]["verdict"] == cm.VERDICT_READY


def test_aggregate_rejects_duplicate_cells(tmp_path: Path):
    a = write_matrix(tmp_path / "a.json", engine="vllm", model="qwen3-8b", statuses={})
    b = write_matrix(tmp_path / "b.json", engine="vllm", model="qwen3-8b", statuses={})
    try:
        cm.aggregate([a, b])
    except cm.MatrixError as err:
        assert "duplicate" in str(err)
    else:
        raise AssertionError("expected MatrixError on duplicate (model, engine)")


def test_load_matrix_rejects_non_matrix(tmp_path: Path):
    bad = tmp_path / "bad.json"
    bad.write_text(json.dumps({"hello": "world"}))
    try:
        cm.load_matrix(bad)
    except cm.MatrixError as err:
        assert "not a run.py matrix" in str(err)
    else:
        raise AssertionError("expected MatrixError on non-matrix input")


def test_markdown_has_a_row_per_cell(tmp_path: Path):
    a = write_matrix(tmp_path / "vllm.json", engine="vllm", model="qwen3-8b", statuses={})
    b = write_matrix(tmp_path / "mlx.json", engine="mlx", model="qwen2.5-1.5b", statuses={"G4": "skip"})
    md = cm.to_markdown(cm.aggregate([a, b]))
    assert "# Agent-capability matrix" in md
    assert "qwen3-8b" in md and "qwen2.5-1.5b" in md
    # Header + separator + 2 data rows = 4 table lines.
    assert sum(1 for line in md.splitlines() if line.startswith("|")) == 4


def test_main_require_ready_fails_on_unsupported(tmp_path: Path):
    b = write_matrix(tmp_path / "bad.json", engine="llamacpp", model="m", statuses={"G3": "fail"})
    out = tmp_path / "cap.json"
    rc = cm.main([str(b), "--output", str(out), "--require-ready"])
    assert rc == 1
    assert out.exists()  # artifact still written even when the gate fails


def test_main_require_ready_passes_when_all_ready(tmp_path: Path):
    a = write_matrix(tmp_path / "ok.json", engine="vllm", model="m", statuses={})
    rc = cm.main([str(a), "--output", str(tmp_path / "cap.json"), "--require-ready"])
    assert rc == 0
