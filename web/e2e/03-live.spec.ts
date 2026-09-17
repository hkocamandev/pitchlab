import { expect, test } from "@playwright/test";

import { panelShot, saveVideo, settle, shot, signIn } from "./helpers";

/** The centrepiece capture: a pitch leaving the replay simulator, crossing
 * Kafka, being scored by the model, landing in PostgreSQL and arriving in the
 * browser over a WebSocket -- with nothing staged. The replay has to be
 * running for this to show anything, which is the point: there is no fixture
 * behind it. */
test("live pitch flow", async ({ page }) => {
  test.setTimeout(180_000);

  await signIn(page);
  await settle(page, 1500);

  // The live list, with outings that are being written to right now.
  await expect(page.getByRole("heading", { name: /Live now/i })).toBeVisible();
  await settle(page, 1500);
  await shot(page, "home-live-now");

  // Into the busiest one.
  await page.locator('a[href^="/live/"]').first().click();
  await page.waitForURL(/\/live\//);

  await expect(page.getByRole("heading", { name: "Live", exact: true }))
    .toBeVisible({ timeout: 20_000 });

  // The snapshot arrives first, then the socket takes over. Waiting for the
  // count to move is waiting for the stream itself rather than for the page.
  const feedRows = page.locator("tbody tr");
  const before = await feedRows.count();

  await expect
    .poll(async () => feedRows.count(), { timeout: 90_000, intervals: [1000] })
    .toBeGreaterThan(before + 3);

  // Let it run long enough that the video shows a rhythm rather than a jump.
  await page.waitForTimeout(25_000);

  await shot(page, "live-session");
  await panelShot(page, /^Feed$/, "live-feed");
  await panelShot(page, /Velocity, rolling window/, "live-velocity");

  // Proof the arrivals were real: the feed grew while the page sat still.
  expect(await feedRows.count()).toBeGreaterThan(before);

  await saveVideo(page, "live-pitch-flow");
});
