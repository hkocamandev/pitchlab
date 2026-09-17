import { expect, type Page } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// The package is ESM, so there is no __dirname. Deriving it from import.meta
// keeps every path in this file relative to the repository rather than to
// whatever directory the capture was started from.
const here = path.dirname(fileURLToPath(import.meta.url));

/** Where the published images land. Committed, unlike the raw Playwright
 * output next to it. */
export const SHOTS = path.resolve(here, "../../screenshots");

/** credentials reads the same .env the stack was started with.
 *
 * Not hard-coded, and not a fixture user either: these captures are meant to
 * show the real login working, so they use the real one. There is no default
 * to fall back on -- a capture that silently signed in with "admin/admin"
 * would be documenting something that does not exist. */
export function credentials(): { username: string; password: string } {
  const envPath = path.resolve(here, "../../.env");
  const raw = fs.readFileSync(envPath, "utf8");

  const read = (key: string): string => {
    const line = raw.split("\n").find((l) => l.startsWith(`${key}=`));
    return line ? line.slice(key.length + 1).trim() : "";
  };

  const username = read("PITCHLAB_ADMIN_USER") || "admin";
  const password = read("PITCHLAB_ADMIN_PASSWORD");
  if (!password) {
    throw new Error(
      "PITCHLAB_ADMIN_PASSWORD is not in .env; run `make secrets` and `make seed`",
    );
  }
  return { username, password };
}

export async function signIn(page: Page): Promise<void> {
  const { username, password } = credentials();

  await page.goto("/");
  await page.getByLabel(/username/i).fill(username);
  await page.getByLabel(/password/i).fill(password);
  await page.getByRole("button", { name: /^sign in$/i }).click();

  // The session badge only renders once /auth/me has answered, so waiting for
  // it is waiting for a real session rather than for a timeout.
  await expect(page.locator(".session-badge")).toBeVisible({ timeout: 15_000 });
}

/** shot writes a screenshot straight to screenshots/ under a stable name.
 *
 * Stable because README links to these paths: a name that carried a timestamp
 * or a run index would break every link on the next capture. */
export async function shot(page: Page, name: string): Promise<void> {
  await page.screenshot({ path: path.join(SHOTS, `${name}.png`) });
}

/** panelShot captures one panel rather than the whole window.
 *
 * A full-window screenshot of a dashboard is mostly the parts you are not
 * talking about. When the README makes a claim about one panel -- the
 * attribution, the predicted-vs-actual bars -- the picture should be of that
 * panel, at a size where the numbers can be read. */
export async function panelShot(
  page: Page, heading: string | RegExp, name: string,
): Promise<void> {
  const panel = page.locator(".panel").filter({ has: page.getByRole("heading", { name: heading }) });
  await panel.first().scrollIntoViewIfNeeded();
  await page.waitForTimeout(400);
  await panel.first().screenshot({ path: path.join(SHOTS, `${name}.png`) });
}

/** saveVideo writes the recording under a stable name.
 *
 * Playwright clears its output directory on every run, so a video left where
 * it was recorded survives exactly until the next capture. It also only
 * finalises the file once the page is closed, which is why this closes the
 * page itself rather than leaving it to the fixture. */
export async function saveVideo(page: Page, name: string): Promise<void> {
  const video = page.video();
  if (!video) throw new Error("video recording is not enabled");
  await page.close();
  await video.saveAs(path.join(SHOTS, `${name}.webm`));
}

/** settle waits for the charts to finish drawing.
 *
 * Recharts animates on mount, and a screenshot taken during the animation
 * catches half-drawn bars -- which looks like a rendering bug rather than a
 * chart. */
export async function settle(page: Page, ms = 1200): Promise<void> {
  await page.waitForTimeout(ms);
}
