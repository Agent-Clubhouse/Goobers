import { defineConfig, devices } from "@playwright/test";

const fixturePort = process.env.PORTAL_E2E_PORT ?? "4173";
const realPort = process.env.PORTAL_E2E_REAL_PORT ?? "4174";
const fixtureBaseURL = `http://127.0.0.1:${fixturePort}`;
const realBaseURL = `http://127.0.0.1:${realPort}`;

export default defineConfig({
  testDir: "./e2e",
  globalTeardown: "./e2e/real-daemon-teardown.mjs",
  outputDir: "./node_modules/.cache/playwright-results",
  fullyParallel: true,
  timeout: 20_000,
  use: {
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "fixture-chromium",
      testIgnore: /real-daemon-smoke\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: fixtureBaseURL },
    },
    {
      name: "real-daemon-chromium",
      testMatch: /real-daemon-smoke\.spec\.ts/,
      use: { ...devices["Desktop Chrome"], baseURL: realBaseURL },
    },
  ],
  webServer: [
    {
      command: "node e2e/fixture-daemon.mjs",
      url: fixtureBaseURL,
      reuseExistingServer: !process.env.CI,
    },
    {
      command: "node e2e/real-daemon.mjs",
      env: { PORTAL_E2E_REAL_PORT: realPort },
      url: realBaseURL,
      reuseExistingServer: false,
      timeout: 180_000,
      gracefulShutdown: { signal: "SIGTERM", timeout: 5_000 },
    },
  ],
});
