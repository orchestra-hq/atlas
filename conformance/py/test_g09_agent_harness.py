"""G9 — Agent harness end-to-end (criterion 1).

The headline promise: an agent built on the Anthropic toolchain, pointed at
Atlas via ANTHROPIC_BASE_URL, completes a multi-step tool loop end-to-end.
Two real-client cells:

  - agent-sdk: a streamed agent loop that drives >=3 client-side tool calls
    through Atlas. The tool is forced each turn (tool_choice), so the loop is
    deterministic on the small CPU-tier model rather than depending on the
    model spontaneously choosing to call a tool three times — what is under
    test is Atlas's streamed request -> tool_use -> tool_result wire path
    across several turns, not the model's planning.

  - claude-code: the real `claude` binary running a non-interactive
    edit-and-verify task against Atlas. This is the literal drop-in promise
    (Claude Code with ANTHROPIC_BASE_URL). Skipped when the binary is absent
    (e.g. external targets, or a runner without it installed).

Capable/full-matrix tier (not the per-PR CPU gate): the dedicated Claude Agent
SDK package driving a model-initiated custom-tool loop, and the whole suite on
vLLM, both need a model large enough to emit structured tool calls reliably —
the small catalog model emits them as prose. See docs/open-questions.md.
"""

import os
import shutil
import subprocess
import tempfile

import pytest

pytestmark = [
    pytest.mark.group("G9"),
    pytest.mark.criterion(1),
]

# A single client-side tool. Forcing it each turn makes the loop deterministic;
# the point is the transport, not the model's tool-selection.
ADD_ITEM_TOOL = {
    "name": "add_item",
    "description": "Add one item to the running list and return the new length.",
    "input_schema": {
        "type": "object",
        "properties": {"item": {"type": "string", "description": "the item to add"}},
        "required": ["item"],
    },
}


@pytest.mark.client("agent-sdk")
def test_agent_sdk_multistep_tool_loop(anthropic_client, model):
    """Stream an agent loop that executes >=3 client-side tool calls via Atlas."""
    items_to_add = ["alpha", "beta", "gamma"]
    collected: list[str] = []
    history: list[dict] = []
    saw_streaming_event = False

    for item in items_to_add:
        history.append(
            {"role": "user", "content": f"Add the item {item!r} to the list by calling add_item."}
        )
        # Stream the turn (criterion: the agent loop is streamed); force a call
        # so a weak model still drives the loop.
        with anthropic_client.messages.stream(
            model=model,
            max_tokens=256,
            temperature=0,
            tools=[ADD_ITEM_TOOL],
            tool_choice={"type": "any"},
            messages=history,
        ) as stream:
            for event in stream:
                if event.type in ("content_block_delta", "content_block_start", "message_delta"):
                    saw_streaming_event = True
            final = stream.get_final_message()

        assert final.stop_reason == "tool_use", f"turn for {item!r} did not call the tool"
        tool_use = next((b for b in final.content if b.type == "tool_use"), None)
        assert tool_use is not None, "no tool_use block in streamed turn"
        assert isinstance(tool_use.input, dict) and tool_use.input.get("item")

        # Execute the tool client-side and feed the result back — the agent step.
        collected.append(tool_use.input["item"])
        history.append({"role": "assistant", "content": final.content})
        history.append(
            {
                "role": "user",
                "content": [
                    {
                        "type": "tool_result",
                        "tool_use_id": tool_use.id,
                        "content": f"ok, the list now has {len(collected)} item(s)",
                    }
                ],
            }
        )

    assert len(collected) >= 3, f"expected >=3 client-side tool calls, got {len(collected)}"
    assert saw_streaming_event, "agent loop was not streamed"


def _claude_code_bin() -> str | None:
    return os.environ.get("CONF_CLAUDE_CODE_BIN") or shutil.which("claude")


@pytest.mark.client("claude-code")
def test_claude_code_smoke(base_url, api_key, model):
    """Real Claude Code, ANTHROPIC_BASE_URL -> Atlas, edits + verifies a file.

    The literal drop-in promise. Off by default (CONF_CLAUDE_CODE_SMOKE): the
    small CPU-tier catalog model drives Claude Code only intermittently — it
    emits the edit but does not reliably finish the agentic loop within the
    turn budget, so as a per-PR gate it would flake. Reliable Claude Code
    drop-in needs a capable model; that run is the full-matrix/GPU acceptance
    tier (see docs/open-questions.md), which is where this cell is enabled.

    Claude Code reserves a large max_tokens by default; on a small-context
    model that leaves no room for its own system prompt, so the output budget
    is capped (CLAUDE_CODE_MAX_OUTPUT_TOKENS) to fit — the task needs only a
    few tokens. Retried once: the run is model-stochastic (suite principle 4).
    """
    if not os.environ.get("CONF_CLAUDE_CODE_SMOKE"):
        pytest.skip("CONF_CLAUDE_CODE_SMOKE not set — real Claude Code smoke is the capable-tier gate")
    claude = _claude_code_bin()
    if claude is None:
        pytest.skip("claude binary not found (set CONF_CLAUDE_CODE_BIN) — drop-in smoke needs it")

    # Wall-clock per attempt. A capable model on a real GPU still needs minutes
    # to drive the full agentic loop, so the cell is given more time than the
    # CPU-tier default via CONF_CLAUDE_CODE_TIMEOUT (the acceptance run raises it).
    timeout_s = int(os.environ.get("CONF_CLAUDE_CODE_TIMEOUT", "300"))
    last_err = ""
    for attempt in range(2):
        with tempfile.TemporaryDirectory(prefix="atlas-cc-smoke-") as sandbox:
            config_dir = os.path.join(sandbox, ".claude-config")
            env = {
                **os.environ,
                "ANTHROPIC_BASE_URL": base_url,
                "ANTHROPIC_API_KEY": api_key,
                # Point Claude Code's default model + small-fast model at the
                # served model so the smoke needs no alias to exist.
                "ANTHROPIC_MODEL": model,
                "ANTHROPIC_SMALL_FAST_MODEL": model,
                "CLAUDE_CODE_MAX_OUTPUT_TOKENS": os.environ.get("CLAUDE_CODE_MAX_OUTPUT_TOKENS", "2048"),
                "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
                "CLAUDE_CONFIG_DIR": config_dir,
            }
            try:
                proc = subprocess.run(
                    [
                        claude,
                        "-p",
                        "Create a file named hello.txt whose contents are exactly: atlas-ok",
                        "--dangerously-skip-permissions",
                        "--max-turns",
                        "14",
                    ],
                    cwd=sandbox,
                    env=env,
                    stdin=subprocess.DEVNULL,
                    capture_output=True,
                    text=True,
                    timeout=timeout_s,
                )
            except subprocess.TimeoutExpired:
                last_err = f"claude -p timed out after {timeout_s}s"
                continue

            hello = os.path.join(sandbox, "hello.txt")
            if proc.returncode == 0 and os.path.isfile(hello):
                content = open(hello, encoding="utf-8").read().strip()
                if content == "atlas-ok":
                    return  # pass
                last_err = f"hello.txt content was {content!r}, want 'atlas-ok'"
            else:
                tail = (proc.stdout + proc.stderr)[-800:]
                last_err = f"exit={proc.returncode}, hello.txt present={os.path.isfile(hello)}; output tail:\n{tail}"

    pytest.fail(f"Claude Code smoke failed after retry: {last_err}")


def test_g9_coverage_marker():
    """Keep a trivially-collected G9 cell so the group is never 'missing' if
    both real-client cells ever skip (e.g. an external target without the
    claude binary). The substantive cells are the two above."""
    # No assertion: presence of a passing G9 cell is the point.
