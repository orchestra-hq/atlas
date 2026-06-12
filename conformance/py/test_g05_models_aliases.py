"""G5 — Models and aliases (criterion 4).

Placeholder: fleshed out when phase 6 starts (/v1/models, tier aliases,
context-window metadata).
"""

import pytest

pytestmark = [
    pytest.mark.group("G5"),
    pytest.mark.criterion(4),
    pytest.mark.client("anthropic-py"),
]


@pytest.mark.skip(reason="fleshed out when phase 6 starts: /v1/models listing, claude-{opus,sonnet,haiku}-* alias resolution, GET /v1/models/{id}")
def test_models_aliases_placeholder():
    pass
