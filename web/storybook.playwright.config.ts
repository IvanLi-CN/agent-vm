import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e-storybook",
  timeout: 30_000,
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? "line" : "list",
  use: {
    baseURL: "http://127.0.0.1:6006",
    trace: "retain-on-failure",
    ...(process.env.CI ? { channel: "chrome" as const } : {}),
    ...devices["Desktop Chrome"],
  },
  webServer: {
    command: "bun run storybook -- --ci --no-open --host 127.0.0.1",
    url: "http://127.0.0.1:6006",
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
});
