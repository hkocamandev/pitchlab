import { defineConfig, devices } from "@playwright/test";

/** Playwright is here to capture the documentation in `screenshots/`, not to
 * test the app — the unit and integration suites do that. Keeping it in its
 * own config makes the distinction structural: `npm test` never waits on a
 * browser, and a capture run never reports as a test failure.
 *
 * Every capture runs against the real stack: the real API, the real
 * processor, the real model. A screenshot of a mocked dashboard would be a
 * drawing of the product rather than a picture of it. */
export default defineConfig({
  testDir: "./e2e",
  outputDir: "../screenshots/.playwright",
  // Serially, because several captures depend on a replay running and on each
  // other's timing. Parallel runs would race for the same live session.
  workers: 1,
  fullyParallel: false,
  // No retries. A capture that only works on the second attempt is a capture
  // of something that does not reliably happen.
  retries: 0,
  timeout: 180_000,
  reporter: [["list"]],
  use: {
    baseURL: process.env.PITCHLAB_WEB_URL ?? "http://localhost:5174",
    viewport: { width: 1280, height: 860 },
    // 2x so the images stay sharp when GitHub scales them down.
    deviceScaleFactor: 2,
    video: {
      mode: "on",
      // Recorded at 1x: a 2x video is four times the bytes for detail nobody
      // sees once it is a GIF in a README.
      size: { width: 1280, height: 860 },
    },
    screenshot: "off",
    trace: "off",
  },
  projects: [
    { name: "capture", use: { ...devices["Desktop Chrome"] } },
  ],
});
