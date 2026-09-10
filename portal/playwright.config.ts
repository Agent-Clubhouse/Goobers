import { defineConfig, devices } from "@playwright/test";

const port = process.env.PORTAL_E2E_PORT ?? "4173";
const baseURL = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: "./e2e",
  outputDir: "./node_modules/.cache/playwright-results",
  fullyParallel: true,
  timeout: 20_000,
  use: {
    baseURL,
    trace: "retain-on-failure",
  },
  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
  webServer: {
    command: "npm run build && node e2e/fixture-daemon.mjs",
    url: baseURL,
    reuseExistingServer: !process.env.CI,
  },
});
