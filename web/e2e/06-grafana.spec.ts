import { expect, test } from "@playwright/test";
import path from "node:path";

import { SHOTS, settle } from "./helpers";

/** The dashboards are provisioned from the repository, so what is captured
 * here is what anybody gets from `make observability` -- not a view somebody
 * arranged by hand for a screenshot. */
const GRAFANA = process.env.PITCHLAB_GRAFANA_URL ?? "http://localhost:3000";

const dashboards = [
  ["pitchlab-pipeline", "grafana-pipeline"],
  ["pitchlab-ml", "grafana-model-serving"],
  ["pitchlab-system", "grafana-system-health"],
  ["pitchlab-live", "grafana-live-channel"],
] as const;

for (const [uid, name] of dashboards) {
  test(`grafana: ${name}`, async ({ page }) => {
    test.setTimeout(120_000);

    // Fifteen minutes, not an hour. The window has to cover the run being
    // shown and not much else: a wider one drags in whatever the machine was
    // doing earlier, which for a screenshot means documenting the person
    // taking it rather than the system.
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(
      `${GRAFANA}/d/${uid}?orgId=1&from=now-15m&to=now&kiosk`,
      { waitUntil: "networkidle" },
    );

    // Panels render asynchronously after the query returns; the title is the
    // first thing to appear and the last thing to be worth waiting on.
    await expect(page.locator(".react-grid-item").first()).toBeVisible({
      timeout: 60_000,
    });
    await settle(page, 6000);

    await page.screenshot({ path: path.join(SHOTS, `${name}.png`) });
  });
}
