"""G1 — Messages basics (acceptance criterion 1 substrate).

Non-streaming /v1/messages through the Anthropic Python SDK. These pass
against the phase-1 stub gateway and stay green against the real gateway
from phase 2 on.
"""

import pytest

pytestmark = [
    pytest.mark.group("G1"),
    pytest.mark.criterion(1),
    pytest.mark.client("anthropic-py"),
]


def _text(message) -> str:
    return "".join(block.text for block in message.content if block.type == "text")


def test_single_turn_text(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=64,
        temperature=0,
        messages=[{"role": "user", "content": 'Reply with exactly "atlas conformance ok" and nothing else.'}],
    )
    assert message.type == "message"
    assert message.role == "assistant"
    assert message.stop_reason == "end_turn"
    assert _text(message).strip()


def test_system_prompt_honored(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=64,
        temperature=0,
        system='Whatever the user says, reply with exactly "system-prompt-honored" and nothing else.',
        messages=[{"role": "user", "content": "Please respond."}],
    )
    assert "system-prompt-honored" in _text(message)


def test_multi_turn(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=64,
        temperature=0,
        messages=[
            {"role": "user", "content": "We are testing multi-turn conversations."},
            {"role": "assistant", "content": "Understood, ready for the next turn."},
            {"role": "user", "content": 'Reply with exactly "multi-turn ok" and nothing else.'},
        ],
    )
    assert message.stop_reason == "end_turn"
    assert "multi-turn ok" in _text(message)


def test_stop_sequence_triggers_stop_reason(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=64,
        temperature=0,
        stop_sequences=["three"],
        messages=[{"role": "user", "content": 'Reply with exactly "one two three four five" and nothing else.'}],
    )
    assert message.stop_reason == "stop_sequence"
    assert message.stop_sequence == "three"
    assert "three" not in _text(message)


def test_max_tokens_triggers_stop_reason(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=5,
        temperature=0,
        messages=[
            {
                "role": "user",
                "content": 'Reply with exactly "alpha bravo charlie delta echo foxtrot golf hotel india juliett kilo lima" and nothing else.',
            }
        ],
    )
    assert message.stop_reason == "max_tokens"
    assert 0 < message.usage.output_tokens <= 5


def test_sampling_params_accepted(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=64,
        temperature=0,
        top_p=1.0,
        top_k=40,
        messages=[{"role": "user", "content": 'Reply with exactly "sampling ok" and nothing else.'}],
    )
    assert _text(message).strip()


def test_usage_populated(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=64,
        temperature=0,
        messages=[{"role": "user", "content": 'Reply with exactly "usage ok" and nothing else.'}],
    )
    assert message.usage.input_tokens > 0
    assert message.usage.output_tokens > 0
