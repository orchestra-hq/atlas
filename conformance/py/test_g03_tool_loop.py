"""G3 — Tool loop (criterion 3).

Expected to FAIL until phase 4 lands tool-loop translation. One real test
proves the harness reports the gap; the rest of the group (round-trip,
parallel calls, input_json_delta, tool_choice variants, is_error) is
fleshed out when phase 4 starts.
"""

import pytest

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


@pytest.mark.skip(reason="fleshed out when phase 4 starts: round-trip, parallel calls, input_json_delta, tool_choice variants, is_error")
def test_tool_loop_remainder_placeholder():
    pass
