import { expect, test } from "@playwright/test";

import { saveVideo, settle, signIn } from "./helpers";

/** The second core capture: what the system says about one pitch, and why.
 *
 * The live video shows that the pipeline moves; this one shows that what it
 * moved is worth having. It walks from a pitcher's aggregate profile down to
 * a single pitch, its prediction against the outcome that actually happened,
 * and the feature attribution behind that prediction. */
test("from a pitcher's profile to one pitch and its attribution", async ({ page }) => {
  test.setTimeout(180_000);

  await signIn(page);
  await settle(page, 1200);

  // A pitcher.
  await page.locator("table").last().locator("tbody tr").first()
    .getByRole("link").click();
  await expect(page.getByRole("heading", { name: "Summary" })).toBeVisible();
  await settle(page, 2000);

  // The arsenal and the movement profile: the domain view, before any model
  // is involved.
  await page.getByRole("heading", { name: /Movement profile/ }).scrollIntoViewIfNeeded();
  await settle(page, 2500);

  await page.getByRole("heading", { name: /Trend by outing/ }).scrollIntoViewIfNeeded();
  await settle(page, 2500);

  // Into one outing.
  await page.getByRole("heading", { name: "Outings" }).scrollIntoViewIfNeeded();
  await settle(page, 1200);
  await page.locator('a[href^="/sessions/"]').first().click();
  await page.waitForURL(/\/sessions\//);
  await expect(page.getByRole("heading", { name: "Outing", exact: true })).toBeVisible();
  await settle(page, 2500);

  // The timeline, then one pitch out of it.
  await page.getByRole("heading", { name: /^Timeline$/ }).scrollIntoViewIfNeeded();
  await settle(page, 1500);

  const rows = page.locator("tbody tr");
  await rows.nth(5).click();

  const verdict = page.getByRole("heading", { name: "Predicted vs actual" });
  await expect(verdict).toBeVisible({ timeout: 15_000 });
  await verdict.scrollIntoViewIfNeeded();
  await settle(page, 3000);

  // And the attribution: which features moved the model, in its own margin
  // space, with everything else collapsed into one honest remainder.
  await page.getByRole("button", { name: /Explain this pitch/i }).click();
  await expect(page.getByText(/in .* space/)).toBeVisible({ timeout: 40_000 });
  await settle(page, 4000);

  // A second pitch, so it is visibly a per-pitch answer and not a fixed panel.
  await rows.nth(9).click();
  await expect(verdict).toBeVisible();
  await settle(page, 2000);
  await page.getByRole("button", { name: /Explain this pitch/i }).click();
  await expect(page.getByText(/in .* space/)).toBeVisible({ timeout: 30_000 });
  await settle(page, 4000);

  await saveVideo(page, "pitch-explained");
});
