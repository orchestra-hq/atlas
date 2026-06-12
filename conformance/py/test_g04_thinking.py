"""G4 — Thinking (criterion 9, ADR-0005).

Placeholder: fleshed out when phase 5 starts. Needs the reasoning-capable
fixture model (the stub gateway has no thinking support, and the matrix's
model dimension only gains a reasoning cell in phase 5).
"""

import pytest

pytestmark = [
    pytest.mark.group("G4"),
    pytest.mark.criterion(9),
    pytest.mark.client("anthropic-py"),
]


@pytest.mark.skip(reason="fleshed out when phase 5 starts: thinking blocks, thinking_delta, budget_tokens, non-reasoning graceful no-op")
def test_thinking_placeholder():
    pass
