"""G7 — Errors (criterion 6).

The envelope-shape subset runs (and passes) from phase 1: the stub gateway
already speaks Anthropic-shaped errors, so these lock the contract in
before the real gateway exists. The cases that need real gateway/engine
machinery (oversized context, 529, retry behavior) are deferred to phase 6.
"""

import anthropic
import pytest
from wire import post_messages

pytestmark = [pytest.mark.group("G7"), pytest.mark.criterion(6)]


def _minimal_body(model: str) -> dict:
    return {"model": model, "max_tokens": 16, "messages": [{"role": "user", "content": "hello"}]}


def _assert_envelope(response, status: int, err_type: str):
    assert response.status_code == status
    body = response.json()
    assert body["type"] == "error"
    assert body["error"]["type"] == err_type
    assert isinstance(body["error"]["message"], str) and body["error"]["message"]


@pytest.mark.client("anthropic-py")
def test_unknown_model_raises_not_found(anthropic_client):
    with pytest.raises(anthropic.NotFoundError):
        anthropic_client.messages.create(**_minimal_body("definitely-not-a-model"))


@pytest.mark.client("anthropic-py")
def test_bad_key_raises_authentication_error(base_url, model):
    bad_client = anthropic.Anthropic(base_url=base_url, api_key="not-the-key", max_retries=0)
    with pytest.raises(anthropic.AuthenticationError):
        bad_client.messages.create(**_minimal_body(model))


@pytest.mark.client("wire")
def test_missing_key_envelope(base_url, model):
    _assert_envelope(post_messages(base_url, None, _minimal_body(model)), 401, "authentication_error")


@pytest.mark.client("wire")
def test_unknown_model_envelope(base_url, api_key):
    _assert_envelope(
        post_messages(base_url, api_key, _minimal_body("definitely-not-a-model")), 404, "not_found_error"
    )


@pytest.mark.client("wire")
def test_malformed_body_envelope(base_url, api_key):
    _assert_envelope(post_messages(base_url, api_key, b'{"this is": not json'), 400, "invalid_request_error")


@pytest.mark.client("wire")
@pytest.mark.skip(reason="fleshed out when phase 6 starts: pre-dispatch context-window rejection needs the real gateway")
def test_oversized_context_rejected_placeholder():
    pass


@pytest.mark.client("anthropic-py")
@pytest.mark.skip(reason="fleshed out when phase 6 starts: 529 + SDK retry behavior needs engine lifecycle control")
def test_engine_down_529_placeholder():
    pass
