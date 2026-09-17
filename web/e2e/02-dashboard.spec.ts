import { expect, test } from "@playwright/test";

import { panelShot, settle, shot, signIn } from "./helpers";

test.describe("the dashboard", () => {
  test("pitcher overview", async ({ page }) => {
    await signIn(page);

    // The busiest pitcher, so the charts have something in them. Picking the
    // first row would sometimes land on an outing that had just started.
    await page.locator("table").last().locator("tbody tr").first()
      .getByRole("link").click();

    await expect(page.getByRole("heading", { name: "Summary" })).toBeVisible();
    await settle(page);
    await shot(page, "athlete-overview");

    // The arsenal and movement panels are the domain-specific half of this
    // page and they sit below the fold at this height.
    await page.getByRole("heading", { name: /Movement profile/ }).scrollIntoViewIfNeeded();
    await settle(page);
    await shot(page, "athlete-arsenal-movement");
  });

  test("outing detail, one pitch, and why the model said so", async ({ page }) => {
    await signIn(page);

    await page.locator("table").last().locator("tbody tr").first()
      .getByRole("link").click();
    await expect(page.getByRole("heading", { name: "Summary" })).toBeVisible();

    // Into the outing. The link is the game date, so it is addressed by where
    // it goes rather than by what it says.
    await page.getByRole("heading", { name: "Outings" }).scrollIntoViewIfNeeded();
    await page.locator('a[href^="/sessions/"]').first().click();

    // exact, because "Outings" and "Trend by outing" are headings on the page
    // we just left and a substring match resolves to both.
    await page.waitForURL(/\/sessions\//);
    await expect(page.getByRole("heading", { name: "Outing", exact: true }))
      .toBeVisible({ timeout: 15_000 });
    await settle(page);
    await shot(page, "session-detail");

    // One pitch. The row click is how the page is actually used.
    const rows = page.locator("tbody tr");
    await rows.nth(3).click();

    const verdict = page.getByRole("heading", { name: "Predicted vs actual" });
    await expect(verdict).toBeVisible({ timeout: 15_000 });
    await verdict.scrollIntoViewIfNeeded();
    await settle(page);
    // The evidence for the central claim: a prediction made before the
    // outcome, shown next to the outcome. Captured as the panel it is, so the
    // percentages are legible in the README rather than a fifth of a
    // screenshot.
    await panelShot(page, /Pitch detail/i, "prediction-vs-actual");
    await shot(page, "session-detail-with-pitch");

    // And the attribution behind it. The first call builds the explainer, so
    // this is the slowest thing in the capture run.
    await page.getByRole("button", { name: /Explain this pitch/i }).click();
    await expect(page.getByText(/in .* space/)).toBeVisible({ timeout: 40_000 });
    await settle(page);
    await panelShot(page, /Pitch detail/i, "pitch-explained");
  });
});
