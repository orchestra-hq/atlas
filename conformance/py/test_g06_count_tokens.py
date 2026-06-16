"""G6 — count_tokens (criterion 5).

Phase 6 adds POST /v1/messages/count_tokens backed by the model's real
tokenizer. Counts must be stable across alias vs canonical id and match
usage.input_tokens for an otherwise identical message request.
"""

import pytest
from wire import get_models, post_count_tokens

pytestmark = [
    pytest.mark.group("G6"),
    pytest.mark.criterion(5),
    pytest.mark.client("anthropic-py"),
]

TIER_PREFIXES = ("claude-opus-", "claude-sonnet-", "claude-haiku-")


def _first_alias_pair(base_url: str, api_key: str) -> tuple[str, str]:
    response = get_models(base_url, api_key)
    assert response.status_code == 200, response.text
    by_id = {item["id"]: item for item in response.json()["data"]}
    for prefix in TIER_PREFIXES:
        aliases = sorted(model_id for model_id in by_id if model_id.startswith(prefix))
        if aliases:
            alias = aliases[0]
            target = by_id[alias]["display_name"]
            assert target in by_id, f"alias target missing from /v1/models: {alias} -> {target}"
            return alias, target
    raise AssertionError("no tier aliases found in /v1/models")


@pytest.mark.client("wire")
def test_count_tokens_wire_shape(base_url, api_key, model):
    response = post_count_tokens(base_url, api_key, {"model": model, "messages": [{"role": "user", "content": "hi"}]})
    assert response.status_code == 200, response.text
    body = response.json()
    assert isinstance(body.get("input_tokens"), int) and body["input_tokens"] > 0


def test_count_tokens_alias_and_real_agree(anthropic_client, base_url, api_key):
    alias, target = _first_alias_pair(base_url, api_key)
    payload = {
        "system": "Be concise.",
        "messages": [{"role": "user", "content": "What is 17 times 19?"}],
    }

    alias_count = anthropic_client.messages.count_tokens(model=alias, **payload)
    target_count = anthropic_client.messages.count_tokens(model=target, **payload)
    assert alias_count.input_tokens == target_count.input_tokens


def test_count_tokens_matches_usage_input_tokens(anthropic_client, model):
    payload = {
        "model": model,
        "system": "Be concise.",
        "messages": [{"role": "user", "content": "What is 17 times 19?"}],
    }
    count = anthropic_client.messages.count_tokens(**payload)
    message = anthropic_client.messages.create(
        **payload,
        max_tokens=64,
        temperature=0,
    )
    assert count.input_tokens == message.usage.input_tokens
