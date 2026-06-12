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

// Expected to FAIL until phase 3 lands SSE streaming (the stub gateway
// rejects stream=true on purpose) — part of the phase-1 structured-failure
// deliverable. The tool-loop subset joins it when phase 4 starts.
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
