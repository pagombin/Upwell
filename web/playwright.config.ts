import { defineConfig } from "@playwright/test";

// Run through dev/e2e.sh, which starts a fresh Upwell with a real running
// migration and sets E2E_BASE and friends.
export default defineConfig({
  testDir: "./e2e",
  outputDir: "./test-results",
  timeout: 120_000,
  expect: { timeout: 15_000, toHaveScreenshot: { maxDiffPixelRatio: 0.03, animations: "disabled", caret: "hide" } },
  workers: 2,
  retries: 0,
  reporter: [["list"], ["html", { open: "never", outputFolder: "playwright-report" }]],
  snapshotPathTemplate: "{testDir}/__visual__/{arg}{ext}",
  use: {
    baseURL: process.env.E2E_BASE || "https://127.0.0.1:8543",
    ignoreHTTPSErrors: true,
    launchOptions: { executablePath: process.env.PW_CHROMIUM || "/opt/pw-browsers/chromium" },
    trace: "retain-on-failure",
  },
  projects: [
    { name: "setup", testMatch: /auth\.setup\.ts/ },
    { name: "ui", testMatch: /.*\.spec\.ts/, dependencies: ["setup"] },
  ],
});
