"""Wire-level client: raw HTTP + SSE capture.

The second assertion layer from docs/conformance-suite.md — SDKs normalize
away details (event order, ping events, delta boundaries) that other
clients depend on, so these helpers keep the raw bytes.
"""

import json

import httpx

ANTHROPIC_VERSION = "2023-06-01"


def headers(api_key: str | None) -> dict:
    h = {"anthropic-version": ANTHROPIC_VERSION, "content-type": "application/json"}
    if api_key is not None:
        h["x-api-key"] = api_key
    return h


def post_messages(base_url: str, api_key: str | None, body: dict | bytes) -> httpx.Response:
    """POST /v1/messages without SDK mediation; body may be raw bytes (malformed-input tests)."""
    kwargs = {"content": body} if isinstance(body, bytes) else {"json": body}
    return httpx.post(f"{base_url}/v1/messages", headers=headers(api_key), timeout=30, **kwargs)


def post_count_tokens(base_url: str, api_key: str | None, body: dict | bytes) -> httpx.Response:
    """POST /v1/messages/count_tokens without SDK mediation."""
    kwargs = {"content": body} if isinstance(body, bytes) else {"json": body}
    return httpx.post(f"{base_url}/v1/messages/count_tokens", headers=headers(api_key), timeout=30, **kwargs)


def get_models(base_url: str, api_key: str | None) -> httpx.Response:
    """GET /v1/models as raw wire JSON."""
    return httpx.get(f"{base_url}/v1/models", headers=headers(api_key), timeout=30)


def get_model(base_url: str, api_key: str | None, model_id: str) -> httpx.Response:
    """GET /v1/models/{id} as raw wire JSON."""
    return httpx.get(f"{base_url}/v1/models/{model_id}", headers=headers(api_key), timeout=30)


def capture_sse(base_url: str, api_key: str, body: dict) -> list[dict]:
    """Stream /v1/messages and return the exact event sequence.

    Each element is {"event": name, "data": parsed-json}. Raises
    AssertionError if the target does not answer with an SSE stream.
    """
    request = {**body, "stream": True}
    events: list[dict] = []
    with httpx.stream(
        "POST",
        f"{base_url}/v1/messages",
        headers={**headers(api_key), "accept": "text/event-stream"},
        json=request,
        timeout=60,
    ) as response:
        if response.status_code != 200:
            detail = response.read().decode(errors="replace")
            raise AssertionError(f"expected SSE stream, got HTTP {response.status_code}: {detail}")
        content_type = response.headers.get("content-type", "")
        assert content_type.startswith("text/event-stream"), f"content-type: {content_type}"

        event_name = None
        for line in response.iter_lines():
            if line.startswith("event:"):
                event_name = line.removeprefix("event:").strip()
            elif line.startswith("data:"):
                events.append(
                    {"event": event_name, "data": json.loads(line.removeprefix("data:").strip())}
                )
                event_name = None
    return events
