import { test } from "@playwright/test";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { SHOTS } from "./helpers";

const here = path.dirname(fileURLToPath(import.meta.url));

/** Some of the evidence is not a web page.
 *
 * The authorisation boundary and the dead-letter queue are shown by what the
 * server and the tools actually say, so the capture is of real captured
 * output rendered legibly -- not a mock-up of a terminal, and not a claim
 * about output that was never produced. The text files next to this spec are
 * the verbatim runs. */
function renderTerminal(title: string, body: string): string {
  const escape = (s: string) =>
    s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");

  // Lines beginning with $ are commands and lines beginning with # are
  // narration; colouring them apart is what makes a wall of output readable.
  const lines = escape(body)
    .split("\n")
    .map((line) => {
      if (line.startsWith("$ ")) return `<span class="cmd">${line}</span>`;
      if (line.startsWith("#")) return `<span class="note">${line}</span>`;
      if (/\b(401|403|429)\b/.test(line)) return `<span class="bad">${line}</span>`;
      if (/\b(200|101)\b/.test(line)) return `<span class="good">${line}</span>`;
      if (/REFUSED|ERROR/.test(line)) return `<span class="warn">${line}</span>`;
      return line;
    })
    .join("\n");

  return `<!doctype html><meta charset="utf-8"><style>
    :root { color-scheme: dark; }
    body { margin: 0; background: #0f1419; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
    .frame { margin: 20px; border: 1px solid #2a3441; border-radius: 10px; overflow: hidden; background: #11161d; }
    .bar { display: flex; align-items: center; gap: 8px; padding: 10px 14px; background: #171d26; border-bottom: 1px solid #2a3441; }
    .dot { width: 11px; height: 11px; border-radius: 50%; }
    .t { margin-left: 8px; color: #8b98a5; font-size: 12px; letter-spacing: .02em; }
    pre { margin: 0; padding: 18px 20px; color: #e6edf3; font-size: 13px; line-height: 1.55; white-space: pre-wrap; }
    .cmd { color: #4a9eff; } .note { color: #8b98a5; }
    .good { color: #3fb950; } .bad { color: #f85149; } .warn { color: #d29922; }
  </style>
  <div class="frame">
    <div class="bar">
      <span class="dot" style="background:#f85149"></span>
      <span class="dot" style="background:#d29922"></span>
      <span class="dot" style="background:#3fb950"></span>
      <span class="t">${escape(title)}</span>
    </div>
    <pre>${lines}</pre>
  </div>`;
}

for (const [file, title, name] of [
  ["auth.txt", "the authorisation boundary", "evidence-auth"],
  ["dlq.txt", "one poison message, and where it ends up", "evidence-dlq"],
] as const) {
  test(`terminal capture: ${name}`, async ({ page }) => {
    const body = fs.readFileSync(path.join(here, file), "utf8").trimEnd();
    await page.setContent(renderTerminal(title, body));
    await page.locator(".frame").screenshot({
      path: path.join(SHOTS, `${name}.png`),
    });
  });
}
