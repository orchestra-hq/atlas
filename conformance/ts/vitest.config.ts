import { defineConfig } from "vitest/config";

// CONF_TS_TIMEOUT (seconds) bounds each streaming test. The default is generous
// because Atlas legitimately serves capable models on slow CPU hardware: a 12B
// reasoning model on the CPU acceptance track takes ~30s+ just to stream one
// thinking response, which the prior 30s default tripped over (a vitest timeout
// looks identical to "no thinking block"). acceptance.sh raises it further for
// the slow tracks, mirroring its engine-ready (15m) and Claude Code (600s) bumps.
const testTimeout = (Number(process.env.CONF_TS_TIMEOUT) || 120) * 1000;

export default defineConfig({
  test: {
    environment: "node",
    include: ["tests/**/*.test.ts"],
    testTimeout,
  },
});
