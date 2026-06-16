"""G8 — OpenAI surface (criterion 7).

The OpenAI Python SDK runs the G3 tool task against `/v1/chat/completions`:
non-streaming + streaming text, tool calls (request → tool_call → tool result →
answer), and the finish_reason mapping (`stop` / `tool_calls` / `length`).
Usage fields are populated throughout.

Atlas owns this surface — it translates core⇄OpenAI itself rather than proxying
the engine (build-time decision 1), so these assertions are structural (shapes,
finish reasons, JSON validity), never exact model prose, exactly like G3.
"""

import json

import pytest

pytestmark = [
    pytest.mark.group("G8"),
    pytest.mark.criterion(7),
    pytest.mark.client("openai-py"),
]

WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "Get the current weather for a city.",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string", "description": "City name"}},
            "required": ["city"],
        },
    },
}


def test_chat_completion_basic(openai_client, model):
    completion = openai_client.chat.completions.create(
        model=model,
        max_tokens=64,
        temperature=0,
        messages=[{"role": "user", "content": 'Reply with exactly "openai surface ok" and nothing else.'}],
    )
    choice = completion.choices[0]
    assert choice.finish_reason == "stop"
    assert choice.message.content and choice.message.content.strip()
    assert completion.usage is not None
    assert completion.usage.prompt_tokens > 0
    assert completion.usage.completion_tokens > 0


def test_system_prompt_honored(openai_client, model):
    completion = openai_client.chat.completions.create(
        model=model,
        max_tokens=64,
        temperature=0,
        messages=[
            {"role": "system", "content": "Always answer in a single word."},
            {"role": "user", "content": "Name a primary color."},
        ],
    )
    choice = completion.choices[0]
    assert choice.finish_reason in ("stop", "length")
    assert choice.message.content and choice.message.content.strip()


def test_finish_reason_length(openai_client, model):
    # A tight token budget on an open-ended prompt forces truncation, which maps
    # to finish_reason "length".
    completion = openai_client.chat.completions.create(
        model=model,
        max_tokens=4,
        temperature=0,
        messages=[{"role": "user", "content": "Write several paragraphs about the ocean."}],
    )
    assert completion.choices[0].finish_reason == "length"


def test_tool_call_emitted(openai_client, model):
    completion = openai_client.chat.completions.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        tool_choice="required",
        messages=[{"role": "user", "content": "What is the weather in Paris right now?"}],
    )
    choice = completion.choices[0]
    assert choice.finish_reason == "tool_calls"
    calls = choice.message.tool_calls
    assert calls, "no tool_calls in response"
    assert calls[0].type == "function"
    assert calls[0].function.name == "get_weather"
    args = json.loads(calls[0].function.arguments)
    assert args.get("city")
    # OpenAI's shape: content is null on a pure tool-call turn.
    assert choice.message.content is None


def test_tool_round_trip(openai_client, model):
    messages = [{"role": "user", "content": "What is the weather in Paris right now?"}]
    first = openai_client.chat.completions.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        tool_choice="required",
        messages=messages,
    )
    msg = first.choices[0].message
    assert msg.tool_calls, "expected a tool call to answer"
    call = msg.tool_calls[0]

    # Feed the assistant tool call back, then the tool result, and expect a
    # natural-language answer rather than another call.
    messages.append(
        {
            "role": "assistant",
            "content": msg.content,
            "tool_calls": [
                {
                    "id": call.id,
                    "type": "function",
                    "function": {"name": call.function.name, "arguments": call.function.arguments},
                }
            ],
        }
    )
    messages.append({"role": "tool", "tool_call_id": call.id, "content": "15°C and sunny"})

    second = openai_client.chat.completions.create(
        model=model,
        max_tokens=256,
        temperature=0,
        tools=[WEATHER_TOOL],
        messages=messages,
    )
    choice = second.choices[0]
    assert choice.finish_reason in ("stop", "length")
    assert choice.message.content and choice.message.content.strip()


def test_stream_text(openai_client, model):
    stream = openai_client.chat.completions.create(
        model=model,
        max_tokens=64,
        temperature=0,
        stream=True,
        stream_options={"include_usage": True},
        messages=[{"role": "user", "content": 'Reply with exactly "streaming ok".'}],
    )

    text = ""
    finish_reason = None
    usage = None
    for chunk in stream:
        assert chunk.object == "chat.completion.chunk"
        if chunk.usage is not None:
            usage = chunk.usage
        if not chunk.choices:
            continue
        delta = chunk.choices[0].delta
        if delta.content:
            text += delta.content
        if chunk.choices[0].finish_reason:
            finish_reason = chunk.choices[0].finish_reason

    assert text.strip(), "no streamed content"
    assert finish_reason == "stop"
    # include_usage was requested, so a usage chunk must arrive.
    assert usage is not None and usage.prompt_tokens > 0 and usage.completion_tokens > 0


def test_stream_tool_call(openai_client, model):
    stream = openai_client.chat.completions.create(
        model=model,
        max_tokens=256,
        temperature=0,
        stream=True,
        tools=[WEATHER_TOOL],
        tool_choice="required",
        messages=[{"role": "user", "content": "What is the weather in Paris right now?"}],
    )

    # Accumulate tool-call name and arguments by index, the way the SDK expects
    # a consumer to reassemble streamed tool calls.
    names: dict[int, str] = {}
    args: dict[int, str] = {}
    finish_reason = None
    for chunk in stream:
        if not chunk.choices:
            continue
        choice = chunk.choices[0]
        if choice.finish_reason:
            finish_reason = choice.finish_reason
        for tc in choice.delta.tool_calls or []:
            if tc.function and tc.function.name:
                names[tc.index] = tc.function.name
            if tc.function and tc.function.arguments:
                args[tc.index] = args.get(tc.index, "") + tc.function.arguments

    assert finish_reason == "tool_calls"
    assert names.get(0) == "get_weather"
    parsed = json.loads(args[0])
    assert parsed.get("city")
