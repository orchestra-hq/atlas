"""G5 — Models and aliases (criterion 4).

Phase 6 adds the models surface:
- GET /v1/models lists real models and tier aliases with context-window metadata.
- GET /v1/models/{id} resolves both canonical ids and aliases.
- claude-{opus,sonnet,haiku}-* aliases are routable message model ids.
"""

import pytest
from wire import get_model, get_models

pytestmark = [
    pytest.mark.group("G5"),
    pytest.mark.criterion(4),
    pytest.mark.client("anthropic-py"),
]

TIER_PREFIXES = ("claude-opus-", "claude-sonnet-", "claude-haiku-")


def _models_by_id(base_url: str, api_key: str) -> tuple[dict, dict[str, dict]]:
    response = get_models(base_url, api_key)
    assert response.status_code == 200, response.text
    body = response.json()
    assert isinstance(body.get("data"), list) and body["data"], "expected non-empty model list"
    by_id = {item["id"]: item for item in body["data"]}
    return body, by_id


def _tier_aliases(by_id: dict[str, dict]) -> dict[str, str]:
    aliases: dict[str, str] = {}
    for prefix in TIER_PREFIXES:
        candidates = sorted(model_id for model_id in by_id if model_id.startswith(prefix))
        assert candidates, f"no alias with prefix {prefix!r} in /v1/models"
        aliases[prefix] = candidates[0]
    return aliases


@pytest.mark.client("wire")
def test_models_list_includes_tiers_and_context_windows(base_url, api_key):
    body, by_id = _models_by_id(base_url, api_key)
    assert body.get("has_more") is False
    assert body.get("first_id") in by_id
    assert body.get("last_id") in by_id

    # Canonical entries identify themselves with display_name == id.
    real_ids = [model_id for model_id, item in by_id.items() if item.get("display_name") == model_id]
    assert real_ids, "expected at least one canonical model entry"

    for model_id, item in by_id.items():
        assert item.get("type") == "model"
        assert isinstance(item.get("context_window"), int) and item["context_window"] > 0

    aliases = _tier_aliases(by_id)
    for alias in aliases.values():
        target = by_id[alias]["display_name"]
        assert target in by_id, f"alias target missing from list: {alias} -> {target}"
        assert by_id[alias]["context_window"] == by_id[target]["context_window"]


@pytest.mark.client("wire")
def test_get_model_works_for_alias_and_real_ids(base_url, api_key):
    _, by_id = _models_by_id(base_url, api_key)
    aliases = _tier_aliases(by_id)

    sample_ids = [next(iter(by_id)), *aliases.values()]
    for model_id in sample_ids:
        response = get_model(base_url, api_key, model_id)
        assert response.status_code == 200, response.text
        model = response.json()
        assert model["id"] == model_id
        assert model["type"] == "model"
        assert isinstance(model.get("context_window"), int) and model["context_window"] > 0


def test_tier_aliases_route_messages(anthropic_client, base_url, api_key):
    _, by_id = _models_by_id(base_url, api_key)
    aliases = _tier_aliases(by_id)

    for alias in aliases.values():
        message = anthropic_client.messages.create(
            model=alias,
            max_tokens=32,
            temperature=0,
            messages=[{"role": "user", "content": "Reply with exactly \"ok\"."}],
        )
        assert message.model == alias
        assert message.stop_reason in ("end_turn", "max_tokens")
        assert message.usage.input_tokens > 0
