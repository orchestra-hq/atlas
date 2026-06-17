"""G10 — Ops minimum (criterion 8).

Liveness (/healthz) and readiness (/readyz) semantics, plus token counts in
the request log. These are the operational floor an orchestrator needs to gate
traffic and an operator needs to see per-request cost.
"""

import re
import time

import pytest

import wire

pytestmark = [
    pytest.mark.group("G10"),
    pytest.mark.criterion(8),
    pytest.mark.client("wire"),
]


def test_healthz_ok(base_url):
    # Liveness: the process is up and serving, unauthenticated.
    resp = wire.get(base_url, "/healthz")
    assert resp.status_code == 200, resp.text


def test_readyz_ready_when_model_servable(base_url):
    # The suite runs against a target with a model deployed, so readiness is
    # affirmative: a model can answer.
    resp = wire.get(base_url, "/readyz")
    assert resp.status_code == 200, resp.text


def test_token_counts_in_request_log(base_url, api_key, model, atlas_log_path):
    # A completed request must leave a log line carrying its input and output
    # token counts (criterion 8). Send one, then read the log the target writes.
    resp = wire.post_messages(
        base_url,
        api_key,
        {
            "model": model,
            "max_tokens": 16,
            "messages": [{"role": "user", "content": 'Reply with exactly "ok".'}],
        },
    )
    assert resp.status_code == 200, resp.text

    # The log line is emitted after the response is flushed; poll briefly.
    pattern = re.compile(
        r"path=/v1/messages.*input_tokens=(\d+).*output_tokens=(\d+)"
    )
    deadline = time.time() + 5
    match = None
    while time.time() < deadline:
        with open(atlas_log_path, encoding="utf-8", errors="replace") as f:
            for line in f:
                m = pattern.search(line)
                if m:
                    match = m
        if match:
            break
        time.sleep(0.25)

    assert match is not None, f"no /v1/messages request log line with token counts in {atlas_log_path}"
    assert int(match.group(1)) > 0, "input_tokens should be non-zero for a real request"
