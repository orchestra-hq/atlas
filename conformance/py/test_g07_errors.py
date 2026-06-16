"""G7 — Errors (criterion 6).

Error-envelope shape has run since phase 1 (stub gateway). Phase 6 extends
this group with real-gateway context-window rejection. The engine-down 529
retry case still needs deterministic engine lifecycle control in the harness.
"""

import anthropic
import pytest
from wire import get_model, post_messages

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
def test_oversized_context_rejected(base_url, api_key, model):
    model_info = get_model(base_url, api_key, model)
    assert model_info.status_code == 200, model_info.text
    window = model_info.json().get("context_window")
    assert isinstance(window, int) and window > 0

    # The gateway rejects max_tokens that alone meet/exceed the model window,
    # before any engine dispatch.
    body = {
        "model": model,
        "max_tokens": window,
        "messages": [{"role": "user", "content": "hello"}],
    }
    _assert_envelope(post_messages(base_url, api_key, body), 400, "invalid_request_error")


@pytest.mark.client("anthropic-py")
@pytest.mark.skip(reason="requires deterministic engine lifecycle control in the harness")
def test_engine_down_529_placeholder():
    pass
