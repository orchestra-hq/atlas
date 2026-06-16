"""G4 — Thinking (criterion 9, ADR-0005).

Thinking-block mapping, landed in m0-build-plan phase 5. A reasoning-capable
model maps its reasoning trace to Anthropic `thinking` content blocks (before
the visible answer), streams them as `thinking_delta`, accepts thinking blocks
echoed back in multi-turn input, and treats `budget_tokens` as advisory. A
non-reasoning model degrades gracefully: the same request succeeds with no
thinking blocks and no error.

The reasoning-model cases use the `reasoning_model` fixture, which skips when
no reasoning model is deployed (e.g. against the stub gateway). The graceful
no-op case runs against the default non-reasoning `model`.

Assertions are structural (a thinking block is present / absent, deltas
concatenate, the request succeeds), never the reasoning text itself.
"""

import pytest
from wire import capture_sse

pytestmark = [
    pytest.mark.group("G4"),
    pytest.mark.criterion(9),
    pytest.mark.client("anthropic-py"),
]


def test_thinking_block_before_text(anthropic_client, reasoning_model):
    message = anthropic_client.messages.create(
        model=reasoning_model,
        max_tokens=512,
        temperature=0,
        thinking={"type": "enabled", "budget_tokens": 1024},
        messages=[{"role": "user", "content": "What is 17 times 19? Reason it through."}],
    )
    types = [b.type for b in message.content]
    assert "thinking" in types, f"no thinking block in response (blocks: {types})"
    # Reasoning precedes the answer.
    assert types.index("thinking") == 0, f"thinking block not first (blocks: {types})"
    thinking = next(b for b in message.content if b.type == "thinking")
    assert thinking.thinking.strip(), "thinking block carried no reasoning text"


def test_budget_tokens_accepted(anthropic_client, reasoning_model):
    # budget_tokens is advisory (ADR-0005): a small budget is accepted, never
    # an error, and never enforced by truncation.
    message = anthropic_client.messages.create(
        model=reasoning_model,
        max_tokens=512,
        temperature=0,
        thinking={"type": "enabled", "budget_tokens": 2048},
        messages=[{"role": "user", "content": "Briefly, why is the sky blue?"}],
    )
    assert message.stop_reason in ("end_turn", "max_tokens")


def test_thinking_multiturn_echo(anthropic_client, reasoning_model):
    history = [{"role": "user", "content": "What is 8 times 7? Reason it through."}]
    first = anthropic_client.messages.create(
        model=reasoning_model,
        max_tokens=512,
        temperature=0,
        thinking={"type": "enabled", "budget_tokens": 1024},
        messages=history,
    )
    # Echo the full assistant turn (thinking block included) back into history;
    # the gateway must accept it (ADR-0005 point 4).
    history.append({"role": "assistant", "content": first.content})
    history.append({"role": "user", "content": "Now double that."})
    second = anthropic_client.messages.create(
        model=reasoning_model,
        max_tokens=512,
        temperature=0,
        thinking={"type": "enabled", "budget_tokens": 1024},
        messages=history,
    )
    assert second.stop_reason in ("end_turn", "max_tokens")
    assert second.content, "expected content on the follow-up turn"


@pytest.mark.client("wire")
def test_thinking_delta_streams(base_url, api_key, reasoning_model):
    events = capture_sse(
        base_url,
        api_key,
        {
            "model": reasoning_model,
            "max_tokens": 512,
            "temperature": 0,
            "thinking": {"type": "enabled", "budget_tokens": 1024},
            "messages": [{"role": "user", "content": "What is 17 times 19? Reason it through."}],
        },
    )

    # A thinking content block is opened and its thinking_delta fragments stream.
    thinking_starts = [
        e
        for e in events
        if e["event"] == "content_block_start"
        and e["data"]["content_block"]["type"] == "thinking"
    ]
    assert thinking_starts, "no thinking content_block_start"
    index = thinking_starts[0]["data"]["index"]

    fragments = [
        e["data"]["delta"]["thinking"]
        for e in events
        if e["event"] == "content_block_delta"
        and e["data"]["index"] == index
        and e["data"]["delta"]["type"] == "thinking_delta"
    ]
    assert fragments, "no thinking_delta fragments"
    assert "".join(fragments).strip(), "thinking_delta fragments were empty"


def test_non_reasoning_model_graceful(anthropic_client, model):
    # A non-reasoning model must succeed with thinking enabled, returning no
    # thinking blocks and no error (ADR-0005 point 2).
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        thinking={"type": "enabled", "budget_tokens": 1024},
        messages=[{"role": "user", "content": "Say hello."}],
    )
    assert message.stop_reason in ("end_turn", "max_tokens")
    assert all(b.type != "thinking" for b in message.content), "non-reasoning model emitted a thinking block"
    assert message.content, "expected a response from the non-reasoning model"
