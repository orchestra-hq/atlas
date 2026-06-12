"""G2 — Streaming wire conformance (criterion 2).

Written against the spec, ahead of the implementation: expected to FAIL
until phase 3 lands Anthropic SSE streaming (the stub gateway rejects
stream=true on purpose). The failures are the phase-1 deliverable — they
prove the harness reports structured failures. Fleshed out to the full
event-sequence cases (per-stop-reason transitions, ping tolerance) when
phase 3 starts.
"""

import pytest
from wire import capture_sse

pytestmark = [pytest.mark.group("G2"), pytest.mark.criterion(2)]


@pytest.mark.client("wire")
def test_stream_event_sequence(base_url, api_key, model):
    events = capture_sse(
        base_url,
        api_key,
        {
            "model": model,
            "max_tokens": 64,
            "temperature": 0,
            "messages": [{"role": "user", "content": 'Reply with exactly "stream me please" and nothing else.'}],
        },
    )
    names = [e["event"] for e in events if e["event"] != "ping"]
    assert names[0] == "message_start"
    assert names[1] == "content_block_start"
    assert "content_block_delta" in names
    assert names[-3:] == ["content_block_stop", "message_delta", "message_stop"]


@pytest.mark.client("wire")
def test_text_deltas_concatenate_to_final_text(base_url, api_key, model):
    events = capture_sse(
        base_url,
        api_key,
        {
            "model": model,
            "max_tokens": 64,
            "temperature": 0,
            "messages": [{"role": "user", "content": 'Reply with exactly "delta concatenation check" and nothing else.'}],
        },
    )
    text = "".join(
        e["data"]["delta"]["text"]
        for e in events
        if e["event"] == "content_block_delta" and e["data"]["delta"]["type"] == "text_delta"
    )
    assert text.strip()
    message_deltas = [e["data"] for e in events if e["event"] == "message_delta"]
    assert message_deltas, "no message_delta event"
    assert message_deltas[-1]["delta"]["stop_reason"] == "end_turn"


@pytest.mark.client("anthropic-py")
def test_sdk_streams_text(anthropic_client, model):
    with anthropic_client.messages.stream(
        model=model,
        max_tokens=64,
        temperature=0,
        messages=[{"role": "user", "content": 'Reply with exactly "sdk stream ok" and nothing else.'}],
    ) as stream:
        streamed = "".join(stream.text_stream)
        final = stream.get_final_message()
    final_text = "".join(b.text for b in final.content if b.type == "text")
    assert streamed == final_text
    assert final.stop_reason == "end_turn"
