import { defineConfig, devices } from "@playwright/test";
import { tmpdir } from "node:os";
import { resolve } from "node:path";

// Deliberately separate from the ordinary fixture-daemon suite. These tests
// must contact an already running interactive-prototype backend, without
// intercepting API requests or substituting browser fixtures.
export const evidenceDirectory = resolve(
  process.env.INTERACTIVE_EVIDENCE_DIR ?? `${tmpdir()}/goobers-interactive-evidence`,
);

export default defineConfig({
  testDir: ".",
  testMatch: "interactive.acceptance.ts",
  outputDir: `${evidenceDirectory}/playwright`,
  reporter: [["list"], ["json", { outputFile: `${evidenceDirectory}/results.json` }]],
  fullyParallel: false,
  workers: 1,
  timeout: 120_000,
  expect: { timeout: 15_000 },
  use: {
    ...devices["Desktop Chrome"],
    baseURL: process.env.INTERACTIVE_BASE_URL ?? "http://127.0.0.1:8097",
    trace: "on",
    video: "on",
    screenshot: "only-on-failure",
  },
});
