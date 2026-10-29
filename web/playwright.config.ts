import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 2 : 0,
  reporter: process.env.CI ? "line" : "list",
  use: {
    baseURL: "http://127.0.0.1:5174/",
    trace: "retain-on-failure",
    ...(process.env.CI ? { channel: "chrome" as const } : {}),
    ...devices["Desktop Chrome"],
  },
  webServer: {
    command: "bun run dev:demo -- --host 127.0.0.1",
    url: "http://127.0.0.1:5174/",
    reuseExistingServer: !process.env.CI,
    timeout: 30_000,
  },
});
