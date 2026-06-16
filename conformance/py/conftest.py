"""Shared fixtures + the results plugin that feeds matrix.json.

Tests are black-box: they reach the target only through ATLAS_BASE_URL /
ATLAS_API_KEY / ATLAS_MODEL, which run.py sets. Every test carries group /
criterion / client markers; the plugin records one structured result per
test so the runner can merge pytest and vitest output into one matrix.
"""

import json
import os

import anthropic
import openai
import pytest

# --- fixtures ---------------------------------------------------------------


def _required_env(name: str) -> str:
    value = os.environ.get(name)
    if not value:
        pytest.exit(f"{name} not set — run the suite via conformance/run.py", returncode=4)
    return value


@pytest.fixture(scope="session")
def base_url() -> str:
    return _required_env("ATLAS_BASE_URL")


@pytest.fixture(scope="session")
def api_key() -> str:
    return _required_env("ATLAS_API_KEY")


@pytest.fixture(scope="session")
def model() -> str:
    return _required_env("ATLAS_MODEL")


@pytest.fixture(scope="session")
def reasoning_model() -> str:
    # The reasoning-capable model (G4). Optional: when no reasoning model is
    # deployed (e.g. the stub gateway), reasoning cases skip rather than fail.
    value = os.environ.get("ATLAS_REASONING_MODEL")
    if not value:
        pytest.skip("ATLAS_REASONING_MODEL not set — no reasoning model deployed")
    return value


# max_retries=0 everywhere: retry behavior is itself under test (G7), and
# the suite's own flake-retry policy lives in the runner, not the clients.
@pytest.fixture(scope="session")
def anthropic_client(base_url, api_key) -> anthropic.Anthropic:
    return anthropic.Anthropic(base_url=base_url, api_key=api_key, max_retries=0)


@pytest.fixture(scope="session")
def openai_client(base_url, api_key) -> openai.OpenAI:
    return openai.OpenAI(base_url=f"{base_url}/v1", api_key=api_key, max_retries=0)


# --- results plugin ----------------------------------------------------------

_RESULTS: list[dict] = []
_BY_NODEID: dict[str, dict] = {}


def pytest_addoption(parser):
    parser.addoption(
        "--results-json",
        default=None,
        help="write one structured conformance result per test to this file",
    )


def _marker_arg(item, name):
    marker = item.get_closest_marker(name)
    return marker.args[0] if marker and marker.args else None


@pytest.hookimpl(wrapper=True)
def pytest_runtest_makereport(item, call):
    report = yield
    record = _BY_NODEID.get(item.nodeid)
    if record is None:
        record = _BY_NODEID[item.nodeid] = {
            "id": item.nodeid,
            "suite": "pytest",
            "group": _marker_arg(item, "group"),
            "criterion": _marker_arg(item, "criterion"),
            "client": _marker_arg(item, "client"),
            "status": "pass",
            "duration_s": 0.0,
            "failure": None,
            "skip_reason": None,
        }
        _RESULTS.append(record)
    record["duration_s"] = round(record["duration_s"] + report.duration, 3)
    if report.skipped:
        record["status"] = "skip"
        if isinstance(report.longrepr, tuple):  # (file, line, reason)
            record["skip_reason"] = report.longrepr[2].removeprefix("Skipped: ")
    elif report.failed:
        record["status"] = "fail"
        record["failure"] = {"phase": report.when, "message": str(report.longrepr)[:4000]}
    return report


def pytest_sessionfinish(session, exitstatus):
    path = session.config.getoption("--results-json")
    if path:
        with open(path, "w", encoding="utf-8") as f:
            json.dump(_RESULTS, f, indent=2)
