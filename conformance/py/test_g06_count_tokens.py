"""G6 — count_tokens (criterion 5).

Placeholder: fleshed out when phase 6 starts (proxied engine tokenize
endpoints; counts must match usage.input_tokens of an identical request).
"""

import pytest

pytestmark = [
    pytest.mark.group("G6"),
    pytest.mark.criterion(5),
    pytest.mark.client("anthropic-py"),
]


@pytest.mark.skip(reason="fleshed out when phase 6 starts: real tokenizer counts, alias/real-name agreement, match with usage.input_tokens")
def test_count_tokens_placeholder():
    pass
