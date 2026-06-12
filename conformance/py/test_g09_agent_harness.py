"""G9 — Agent harness end-to-end (criterion 1).

Placeholder: the real G9 runs are not pytest at all — a scripted Claude
Agent SDK task (>=3 client-side tool calls) and a Claude Code smoke test
via ANTHROPIC_BASE_URL, executed as separate runner steps. They land in
phase 11; this placeholder keeps the criterion mapping bidirectional until
the runner grows those steps.
"""

import pytest

pytestmark = [
    pytest.mark.group("G9"),
    pytest.mark.criterion(1),
    pytest.mark.client("agent-sdk"),
]


@pytest.mark.skip(reason="lands in phase 11 as scripted runner steps (Claude Agent SDK task + Claude Code smoke), not pytest")
def test_agent_harness_placeholder():
    pass
