"""G10 — Ops minimum (criterion 8).

Placeholder: fleshed out when phase 10 starts (/healthz, /readyz readiness
semantics, token counts in request logs).
"""

import pytest

pytestmark = [
    pytest.mark.group("G10"),
    pytest.mark.criterion(8),
    pytest.mark.client("wire"),
]


@pytest.mark.skip(reason="fleshed out when phase 10 starts: /healthz, /readyz ready-only-after-servable, token counts in logs")
def test_ops_placeholder():
    pass
