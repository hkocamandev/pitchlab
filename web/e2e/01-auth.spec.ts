import { expect, test } from "@playwright/test";

import { shot, signIn } from "./helpers";

test.describe("authentication", () => {
  // The sign-in card is the only thing on the page, so the full dashboard
  // viewport would frame it in a field of empty background. A tighter window
  // is the honest crop: it is the same page, just not mostly nothing.
  test.use({ viewport: { width: 880, height: 560 } });

  test("the login page", async ({ page }) => {
    await page.goto("/");
    await expect(page.getByRole("button", { name: /^sign in$/i })).toBeVisible();
    await shot(page, "auth-login");
  });

  test("a wrong password gets one message", async ({ page }) => {
    await page.goto("/");
    await page.getByLabel(/username/i).fill("admin");
    await page.getByLabel(/password/i).fill("not-the-password");
    await page.getByRole("button", { name: /^sign in$/i }).click();

    const alert = page.getByRole("alert");
    await expect(alert).toBeVisible();
    // The captured image is the evidence for a claim in the README: the same
    // message for every kind of failure, so it cannot be used to find out
    // whether an account exists.
    await expect(alert).toHaveText(/did not match/i);
    await shot(page, "auth-login-rejected");
  });

  test("the dashboard behind the session", async ({ page }) => {
    // Back to the dashboard's own size: this one is a picture of the
    // application, not of a form.
    await page.setViewportSize({ width: 1280, height: 860 });
    await signIn(page);
    await expect(page.locator(".session-badge")).toContainText("admin");
    await shot(page, "auth-signed-in");
  });
});
