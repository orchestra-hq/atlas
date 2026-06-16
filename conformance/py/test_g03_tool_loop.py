"""G3 — Tool loop (criterion 3).

Anthropic tool use, landed in m0-build-plan phase 4. Covers the tool_use
emission, the tool_choice variants (auto / any / specific tool), the full
request → tool_use → tool_result → answer round-trip, the is_error result
path, parallel calls, and the wire-level input_json_delta fragments that agent
SDKs depend on.

Assertions are structural (shapes, stop reasons, JSON validity), never exact
text: the tasks are designed so a competent model must call the tool, but the
model's prose is never asserted.

tool_choice forcing is exercised on prompt-motivated tasks. The pinned
llama.cpp build treats forcing as advisory rather than a hard decode
constraint (see docs/open-questions.md), so these tests verify the named /
appropriate tool is selected and the wire shape is correct — not that a call is
forced against a contrary prompt.
"""

import json

import pytest
from wire import capture_sse

pytestmark = [
    pytest.mark.group("G3"),
    pytest.mark.criterion(3),
    pytest.mark.client("anthropic-py"),
]

WEATHER_TOOL = {
    "name": "get_weather",
    "description": "Get the current weather for a city.",
    "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string", "description": "City name"}},
        "required": ["city"],
    },
}

TIME_TOOL = {
    "name": "get_time",
    "description": "Get the current local time in a city.",
    "input_schema": {
        "type": "object",
        "properties": {"city": {"type": "string", "description": "City name"}},
        "required": ["city"],
    },
}


def test_tool_use_block_emitted(anthropic_client, model):
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        tool_choice={"type": "any"},
        messages=[{"role": "user", "content": "What is the weather in Paris right now?"}],
    )
    assert message.stop_reason == "tool_use"
    tool_blocks = [b for b in message.content if b.type == "tool_use"]
    assert tool_blocks, "no tool_use block in response"
    assert tool_blocks[0].name == "get_weather"
    assert isinstance(tool_blocks[0].input, dict)
    assert tool_blocks[0].input.get("city")


def test_tool_choice_auto_calls_when_needed(anthropic_client, model):
    # The answer is only obtainable by calling the tool, so auto must call it.
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        tool_choice={"type": "auto"},
        messages=[{"role": "user", "content": "What is the weather in Paris right now?"}],
    )
    assert message.stop_reason == "tool_use"
    assert any(b.type == "tool_use" and b.name == "get_weather" for b in message.content)


def test_tool_choice_specific_tool(anthropic_client, model):
    # Two tools offered; tool_choice names the one to use for a task that
    # motivates it. The named tool — not the other — must be the one called.
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[TIME_TOOL, WEATHER_TOOL],
        tool_choice={"type": "tool", "name": "get_weather"},
        messages=[{"role": "user", "content": "What is the weather in Paris right now?"}],
    )
    assert message.stop_reason == "tool_use"
    tool_blocks = [b for b in message.content if b.type == "tool_use"]
    assert tool_blocks and tool_blocks[0].name == "get_weather"


def test_tool_round_trip(anthropic_client, model):
    history = [{"role": "user", "content": "What is the weather in Paris right now?"}]
    first = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        tool_choice={"type": "any"},
        messages=history,
    )
    assert first.stop_reason == "tool_use"
    tool = next(b for b in first.content if b.type == "tool_use")

    # Feed the assistant's tool_use back, then the tool_result, and expect a
    # final natural-language answer rather than another tool call.
    history.append({"role": "assistant", "content": first.content})
    history.append(
        {
            "role": "user",
            "content": [
                {"type": "tool_result", "tool_use_id": tool.id, "content": "15°C and sunny"}
            ],
        }
    )
    second = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        messages=history,
    )
    assert second.stop_reason in ("end_turn", "max_tokens")
    text = "".join(b.text for b in second.content if b.type == "text")
    assert text.strip(), "expected a textual final answer after the tool result"


def test_tool_result_is_error_handled(anthropic_client, model):
    first = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        tool_choice={"type": "any"},
        messages=[{"role": "user", "content": "What is the weather in Paris right now?"}],
    )
    tool = next(b for b in first.content if b.type == "tool_use")

    # An is_error tool_result must round-trip without breaking the gateway.
    second = anthropic_client.messages.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        messages=[
            {"role": "user", "content": "What is the weather in Paris right now?"},
            {"role": "assistant", "content": first.content},
            {
                "role": "user",
                "content": [
                    {
                        "type": "tool_result",
                        "tool_use_id": tool.id,
                        "content": "weather service timed out",
                        "is_error": True,
                    }
                ],
            },
        ],
    )
    assert second.stop_reason in ("end_turn", "max_tokens", "tool_use")
    assert second.content, "expected some content after an error tool_result"


def test_parallel_tool_calls(anthropic_client, model):
    # Encourage two calls in one turn. The transport must carry however many the
    # model emits as distinct tool_use blocks with valid, independent input.
    message = anthropic_client.messages.create(
        model=model,
        max_tokens=512,
        temperature=0,
        tools=[WEATHER_TOOL],
        tool_choice={"type": "any"},
        messages=[
            {
                "role": "user",
                "content": "Get the current weather in both Paris and Tokyo. "
                "Call the get_weather tool separately for each city.",
            }
        ],
    )
    assert message.stop_reason == "tool_use"
    tool_blocks = [b for b in message.content if b.type == "tool_use"]
    assert tool_blocks, "no tool_use block in response"
    # Each call carries schema-valid, independent input; ids are unique.
    ids = [b.id for b in tool_blocks]
    assert len(ids) == len(set(ids)), "tool_use ids must be unique"
    for b in tool_blocks:
        assert isinstance(b.input, dict) and b.input.get("city")


@pytest.mark.client("wire")
def test_input_json_delta_streams(base_url, api_key, model):
    events = capture_sse(
        base_url,
        api_key,
        {
            "model": model,
            "max_tokens": 256,
            "temperature": 0,
            "tools": [WEATHER_TOOL],
            "tool_choice": {"type": "any"},
            "messages": [{"role": "user", "content": "What is the weather in Paris right now?"}],
        },
    )

    # A tool_use content block is opened, and its input_json_delta fragments
    # concatenate to the valid JSON arguments.
    tool_starts = [
        e
        for e in events
        if e["event"] == "content_block_start"
        and e["data"]["content_block"]["type"] == "tool_use"
    ]
    assert tool_starts, "no tool_use content_block_start"
    block = tool_starts[0]["data"]["content_block"]
    assert block["name"] == "get_weather" and block["id"]
    index = tool_starts[0]["data"]["index"]

    fragments = [
        e["data"]["delta"]["partial_json"]
        for e in events
        if e["event"] == "content_block_delta"
        and e["data"]["index"] == index
        and e["data"]["delta"]["type"] == "input_json_delta"
    ]
    parsed = json.loads("".join(fragments))
    assert parsed.get("city")

    message_deltas = [e["data"] for e in events if e["event"] == "message_delta"]
    assert message_deltas and message_deltas[-1]["delta"]["stop_reason"] == "tool_use"
