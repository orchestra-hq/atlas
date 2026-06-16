// Anthropic TS SDK subset: per docs/conformance-suite.md the TS client runs
// the streaming + tool-loop subset on one engine; one non-streaming basic
// test keeps the SDK wiring itself covered.
//
// Titles carry matrix coordinates as "[Gx][cN][client]" — conformance/run.py
// parses them when merging vitest output into matrix.json.
import Anthropic from "@anthropic-ai/sdk";
import { expect, it } from "vitest";

const baseURL = process.env.ATLAS_BASE_URL;
if (!baseURL) throw new Error("ATLAS_BASE_URL not set — run via conformance/run.py");

const client = new Anthropic({ baseURL, apiKey: process.env.ATLAS_API_KEY ?? "" });
const model = process.env.ATLAS_MODEL ?? "stub-small";

function textOf(message: Anthropic.Message): string {
  return message.content
    .filter((block): block is Anthropic.TextBlock => block.type === "text")
    .map((block) => block.text)
    .join("");
}

it('[G1][c1][anthropic-ts] non-streaming single turn returns text', async () => {
  const message = await client.messages.create({
    model,
    max_tokens: 64,
    temperature: 0,
    messages: [{ role: "user", content: 'Reply with exactly "anthropic-ts ok" and nothing else.' }],
  });
  expect(message.role).toBe("assistant");
  expect(message.stop_reason).toBe("end_turn");
  expect(textOf(message)).toContain("anthropic-ts ok");
  expect(message.usage.output_tokens).toBeGreaterThan(0);
});

it('[G2][c2][anthropic-ts] streaming text deltas match the final message', async () => {
  const stream = client.messages.stream({
    model,
    max_tokens: 64,
    temperature: 0,
    messages: [{ role: "user", content: 'Reply with exactly "anthropic-ts stream ok" and nothing else.' }],
  });
  let streamed = "";
  stream.on("text", (delta) => {
    streamed += delta;
  });
  const final = await stream.finalMessage();
  expect(final.stop_reason).toBe("end_turn");
  expect(textOf(final)).toBe(streamed);
  expect(streamed.length).toBeGreaterThan(0);
});

// Tool-loop subset on the TS client: streaming a tool call must surface a
// tool_use block whose input_json_delta fragments the SDK reassembles into a
// valid input object.
const WEATHER_TOOL: Anthropic.Tool = {
  name: "get_weather",
  description: "Get the current weather for a city.",
  input_schema: {
    type: "object",
    properties: { city: { type: "string", description: "City name" } },
    required: ["city"],
  },
};

it('[G3][c3][anthropic-ts] streaming tool use yields a valid tool_use block', async () => {
  const stream = client.messages.stream({
    model,
    max_tokens: 256,
    temperature: 0,
    tools: [WEATHER_TOOL],
    tool_choice: { type: "any" },
    messages: [{ role: "user", content: "What is the weather in Paris right now?" }],
  });
  const final = await stream.finalMessage();

  expect(final.stop_reason).toBe("tool_use");
  const toolUse = final.content.find(
    (block): block is Anthropic.ToolUseBlock => block.type === "tool_use",
  );
  expect(toolUse).toBeDefined();
  expect(toolUse!.name).toBe("get_weather");
  expect((toolUse!.input as { city?: string }).city).toBeTruthy();
});
