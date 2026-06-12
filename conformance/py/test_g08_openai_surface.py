"""G8 — OpenAI surface (criterion 7).

Expected to FAIL until phase 7 lands /v1/chat/completions. One real test
proves the harness reports the gap; streaming + tools + finish_reason
mapping are fleshed out when phase 7 starts.
"""

import pytest

pytestmark = [
    pytest.mark.group("G8"),
    pytest.mark.criterion(7),
    pytest.mark.client("openai-py"),
]


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


@pytest.mark.skip(reason="fleshed out when phase 7 starts: streaming, tools, finish_reason mapping (stop/tool_calls/length)")
def test_openai_surface_remainder_placeholder():
    pass
